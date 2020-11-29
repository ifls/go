// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// A WaitGroup waits for a collection of goroutines to finish. 等待一系列g完成
// The main goroutine calls Add to set the number of
// goroutines to wait for. Then each of the goroutines
// runs and calls Done when finished. At the same time,
// Wait can be used to block until all goroutines have finished.
//
// A WaitGroup must not be copied after first use.
// 没有集成锁, 也不要外部加锁
type WaitGroup struct {
	noCopy noCopy

	// 64-bit value: high 32 bits are counter, low 32 bits are waiter count.
	// 64-bit atomic operations require 64-bit alignment, but 32-bit compilers do not ensure it. 需要64位对齐,
	// 但是32bit 的编译器, 不保证这一点
	// So we allocate 12 bytes and then use
	// the aligned 8 bytes in them as state, and the other 4 as storage for the sema.
	// 8B 状态, 4B 信号量
	// 64bit: waiter的数量(32b), counter计数值(32b), sema(32b)
	// 32bit: 32bit, sema(32b), waiter(32b), counter(32b)
	state1 [3]uint32 // 默认了, 小端序??
}

// state returns pointers to the state and sema fields stored within wg.state1.
func (wg *WaitGroup) state() (statep *uint64, semap *uint32) { // 对statep 的操作 需要保证 64位对齐, 才能保证原子性
	if uintptr(unsafe.Pointer(&wg.state1))%8 == 0 {            // 地址8B对齐
		return (*uint64)(unsafe.Pointer(&wg.state1)), &wg.state1[2]
	} else {
		return (*uint64)(unsafe.Pointer(&wg.state1[1])), &wg.state1[0]
	}
}

// Add adds delta, which may be negative, to the WaitGroup counter.
// If the counter becomes zero, all goroutines blocked on Wait are released.
// If the counter goes negative, Add panics.
//
// Note that calls with a positive delta that occur when the counter is zero must happen before a Wait. 计数器 从0加到某个数, 必须在Wait之前
// Calls with a negative delta, or calls with a positive delta that start when the counter is greater than zero,
// may happen at any time. 计数>0时, add和 done 随时都有可能发生
// Typically this means通常这意味着 the calls to Add should execute before the statement  add 应该在go 之前调用
// creating the goroutine or other event to be waited for.
// If a WaitGroup is reused to wait for several independent sets of events,
// new Add calls must happen after all previous Wait calls have returned. 所有阻塞的g都从Wait返回, 才能重用add, 不然还有g挂在信号量上,
// 所以 一般只有一个g 阻塞在waitgroup, 就不用考虑这个, 多个wait的g, 不能在唤醒后立刻add
// See the WaitGroup example.
func (wg *WaitGroup) Add(delta int) {
	statep, semap := wg.state()
	if race.Enabled {
		_ = *statep // trigger nil deref early
		if delta < 0 {
			// Synchronize decrements with Wait.
			race.ReleaseMerge(unsafe.Pointer(wg))
		}
		race.Disable()
		defer race.Enable()
	}

	// 负数 转正数 补码运算, 可以加负数
	state := atomic.AddUint64(statep, uint64(delta)<<32) // 加计数数量
	v := int32(state >> 32)                              // 当前计数值, 在高位
	w := uint32(state)                                   // waiter count
	if race.Enabled && delta > 0 && v == int32(delta) {
		// The first increment must be synchronized with Wait.
		// Need to model this as a read, because there can be
		// several concurrent wg.counter transitions from 0.
		race.Read(unsafe.Pointer(semap))
	}
	if v < 0 { // 计数为负数
		panic("sync: negative WaitGroup counter")
	}

	// 计数器和等待者, 不能同时去达到 > 0的状态, 只能一先一后
	if w != 0 && delta > 0 && v == int32(delta) { // 还有waiter, 不允许同时又add, 只允许done 并发调用, 不允许
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	if v > 0 || w == 0 {
		return
	}

	// v <= 0 && w != 0 这里计数器为0, 需要唤醒所有Wait

	// This goroutine has set counter to 0 when waiters > 0.
	// Now there can't be concurrent mutations of state:
	// - Adds must not happen concurrently with Wait, add和wait 不能并发调用
	// - Wait does not increment waiters if it sees counter == 0.
	// Still do a cheap sanity check to detect WaitGroup misuse.
	if *statep != state { // Add方法不应该和wait并发调用, 这里如果状态不同, add肯定是并发调用了, 而且 w != 0, 说明wait也调用过, 算得上是并发调用了
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	// Reset waiters count to 0.
	*statep = 0
	// 计数为0, 释放所有等待者
	for ; w != 0; w-- { // 计数都已经done完了, 唤醒所有调用wait阻塞的 goroutine
		runtime_Semrelease(semap, false, 0) // 唤醒w个g
	}
}

// Done decrements the WaitGroup counter by one.
func (wg *WaitGroup) Done() {
	wg.Add(-1)
}

// Wait blocks until the WaitGroup counter is zero.
// 可能有多个goroutine 调用wait方法
func (wg *WaitGroup) Wait() {
	statep, semap := wg.state()
	if race.Enabled {
		_ = *statep // trigger nil deref early
		race.Disable()
	}
	for {
		state := atomic.LoadUint64(statep)
		v := int32(state >> 32)
		w := uint32(state)
		if v == 0 { // 计数器为0, 就表示完成, 直接返回
			// Counter is 0, no need to wait.
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}

		// 到了这里就要阻塞g了
		// Increment waiters count.
		if atomic.CompareAndSwapUint64(statep, state, state+1) { // +1. 表示又多了一个goroutine 阻塞了
			if race.Enabled && w == 0 {
				// Wait must be synchronized with the first Add.
				// Need to model this is as a write to race with the read in Add.
				// As a consequence, can do the write only for the first waiter,
				// otherwise concurrent Waits will race with each other.
				race.Write(unsafe.Pointer(semap))
			}
			// 休眠
			runtime_Semacquire(semap)
			// 唤醒时, 必须 waiter和counter都 == 0
			if *statep != 0 { // Add 在唤醒时, 置为了0
				panic("sync: WaitGroup is reused before previous Wait has returned")
			}
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
	}
}
