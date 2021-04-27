// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package math

import "runtime/internal/sys"

const MaxUintptr = ^uintptr(0)

// MulUintptr returns a * b and whether the multiplication overflowed.
// On supported platforms this is an intrinsic lowered by the compiler.
func MulUintptr(a, b uintptr) (uintptr, bool) {
	// math.MaxUint32|math.MaxUint32 < 1<<(4*PtrSize) == true, 高位没有1的数, 乘起来, 肯定不超过 2^32
	if a|b < 1<<(4*sys.PtrSize) || a == 0 {
		return a * b, false
	}
	overflow := b > MaxUintptr/a
	return a * b, overflow
}
