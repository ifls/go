// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build aix darwin dragonfly freebsd js,wasm linux netbsd openbsd solaris
// c语言 时间结构体 转 纳秒
package syscall

// TimespecToNsec converts a Timespec value into a number of
// nanoseconds since the Unix epoch.
// 秒/纳秒 -> 纳秒
func TimespecToNsec(ts Timespec) int64 { return ts.Nano() }

// NsecToTimespec takes a number of nanoseconds since the Unix epoch
// and returns the corresponding Timespec value.
func NsecToTimespec(nsec int64) Timespec {
	sec := nsec / 1e9
	nsec = nsec % 1e9
	if nsec < 0 {
		nsec += 1e9
		sec--
	}
	return setTimespec(sec, nsec)
}

// TimevalToNsec converts a Timeval value into a number of nanoseconds
// since the Unix epoch.
// 秒/微秒 ->纳秒
func TimevalToNsec(tv Timeval) int64 { return tv.Nano() }

// NsecToTimeval takes a number of nanoseconds since the Unix epoch
// and returns the corresponding Timeval value.
func NsecToTimeval(nsec int64) Timeval {
	nsec += 999 // round up to microsecond
	usec := nsec % 1e9 / 1e3
	sec := nsec / 1e9
	if usec < 0 {
		usec += 1e6
		sec--
	}
	return setTimeval(sec, usec)
}
