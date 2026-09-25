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

package fsutil

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// TestMmapCachedFileGuestPageMappings checks that mapping references taken
// for a vma are released exactly when the vma is split at guest page
// boundaries and its pieces are removed separately, so that the file is
// neither closed while mapped nor kept open after its last mapping is gone.
func TestMmapCachedFileGuestPageMappings(t *testing.T) {
	const pg = hostarch.GuestPageSize
	file, err := os.CreateTemp(t.TempDir(), "mmap")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	fd, err := unix.Dup(int(file.Fd()))
	file.Close()
	if err != nil {
		t.Fatalf("Dup: %v", err)
	}
	var f MmapCachedFile
	f.SetFD(fd)
	t.Cleanup(func() {
		if fd := f.FD(); fd >= 0 {
			unix.Close(fd)
		}
	})

	ar := hostarch.AddrRange{Start: 0x100000, End: 0x100000 + 4*pg}
	f.AddMapping(ar, 0)
	// A second vma mapping only the first guest page.
	f.AddMapping(hostarch.AddrRange{Start: 0x200000, End: 0x200000 + pg}, 0)
	f.MappableRelease()

	// Split the first vma at guest page boundaries and remove the pieces.
	f.RemoveMapping(hostarch.AddrRange{Start: ar.Start, End: ar.Start + pg}, 0)
	f.RemoveMapping(hostarch.AddrRange{Start: ar.Start + pg, End: ar.Start + 3*pg}, pg)
	f.RemoveMapping(hostarch.AddrRange{Start: ar.Start + 3*pg, End: ar.End}, 3*pg)
	if f.FD() < 0 {
		t.Fatalf("file closed while a guest page is still mapped")
	}
	f.RemoveMapping(hostarch.AddrRange{Start: 0x200000, End: 0x200000 + pg}, 0)
	if f.FD() >= 0 {
		t.Errorf("file still open after its last mapping was removed")
	}
}

type countingCloser struct {
	closed int
}

func (c *countingCloser) Close() error {
	c.closed++
	return nil
}

// TestMmapFileRefsGuestPages checks that references on single guest pages
// keep the file open after MappableRelease until they are dropped.
func TestMmapFileRefsGuestPages(t *testing.T) {
	const pg = hostarch.GuestPageSize
	var c countingCloser
	r := MmapFileRefs{Closer: &c}
	r.IncRef(memmap.FileRange{Start: pg, End: 2 * pg}, 0)
	r.IncRef(memmap.FileRange{Start: 0, End: pg}, 0)
	r.MappableRelease()
	r.DecRef(memmap.FileRange{Start: pg, End: 2 * pg})
	if c.closed != 0 {
		t.Fatalf("file closed while a guest page is still referenced")
	}
	r.DecRef(memmap.FileRange{Start: 0, End: pg})
	if c.closed != 1 {
		t.Errorf("file closed %d times after its last reference was dropped, want 1", c.closed)
	}
}
