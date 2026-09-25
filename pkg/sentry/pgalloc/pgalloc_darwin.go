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

//go:build darwin
// +build darwin

package pgalloc

import (
	"unsafe"

	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/safemem"
)

// madviseHugepage is a no-op on darwin (no huge page madvise).
func madviseHugepage(addr, length uintptr) {}

// madviseNohugepage is a no-op on darwin.
func madviseNohugepage(addr, length uintptr) {}

// madvisePopulateWrite is not supported on darwin.
func madvisePopulateWrite(b safemem.Block) unix.Errno {
	return unix.ENOSYS
}

// fallocateCommit is a no-op on darwin (fallocate doesn't exist).
// On macOS, file space is allocated on write.
func fallocateCommit(fd int, off, length int64) error {
	return nil
}

// fallocateDecommit deallocates a range of the file using F_PUNCHHOLE
// on macOS. This is equivalent to Linux's fallocate(PUNCH_HOLE) —
// it zeroes the region and releases the underlying storage. Available
// on APFS since macOS 10.12.
func fallocateDecommit(fd int, off, length int64) error {
	type fpunchhole struct {
		Flags    uint32
		Reserved uint32
		Offset   int64
		Length   int64
	}
	ph := fpunchhole{Offset: off, Length: length}
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), 99, uintptr(unsafe.Pointer(&ph)))
	if errno != 0 {
		return errno
	}
	return nil
}

// mmapChunks maps MemoryFile chunks with MAP_SHARED. HVF's stage-2
// translation follows this mapping (the file's VM object), so pages the guest
// maps stay coherent with the sentry's view across host paging and hole
// punching.
func mmapChunks(fd uintptr, size, offset uintptr) (uintptr, error) {
	m, _, errno := unix.Syscall6(
		unix.SYS_MMAP,
		0, size,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED, fd, offset)
	if errno != 0 {
		return 0, errno
	}
	return m, nil
}
