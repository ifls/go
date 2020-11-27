// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// runtime/panic.go
func throw(string) // provided by runtime

// A Mutex is a mutual exclusion互斥 lock.
// The zero value for a Mutex is an unlocked mutex. 零值可用
//
// A Mutex must not be copied after first use. 使用后, 不要进行拷贝失败,
type Mutex struct { // 8B
	state int32
	sema  uint32
}

// A Locker represents an object that can be locked and unlocked. go vet 有针对这个接口检查 防拷贝
type Locker interface {
	Lock()
	Unlock()
}

const (
	// 32 bit state 00000000 00000000 00000000 00000 1 1 1  饥饿标志, 唤醒标记, 上锁标记
	mutexLocked      = 1 << iota // mutex is locked
	mutexWoken                   // 当前是否有唤醒的 goroutine
	mutexStarving                // 饥饿标志, 饥饿状态基本等价于 上锁的状态
	mutexWaiterShift = iota      // state >> itoa 是 阻塞的g的数量

	// Mutex fairness公平.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over the ownership.
	// New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue.
	//
	// If a waiter fails to acquire the mutex for more than 1ms, it switches mutex to the starvation mode.
	// 饥饿模式
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue. 直接交给队列第一个
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin 新来者不尝试拿锁,也不自旋. Instead they queue themselves at
	// the tail of the wait queue. 直接到队列尾部
	//
	// If a waiter receives ownership of the mutex and sees that满足 either
	// (1) it is the last waiter in the queue, 队列中唯一的等待者
	// or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row连续获得几次锁,不中断 even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency. 防止尾部延迟的病态情况
	starvationThresholdNs = 1e6 // 1ms
)

// Lock locks m.
// If the lock is already in use, the calling goroutine blocks阻塞 until the mutex is available.
func (m *Mutex) Lock() { // 拆成两部分, 方便内联, 在单g的情况, 只有一个cas的耗时
	// Fast path: grab unlocked mutex.
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) { // 没有竞争者的时候, 这是对空锁上锁  0->1
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined外放 so that the fast path can be inlined)
	m.lockSlow() // 包含for循环, select 等语句的函数不会内联, 所以抽出此函数, 可以让编译器达成内联的效果
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64
	starving := false
	awoke := false // 唤醒标记 表示当前有goroutine 积极工作, 其他goroutine 退散
	iter := 0      // 迭代次数
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// 当前是上锁,且非饥饿状态, 自旋一会, 然后进入下一次循环, 继续争抢锁
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.  设置唤醒标记, 是让unlock() 不去|别去唤醒阻塞者, 而是让给当前唤醒者,自旋者,
			// 非唤醒状态, 且无唤醒, 有等待者, 标记唤醒成功,
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true // 标记有唤醒者
			}
			runtime_doSpin()
			iter++
			old = m.state // 再次更新状态
			continue
		}

		// 准备抢锁
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		// 如果是饥饿状态, 不允许抢锁, 非饥饿, 才可以抢
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}

		// 原来 已上锁 或者是饥饿状态 , 直接进入等待者列表
		if old&(mutexLocked|mutexStarving) != 0 { // 饥饿, 等价于有锁, 新来的不能抢锁, 强制等待
			new += 1 << mutexWaiterShift // 正常模式增加等待者
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		// 已上锁, 是饥饿, 标记饥饿状态
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		if awoke { // 清除唤醒标志
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken // 开启唤醒
		}
		if atomic.CompareAndSwapInt32(&m.state, old, new) { // 更新状态, 但并不是上锁成功
			if old&(mutexLocked|mutexStarving) == 0 { // 原来非上锁且非饥饿, 表明加锁成功
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			queueLifo := waitStartTime != 0 // last in first out 等于放在对头(之前等待过的情况)
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)
			// 被唤醒之后
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs // 最长等待1ms, 就进入饥饿状态

			old = m.state
			// 如果已经是饥饿状态, 直接抢到锁, 返回
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				// 必须是无锁,无唤醒
				// 有一个等待者,至少有自己
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// 加锁, 并将 waiter -1
				delta := int32(mutexLocked - 1<<mutexWaiterShift) // 对应饥饿模式下, 减少等待者
				if !starving || old>>mutexWaiterShift == 1 {      // 等待少于1ms, 或者这个g是最后一个等待者, 就退出
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving // 退出饥饿模式
				}
				atomic.AddInt32(&m.state, delta) // 减少一个等待者, 并上锁,
				break                            //  拿到锁了, 退出
			}

			// 非饥饿状态
			awoke = true // 表示此goroutine是从上面runtime_SemacquireMutex休眠, 然后被唤醒之后的, 表示当前有一个唤醒的goroutine在工作
			// 更新自旋状态
			iter = 0
		} else {
			// 更新失败, 拿到最新状态, 继续下一次循环
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked) // -1 解锁, 必须成功
	if new != 0 {                                  // == 0 表示, 解锁成功, 无其他等待者, 无其他标志
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	if (new+mutexLocked)&mutexLocked == 0 { // 检查 new最后一位 是 0
		throw("sync: unlock of unlocked mutex")
	}

	if new&mutexStarving == 0 {
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			// 没有其他等待者 直接返回, 解锁成功
			// 有锁, 有唤醒, 有饥饿表示已经有在处理的情况直接返回??
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}

			// 有等待者, 或者 3个标志都没有
			// Grab the right to wake someone.
			new = (old - 1<<mutexWaiterShift) | mutexWoken // 非饥饿模式, 减少一个等待者, 设置唤醒标志, 不去唤醒g
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				// 唤醒别人
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			// 失败, 继续死循环
			old = m.state
		}
	} else { // 饥饿状态, 直接唤醒别人, 唤醒队列首的g, 让出自己的时间片
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set, 饥饿状态, 锁被认为一直是持有的, 新来的拿不到锁
		// so new coming goroutines won't acquire it.
		runtime_Semrelease(&m.sema, true, 1) // 唤醒一个g
	}
}
