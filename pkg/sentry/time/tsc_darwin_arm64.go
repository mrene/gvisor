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

package time

import (
	"math/bits"
	_ "unsafe" // for go:linkname
)

// nanotime is mach_absolute_time() converted to nanoseconds with the
// timebase from mach_timebase_info().
//
//go:linkname nanotime runtime.nanotime
func nanotime() int64

// Rdtsc returns the counter that Hypervisor.framework derives guests'
// CNTVCT_EL0 from: mach_absolute_time(), in ticks of CNTFRQ_EL0. The VDSO
// computes guest time from CNTVCT_EL0 with parameters calibrated against
// this value. CNTVCT_EL0 as read by host threads is not the same counter.
func Rdtsc() TSCValue {
	hi, lo := bits.Mul64(uint64(nanotime()), uint64(getCNTFRQ()))
	ticks, _ := bits.Div64(hi, lo, 1e9)
	return TSCValue(ticks)
}
