// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// A Pool is a set of temporary objects that may be individually 单独地 saved and
// retrieved.
//
// Any item stored in the Pool may be removed automatically at any time without notification. 随时可能被回收，只能做临时缓冲池
// If the Pool holds the only reference when this happens, the
// item might be deallocated. 释放，分配的反义词
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool's purpose is to cache allocated but unused items for later reuse,
// relieving减少 pressure on the garbage collector.
// That is, it makes it easy to build efficient, thread-safe free lists. 空闲列表
// However, it is not suitable for all free lists.
//
// An appropriate use of a Pool is to manage a group of temporary items
// silently shared among and potentially reused by concurrent independent clients并发独立的客户端 of a package.
// Pool provides a way to amortize均摊 allocation overhead开销 across many clients.
//
// An example of good use of a Pool is in the fmt package, which maintains a
// dynamically-sized store of temporary output buffers. The store scales under
// load (when many goroutines are actively printing) and shrinks when 伸缩
// quiescent不活动的. fmt使用 []byte Pool 注意Put放回时的 切片不能过大
//
// On the other hand, a free list maintained as part of a short-lived object is 不适用对象本身只是短期生命周期的空闲列表？？
// not a suitable use for a Pool, since the overhead does not amortize well in
// that scenario. It is more efficient to have such objects implement their own
// free list.
//
// A Pool must not be copied after first use.
// 这里实现的Pool 里的对象还是可能会被回收
// 只能 保存一组可独立访问的临时对象
// 不可复制
// 本身是线程安全

type Pool struct {
	noCopy noCopy

	// local 和 victim 相关于 两级缓存
	// local 优先

	local     unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal
	localSize uintptr        // size of the local array

	// 这里会被回收, 然后local 的数据 会放到victim, local会被清空
	victim     unsafe.Pointer // local from previous cycle
	victimSize uintptr        // size of victims array

	// New optionally specifies a function to generate
	// a value when Get would otherwise return nil.
	// It may not be changed concurrently with calls to Get. 也许不能当call调用的同时，改变New函数
	New func() interface{}
}

// Local per-P Pool appendix. 附加物？附录？
type poolLocalInternal struct {
	private interface{} // Can be used only by the respective P. 每个p一个私有缓存对象
	shared  poolChain   // 无锁队列 Local P can pushHead/popHead; any P can popTail. 一个生产者, 多个消费者
}

type poolLocal struct {
	poolLocalInternal

	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .
	// 保证 pool Local 结构体 只在一块cache line, 更新不影响其他对象，不会导致一更新，统一cache line其他对象失效，private，队列头指针和尾指针经常改变，
	// 也保证多个poolLocal 不在同一个 cache line，不同p的缓存不会相互影响
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte // cpu cache 对齐., 防止伪共享
}

// from runtime/subs.go 拿到多线程随机种子
func fastrand() uint32

var poolRaceHash [128]uint64

// poolRaceAddr returns an address to use as the synchronization point
// for race detector logic. We don't use the actual pointer stored in x
// directly, for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
func poolRaceAddr(x interface{}) unsafe.Pointer {
	ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
	h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
	return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}

// Put adds x to the pool.
func (p *Pool) Put(x interface{}) {
	if x == nil {
		return
	}
	if race.Enabled {
		if fastrand()%4 == 0 {
			// Randomly drop x on floor.
			return
		}
		race.ReleaseMerge(poolRaceAddr(x))
		race.Disable()
	}
	// 将 g 绑定到当前p 避免查找元素期间被其它的 P 执行
	// 禁止抢占
	l, _ := p.pin()
	if l.private == nil {
		l.private = x // 放到私有缓存
		x = nil
	}
	if x != nil {
		l.shared.pushHead(x) // 放到队列头
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
	}
}

// Get selects an arbitrary任意选一个 item from the Pool, removes it from the
// Pool, and returns it to the caller.
// Get may choose to ignore the pool and treat it as empty.
// Callers should not assume any relation between values passed to Put and
// the values returned by Get.
//
// If Get would otherwise return nil and p.New is non-nil, Get returns
// the result of calling p.New.
func (p *Pool) Get() interface{} {
	if race.Enabled {
		race.Disable()
	}
	// 将 g 绑定到当前p 避免查找元素期间被其它的 P 执行, 返回当前p 的 poolLocal
	l, pid := p.pin()
	x := l.private // 先从单个缓存里面拿
	l.private = nil
	if x == nil {
		// Try to pop the head of the local shard. We prefer
		// the head over the tail for temporal locality of
		// reuse.
		x, _ = l.shared.popHead() // lock free 队列, 本地p可以从头部拿
		if x == nil {
			x = p.getSlow(pid) // 其他p只能从队列尾部拿
		}
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
		if x != nil {
			race.Acquire(poolRaceAddr(x))
		}
	}

	//
	if x == nil && p.New != nil {
		x = p.New() // 新生成一个
	}
	return x
}

