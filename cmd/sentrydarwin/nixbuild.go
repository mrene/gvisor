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

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

// nixBuild is a derivation build requested by Nix through its external
// builder protocol (the external-builders setting): Nix runs
// `sentrydarwin --nix-build ... BUILD_JSON` in the build directory and expects
// the build's outputs under RealStoreDir when the program exits successfully.
type nixBuild struct {
	Version         int               `json:"version"`
	Builder         string            `json:"builder"`
	Args            []string          `json:"args"`
	Env             map[string]string `json:"env"`
	Outputs         map[string]string `json:"outputs"`
	StoreDir        string            `json:"storeDir"`
	RealStoreDir    string            `json:"realStoreDir"`
	TmpDir          string            `json:"tmpDir"`
	TmpDirInSandbox string            `json:"tmpDirInSandbox"`
	TopTmpDir       string            `json:"topTmpDir"`
	System          string            `json:"system"`
}

// nixSandboxStoreDir is the only store directory supported: the host store is
// mounted there in the guest.
const nixSandboxStoreDir = "/nix/store"

// readNixBuild reads and validates the build description at path.
func readNixBuild(path string) (*nixBuild, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b nixBuild
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := b.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &b, nil
}

func (b *nixBuild) validate() error {
	if b.Version != 1 {
		return fmt.Errorf("unsupported external builder protocol version %d", b.Version)
	}
	if b.System != "aarch64-linux" {
		return fmt.Errorf("unsupported system %q", b.System)
	}
	if b.StoreDir != nixSandboxStoreDir {
		return fmt.Errorf("unsupported store directory %q (want %s)", b.StoreDir, nixSandboxStoreDir)
	}
	if b.Builder == "" {
		return fmt.Errorf("no builder")
	}
	for _, p := range []string{b.RealStoreDir, b.TmpDir, b.TmpDirInSandbox} {
		if !path.IsAbs(p) {
			return fmt.Errorf("path %q is not absolute", p)
		}
	}
	if len(b.Outputs) == 0 {
		return fmt.Errorf("no outputs")
	}
	for name, out := range b.Outputs {
		if path.Dir(out) != b.StoreDir {
			return fmt.Errorf("output %s path %q is not in %s", name, out, b.StoreDir)
		}
	}
	return nil
}

// argv returns the builder's argument vector.
func (b *nixBuild) argv() []string {
	return append([]string{b.Builder}, b.Args...)
}

// envv returns the builder's environment, which is exactly the environment
// Nix specified.
func (b *nixBuild) envv() []string {
	envv := make([]string, 0, len(b.Env))
	for k, v := range b.Env {
		envv = append(envv, k+"="+v)
	}
	sort.Strings(envv)
	return envv
}

// hostTmpPath returns the host path of p, a path in the sandbox's build
// directory.
func (b *nixBuild) hostTmpPath(p string) (string, bool) {
	rel, ok := pathUnder(p, b.TmpDirInSandbox)
	if !ok {
		return "", false
	}
	return filepath.Join(b.TmpDir, rel), true
}

// fixedOutput returns true if this is a fixed-output derivation, which, as in
// Nix's own sandbox, gets network access. The output hash is an environment
// variable, or with structured attributes an attribute in the JSON file that
// NIX_ATTRS_JSON_FILE names.
func (b *nixBuild) fixedOutput() (bool, error) {
	if _, ok := b.Env["outputHash"]; ok {
		return true, nil
	}
	attrsFile, ok := b.Env["NIX_ATTRS_JSON_FILE"]
	if !ok {
		return false, nil
	}
	hostPath, ok := b.hostTmpPath(attrsFile)
	if !ok {
		return false, fmt.Errorf("NIX_ATTRS_JSON_FILE %q is not in %s", attrsFile, b.TmpDirInSandbox)
	}
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return false, err
	}
	var attrs map[string]json.RawMessage
	if err := json.Unmarshal(data, &attrs); err != nil {
		return false, fmt.Errorf("parse %s: %w", hostPath, err)
	}
	_, ok = attrs["outputHash"]
	return ok, nil
}

// Identity of the build user in Nix's Linux sandbox, which /etc/passwd and
// /etc/group describe. The builder itself runs as root in the guest: the
// build directory is shared from the host, which owns it.
const (
	nixSandboxUID = 1000
	nixSandboxGID = 100
)

// sandboxFiles returns the contents of the files that Nix's Linux sandbox
// creates for builders, keyed by guest path. Fixed-output derivations also get
// the host's name resolution configuration, since they have network access.
func (b *nixBuild) sandboxFiles(fixedOutput bool) map[string]string {
	files := map[string]string{
		"/etc/passwd": fmt.Sprintf("root:x:0:0:Nix build user:%[1]s:/noshell\n"+
			"nixbld:x:%[2]d:%[3]d:Nix build user:%[1]s:/noshell\n"+
			"nobody:x:65534:65534:Nobody:/:/noshell\n", b.TmpDirInSandbox, nixSandboxUID, nixSandboxGID),
		"/etc/group": fmt.Sprintf("root:x:0:\nnixbld:!:%d:\nnogroup:x:65534:\n", nixSandboxGID),
		"/etc/hosts": "127.0.0.1 localhost\n::1 localhost\n",
	}
	if fixedOutput {
		for _, p := range []string{"/etc/resolv.conf", "/etc/services", "/etc/hosts"} {
			if data, err := os.ReadFile(p); err == nil {
				files[p] = string(data)
			}
		}
	}
	return files
}

