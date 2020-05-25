// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Fixed-size object固定大小对象分配器 allocator. Returned memory is not zeroed未初始化.
//
// See malloc.go for overview.

package runtime

import "unsafe"

// FixAlloc is a simple free-list allocator for fixed size objects.
// Malloc uses a FixAlloc wrapped around sysAlloc to manage its mcache and mspan objects.
//
// Memory returned by fixalloc.alloc is zeroed by default, but the
// caller may take responsibility for zeroing allocations by setting the zero flag to false.
// This is only safe if the memory never contains heap pointers.
//
// The caller is responsible for locking加锁 around FixAlloc calls.
// Callers can keep state in the object but the first word (next指针) is smashed修改破坏 by freeing and reallocating.
//
// Consider marking fixalloc'd types go:notinheap.
// 简单空闲链表分配器 for 固定大小的对象
type fixalloc struct {
	size   uintptr
	first  func(arg, p unsafe.Pointer) // 第一个参数是管理者 堆指针, 第二个参数是要返回的分配的对象 called first time p is returned
	arg    unsafe.Pointer
	list   *mlink
	chunk  uintptr //一次性分配16k堆外空间 避免写屏障 use uintptr instead of unsafe.Pointer to avoid write barriers
	nchunk uint32  //可以分配的字节数
	inuse  uintptr // 累积在使用的字节数 in-use bytes now
	stat   *uint64
	zero   bool // 是否进行零初始化 zero allocations
}

// A generic linked list of blocks.  (Typically the block is bigger than sizeof(MLink).)
// Since assignments to mlink.next will result in a write barrier being performed
// this cannot be used by some of the internal GC structures. For example when
// the sweeper is placing an unmarked object on the free list it does not want the
// write barrier to be called since that could result in the object being reachable.
// 分配出去的对象必须是next指针作为第一个参数
//go:notinheap
type mlink struct {
	next *mlink
}

// Initialize f to allocate objects of the given size,
// using the allocator to obtain chunks of memory.
// 初始化
func (f *fixalloc) init(size uintptr, first func(arg, p unsafe.Pointer), arg unsafe.Pointer, stat *uint64) {
	f.size = size	//一次分配的尺寸
	f.first = first		//每次分配都要执行的函数
	f.arg = arg		//*mheap
	f.list = nil
	f.chunk = 0
	f.nchunk = 0
	f.inuse = 0
	f.stat = stat	//统计项
	f.zero = true
}

//获取下一个空闲内存空间
func (f *fixalloc) alloc() unsafe.Pointer {
	if f.size == 0 {
		print("runtime: use of FixAlloc_Alloc before FixAlloc_Init\n")
		throw("runtime: internal error")
	}

	//缓存
	if f.list != nil {
		//从空闲列表拿
		v := unsafe.Pointer(f.list)
		f.list = f.list.next
		f.inuse += f.size
		if f.zero {
			memclrNoHeapPointers(v, f.size)
		}
		return v
	}

	if uintptr(f.nchunk) < f.size {
		// 分配堆外空间
		f.chunk = uintptr(persistentalloc(_FixAllocChunk, 0, f.stat))
		// 16k
		f.nchunk = _FixAllocChunk
	}

	v := unsafe.Pointer(f.chunk)
	if f.first != nil {
		f.first(f.arg, v)
	}
	//位移
	f.chunk = f.chunk + f.size
	f.nchunk -= uint32(f.size)
	f.inuse += f.size
	return v
}

//释放指针指向的内存空间
func (f *fixalloc) free(p unsafe.Pointer) {
	f.inuse -= f.size
	//放入链表
	v := (*mlink)(p)
	v.next = f.list
	f.list = v
}
