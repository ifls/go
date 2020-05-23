// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package syslog provides a simple interface to the system log service.
// It can send messages to the syslog daemon系统日志守护进程 using UNIX domain sockets, UDP or TCP. 使用unix预套接字/tcp/udp
//
// Only one call to Dial is necessary.
// On write failures, the syslog client will attempt to reconnect to the server and write again. 失败，重连重试
//
// The syslog package is frozen冻结 and is not accepting new features.
// Some external packages provide more functionality. See:
//
//   https://godoc.org/?q=syslog
package syslog

// BUG(brainman): This package is not implemented on Windows.

// As the syslog package is frozen, Windows users are encouraged to use a package outside of the standard library.
// For background, see https://golang.org/issue/1108.

// BUG(akumar): This package is not implemented on Plan 9.
