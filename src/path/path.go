// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package path implements utility routines for manipulating slash-separated paths.  常规操作'/'分割的路径
//
// The path package should only be used for paths separated by forward slashes, such as the paths in URLs.  url路径
// This package does not deal with Windows paths with drive letters or backslashes 反下划线;
// to manipulate operating system paths, use the path/filepath package. 操作系统文件路径使用filepath子包
package path

import (
	"strings"
)

// A lazybuf is a laszily constructed path buffer.
// It supports append, reading previously appended bytes, and retrieving the final string.
// It does not allocate a buffer to hold the output until that output diverges分歧 from s.
type lazybuf struct {
	s   string
	buf []byte	//和s有一个字节以上的分歧, 才分配空间
	w   int		//write position
}

func (b *lazybuf) index(i int) byte {
	if b.buf != nil {
		return b.buf[i]
	}
	return b.s[i]
}

func (b *lazybuf) append(c byte) {
	if b.buf == nil {
		if b.w < len(b.s) && b.s[b.w] == c {
			b.w++
			return
		}
		b.buf = make([]byte, len(b.s))
		copy(b.buf, b.s[:b.w])
	}
	b.buf[b.w] = c
	b.w++
}

func (b *lazybuf) string() string {
	if b.buf == nil {
		return b.s[:b.w]
	}
	return string(b.buf[:b.w])
}

// Clean returns the shortest path name equivalent to path by purely lexical processing 词汇处理. 得到等价最短路径
// It applies the following rules iteratively迭代 until no further processing can be done:
//
//	1. Replace multiple slashes with a single slash. 多/ -> 单/
//	2. Eliminate each . path name element (the current directory). 消除单.
//	3. Eliminate each inner .. path name element (the parent directory) along with the non-.. element that precedes it.
//	4. Eliminate .. elements that begin a rooted path: 		that is, replace "/.." by "/" at the beginning of a path.
//
// The returned path ends in a slash only if it is the root "/". 不以/结尾
//
// If the result of this process is an empty string, Clean returns the string ".".  空字符串当做 当前目录
//
// See also Rob Pike, ``Lexical File Names in Plan 9 or Getting Dot-Dot Right,'' TODO https://9p.io/sys/doc/lexnames.html


//
func Clean(path string) string {
	if path == "" {
		return "."
	}

	rooted := path[0] == '/'
	n := len(path)

	// Invariants:
	//	reading from path; r is index of next byte to process.
	//	writing to buf; w is index of next byte to write.
	//	dotdot is index in buf where .. must stop, either because
	//		it is the leading slash or it is a leading ../../.. prefix.
	out := lazybuf{s: path}
	r, dotdot := 0, 0
	if rooted {
		out.append('/')
		r, dotdot = 1, 1
	}

	//TODO
	for r < n {
		switch {
		case path[r] == '/':
			// empty path element 跳过
			r++
		case path[r] == '.' && (r+1 == n || path[r+1] == '/'):
			// . element 结尾的. 或者 ./
			r++
		case path[r] == '.' && path[r+1] == '.' && (r+2 == n || path[r+2] == '/'):
			// .. element: remove to last /
			// .. 或者 ../
			r += 2
			switch {
			case out.w > dotdot:
				// can backtrack
				out.w--
				for out.w > dotdot && out.index(out.w) != '/' {
					out.w--
				}
			case !rooted:
				// cannot backtrack, but not rooted, so append .. element.
				if out.w > 0 {
					out.append('/')
				}
				out.append('.')
				out.append('.')
				dotdot = out.w
			}
		default:
			// real path element.
			// add slash if needed
			if rooted && out.w != 1 || !rooted && out.w != 0 {
				out.append('/')
			}

			// append copy element
			for ; r < n && path[r] != '/'; r++ {
				out.append(path[r])
			}
		}
	}

	// Turn empty string into "."
	if out.w == 0 {
		return "."
	}

	return out.string()
}

// Split splits path immediately直接地 following the final slash, separating分离 it into a directory and file name component部分.
// If there is no slash in path, Split returns an empty dir and file set to path. 没有/返回-1, 就是目录名为空, 全部是文件名
// The returned values have the property that path = dir+file.
//切分路径和文件名 路径带下划线
func Split(path string) (dir, file string) {
	i := strings.LastIndex(path, "/")
	return path[:i+1], path[i+1:]
}

// Join joins any number of path elements into a single path,
// separating them with slashes. Empty elements are ignored.
// The result is Cleaned. However, if the argument list is
// empty or all its elements are empty, Join returns
// an empty string.
func Join(elem ...string) string {
	for i, e := range elem {
		//忽略前面的空字符串
		if e != "" {
			//返回之后的字符串, 中间用/连接, 再清理成最短路径
			return Clean(strings.Join(elem[i:], "/"))
		}
	}
	return ""
}

// Ext returns the file name extension used by path.
// The extension is the suffix beginning at the final dot
// in the final slash-separated element of path;
// it is empty if there is no dot.
//取出后缀名， 包含"."
func Ext(path string) string {
	for i := len(path) - 1; i >= 0 && path[i] != '/'; i-- {
		if path[i] == '.' {
			return path[i:]
		}
	}
	return ""
}

// Base returns the last element of path. 返回路径最后一个元素 可能是目录名, 也可能是文件名
// Trailing slashes are removed before extracting the last element. 先去掉结尾的/
// If the path is empty, Base returns ".".
// If the path consists entirely of slashes, Base returns "/". 如果完全由/组成, 则返回/
func Base(path string) string {
	if path == "" {
		return "."
	}

	// Strip trailing slashes.
	for len(path) > 0 && path[len(path)-1] == '/' {
		path = path[0 : len(path)-1]
	}

	// Find the last element
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}

	// If empty now, it had only slashes.
	if path == "" {
		return "/"
	}
	return path
}

// IsAbs reports whether the path is absolute. 是否是绝对路径
func IsAbs(path string) bool {
	return len(path) > 0 && path[0] == '/'
}

// Dir returns all but the last element of path, typically the path's directory.
// After dropping the final element using Split, the path is Cleaned and trailing
// slashes are removed.
// If the path is empty, Dir returns ".".
// If the path consists entirely of slashes followed by non-slash bytes, Dir
// returns a single slash. In any other case, the returned path does not end in a
// slash.
//获取目录部分
func Dir(path string) string {
	dir, _ := Split(path)
	return Clean(dir)
}
