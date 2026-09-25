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

//go:build darwin && arm64

package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	goferfs "gvisor.dev/gvisor/pkg/sentry/fsimpl/gofer"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/overlay"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// bindMount exposes a host directory at a guest path through an in-process
// gofer.
type bindMount struct {
	host     string
	guest    string
	readOnly bool
}

func (b bindMount) String() string {
	mode := "rw"
	if b.readOnly {
		mode = "ro"
	}
	return fmt.Sprintf("%s:%s:%s", b.host, b.guest, mode)
}

// mountFlag collects repeated --mount HOST[:GUEST][:ro|:rw] flags.
type mountFlag []bindMount

// String implements flag.Value.String.
func (m *mountFlag) String() string {
	specs := make([]string, len(*m))
	for i, b := range *m {
		specs[i] = b.String()
	}
	return strings.Join(specs, ",")
}

// Set implements flag.Value.Set.
func (m *mountFlag) Set(spec string) error {
	b, err := parseBindMount(spec)
	if err != nil {
		return err
	}
	*m = append(*m, b)
	return nil
}

// parseBindMount parses HOST[:GUEST][:ro|:rw]. HOST may be relative to the
// host working directory; GUEST defaults to the absolute HOST path. Mounts
// are read-write unless ":ro" is given.
func parseBindMount(spec string) (bindMount, error) {
	parts := strings.Split(spec, ":")
	var b bindMount
	if n := len(parts); n > 1 {
		switch parts[n-1] {
		case "ro":
			b.readOnly = true
			parts = parts[:n-1]
		case "rw":
			parts = parts[:n-1]
		}
	}
	if len(parts) > 2 || parts[0] == "" {
		return bindMount{}, fmt.Errorf("invalid mount %q: want HOST[:GUEST][:ro|:rw]", spec)
	}
	host, err := filepath.Abs(parts[0])
	if err != nil {
		return bindMount{}, fmt.Errorf("invalid mount %q: %w", spec, err)
	}
	b.host = host
	b.guest = host
	if len(parts) == 2 {
		if !filepath.IsAbs(parts[1]) {
			return bindMount{}, fmt.Errorf("invalid mount %q: guest path %q must be absolute", spec, parts[1])
		}
		b.guest = filepath.Clean(parts[1])
	}
	return b, nil
}

// pathUnder returns p relative to dir if p is dir or below it.
func pathUnder(p, dir string) (string, bool) {
	if p == dir {
		return "", true
	}
	if dir == "/" {
		return strings.TrimPrefix(p, "/"), strings.HasPrefix(p, "/")
	}
	if strings.HasPrefix(p, dir+"/") {
		return p[len(dir)+1:], true
	}
	return "", false
}

// reservedGuestPath reports whether a host directory must not be mounted at
// guest path p because it would hide the guest root or its system mounts.
func reservedGuestPath(p string) bool {
	for _, dir := range []string{"/proc", "/dev"} {
		if _, ok := pathUnder(p, dir); ok {
			return true
		}
	}
	return p == "/"
}

// guestPathOf returns the guest path at which host path p is visible through
// mounts, using the mount with the longest matching host path.
func guestPathOf(p string, mounts []bindMount) (string, bool) {
	best := -1
	var guest string
	for _, m := range mounts {
		if rel, ok := pathUnder(p, m.host); ok && len(m.host) > best {
			best = len(m.host)
			guest = filepath.Join(m.guest, rel)
		}
	}
	return guest, best >= 0
}

// guestExecPath returns the guest path at which the host file p is visible
// through mounts. Symlinks in p are resolved on the host first, since they may
// lead outside the mounts (e.g. ./result -> /nix/store/...), and mount host
// paths are compared with their symlinks resolved, as the gofer mounts them.
func guestExecPath(p string, mounts []bindMount) (string, bool) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", false
	}
	if real, err = filepath.Abs(real); err != nil {
		return "", false
	}
	resolved := make([]bindMount, 0, len(mounts))
	for _, m := range mounts {
		if host, err := filepath.EvalSymlinks(m.host); err == nil {
			m.host = host
			resolved = append(resolved, m)
		}
	}
	return guestPathOf(real, resolved)
}

