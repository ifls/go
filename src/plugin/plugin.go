// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package plugin implements loading and symbol resolution of Go plugins.
// 实现go插件的加载和符号解析
// A plugin is a Go main package with exported functions and variables that has been built with:
//
//	go build -buildmode=plugin
//
// When a plugin is first opened, the init functions of all packages not already part of the program are called. 非重复的init函数会被执行
// The main function is not run. 不允许
// A plugin is only initialized once, and cannot be closed. 只能初始化，并且无法被关闭
//
// Currently plugins are only supported on Linux, FreeBSD, and macOS.
// Please report any issues.
package plugin

// Plugin is a loaded Go plugin.
type Plugin struct {
	pluginpath string		//路径
	err        string        // set if plugin failed to load
	loaded     chan struct{} // closed when loaded 加载完成就关闭
	syms       map[string]interface{}	//符号表
}

// Open opens a Go plugin.
// If a path has already been opened, then the existing *Plugin is returned.
// It is safe for concurrent use by multiple goroutines.
func Open(path string) (*Plugin, error) {
	return open(path)
}

// Lookup searches for a symbol named symName in plugin p.
// A symbol is any exported variable or function.
// It reports an error if the symbol is not found.
// It is safe for concurrent use by multiple goroutines.
func (p *Plugin) Lookup(symName string) (Symbol, error) {
	return lookup(p, symName)
}

// A Symbol is a pointer to a variable or function.
//
// For example, a plugin defined as
//
//	package main
//
//	import "fmt"
//
//	var V int
//
//	func F() { fmt.Printf("Hello, number %d\n", V) }
//
// may be loaded with the Open function and then the exported package
// symbols V and F can be accessed
//
//	p, err := plugin.Open("plugin_name.so")
//	if err != nil {
//		panic(err)
//	}
//	v, err := p.Lookup("V")
//	if err != nil {
//		panic(err)
//	}
//	f, err := p.Lookup("F")
//	if err != nil {
//		panic(err)
//	}
//	*v.(*int) = 7
//	f.(func())() // prints "Hello, number 7"
type Symbol interface{}
