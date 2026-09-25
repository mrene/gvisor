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
	"fmt"

	"golang.org/x/sys/unix"
)

// STATX constants used by the lisafs protocol. On Linux these come from
// golang.org/x/sys/unix.
const (
	statxMode  = unix.STATX_MODE
	statxUID   = unix.STATX_UID
	statxGID   = unix.STATX_GID
	statxSize  = unix.STATX_SIZE
	statxAtime = unix.STATX_ATIME
	statxMtime = unix.STATX_MTIME
)

// errREMOTEIO is the errno used for panics in RPC handlers. On Linux this is
// EREMOTEIO.
const errREMOTEIO = unix.EREMOTEIO

// hostErrnoToLinux returns the Linux errno, which the protocol carries, for
// host errno e. On Linux they are the same.
func hostErrnoToLinux(e unix.Errno) unix.Errno {
	return e
}

// waitHangup blocks until the socket fd is shut down locally or its peer
// hangs up.
func waitHangup(fd int) error {
	events := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLHUP | unix.POLLRDHUP}}
	for {
		n, err := unix.Ppoll(events, nil, nil)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("got %d events, wanted 1", n)
		}
		return nil
	}
}
