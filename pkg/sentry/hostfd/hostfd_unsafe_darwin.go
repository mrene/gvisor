// Copyright 2020 The gVisor Authors.
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

package hostfd

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	sizeofIovec  = unsafe.Sizeof(unix.Iovec{})
	sizeofMsghdr = unsafe.Sizeof(unix.Msghdr{})
)

// iovecsReadWrite performs a multi-iovec read or write. If offset is -1, it
// uses readv/writev at the host file description's offset, so that each call
// (of up to MaxReadWriteIov iovecs) is a single host I/O, e.g. atomic with
// respect to O_APPEND. Otherwise, it uses sequential pread/pwrite calls, since
// macOS does not have preadv2/pwritev2.
func iovecsReadWrite(read bool, fd int32, iovs []unix.Iovec, offset int64) (uintptr, unix.Errno) {
	if offset == -1 {
		sysno := uintptr(unix.SYS_WRITEV)
		if read {
			sysno = unix.SYS_READV
		}
		return iovecsReadvWritev(sysno, fd, iovs)
	}
	var total uintptr
	for i := range iovs {
		iov := &iovs[i]
		curOff := offset + int64(total)
		var (
			cur uintptr
			e   unix.Errno
		)
		if read {
			cur, _, e = unix.Syscall6(unix.SYS_PREAD, uintptr(fd), uintptr(unsafe.Pointer(iov.Base)), uintptr(iov.Len), uintptr(curOff), 0, 0)
		} else {
			cur, _, e = unix.Syscall6(unix.SYS_PWRITE, uintptr(fd), uintptr(unsafe.Pointer(iov.Base)), uintptr(iov.Len), uintptr(curOff), 0, 0)
		}
		if cur > 0 {
			total += cur
		}
		if e != 0 {
			return total, e
		}
		// Short read/write: stop early.
		if uint64(cur) < iov.Len {
			return total, 0
		}
	}
	return total, 0
}

// iovecsReadvWritev performs readv or writev (sysno) on fd at the host file
// description's offset, in chunks of at most MaxReadWriteIov iovecs.
func iovecsReadvWritev(sysno uintptr, fd int32, iovs []unix.Iovec) (uintptr, unix.Errno) {
	var total uintptr
	for start := 0; start < len(iovs); start += MaxReadWriteIov {
		size := len(iovs) - start
		if size > MaxReadWriteIov {
			size = MaxReadWriteIov
		}
		cur, _, e := unix.Syscall(sysno, uintptr(fd), uintptr(unsafe.Pointer(&iovs[start])), uintptr(size))
		if e != 0 {
			return total, e
		}
		total += cur
		// Short read/write: stop early.
		var curTotal uint64
		for _, iov := range iovs[start : start+size] {
			curTotal += iov.Len
		}
		if uint64(cur) < curTotal {
			break
		}
	}
	return total, 0
}
