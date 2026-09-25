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

package fsgofer_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/runsc/fsgofer"
)

func init() {
	if err := fsgofer.OpenProcSelfFD("/proc/self/fd"); err != nil {
		panic(err)
	}
}

// serve serves dir through the gofer and returns a client and its root FD.
func serve(t *testing.T, dir string) (*lisafs.Client, lisafs.ClientFD) {
	t.Helper()
	serverSock, clientSock, err := unet.SocketPair(false)
	if err != nil {
		t.Fatalf("SocketPair: %v", err)
	}
	server := lisafs.NewServer()
	conn, err := server.CreateConnection(serverSock, dir, lisafs.ConnectionOpts{}, fsgofer.NewConnectionImpl(&fsgofer.Config{}))
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	server.StartConnection(conn)
	c, root, _, err := lisafs.NewClient(clientSock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.StartChannels(); err != nil {
		t.Fatalf("StartChannels: %v", err)
	}
	rootFD := c.NewFD(root.ControlFD)
	t.Cleanup(func() {
		rootFD.Close(context.Background(), true /* flush */)
		server.Destroy()
		c.Close()
		server.Wait()
	})
	return c, rootFD
}

// TestOpenCreateWriteOnlyMode checks that open(O_CREAT|O_WRONLY) with a mode
// that denies writing still returns writable FDs, as open(2) does: the mode
// only applies to later opens. cp -p creates files this way before copying
// their contents.
func TestOpenCreateWriteOnlyMode(t *testing.T) {
	dir := t.TempDir()
	c, root := serve(t, dir)
	ctx := context.Background()

	child, openFD, hostFD, err := root.OpenCreateAt(ctx, "f", unix.O_WRONLY, 0400, lisafs.UID(unix.Getuid()), lisafs.GID(unix.Getgid()))
	if err != nil {
		t.Fatalf("OpenCreateAt(O_WRONLY, 0400): %v", err)
	}
	control := c.NewFD(child.ControlFD)
	defer control.Close(ctx, true /* flush */)
	fd := c.NewFD(openFD)
	defer fd.Close(ctx, true /* flush */)
	if hostFD < 0 {
		t.Fatalf("OpenCreateAt donated no host FD")
	}
	defer unix.Close(hostFD)

	if n, err := fd.Write(ctx, []byte("rpc,"), 0); err != nil || n != 4 {
		t.Errorf("PWrite RPC = (%d, %v), want (4, nil)", n, err)
	}
	if n, err := unix.Pwrite(hostFD, []byte("host"), 4); err != nil || n != 4 {
		t.Errorf("pwrite on donated FD = (%d, %v), want (4, nil)", n, err)
	}

	path := filepath.Join(dir, "f")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if want := "rpc,host"; string(got) != want {
		t.Errorf("file contents = %q, want %q", got, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0400 {
		t.Errorf("file mode = %#o, want 0400", perm)
	}
}

// TestOpenWriteDeniedByMode checks that opening an existing file for writing
// fails with EACCES when its mode denies the gofer write access, rather than
// succeeding with an FD that cannot write.
func TestOpenWriteDeniedByMode(t *testing.T) {
	if unix.Geteuid() == 0 {
		t.Skip("root bypasses file mode checks")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("x"), 0400); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	c, root := serve(t, dir)
	ctx := context.Background()

	status, inodes, err := root.WalkMultiple(ctx, []string{"f"})
	if err != nil || status != lisafs.WalkSuccess || len(inodes) != 1 {
		t.Fatalf("Walk(f) = (%v, %d inodes, %v), want (Success, 1, nil)", status, len(inodes), err)
	}
	control := c.NewFD(inodes[0].ControlFD)
	defer control.Close(ctx, true /* flush */)

	openFD, hostFD, err := control.OpenAt(ctx, unix.O_WRONLY)
	if err == nil {
		fd := c.NewFD(openFD)
		fd.Close(ctx, true /* flush */)
		if hostFD >= 0 {
			unix.Close(hostFD)
		}
		t.Fatalf("OpenAt(O_WRONLY) on a 0400 file succeeded, want EACCES")
	}
	if !errors.Is(err, unix.EACCES) {
		t.Errorf("OpenAt(O_WRONLY) on a 0400 file = %v, want EACCES", err)
	}
}
