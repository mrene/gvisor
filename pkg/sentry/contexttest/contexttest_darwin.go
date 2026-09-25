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

package contexttest

import (
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// newPlatform returns the platform.Platform used by test contexts.
//
// The only darwin platform, hvf, requires a binary signed with the hypervisor
// entitlement, which test binaries are not. Tests only need a Platform to
// construct MemoryManagers and address layouts, so use testPlatform instead.
func newPlatform() (platform.Platform, error) {
	return testPlatform{}, nil
}

// testPlatform implements platform.Platform for tests that do not execute
// application code. Its AddressSpaces map nothing, so MemoryManagers perform
// all application memory I/O through internal mappings.
type testPlatform struct {
	platform.NoCPUPreemptionDetection
	platform.NoCPUNumbers
}

// SupportsAddressSpaceIO implements platform.Platform.SupportsAddressSpaceIO.
func (testPlatform) SupportsAddressSpaceIO() bool {
	return false
}

// HaveGlobalMemoryBarrier implements platform.Platform.HaveGlobalMemoryBarrier.
func (testPlatform) HaveGlobalMemoryBarrier() bool {
	return false
}

// GlobalMemoryBarrier implements platform.Platform.GlobalMemoryBarrier.
func (testPlatform) GlobalMemoryBarrier() error {
	panic("contexttest platform does not support global memory barriers")
}

// MapUnit implements platform.Platform.MapUnit.
func (testPlatform) MapUnit() uint64 {
	return 0
}

// MinUserAddress implements platform.Platform.MinUserAddress.
func (testPlatform) MinUserAddress() hostarch.Addr {
	return hostarch.PageSize
}

// MaxUserAddress implements platform.Platform.MaxUserAddress.
func (testPlatform) MaxUserAddress() hostarch.Addr {
	return 1 << 47
}

// NewAddressSpace implements platform.Platform.NewAddressSpace.
func (testPlatform) NewAddressSpace() (platform.AddressSpace, error) {
	return testAddressSpace{}, nil
}

// NewContext implements platform.Platform.NewContext.
func (testPlatform) NewContext(context.Context) platform.Context {
	panic("contexttest platform cannot execute application code")
}

// SeccompInfo implements platform.Platform.SeccompInfo.
func (testPlatform) SeccompInfo() platform.SeccompInfo {
	return platform.StaticSeccompInfo{PlatformName: "contexttest"}
}

// ConcurrencyCount implements platform.Platform.ConcurrencyCount.
func (testPlatform) ConcurrencyCount() int {
	return 1
}

// testAddressSpace implements platform.AddressSpace without mapping anything.
type testAddressSpace struct {
	platform.NoAddressSpaceIO
}

// MapFile implements platform.AddressSpace.MapFile.
func (testAddressSpace) MapFile(hostarch.Addr, memmap.File, memmap.FileRange, hostarch.AccessType, bool) error {
	return nil
}

// Unmap implements platform.AddressSpace.Unmap.
func (testAddressSpace) Unmap(hostarch.Addr, uint64) {}

// Release implements platform.AddressSpace.Release.
func (testAddressSpace) Release() {}

// PreFork implements platform.AddressSpace.PreFork.
func (testAddressSpace) PreFork() {}

// PostFork implements platform.AddressSpace.PostFork.
func (testAddressSpace) PostFork() {}
