// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

const (
	kindBool = 1 + iota // 1
	kindInt             // 2
	kindInt8
	kindInt16
	kindInt32 // 5
	kindInt64

	kindUint
	kindUint8
	kindUint16
	kindUint32 // 10
	kindUint64

	kindUintptr

	kindFloat32
	kindFloat64

	kindComplex64 // 15
	kindComplex128

	kindArray     // [3]int
	kindChan      // chan int
	kindFunc      // func name()
	kindInterface // interface{}  20
	kindMap       // map 21 or 53
	kindPtr       // 22 指针

	kindSlice         // []int
	kindString        // ""
	kindStruct        // struct{} 25
	kindUnsafePointer // unsafe.Pointer 26

	kindDirectIface = 1 << 5
	kindGCProg      = 1 << 6
	kindMask        = (1 << 5) - 1

	// 54 = 32 + 22  表示, 带方法接口, 内部value是指针
)

// isDirectIface reports whether t is stored directly in an interface value.
func isDirectIface(t *_type) bool {
	// 对应位 是1
	return t.kind&kindDirectIface != 0
}
