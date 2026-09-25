// Copyright 2018 The gVisor Authors.
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

package mm

import (
	"bytes"
	"fmt"
	"testing"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/usermem"
)

func testMemoryManagerWithMmapDirection(ctx context.Context, t *testing.T, mmapDirection arch.MmapDirection) *MemoryManager {
	p := platform.FromContext(ctx)
	mm, err := NewMemoryManager(p, pgalloc.MemoryFileFromContext(ctx))
	if err != nil {
		t.Fatalf("failed to create MemoryManager: %s", err)
	}
	mm.layout = arch.MmapLayout{
		MinAddr:          p.MinUserAddress(),
		MaxAddr:          p.MaxUserAddress(),
		BottomUpBase:     p.MinUserAddress(),
		TopDownBase:      p.MaxUserAddress(),
		DefaultDirection: mmapDirection,
	}
	return mm
}

func testMemoryManager(ctx context.Context, t *testing.T) *MemoryManager {
	return testMemoryManagerWithMmapDirection(ctx, t, arch.MmapBottomUp)
}

func (mm *MemoryManager) realUsageAS() uint64 {
	return uint64(mm.vmas.Span())
}

func TestUsageASUpdates(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:  2 * hostarch.PageSize,
		Private: true,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	realUsage := mm.realUsageAS()
	if mm.usageAS != realUsage {
		t.Fatalf("usageAS believes %v bytes are mapped; %v bytes are actually mapped", mm.usageAS, realUsage)
	}

	mm.MUnmap(ctx, addr, hostarch.PageSize)
	realUsage = mm.realUsageAS()
	if mm.usageAS != realUsage {
		t.Fatalf("usageAS believes %v bytes are mapped; %v bytes are actually mapped", mm.usageAS, realUsage)
	}
}

func (mm *MemoryManager) realDataAS() uint64 {
	var sz uint64
	for seg := mm.vmas.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		vma := seg.ValuePtr()
		if vma.isPrivateDataLocked() {
			sz += uint64(seg.Range().Length())
		}
	}
	return sz
}

func TestDataASUpdates(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   3 * hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.Write,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	if mm.dataAS == 0 {
		t.Fatalf("dataAS is 0, wanted not 0")
	}
	realDataAS := mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}

	mm.MUnmap(ctx, addr, hostarch.PageSize)
	realDataAS = mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}

	mm.MProtect(addr+hostarch.PageSize, hostarch.PageSize, hostarch.Read, false)
	realDataAS = mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}

	mm.MRemap(ctx, addr+2*hostarch.PageSize, hostarch.PageSize, 2*hostarch.PageSize, MRemapOpts{
		Move: MRemapMayMove,
	})
	realDataAS = mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}
}

func TestBrkDataLimitUpdates(t *testing.T) {
	limitSet := limits.NewLimitSet()
	limitSet.Set(limits.Data, limits.Limit{}, true /* privileged */) // zero RLIMIT_DATA

	ctx := contexttest.WithLimitSet(contexttest.Context(t), limitSet)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	// Try to extend the brk by one page and expect doing so to fail.
	oldBrk, _ := mm.Brk(ctx, 0)
	if newBrk, _ := mm.Brk(ctx, oldBrk+hostarch.PageSize); newBrk != oldBrk {
		t.Errorf("brk() increased data segment above RLIMIT_DATA (old brk = %#x, new brk = %#x", oldBrk, newBrk)
	}
}

// TestIOAfterUnmap ensures that IO fails after unmap.
func TestIOAfterUnmap(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.Read,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}

	// IO works before munmap.
	b := make([]byte, 1)
	n, err := mm.CopyIn(ctx, addr, b, usermem.IOOpts{})
	if err != nil {
		t.Errorf("CopyIn got err %v want nil", err)
	}
	if n != 1 {
		t.Errorf("CopyIn got %d want 1", n)
	}

	err = mm.MUnmap(ctx, addr, hostarch.PageSize)
	if err != nil {
		t.Fatalf("MUnmap got err %v want nil", err)
	}

	n, err = mm.CopyIn(ctx, addr, b, usermem.IOOpts{})
	if !linuxerr.Equals(linuxerr.EFAULT, err) {
		t.Errorf("CopyIn got err %v want EFAULT", err)
	}
	if n != 0 {
		t.Errorf("CopyIn got %d want 0", n)
	}
}

// TestIOAfterMProtect tests IO interaction with mprotect permissions.
func TestIOAfterMProtect(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}

	// Writing works before mprotect.
	b := make([]byte, 1)
	n, err := mm.CopyOut(ctx, addr, b, usermem.IOOpts{})
	if err != nil {
		t.Errorf("CopyOut got err %v want nil", err)
	}
	if n != 1 {
		t.Errorf("CopyOut got %d want 1", n)
	}

	err = mm.MProtect(addr, hostarch.PageSize, hostarch.Read, false)
	if err != nil {
		t.Errorf("MProtect got err %v want nil", err)
	}

	// Without IgnorePermissions, CopyOut should no longer succeed.
	n, err = mm.CopyOut(ctx, addr, b, usermem.IOOpts{})
	if !linuxerr.Equals(linuxerr.EFAULT, err) {
		t.Errorf("CopyOut got err %v want EFAULT", err)
	}
	if n != 0 {
		t.Errorf("CopyOut got %d want 0", n)
	}

	// With IgnorePermissions, CopyOut should succeed despite mprotect.
	n, err = mm.CopyOut(ctx, addr, b, usermem.IOOpts{
		IgnorePermissions: true,
	})
	if err != nil {
		t.Errorf("CopyOut got err %v want nil", err)
	}
	if n != 1 {
		t.Errorf("CopyOut got %d want 1", n)
	}
}

