// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Cond implements a condition variable, a rendezvous约会地点 point
// for goroutines waiting for or announcing the occurrence
// of an event. 等待或者宣布事件的发生
//
// Each Cond has an associated Locker L (often a *Mutex or *RWMutex),
// which must be held when changing the condition and  改变条件也需要加锁, 但是调用Signal和Broadcast不用加锁
// when calling the Wait method.
//
// A Cond must not be copied after first use.
// 相比channel 实现 通知/等待的优势
// 1. 有锁
// 2. 可以broadcast多次
type Cond struct {
	noCopy noCopy

	// L is held while observing or changing the condition 观测和修改条件
	L Locker

	notify  notifyList // 通知, 等待队列
	checker copyChecker
}

// NewCond returns a new Cond with Locker l.
func NewCond(l Locker) *Cond {
	return &Cond{L: l}
}

// Wait atomically unlocks c.L and suspends execution of the calling goroutine.
// After later resuming execution, Wait locks c.L before returning.
// Unlike in other systems不像其他系统, Wait cannot return unless awoken by Broadcast or Signal. 这里只能被主动唤醒
//
// Because c.L is not locked when Wait first resumes, the caller
// typically cannot assume that the condition is true when
// Wait returns. Instead, the caller should Wait in a loop:
// 标准用法，不能假设从wait中返回时，条件是满足的
//    c.L.Lock()
//    for !condition() {
//        c.Wait()
//    }
//    无法假设 从Wait返回后, 条件一定满足, 可能其他g抢到锁, 把条件改为false了
//    也可能 每次唤醒, 只表示更进一步, 而不是条件直接true了
//    ... make use of condition ...
//    c.L.Unlock()
//
func (c *Cond) Wait() {
	c.checker.check()
	t := runtime_notifyListAdd(&c.notify)
	c.L.Unlock()                         //调用wait前必须加锁, 这里必须解锁，因为下一行会休眠，不能继续持有锁，以便让其他g获取锁
	runtime_notifyListWait(&c.notify, t) //暂停当前g执行，也就是阻塞
	c.L.Lock()                           //唤醒后继续持有锁
}

// Signal wakes one goroutine waiting on c, if there is any.
//
// It is allowed but not required for the caller to hold c.L
// during the call.
func (c *Cond) Signal() {
	c.checker.check()
	runtime_notifyListNotifyOne(&c.notify)
}

// Broadcast wakes all goroutines waiting on c.
// 允许但是不要求加锁, 才能调用此函数
// It is allowed but not required for the caller to hold c.L
// during the call.
func (c *Cond) Broadcast() {
	c.checker.check()
	runtime_notifyListNotifyAll(&c.notify)
}

// copyChecker holds back pointer to itself to detect object copying.
type copyChecker uintptr //运行时 防拷贝检查

func (c *copyChecker) check() {
	if uintptr(*c) != uintptr(unsafe.Pointer(c)) &&
		// 未被交换，也就是 指针值不是0
		!atomic.CompareAndSwapUintptr((*uintptr)(c), 0, uintptr(unsafe.Pointer(c))) &&
		// c != 0
		uintptr(*c) != uintptr(unsafe.Pointer(c)) {
		panic("sync.Cond is copied")
	}
}

// noCopy may be embedded into structs which must not be copied
// after the first use.
//
// See https://golang.org/issues/8005#issuecomment-190753527
// for details.
type noCopy struct{} // 实现 locker 接口, 利用 go vet 检测机制防止拷贝

// Lock is a no-op used by -copylocks checker from `go vet`.
func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}
