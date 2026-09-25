// Copyright 2024 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin && arm64

package hvf

/*
#include <Hypervisor/Hypervisor.h>
#include <libkern/OSCacheControl.h>
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sync"
)

// ipaAllocator manages the guest IPA (Intermediate Physical Address)
// space for the HVF VM below memFileIPABase. It assigns unique IPAs to host
// memory pages that are not in the sentry's MemoryFile (see memFileMapper),
// enabling per-process page tables where different VAs can map to
// different physical pages.
//
// hv_vm_map maps each host page itself, so the guest and the sentry access
// the same memory. HVF's stage-2 translation follows the host mapping (the
// VM object behind the host VA), not the physical pages it had when mapped:
// host and guest stay coherent across host paging, compression and hole
// punching, for MemoryFile (MAP_SHARED) and host file (MAP_PRIVATE, see
// fsutil.MmapCachedFile) mappings alike. However, replacing the host mapping
// behind a live IPA (munmap, or mmap with MAP_FIXED) leaves the guest with
// the old pages. IPAs are identified by host VA, so each IPA must be
// released before the host mapping of its page is replaced: callers hold
// references on the mapped memmap.File range for at least as long as their
// references on the IPA.
type ipaAllocator struct {
	mu      sync.Mutex
	nextIPA uint64
	// hostToIPA maps host page address → assigned IPA.
	hostToIPA map[uintptr]uint64
	// refCount tracks how many references exist to each IPA.
	refCount map[uint64]int
	// freeIPAs holds IPAs that were unmapped and can be reused,
	// keyed by page size. Prevents nextIPA from growing without bound.
	freeIPAs map[uintptr][]uint64
	// ipaToHost is the reverse of hostToIPA for O(1) cleanup.
	ipaToHost map[uint64]uintptr
	// ipaSize tracks the mapped size of each IPA for correct unmapping.
	ipaSize map[uint64]uintptr
	// icacheSynced holds IPAs whose page the instruction cache has been
	// made coherent with by prepareExec.
	icacheSynced map[uint64]struct{}
}

// ipaBase is the start of the allocatable IPA range. The first 16MB
// is reserved for vectors, page tables, and other fixed mappings.
// Page tables start at ptBase (0x10000) and each 16K page consumes
// one IPA slot, so 16MB allows ~1020 page table pages — enough for
// hundreds of concurrent processes.
const ipaBase = 0x1000000 // 16MB

// memFileIPABase is the end of the ipaAllocator range and the IPA of offset 0
// of the sentry's MemoryFile (see memFileMapper).
const memFileIPABase = 1 << 39 // 512GB

// ipaMax is the maximum IPA (40-bit, matching hv_vm_config IPA size).
const ipaMax = 1 << 40 // 1TB

func newIPAAllocator() *ipaAllocator {
	return &ipaAllocator{
		nextIPA:      ipaBase,
		hostToIPA:    make(map[uintptr]uint64),
		ipaToHost:    make(map[uint64]uintptr),
		ipaSize:      make(map[uint64]uintptr),
		refCount:     make(map[uint64]int),
		freeIPAs:     make(map[uintptr][]uint64),
		icacheSynced: make(map[uint64]struct{}),
	}
}

// mapPage ensures a host page is mapped in the HVF IPA space and
// returns its IPA. If already mapped, returns the existing IPA and
// increments the reference count.
func (a *ipaAllocator) mapPage(hostAddr uintptr, size uintptr) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if ipa, ok := a.hostToIPA[hostAddr]; ok {
		a.refCount[ipa]++
		return ipa, nil
	}

	// Prefer fresh IPAs until we hit 256GB, then reuse freed ones.
	// Early reuse causes stage-2 TLB staleness. Deferring reuse
	// until we've consumed significant IPA space gives HVF time
	// to flush stage-2 TLB entries for unmapped IPAs.
	var ipa uint64
	const ipaReuseThreshold = memFileIPABase / 2
	freeList := a.freeIPAs[size]
	if a.nextIPA < ipaReuseThreshold || len(freeList) == 0 {
		// Keep every IPA naturally aligned to its mapping size. With a 4K
		// guest granule, user pages advance nextIPA in 4K steps, while
		// per-vCPU state pages are 16K mappings translated through the
		// 16K-granule kernel page table. A misaligned 16K IPA would be
		// truncated by the kernel PTE and alias unrelated guest pages.
		ipa = (a.nextIPA + uint64(size) - 1) &^ (uint64(size) - 1)
		if ipa+uint64(size) > memFileIPABase {
			return 0, fmt.Errorf("IPA space exhausted (next=%#x, max=%#x)", a.nextIPA, uint64(memFileIPABase))
		}
		a.nextIPA = ipa + uint64(size)
	} else {
		ipa = freeList[len(freeList)-1]
		a.freeIPAs[size] = freeList[:len(freeList)-1]
	}

	ret := C.hv_vm_map(unsafe.Pointer(hostAddr), C.hv_ipa_t(ipa), C.size_t(size),
		C.HV_MEMORY_READ|C.HV_MEMORY_WRITE|C.HV_MEMORY_EXEC)
	if ret != C.HV_SUCCESS {
		return 0, fmt.Errorf("hv_vm_map(host=%#x, ipa=%#x, len=%d): %d", hostAddr, ipa, size, ret)
	}

	a.hostToIPA[hostAddr] = ipa
	a.ipaToHost[ipa] = hostAddr
	a.ipaSize[ipa] = size
	a.refCount[ipa] = 1
	return ipa, nil
}

// prepareExec makes the instruction cache coherent with the page mapped at
// ipa, which the guest is about to be allowed to execute. The page's current
// contents may have been written through its host mapping over instructions
// that are still cached, and CTR_EL0.DIC is 0. This is done once per IPA:
// later writes to a page that the guest may execute are the writer's
// responsibility (the guest's own cache maintenance for its stores; like
// Linux, the sentry does not synchronize existing executable mappings with
// file writes), and a freed page can only be reused under a new IPA.
//
// Preconditions: The caller holds a reference on ipa.
func (a *ipaAllocator) prepareExec(ipa uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.icacheSynced[ipa]; ok {
		return
	}
	C.sys_icache_invalidate(unsafe.Pointer(a.ipaToHost[ipa]), C.size_t(a.ipaSize[ipa]))
	a.icacheSynced[ipa] = struct{}{}
}

// unmapIPA decrements the refcount for an IPA mapping. When the
// refcount reaches zero, the IPA is unmapped from HVF's stage-2.
// Callers release IPAs only after no guest PTE references them and no vCPU
// can hold a stage-1 translation to them (see addressSpace.releaseStaleLocked).
// MemoryFile IPAs (see memFileMapper) are not reference counted, and are
// ignored.
func (a *ipaAllocator) unmapIPA(ipa uint64) {
	if ipa >= memFileIPABase {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.refCount[ipa]; !ok {
		return
	}
	a.refCount[ipa]--
	if a.refCount[ipa] <= 0 {
		size := a.ipaSize[ipa]
		if h, ok := a.ipaToHost[ipa]; ok {
			delete(a.hostToIPA, h)
			delete(a.ipaToHost, ipa)
		}
		delete(a.refCount, ipa)
		delete(a.ipaSize, ipa)
		delete(a.icacheSynced, ipa)

		// Force stage-2 TLB invalidation for this IPA before unmapping.
		// ARM64 architecture requires break-before-make: removing permissions
		// forces the kernel to issue TLBI for the affected IPA range.
		C.hv_vm_protect(C.hv_ipa_t(ipa), C.size_t(size), 0)
		C.hv_vm_unmap(C.hv_ipa_t(ipa), C.size_t(size))

		a.freeIPAs[size] = append(a.freeIPAs[size], ipa)
	}
}

// memFileMapper maps the sentry's main MemoryFile into the IPA space: each
// chunk of the file is mapped once, when a guest first maps a page in it, at
// memFileIPABase plus its file offset, and stays mapped for the VM's lifetime.
// Guest page tables map MemoryFile pages to these IPAs directly, without a
// hv_vm_map or ipaAllocator reference per page: a page that is freed keeps
// its IPA, and flushing the stage-1 translations to it (see
// addressSpace.releaseStaleLocked) is all that stops guests from reaching it.
// This relies on the host mapping of each chunk never being replaced while
// the VM runs (see pgalloc.MemoryFile.ChunkMapping).
type memFileMapper struct {
	// mf is the MemoryFile, or nil if there is none. mf is immutable once
	// address spaces exist.
	mf *pgalloc.MemoryFile

	mu sync.Mutex
	// mapped holds the file offsets of the chunks of mf that are mapped.
	mapped map[uint64]struct{}
}

// mapRange ensures that the chunks of m.mf spanning fr are mapped, and returns
// the IPA of fr.Start.
func (m *memFileMapper) mapRange(fr memmap.FileRange) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for off := fr.Start; off < fr.End; {
		chunk, host := m.mf.ChunkMapping(off)
		off = chunk.End
		if _, ok := m.mapped[chunk.Start]; ok {
			continue
		}
		ipa := memFileIPABase + chunk.Start
		if ipa+chunk.Length() > ipaMax {
			return 0, fmt.Errorf("MemoryFile chunk %v is beyond the IPA space", chunk)
		}
		ret := C.hv_vm_map(unsafe.Pointer(host), C.hv_ipa_t(ipa), C.size_t(chunk.Length()),
			C.HV_MEMORY_READ|C.HV_MEMORY_WRITE|C.HV_MEMORY_EXEC)
		if ret != C.HV_SUCCESS {
			return 0, fmt.Errorf("hv_vm_map(host=%#x, ipa=%#x, len=%d): %d", host, ipa, chunk.Length(), ret)
		}
		m.mapped[chunk.Start] = struct{}{}
	}
	return memFileIPABase + fr.Start, nil
}

// prepareExec makes the instruction cache coherent with the MemoryFile pages
// at host addresses [addr, addr+length), which the guest is about to be
// allowed to execute. Unlike ipaAllocator.prepareExec, this is needed every
// time: a freed MemoryFile page can be reused for other instructions under
// the same IPA.
func (m *memFileMapper) prepareExec(addr, length uintptr) {
	C.sys_icache_invalidate(unsafe.Pointer(addr), C.size_t(length))
}

// ptPageAllocator allocates page table pages in the HVF IPA space.
// Page table pages must be accessible to the guest MMU via IPAs.
// Released pages are recycled via a free list.
type ptPageAllocator struct {
	mu       sync.Mutex
	nextIPA  uint64
	freeList []ptPage // Recycled pages ready for reuse.
}

type ptPage struct {
	hostMem unsafe.Pointer
	ipa     uint64
	size    uint64
}

// ptBase is the start of the page-table IPA range (within the first 1MB).
const ptBase = 0x10000 // 64K, after vectors (0-16K) and old PT (16K-32K)

func newPTPageAllocator() *ptPageAllocator {
	return &ptPageAllocator{
		nextIPA: ptBase,
	}
}

// allocPage allocates a page table page and maps it into HVF.
// Uses 16K for kernel page tables (always 16K granule) when the
// caller's page table uses 16K entries. The size is determined by
// hvfPageSize for guest PTs, but kernel PTs always need 16K.
// It reuses pages from the free list when available.
func (p *ptPageAllocator) allocPage() (hostMem unsafe.Pointer, ipa uint64, err error) {
	return p.allocPageSize(uint64(hvfPageSize))
}

// allocKernelPage allocates a 16K page table page for kernel (TTBR1)
// page tables, which always use 16K granule regardless of hvfPageSize.
func (p *ptPageAllocator) allocKernelPage() (hostMem unsafe.Pointer, ipa uint64, err error) {
	return p.allocPageSize(16384)
}

func (p *ptPageAllocator) allocPageSize(size uint64) (hostMem unsafe.Pointer, ipa uint64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Try to reuse a free page of matching size.
	for i := len(p.freeList) - 1; i >= 0; i-- {
		if p.freeList[i].size == size {
			page := p.freeList[i]
			p.freeList = append(p.freeList[:i], p.freeList[i+1:]...)
			C.memset(page.hostMem, 0, C.size_t(size))
			return page.hostMem, page.ipa, nil
		}
	}

	var mem unsafe.Pointer
	if ret := C.posix_memalign(&mem, C.size_t(16384), C.size_t(size)); ret != 0 || mem == nil {
		return nil, 0, fmt.Errorf("posix_memalign failed for page table page (ret=%d)", ret)
	}
	C.memset(mem, 0, C.size_t(size))

	// Align nextIPA to the allocation size.
	aligned := (p.nextIPA + size - 1) &^ (size - 1)
	ipa = aligned
	if ipa+size > ipaBase {
		C.free(mem)
		return nil, 0, fmt.Errorf("page table IPA space exhausted (next=%#x, limit=%#x)", ipa, ipaBase)
	}
	p.nextIPA = ipa + size

	ret := C.hv_vm_map(mem, C.hv_ipa_t(ipa), C.size_t(size),
		C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	if ret != C.HV_SUCCESS {
		C.free(mem)
		return nil, 0, fmt.Errorf("hv_vm_map page table page: %d", ret)
	}

	return mem, ipa, nil
}

// freePage returns a page table page to the free list for reuse.
func (p *ptPageAllocator) freePage(hostMem unsafe.Pointer, ipa uint64, size uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.freeList = append(p.freeList, ptPage{hostMem: hostMem, ipa: ipa, size: size})
}
