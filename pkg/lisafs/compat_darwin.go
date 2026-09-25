// Copyright 2021 The gVisor Authors.
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

package lisafs

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/syserr"
)

// STATX constants used by the lisafs protocol. These are protocol-level
// constants with the same values as on Linux, used to interpret wire messages
// rather than being passed to macOS syscalls.
const (
	statxMode  = 0x2
	statxUID   = 0x8
	statxGID   = 0x10
	statxSize  = 0x200
	statxAtime = 0x20
	statxMtime = 0x40
)

// errREMOTEIO is the errno used for panics in RPC handlers. EREMOTEIO does not
// exist on macOS; use EIO as a fallback.
const errREMOTEIO = unix.EIO

// hostErrnoToLinux returns the Linux errno, which the protocol carries, for
// host errno e. macOS and Linux errno values differ from 35 (EAGAIN) up.
func hostErrnoToLinux(e unix.Errno) unix.Errno {
	if !syserr.IsValid(e) {
		return unix.EIO // Same value on Linux.
	}
	return unix.Errno(syserr.FromHost(e).ToLinux())
}

// waitHangup blocks until the socket fd is shut down locally or its peer
// hangs up. poll(2) cannot do this on macOS: when only POLLHUP is requested, a
// hangup that follows unread data (e.g. an RPC response not yet read by its
// caller) is never reported. EVFILT_READ reports EV_EOF on hangup whether or
// not data is queued; EV_CLEAR limits wakeups to receive buffer changes.
func waitHangup(fd int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)
	var ev [1]unix.Kevent_t
	unix.SetKevent(&ev[0], fd, unix.EVFILT_READ, unix.EV_ADD|unix.EV_CLEAR)
	if _, err := unix.Kevent(kq, ev[:], nil, nil); err != nil {
		return err
	}
	for {
		n, err := unix.Kevent(kq, nil, ev[:], nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 1 && ev[0].Flags&unix.EV_ERROR != 0 {
			return unix.Errno(ev[0].Data)
		}
		if n == 1 && ev[0].Flags&unix.EV_EOF != 0 {
			return nil
		}
	}
}
