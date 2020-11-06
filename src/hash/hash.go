// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package hash provides interfaces for hash functions.
package hash

import "io"

// Hash is the common interface implemented by all hash functions.
// 哈希算法一般生成定长不可逆的 []byte 也可以说是摘要, 加密算法生成变长可逆可解码的 []byte
// Hash implementations in the standard library (e.g. hash/crc32 and
// crypto/sha256) implement the encoding.BinaryMarshaler and encoding.BinaryUnmarshaler interfaces.
// Marshaling a hash implementation allows its internal state to be saved and used for additional processing later,
// without having to re-write the data previously written to the hash.
// The hash state may contain portions of the input in its original form,
// which users are expected to handle for any possible security implications.
//
// Compatibility: Any future changes to hash or crypto packages will endeavor
// to maintain compatibility with state encoded using previous versions.
// That is, any released versions of the packages should be able to
// decode data written with any previously released version,
// subject to issues such as security fixes.
// See the Go compatibility document for background:TODO  https://golang.org/doc/go1compat
type Hash interface {
	// Write (via the embedded io.Writer interface) adds more data to the running hash.
	// It never returns an error.
	io.Writer // 内嵌Write方法

	// Sum appends the current hash to b and returns the resulting slice.
	// It does not change the underlying hash state.
	Sum(b []byte) []byte

	// Reset resets the Hash to its initial state.
	Reset() //

	// Size returns the number of bytes Sum() will return.
	Size() int // Sum 返回的 []byte 的长度

	// BlockSize returns the hash's underlying block size.
	// The Write method must be able to accept any amount
	// of data, but it may operate more efficiently if all writes
	// are a multiple of the block size. 块大小的倍数的输入参数, 会更有效, 不是 摘要的大小
	BlockSize() int
}

// Hash32 is the common interface implemented by all 32-bit hash functions.
type Hash32 interface {
	Hash
	Sum32() uint32
}

// Hash64 is the common interface implemented by all 64-bit hash functions.
type Hash64 interface {
	Hash
	Sum64() uint64
}
