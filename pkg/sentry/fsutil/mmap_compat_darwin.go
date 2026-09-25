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

package fsutil

import "golang.org/x/sys/unix"

// mapFixedNoreplace is MAP_FIXED on macOS since MAP_FIXED_NOREPLACE
// is not available. The caller handles the case where the address
// conflicts with an existing mapping.
const mapFixedNoreplace = unix.MAP_FIXED

// mmapSharedFlag is MAP_PRIVATE on macOS because HVF (Hypervisor.framework)
// rejects hv_vm_map of MAP_SHARED pages from files with security attributes
// (com.apple.provenance/quarantine). MAP_PRIVATE creates copy-on-write
// pages that HVF accepts and can map directly into the guest VM.
const mmapSharedFlag = unix.MAP_PRIVATE

// mapChunksWritable is true on macOS: MmapCachedFile chunks are MAP_PRIVATE
// there, so writes through them never reach the file, and mapping every chunk
// writable from the start means that a chunk's mapping is never replaced to
// make it writable. HVF maps chunk pages into the guest by host address and
// would keep using the pages of a replaced mapping.
const mapChunksWritable = true