// TestAIOPrepareAfterDestroy tests that AIOContext should not be able to be
// prepared after destruction.
func TestAIOPrepareAfterDestroy(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	id, err := mm.NewAIOContext(ctx, 1)
	if err != nil {
		t.Fatalf("mm.NewAIOContext got err %v want nil", err)
	}
	aioCtx, ok := mm.LookupAIOContext(ctx, id)
	if !ok {
		t.Fatalf("AIOContext not found")
	}
	mm.DestroyAIOContext(ctx, id)

	// Prepare should fail because aioCtx should be destroyed.
	if err := aioCtx.Prepare(); !linuxerr.Equals(linuxerr.EINVAL, err) {
		t.Errorf("aioCtx.Prepare got err %v want nil", err)
	} else if err == nil {
		aioCtx.CancelPendingRequest()
	}
}

// TestAIOLookupAfterDestroy tests that AIOContext should not be able to be
// looked up after memory manager is destroyed.
func TestAIOLookupAfterDestroy(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)

	id, err := mm.NewAIOContext(ctx, 1)
	if err != nil {
		mm.DecUsers(ctx)
		t.Fatalf("mm.NewAIOContext got err %v want nil", err)
	}
	mm.DecUsers(ctx) // This destroys the AIOContext manager.

	if _, ok := mm.LookupAIOContext(ctx, id); ok {
		t.Errorf("AIOContext found even after AIOContext manager is destroyed")
	}
}

func TestGetAllocationDirection(t *testing.T) {
	testCases := []struct {
		name          string
		mmapDirection arch.MmapDirection
		ar            hostarch.AddrRange
		vma           *vma
		expected      pgalloc.Direction
	}{
		{
			"No last fault in vma with mmap direction BottomUp",
			arch.MmapBottomUp,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 0},
			pgalloc.BottomUp,
		},
		{
			"No last fault in vma with mmap direction TopDown",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 0},
			pgalloc.TopDown,
		},
		{
			"Last fault in vma equals to addr range, with mmap direction BottomUp",
			arch.MmapBottomUp,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 123},
			pgalloc.BottomUp,
		},
		{
			"Last fault in vma equals to addr range, with mmap direction TopDown",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 123},
			pgalloc.TopDown,
		},
		{
			"Last fault in vma greater than addr range",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 456},
			pgalloc.TopDown,
		},
		{
			"Last fault in vma smaller than addr range",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 100},
			pgalloc.BottomUp,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			ctx := contexttest.Context(t)
			mm := testMemoryManagerWithMmapDirection(ctx, t, test.mmapDirection)
			actual := mm.getAllocationDirection(test.ar, test.vma)
			if actual != test.expected {
				t.Errorf("Unexpected allocation direction. Expected: %s, Actual: %s", test.expected, actual)
			}
		})
	}
}

// mapFilledGuestPages maps an anonymous private read-write region of n guest
// pages and fills guest page i with byte 0xa0+i.
func mapFilledGuestPages(ctx context.Context, t *testing.T, mm *MemoryManager, n int) hostarch.Addr {
	t.Helper()
	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   uint64(n) * hostarch.GuestPageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	for i := 0; i < n; i++ {
		b := bytes.Repeat([]byte{byte(0xa0 + i)}, hostarch.GuestPageSize)
		if _, err := mm.CopyOut(ctx, addr+hostarch.Addr(i)*hostarch.GuestPageSize, b, usermem.IOOpts{}); err != nil {
			t.Fatalf("CopyOut to guest page %d got err %v want nil", i, err)
		}
	}
	return addr
}

// checkGuestPage checks that every byte of the guest page at addr is want.
func checkGuestPage(ctx context.Context, t *testing.T, mm *MemoryManager, addr hostarch.Addr, want byte) {
	t.Helper()
	b := make([]byte, hostarch.GuestPageSize)
	if _, err := mm.CopyIn(ctx, addr, b, usermem.IOOpts{}); err != nil {
		t.Errorf("CopyIn at %#x got err %v want nil", addr, err)
		return
	}
	if !bytes.Equal(b, bytes.Repeat([]byte{want}, hostarch.GuestPageSize)) {
		t.Errorf("guest page at %#x starts with %#x, want every byte %#x", addr, b[0], want)
	}
}

// checkGuestPageUnmapped checks that the guest page at addr is not accessible.
func checkGuestPageUnmapped(ctx context.Context, t *testing.T, mm *MemoryManager, addr hostarch.Addr) {
	t.Helper()
	b := make([]byte, 1)
	if n, err := mm.CopyIn(ctx, addr, b, usermem.IOOpts{}); !linuxerr.Equals(linuxerr.EFAULT, err) || n != 0 {
		t.Errorf("CopyIn at %#x got (%d, %v) want (0, EFAULT)", addr, n, err)
	}
}

