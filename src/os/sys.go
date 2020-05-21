// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package os

// syscall.63 SYS_UNAME
// 返回主机名
// Hostname returns the host name reported by the kernel.
func Hostname() (name string, err error) {
	return hostname()
}
