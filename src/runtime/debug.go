// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// GOMAXPROCS sets the maximum number of CPUs that can be executing
// 同时simultaneously and returns the previous setting 返回之前设置的数量. If n < 1, it does not
// change the current setting. 如果 n < 1 返回以前设置的数量
// The number of logical CPUs on the local machine can be queried with NumCPU.
// This call will go away when the scheduler improves.
func GOMAXPROCS(n int) int {
	if GOARCH == "wasm" && n > 1 {
		n = 1 // WebAssembly has no threads yet, so only one CPU is possible.
	}

	lock(&sched.lock)
	ret := int(gomaxprocs)
	unlock(&sched.lock)
	if n <= 0 || n == ret {
		return ret
	}

	stopTheWorldGC("GOMAXPROCS")

	// newprocs will be processed by startTheWorld
	newprocs = int32(n)

	startTheWorldGC()
	return ret
}

// NumCPU returns the number of logical CPUs usable by the current process.
//
// The set of available CPUs is checked by querying the operating system
// at process startup. Changes to operating system CPU allocation after
// process startup are not reflected.
func NumCPU() int {
	return int(ncpu)
}

//返回cgo调用次数
// NumCgoCall returns the number of cgo calls made by the current process.
func NumCgoCall() int64 {
	var n int64
	for mp := (*m)(atomic.Loadp(unsafe.Pointer(&allm))); mp != nil; mp = mp.alllink {
		n += int64(mp.ncgocall)
	}
	return n
}

//返回当前创建的协程数量 用于 metrics
// NumGoroutine returns the number of goroutines that currently exist.
func NumGoroutine() int {
	return int(gcount())
}

//go:linkname debug_modinfo runtime/debug.modinfo
func debug_modinfo() string {
	return modinfo
}