// mountPlan is the set of host directories to mount and the initial guest
// working directory.
type mountPlan struct {
	// mounts are ordered so that parents are mounted before children.
	mounts []bindMount
	// workDir is the initial guest working directory; empty means "/".
	workDir string
}

// planMounts combines explicit --mount flags with the optional $HOME and
// working directory shares. home and cwd are host paths, or empty if not
// shared. existing lists mounts set up elsewhere (such as the Nix store); they
// are only used to find where cwd is already visible.
//
// home and cwd are mounted at their host paths unless already visible there.
// The guest starts in cwd, translated through any mount that already exposes
// it. Explicit mounts take precedence over the automatic shares.
func planMounts(explicit []bindMount, home, cwd string, existing []bindMount) (mountPlan, error) {
	var plan mountPlan
	guests := make(map[string]bool)
	for _, b := range explicit {
		if reservedGuestPath(b.guest) {
			return mountPlan{}, fmt.Errorf("cannot mount %s at reserved guest path %s", b.host, b.guest)
		}
		if guests[b.guest] {
			return mountPlan{}, fmt.Errorf("multiple mounts at guest path %s", b.guest)
		}
		guests[b.guest] = true
		plan.mounts = append(plan.mounts, b)
	}
	visible := func() []bindMount {
		return append(append([]bindMount(nil), plan.mounts...), existing...)
	}
	if home != "" && !reservedGuestPath(home) && !guests[home] {
		if g, ok := guestPathOf(home, visible()); !ok || g != home {
			plan.mounts = append(plan.mounts, bindMount{host: home, guest: home})
			guests[home] = true
		}
	}
	if cwd != "" {
		if g, ok := guestPathOf(cwd, visible()); ok {
			plan.workDir = g
		} else if !reservedGuestPath(cwd) && !guests[cwd] {
			plan.mounts = append(plan.mounts, bindMount{host: cwd, guest: cwd})
			plan.workDir = cwd
		}
	}
	sort.SliceStable(plan.mounts, func(i, j int) bool {
		return len(plan.mounts[i].guest) < len(plan.mounts[j].guest)
	})
	return plan, nil
}

// mkdirAllGuest creates guest directory p and any missing parents.
func mkdirAllGuest(k *kernel.Kernel, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, p string) error {
	cur := ""
	for _, comp := range strings.Split(p, "/") {
		if comp == "" {
			continue
		}
		cur += "/" + comp
		pop := vfs.PathOperation{
			Root:  root,
			Start: root,
			Path:  fspath.Parse(cur),
		}
		if err := vfsObj.MkdirAt(k.SupervisorContext(), creds, &pop, &vfs.MkdirOptions{Mode: 0755}); err != nil && !linuxerr.Equals(linuxerr.EEXIST, err) {
			return fmt.Errorf("mkdir %s: %w", cur, err)
		}
	}
	return nil
}

// goferMountData returns the gofer mount data for a filesystem served over
// clientFD.
//
// force_page_cache keeps the contents of memory-mapped files in the sentry's
// MemoryFile, so that shared mappings are coherent with file I/O and are
// written back to the host file. Otherwise, gofer files are mapped through
// host FDs, which are MAP_PRIVATE on darwin (see fsutil.MmapCachedFile):
// guest stores to shared mappings would never reach the file, and later
// changes to the file would not reach the mappings.
func goferMountData(clientFD int) string {
	data := fmt.Sprintf("trans=fd,rfdno=%d,wfdno=%d,cache=remote_revalidating,force_page_cache", clientFD, clientFD)
	if *flagDirectfs {
		data += ",directfs"
	}
	return data
}

