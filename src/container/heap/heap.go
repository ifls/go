// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package heap provides heap operations for any type that implements heap.Interface. 提供堆操作
// A heap is a tree with the property that each node is the minimum-valued node in its subtree. 小顶堆
// 每个节点都是其子树的最小值
// The minimum element in the tree is the root, at index 0.
//
// A heap is a common way to implement a priority queue. 实现优化队列
// To build a priority queue, implement the Heap interface with the (negative) priority as the
// ordering for the Less method, so Push adds items while Pop removes the highest-priority item from the queue.

// The Examples include such an implementation; the file example_pq_test.go has the complete source.
//
package heap

import "sort"

// The Interface type describes the requirements for a type using the routines常规操作 in this package.
// Any type that implements it may be used as a
// min-heap with the following invariants不变式 (established after
// Init has been called or if the data is empty or sorted):
//  a[j] >= a[i]
//	!h.Less(j, i) for 0 <= i < h.Len() and 2*i+1 <= j <= 2*i+2 and j < h.Len()
//
// Note that Push and Pop in this interface are for package heap's implementation to call.
// To add and remove things from the heap, use heap.Push and heap.Pop.
type Interface interface {
	sort.Interface
	Push(x interface{}) // add x as element Len() 加到最后
	Pop() interface{}   // remove and return element Len() - 1.  弹出最后一个值
}

// Init establishes the heap invariants required by the other routines in this package.

// Init is idempotent幂等的 with respect to the heap invariants and may be called whenever the heap invariants may have been invalidated.

// The complexity is O(n) where n = h.Len().
func Init(h Interface) {
	//建立堆不变式
	// heapify 从最后一个父节点开始下沉,
	n := h.Len()
	for i := n/2 - 1; i >= 0; i-- {
		// [i,n)
		down(h, i, n)
	}
}

// Push pushes the element x onto the heap.

// The complexity is O(log n) where n = h.Len().
func Push(h Interface, x interface{}) {
	//放在最后
	h.Push(x)
	//尝试上浮
	up(h, h.Len()-1)
}

// Pop removes and returns the minimum element (according to Less) from the heap.

// The complexity is O(log n) where n = h.Len().

// Pop is equivalent to Remove(h, 0). 等价于 Remove(h, 0)
func Pop(h Interface) interface{} {
	n := h.Len() - 1
	//交换到最后
	h.Swap(0, n)
	// [0, len-1) 之间下沉
	down(h, 0, n)
	return h.Pop()
}

// Remove removes and returns the element at index i from the heap.
// The complexity is O(log n) where n = h.Len().
func Remove(h Interface, i int) interface{} {
	n := h.Len() - 1
	if n != i {
		//i 与结尾交换
		h.Swap(i, n)

		//相当于h[i]上的元素被修改了，需要Fix
		//下沉无效，尝试上浮
		if !down(h, i, n) {
			//上浮
			up(h, i)
		}
	}
	return h.Pop()
}

// Fix re-establishes the heap ordering after the element at index i has changed its value.

// Changing the value of the element at index i and then calling Fix is equivalent to,
// but less expensive than, calling Remove(h, i) followed by a Push of the new value. 比先remove 再push 一个值更廉价

// The complexity is O(log n) where n = h.Len().
func Fix(h Interface, i int) {
	//下沉无效，尝试上浮
	if !down(h, i, h.Len()) {
		up(h, i)
	}
}

func up(h Interface, j int) {
	for {
		i := (j - 1) / 2 // parent
		if i == j || !h.Less(j, i) {
			// i <= j 已满足条件，无需上浮
			break
		}
		//与父节点交换
		h.Swap(i, j)
		//在父节点继续进行下一轮交换判断
		j = i
	}
}

//[i0, n)
func down(h Interface, i0, n int) bool {
	i := i0
	for {
		j1 := 2*i + 1	//children left
		if j1 >= n || j1 < 0 { // j1 < 0 after int overflow
			//越界
			break
		}

		j := j1 // left child
		if j2 := j1 + 1; j2 < n && h.Less(j2, j1) {
			//取最小的
			j = j2 // = 2*i + 2  // right child
		}

		if !h.Less(j, i) {
			// i <= j 退出
			break
		}
		//交换
		h.Swap(i, j)
		//父节点继续
		i = j
	}
	// 成功下沉
	return i > i0
}