func (p *Pool) getSlow(pid int) interface{} {
	// See the comment in pin regarding ordering of the loads.
	size := atomic.LoadUintptr(&p.localSize) // load-acquire 拿到val 相当于 return *addr
	locals := p.local                        // load-consume
	// Try to steal one element from other procs.
	for i := 0; i < int(size); i++ { // 遍历所有 p的队列, 从尾部拿一个
		l := indexLocal(locals, (pid+i+1)%int(size)) // pid 已经拿过了， 先从pid下一个开始
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Try the victim cache. We do this after attempting to steal
	// from all primary caches because we want objects in the
	// victim cache to age out if at all possible.
	// 从受害者缓存拿
	size = atomic.LoadUintptr(&p.victimSize)
	if uintptr(pid) >= size {
		return nil
	}

	// 只在gc 回收时， victim 代替了local，原有victim被清掉了
	locals = p.victim
	l := indexLocal(locals, pid)
	if x := l.private; x != nil { // 从单个私有里拿
		l.private = nil
		return x
	}
	for i := 0; i < int(size); i++ { // 直接都从尾部拿, 不从头部拿
		l := indexLocal(locals, (pid+i)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Mark the victim cache as empty for future gets don't bother
	// with it.
	// 如果victim中都没有，则把这个victim标记为空，以后的查找可以快速跳过了
	atomic.StoreUintptr(&p.victimSize, 0)

	return nil
}

// pin pins the current goroutine to P, disables preemption and
// returns poolLocal pool for the P and the P's id.
// Caller must call runtime_procUnpin() when done with the pool.
func (p *Pool) pin() (*poolLocal, int) {
	pid := runtime_procPin()
	// In pinSlow we store to local and then to localSize, here we load in opposite order.
	// Since we've disabled preemption, GC cannot happen in between.
	// Thus here we must observe local at least as large localSize.
	// We can observe a newer/larger local, it is fine (we must observe its zero-initialized-ness).
	s := atomic.LoadUintptr(&p.localSize) // load-acquire
	l := p.local                          // load-consume
	if uintptr(pid) < s {
		// 从已有的poolLocal里面查找
		return indexLocal(l, pid), pid
	}
	return p.pinSlow()
}

func (p *Pool) pinSlow() (*poolLocal, int) {
	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	runtime_procUnpin() //允许抢占，
	allPoolsMu.Lock() // 直接拿全局锁，保证互斥性
	defer allPoolsMu.Unlock()
	pid := runtime_procPin()
	// poolCleanup won't be called while we are pinned.
	s := p.localSize
	l := p.local	//unsafe.Pointer 内部实现是什么？？ 为什么空值是nil??
	if uintptr(pid) < s { // double-check
		return indexLocal(l, pid), pid
	}

	// 下面要创建poolLocal, 先加到全局池，以便回收
	if p.local == nil {
		allPools = append(allPools, p)
	}
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	size := runtime.GOMAXPROCS(0)  // 入参为0，表示不设置，只是读取
	local := make([]poolLocal, size)  //一次创建所有 p 的 poolLocal
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
	atomic.StoreUintptr(&p.localSize, uintptr(size))         // store-release
	return &local[pid], pid
}

func poolCleanup() {
	// This function is called with the world stopped, at the beginning of a garbage collection.
	// It must not allocate and probably should not call any runtime functions.

	// Because the world is stopped, no pool user can be in a
	// pinned section (in effect, this has all Ps pinned).

	// Drop victim caches from all pools.
	for _, p := range oldPools {
		p.victim = nil
		p.victimSize = 0
	}

	// Move primary cache to victim cache.
	for _, p := range allPools {
		p.victim = p.local
		p.victimSize = p.localSize
		p.local = nil
		p.localSize = 0
	}

	// The pools with non-empty primary caches now have non-empty
	// victim caches and no pools have primary caches.
	oldPools, allPools = allPools, nil
}

var (
	allPoolsMu Mutex

	// allPools is the set of pools that have non-empty primary
	// caches. Protected by either 1) allPoolsMu and pinning or 2)
	// STW.
	allPools []*Pool

	// oldPools is the set of pools that may have non-empty victim
	// caches. Protected by STW.
	oldPools []*Pool
)

func init() {
	// 对于垃圾回收, 补充针对 Pool 有额外的回收逻辑 func poolCleanup
	runtime_registerPoolCleanup(poolCleanup)
}

// 就是基址+size * i, 数组长度偏移
func indexLocal(l unsafe.Pointer, i int) *poolLocal {
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
	return (*poolLocal)(lp)
}

// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func())
func runtime_procPin() int
func runtime_procUnpin()