// goferMountOptions starts an in-process gofer serving host directory hostDir
// and returns the options for mounting it.
func goferMountOptions(k *kernel.Kernel, hostDir string, readOnly bool, id string) (*vfs.MountOptions, error) {
	// The gofer opens its root with O_NOFOLLOW, so resolve symlinks such as
	// macOS's /tmp -> /private/tmp first.
	realDir, err := filepath.EvalSymlinks(hostDir)
	if err != nil {
		return nil, err
	}
	// Check access up front: when the gofer fails to open its root, the
	// lisafs client currently hangs in Client.Close on macOS instead of
	// returning the error.
	fd, err := unix.Open(realDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.EPERM {
			return nil, fmt.Errorf("open %s: %w (blocked by macOS privacy settings?)", realDir, err)
		}
		return nil, fmt.Errorf("open %s: %w", realDir, err)
	}
	unix.Close(fd)

	clientFD := setupGoferConnection(k, realDir, readOnly)
	mountOpts := goferMountData(clientFD)
	return &vfs.MountOptions{
		ReadOnly: readOnly,
		GetFilesystemOptions: vfs.GetFilesystemOptions{
			Data: mountOpts,
			InternalData: goferfs.InternalFilesystemOptions{
				UniqueID:       checkpoint.ResourceID{Path: id},
				LeakConnection: true,
			},
			InternalMount: true,
		},
	}, nil
}

// mountGofer mounts host directory hostDir at guestPath through a new
// in-process gofer connection.
func mountGofer(k *kernel.Kernel, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, hostDir, guestPath string, readOnly bool, id string) error {
	opts, err := goferMountOptions(k, hostDir, readOnly, id)
	if err != nil {
		return err
	}
	pop := vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(guestPath),
	}
	_, err = vfsObj.MountAt(k.SupervisorContext(), creds, "none", &pop, goferfs.Name, opts)
	return err
}

// mountGoferOverlay mounts host directory hostDir at guestPath as the
// read-only lower layer of an overlay whose upper layer is an in-memory tmpfs.
// The guest can modify guestPath, but changes never reach the host and are
// discarded when the sandbox exits.
func mountGoferOverlay(k *kernel.Kernel, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, hostDir, guestPath, id string) error {
	ctx := k.SupervisorContext()
	lowerOpts, err := goferMountOptions(k, hostDir, true /* readOnly */, id)
	if err != nil {
		return err
	}
	lower, err := vfsObj.MountDisconnected(ctx, creds, "none", goferfs.Name, lowerOpts)
	if err != nil {
		return fmt.Errorf("lower layer: %w", err)
	}
	defer lower.DecRef(ctx)
	lowerRoot := vfs.MakeVirtualDentry(lower, lower.Root())
	stat, err := vfsObj.StatAt(ctx, creds, &vfs.PathOperation{Root: lowerRoot, Start: lowerRoot}, &vfs.StatOptions{
		Mask: linux.STATX_UID | linux.STATX_GID | linux.STATX_MODE,
	})
	if err != nil {
		return fmt.Errorf("stat lower layer: %w", err)
	}

	upper, err := vfsObj.MountDisconnected(ctx, creds, "none", tmpfs.Name, &vfs.MountOptions{
		GetFilesystemOptions: vfs.GetFilesystemOptions{
			InternalMount: true,
			InternalData: tmpfs.FilesystemOpts{
				RootFileType:            linux.S_IFDIR,
				DisableDefaultSizeLimit: true,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("upper layer: %w", err)
	}
	defer upper.DecRef(ctx)
	upperRoot := vfs.MakeVirtualDentry(upper, upper.Root())
	// The overlay root takes its owner and mode from the upper layer, so
	// match the host directory (e.g. the Nix store's sticky bit).
	if err := vfsObj.SetStatAt(ctx, creds, &vfs.PathOperation{Root: upperRoot, Start: upperRoot}, &vfs.SetStatOptions{
		Stat: linux.Statx{
			Mask: (linux.STATX_UID | linux.STATX_GID | linux.STATX_MODE) & stat.Mask,
			UID:  stat.UID,
			GID:  stat.GID,
			Mode: stat.Mode,
		},
	}); err != nil {
		return fmt.Errorf("set upper layer attributes: %w", err)
	}

	pop := vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(guestPath),
	}
	_, err = vfsObj.MountAt(ctx, creds, "none", &pop, overlay.Name, &vfs.MountOptions{
		GetFilesystemOptions: vfs.GetFilesystemOptions{
			InternalMount: true,
			InternalData: overlay.FilesystemOptions{
				UpperRoot:  upperRoot,
				LowerRoots: []vfs.VirtualDentry{lowerRoot},
			},
		},
	})
	return err
}
