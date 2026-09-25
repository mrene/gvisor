// Copyright 2018 The gVisor Authors.
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

//go:build !(darwin && arm64)

package time

// Rdtsc reads the TSC.
//
// Intel SDM, Vol 3, Ch 17.15:
// "The RDTSC instruction reads the time-stamp counter and is guaranteed to
// return a monotonically increasing unique value whenever executed, except for
// a 64-bit counter wraparound. Intel guarantees that the time-stamp counter
// will not wraparound within 10 years after being reset."
//
// We use int64, so we have 5 years before wrap-around.
func Rdtsc() TSCValue