// setupNixBuildRoot prepares the guest root for b: the sandbox files, /bin/sh
// (a symlink to sh, if set) and the build directory, shared read-write from
// the host at its sandbox path.
func setupNixBuildRoot(k *kernel.Kernel, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, b *nixBuild, sh string, fixedOutput bool) error {
	ctx := k.SupervisorContext()
	for p, data := range b.sandboxFiles(fixedOutput) {
		if err := mkdirAllGuest(k, vfsObj, creds, root, path.Dir(p)); err != nil {
			return err
		}
		if err := writeGuestFile(ctx, vfsObj, creds, root, p, data); err != nil {
			return fmt.Errorf("create %s: %w", p, err)
		}
	}
	if sh != "" {
		if err := mkdirAllGuest(k, vfsObj, creds, root, "/bin"); err != nil {
			return err
		}
		pop := vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse("/bin/sh")}
		if err := vfsObj.SymlinkAt(ctx, creds, &pop, sh); err != nil {
			return fmt.Errorf("create /bin/sh: %w", err)
		}
	}
	if err := mkdirAllGuest(k, vfsObj, creds, root, b.TmpDirInSandbox); err != nil {
		return err
	}
	if err := mountGofer(k, vfsObj, creds, root, b.TmpDir, b.TmpDirInSandbox, false /* readOnly */, "nix-build-dir"); err != nil {
		return fmt.Errorf("mount build directory %s at %s: %w", b.TmpDir, b.TmpDirInSandbox, err)
	}
	return nil
}

// writeGuestFile creates the guest file at p with contents data.
func writeGuestFile(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, p, data string) error {
	pop := vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(p)}
	fd, err := vfsObj.OpenAt(ctx, creds, &pop, &vfs.OpenOptions{
		Flags: linux.O_WRONLY | linux.O_CREAT | linux.O_TRUNC,
		Mode:  0644,
	})
	if err != nil {
		return err
	}
	defer fd.DecRef(ctx)
	_, err = fd.Write(ctx, usermem.BytesIOSequence([]byte(data)), vfs.WriteOptions{})
	return err
}

// copyNixBuildOutputs copies b's outputs from the guest's store, where the
// builder created them in the in-memory upper layer, to the host store. An
// output the builder did not create is skipped: Nix reports it.
func copyNixBuildOutputs(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, b *nixBuild) error {
	for _, out := range b.Outputs {
		pop := vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(out)}
		if _, err := vfsObj.StatAt(ctx, creds, &pop, &vfs.StatOptions{Mask: linux.STATX_TYPE}); err != nil {
			continue
		}
		if err := copyOutOfGuest(ctx, vfsObj, creds, root, out, filepath.Join(b.RealStoreDir, path.Base(out))); err != nil {
			return fmt.Errorf("copy output %s: %w", out, err)
		}
	}
	return nil
}

// copyOutOfGuest copies the guest file tree at guestPath to hostPath, which
// must not exist. As in the Nix store, only file types, contents, symlink
// targets and the executable bit are preserved.
func copyOutOfGuest(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, guestPath, hostPath string) error {
	pop := vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(guestPath)}
	stat, err := vfsObj.StatAt(ctx, creds, &pop, &vfs.StatOptions{Mask: linux.STATX_TYPE | linux.STATX_MODE})
	if err != nil {
		return err
	}
	switch stat.Mode & linux.S_IFMT {
	case linux.S_IFLNK:
		target, err := vfsObj.ReadlinkAt(ctx, creds, &pop)
		if err != nil {
			return err
		}
		return os.Symlink(target, hostPath)

	case linux.S_IFDIR:
		if err := os.Mkdir(hostPath, 0755); err != nil {
			return err
		}
		names, err := guestDirNames(ctx, vfsObj, creds, &pop)
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := copyOutOfGuest(ctx, vfsObj, creds, root, path.Join(guestPath, name), filepath.Join(hostPath, name)); err != nil {
				return err
			}
		}
		return nil

	case linux.S_IFREG:
		fd, err := vfsObj.OpenAt(ctx, creds, &pop, &vfs.OpenOptions{Flags: linux.O_RDONLY})
		if err != nil {
			return err
		}
		defer fd.DecRef(ctx)
		perm := os.FileMode(0644)
		if stat.Mode&0111 != 0 {
			perm = 0755
		}
		f, err := os.OpenFile(hostPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return err
		}
		buf := make([]byte, 1<<20)
		for {
			n, rerr := fd.Read(ctx, usermem.BytesIOSequence(buf), vfs.ReadOptions{})
			if n > 0 {
				if _, err := f.Write(buf[:n]); err != nil {
					f.Close()
					return err
				}
			}
			if rerr == io.EOF || (rerr == nil && n == 0) {
				break
			}
			if rerr != nil {
				f.Close()
				return rerr
			}
		}
		return f.Close()

	default:
		return fmt.Errorf("%s: unsupported file type %#o", guestPath, stat.Mode&linux.S_IFMT)
	}
}

// guestDirNames returns the names of the entries of the guest directory at
// pop, excluding "." and "..".
func guestDirNames(ctx context.Context, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, pop *vfs.PathOperation) ([]string, error) {
	fd, err := vfsObj.OpenAt(ctx, creds, pop, &vfs.OpenOptions{Flags: linux.O_RDONLY | linux.O_DIRECTORY})
	if err != nil {
		return nil, err
	}
	defer fd.DecRef(ctx)
	var names []string
	err = fd.IterDirents(ctx, vfs.IterDirentsCallbackFunc(func(d vfs.Dirent) error {
		if d.Name != "." && d.Name != ".." {
			names = append(names, d.Name)
		}
		return nil
	}))
	return names, err
}