// TestMMapFixedGuestPageKeepsNeighbours checks that a guest-page-sized
// MAP_FIXED mapping replaces exactly the pages it covers, even when those
// pages share a host page with their neighbours.
func TestMMapFixedGuestPageKeepsNeighbours(t *testing.T) {
	const pg = hostarch.GuestPageSize
	for _, pages := range []int{1, 5} {
		t.Run(fmt.Sprintf("%d pages", pages), func(t *testing.T) {
			ctx := contexttest.Context(t)
			mm := testMemoryManager(ctx, t)
			defer mm.DecUsers(ctx)
			addr := mapFilledGuestPages(ctx, t, mm, 8)

			if _, err := mm.MMap(ctx, memmap.MMapOpts{
				Length:   uint64(pages) * pg,
				Addr:     addr + pg,
				Fixed:    true,
				Unmap:    true,
				Private:  true,
				Perms:    hostarch.ReadWrite,
				MaxPerms: hostarch.AnyAccess,
			}); err != nil {
				t.Fatalf("MMap(MAP_FIXED, %d pages) got err %v want nil", pages, err)
			}
			for i := 0; i < 8; i++ {
				want := byte(0xa0 + i)
				if i >= 1 && i <= pages {
					want = 0
				}
				checkGuestPage(ctx, t, mm, addr+hostarch.Addr(i)*pg, want)
			}
		})
	}
}

// TestMUnmapGuestPage checks that munmap of guest-page-granular ranges unmaps
// exactly those pages.
func TestMUnmapGuestPage(t *testing.T) {
	const pg = hostarch.GuestPageSize
	for _, pages := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d pages", pages), func(t *testing.T) {
			ctx := contexttest.Context(t)
			mm := testMemoryManager(ctx, t)
			defer mm.DecUsers(ctx)
			addr := mapFilledGuestPages(ctx, t, mm, 4)

			if err := mm.MUnmap(ctx, addr+pg, uint64(pages)*pg); err != nil {
				t.Fatalf("MUnmap(%d pages) got err %v want nil", pages, err)
			}
			if got, want := mm.realUsageAS(), uint64(4-pages)*pg; got != want {
				t.Errorf("mapped bytes got %d want %d", got, want)
			}
			checkGuestPage(ctx, t, mm, addr, 0xa0)
			for i := 1; i <= pages; i++ {
				checkGuestPageUnmapped(ctx, t, mm, addr+hostarch.Addr(i)*pg)
			}
			for i := pages + 1; i < 4; i++ {
				checkGuestPage(ctx, t, mm, addr+hostarch.Addr(i)*pg, byte(0xa0+i))
			}
		})
	}
}

// TestMMapFixedProtNoneGuestPage checks that a guest-page-sized PROT_NONE
// MAP_FIXED mapping makes the page inaccessible and discards its contents.
func TestMMapFixedProtNoneGuestPage(t *testing.T) {
	const pg = hostarch.GuestPageSize
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)
	addr := mapFilledGuestPages(ctx, t, mm, 4)

	if _, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   pg,
		Addr:     addr + pg,
		Fixed:    true,
		Unmap:    true,
		Private:  true,
		MaxPerms: hostarch.AnyAccess,
	}); err != nil {
		t.Fatalf("MMap(PROT_NONE, MAP_FIXED) got err %v want nil", err)
	}
	checkGuestPageUnmapped(ctx, t, mm, addr+pg)
	if err := mm.MProtect(addr+pg, pg, hostarch.ReadWrite, false); err != nil {
		t.Fatalf("MProtect got err %v want nil", err)
	}
	checkGuestPage(ctx, t, mm, addr, 0xa0)
	checkGuestPage(ctx, t, mm, addr+pg, 0)
	checkGuestPage(ctx, t, mm, addr+2*pg, 0xa2)
}

// TestMRemapGuestPage checks that mremap accepts guest-page-aligned ranges
// that are not host-page-aligned, and moves exactly those pages.
func TestMRemapGuestPage(t *testing.T) {
	const pg = hostarch.GuestPageSize
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)
	addr := mapFilledGuestPages(ctx, t, mm, 4)

	newAddr, err := mm.MRemap(ctx, addr+pg, pg, 2*pg, MRemapOpts{
		Move: MRemapMayMove,
	})
	if err != nil {
		t.Fatalf("MRemap got err %v want nil", err)
	}
	if newAddr == addr+pg {
		t.Fatalf("MRemap grew in place over mapped page %#x", addr+2*pg)
	}
	checkGuestPage(ctx, t, mm, newAddr, 0xa1)
	checkGuestPage(ctx, t, mm, newAddr+pg, 0)
	checkGuestPage(ctx, t, mm, addr, 0xa0)
	checkGuestPageUnmapped(ctx, t, mm, addr+pg)
	checkGuestPage(ctx, t, mm, addr+2*pg, 0xa2)
}
