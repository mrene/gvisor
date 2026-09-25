// Copyright 2026 The gVisor Authors.
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
	"bytes"
	"io"
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

func newTestMemoryFile(t *testing.T) *pgalloc.MemoryFile {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "memfile")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		t.Fatalf("NewMemoryFile: %v", err)
	}
	t.Cleanup(mf.Destroy)
	return mf
}

// cachedBytes returns the contents of s in mr, which must be cached.
func cachedBytes(t *testing.T, s *FileRangeSet, mf *pgalloc.MemoryFile, mr memmap.MappableRange) []byte {
	t.Helper()
	buf := make([]byte, mr.Length())
	for off := mr.Start; off < mr.End; off += hostarch.GuestPageSize {
		seg := s.FindSegment(off)
		if !seg.Ok() {
			t.Fatalf("offset %#x is not cached: %v", off, s)
		}
		ims, err := mf.MapInternal(seg.FileRangeOf(memmap.MappableRange{Start: off, End: off + hostarch.GuestPageSize}), hostarch.Read)
		if err != nil {
			t.Fatalf("MapInternal: %v", err)
		}
		dst := buf[off-mr.Start : off-mr.Start+hostarch.GuestPageSize]
		if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(dst)), ims); err != nil {
			t.Fatalf("CopySeq: %v", err)
		}
	}
	return buf
}

// TestFillGuestPageRanges checks that filling guest-page-aligned ranges that
// are not host-page-aligned, next to and overlapping existing segments,
// caches whole host pages with the file's contents, zero-filled past EOF, and
// allocates as many pages as PagesToFill predicts.
func TestFillGuestPageRanges(t *testing.T) {
	const (
		gpg = hostarch.GuestPageSize
		hpg = hostarch.PageSize
	)
	mf := newTestMemoryFile(t)
	ctx := context.Background()

	// The file ends in the middle of its fourth host page.
	fileSize := uint64(3*hpg + gpg)
	contents := make([]byte, fileSize)
	for i := range contents {
		contents[i] = byte(i/int(gpg)) + 1
	}
	readAt := func(ctx context.Context, dsts safemem.BlockSeq, offset uint64) (uint64, error) {
		return safemem.CopySeq(dsts, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(contents[offset:])))
	}

	var s FileRangeSet
	defer s.DropAll(mf)
	fill := func(start, end uint64, wantErr error) {
		t.Helper()
		mr := memmap.MappableRange{Start: start, End: end}
		want := s.PagesToFill(mr, mr)
		got, err := s.Fill(ctx, mr, mr, fileSize, mf, pgalloc.AllocOpts{Kind: usage.PageCache}, readAt)
		if err != wantErr {
			t.Fatalf("Fill(%v): got error %v, want %v", mr, err, wantErr)
		}
		if got != want {
			t.Errorf("Fill(%v) allocated %d pages, PagesToFill returned %d", mr, got, want)
		}
	}
	// A guest page in the second host page, then guest pages straddling the
	// first and second host pages, then the guest page containing EOF.
	fill(hpg+gpg, hpg+2*gpg, nil)
	fill(hpg-gpg, hpg+gpg, nil)
	fill(3*hpg, 3*hpg+gpg, io.EOF)

	for seg := s.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		if !hostarch.IsPageAligned(seg.Start()) || !hostarch.IsPageAligned(seg.End()) {
			t.Errorf("segment %v is not host-page-aligned", seg.Range())
		}
	}
	if got, want := cachedBytes(t, &s, mf, memmap.MappableRange{Start: 0, End: 2 * hpg}), contents[:2*hpg]; !bytes.Equal(got, want) {
		t.Errorf("cached contents of the first two host pages differ from the file")
	}
	if s.FindSegment(2 * hpg).Ok() {
		t.Errorf("unrequested host page at %#x is cached", 2*hpg)
	}
	last := cachedBytes(t, &s, mf, memmap.MappableRange{Start: 3 * hpg, End: 4 * hpg})
	if !bytes.Equal(last[:gpg], contents[3*hpg:]) {
		t.Errorf("cached contents before EOF differ from the file")
	}
	if !bytes.Equal(last[gpg:], make([]byte, hpg-gpg)) {
		t.Errorf("cached contents after EOF are not zero")
	}

	// Writeback of the whole dirty last host page stops at EOF.
	var dirty DirtySet
	dirty.MarkDirty(memmap.MappableRange{Start: 3 * hpg, End: 4 * hpg})
	var writtenEnd uint64
	writeAt := func(ctx context.Context, srcs safemem.BlockSeq, offset uint64) (uint64, error) {
		writtenEnd = max(writtenEnd, offset+srcs.NumBytes())
		return srcs.NumBytes(), nil
	}
	if err := SyncDirtyAll(ctx, &s, &dirty, fileSize, mf, writeAt); err != nil {
		t.Fatalf("SyncDirtyAll: %v", err)
	}
	if writtenEnd != fileSize {
		t.Errorf("writeback ended at %#x, want file size %#x", writtenEnd, fileSize)
	}
}
