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
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestUtimensatEmptyName checks that an empty name sets the times of dirFd
// itself, for files and directories.
func TestUtimensatEmptyName(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		path  string
		flags int
	}{
		{name: "file", path: file, flags: unix.O_RDWR},
		{name: "directory", path: dir, flags: unix.O_RDONLY | unix.O_DIRECTORY},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd, err := unix.Open(tc.path, tc.flags, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			atime := unix.Timespec{Sec: 1577934000, Nsec: 1}
			mtime := unix.Timespec{Sec: 1577934245, Nsec: 123456789}
			if err := Utimensat(fd, "", [2]unix.Timespec{atime, mtime}, 0); err != nil {
				t.Fatalf("Utimensat: %v", err)
			}
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				t.Fatal(err)
			}
			if st.Atim != atime || st.Mtim != mtime {
				t.Errorf("atime, mtime = %v, %v; want %v, %v", st.Atim, st.Mtim, atime, mtime)
			}
		})
	}
}
