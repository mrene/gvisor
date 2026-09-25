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
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sync"
)

// addressSpace implements platform.AddressSpace using per-MM page tables.
//
// Each address space has its own ARM64 L2+L3 page table tree. The guest
// MMU translates VA → IPA using these tables (stage 1). HVF then
// translates IPA → HPA using its own stage 2 tables (managed via
// hv_vm_map). This enables fork+exec: different address spaces can
// map the same VA to different physical pages.
type addressSpace struct {
	platform.NoAddressSpaceIO

	mu      sync.Mutex
	machine *machine
	pt      *guestPageTable

	// stale holds IPA references dropped from pt that are released by
	// releaseStaleLocked. It is reused across calls to avoid allocation.
	// Protected by mu.
	stale []uint64
}

// hvfPageSize is the page size for HVF mappings on macOS ARM64.
// Defaults to 16K; set to 4K via setGuestPageSize(4096) for --page4k.
var hvfPageSize uintptr = 16384

func setGuestPageSize(size uintptr) {
	hvfPageSize = size
	if size == 4096 {
		initPageTableFor4K()
	}
}

func newAddressSpace(m *machine) (*addressSpace, error) {
	pt, err := newGuestPageTable(m)
	if err != nil {
		return nil, err
	}
	return &addressSpace{
		machine: m,
		pt:      pt,
	}, nil
}

// MapFile implements platform.AddressSpace.MapFile.
func (as *addressSpace) MapFile(addr hostarch.Addr, f memmap.File, fr memmap.FileRange,
	at hostarch.AccessType, precommit bool) error {

	as.mu.Lock()
	defer as.mu.Unlock()

	// Get the host virtual address mappings for this file region.
	bs, err := f.MapInternal(fr, hostarch.AccessType{
		Read:  at.Read || at.Execute || precommit,
		Write: at.Write,
	})
	if err != nil {
		return err
	}

	// Map each block: assign IPA via allocator, then update page table.
	// MemoryFile pages already have an IPA: see memFileMapper.
	// Track mapped bytes for rollback on error (including partial blocks).
	memFile := &as.machine.memFile
	inMemFile := memFile.mf != nil && f == memmap.File(memFile.mf)
	startAddr := addr
	fileOff := fr.Start
	var mappedBytes uint64
	flush := false
	for !bs.IsEmpty() {
		b := bs.Head()
		bs = bs.Tail()

		bLen := uintptr(b.Len())
		gva := uintptr(addr)
		srcAddr := uintptr(b.Addr())
		pageSz := uintptr(hvfPageSize)

		var memFileIPA uint64
		if inMemFile {
			memFileIPA, err = memFile.mapRange(memmap.FileRange{fileOff, fileOff + uint64(bLen)})
			if err != nil {
				as.unmapLocked(startAddr, mappedBytes)
				return err
			}
			if at.Execute {
				start := srcAddr &^ (pageSz - 1)
				memFile.prepareExec(start, (srcAddr+bLen+pageSz-1)&^(pageSz-1)-start)
			}
		}
		for off := uintptr(0); off < bLen; off += pageSz {
			pageGVA := (gva + off) &^ (pageSz - 1)

			var ipa uint64
			if inMemFile {
				ipa = memFileIPA + uint64(off)
			} else {
				ipa, err = as.machine.ipaAlloc.mapPage((srcAddr+off)&^(pageSz-1), pageSz)
				if err != nil {
					as.unmapLocked(startAddr, mappedBytes)
					return err
				}
				if at.Execute {
					as.machine.ipaAlloc.prepareExec(ipa)
				}
			}

			stale, staleFlush, err := as.pt.mapPage(uint64(pageGVA), ipa, at)
			if err != nil {
				as.machine.ipaAlloc.unmapIPA(ipa)
				as.unmapLocked(startAddr, mappedBytes)
				return err
			}
			if stale != 0 {
				as.stale = append(as.stale, stale)
				flush = flush || staleFlush
			}
			mappedBytes += uint64(pageSz)
		}

		addr += hostarch.Addr(bLen)
		fileOff += uint64(bLen)
	}
	as.releaseStaleLocked(flush)

	return nil
}

// Unmap implements platform.AddressSpace.Unmap.
func (as *addressSpace) Unmap(addr hostarch.Addr, length uint64) {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.unmapLocked(addr, length)
}

// unmapLocked clears PTEs and releases IPA mappings, along with any
// references pending in as.stale.
// For large ranges (>1GB), uses the page table's internal structure
// to skip unmapped regions instead of iterating every page.
func (as *addressSpace) unmapLocked(addr hostarch.Addr, length uint64) {
	end := uint64(addr) + length
	// For ranges larger than 1GB, iterate only mapped L3 entries
	// to avoid O(n) iteration over sparse address spaces.
	if length > 1<<30 {
		as.stale = as.pt.unmapRange(uint64(addr), end, as.stale)
	} else {
		for off := uint64(0); off < length; off += uint64(hvfPageSize) {
			if ipa := as.pt.unmapPage(uint64(addr) + off); ipa != 0 {
				as.stale = append(as.stale, ipa)
			}
		}
	}
	as.releaseStaleLocked(true /* flush */)
}

// releaseStaleLocked releases the IPA references in as.stale. If flush is
// set, vCPUs may hold translations from the cleared or replaced PTEs, and
// they are flushed first: otherwise a thread still running on another vCPU
// could access a page after it has been freed and reused.
func (as *addressSpace) releaseStaleLocked(flush bool) {
	if len(as.stale) == 0 {
		return
	}
	if flush {
		as.machine.flushTLB(as)
	}
	for _, ipa := range as.stale {
		as.machine.ipaAlloc.unmapIPA(ipa)
	}
	as.stale = as.stale[:0]
}

// Release implements platform.AddressSpace.Release.
func (as *addressSpace) Release() {
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.pt != nil {
		as.pt.release()
	}
}

// PreFork implements platform.AddressSpace.PreFork.
func (as *addressSpace) PreFork() {}

// PostFork implements platform.AddressSpace.PostFork.
func (as *addressSpace) PostFork() {}
