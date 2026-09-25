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

package lisafs

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux/errno"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

// TestExtractErrnoIsLinux checks that errors are sent as Linux errnos, which
// differ from host errnos on some hosts (e.g. ENOTEMPTY is 66 on macOS).
func TestExtractErrnoIsLinux(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want errno.Errno
	}{
		{name: "host ENOENT", err: unix.ENOENT, want: errno.ENOENT},
		{name: "host ENOTEMPTY", err: unix.ENOTEMPTY, want: errno.ENOTEMPTY},
		{name: "host ELOOP", err: unix.ELOOP, want: errno.ELOOP},
		{name: "host ENAMETOOLONG", err: unix.ENAMETOOLONG, want: errno.ENAMETOOLONG},
		{name: "host ENOSYS", err: unix.ENOSYS, want: errno.ENOSYS},
		{name: "wrapped host errno", err: &os.PathError{Op: "rmdir", Path: "d", Err: unix.ENOTEMPTY}, want: errno.ENOTEMPTY},
		{name: "sentry error", err: linuxerr.ENOTEMPTY, want: errno.ENOTEMPTY},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractErrno(tc.err); got != unix.Errno(tc.want) {
				t.Errorf("ExtractErrno(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
