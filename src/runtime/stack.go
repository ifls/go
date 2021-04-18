// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

/*
Stack layout parameters.
Included both by runtime (compiled via 6c) and linkers (compiled via gcc).

The per-goroutine g->stackguard is set to point StackGuard bytes
above the bottom of the stack.  栈底
Each function compares its stack pointer against g->stackguard to check for overflow. 比较栈底，检查溢出
To cut one instruction from the check sequence for functions with tiny frames,
the stack is allowed to protrude突出 StackSmall bytes below the stack
guard.  Functions with large frames大栈帧 don't bother with the check 麻烦做此检查 and
always call morestack 总是调用扩展栈函数.  The sequences are (for amd64, others are
similar):

	guard = g->stackguard
	frame = function's stack frame size
	argsize = size of function arguments (call + return)

	stack frame size <= StackSmall:
		CMPQ guard, SP
		JHI 3(PC)
		MOVQ m->morearg, $(argsize << 32)
		CALL morestack(SB)

	stack frame size > StackSmall but < StackBig
		LEAQ (frame-StackSmall)(SP), R0
		CMPQ guard, R0
		JHI 3(PC)
		MOVQ m->morearg, $(argsize << 32)
		CALL morestack(SB)

	stack frame size >= StackBig:
		MOVQ m->morearg, $((argsize << 32) | frame)
		CALL morestack(SB)

The bottom StackGuard - StackSmall bytes are important: there has
to be enough room空间 to execute functions that refuse to check for
stack overflow, either because they need to be adjacent临近的 to the
actual caller's frame (deferproc) or because they handle the imminent即将到来的
stack overflow (morestack).

For example, deferproc might call malloc, which does one of the
above checks (without allocating a full frame), which might trigger
a call to morestack.  This sequence needs to fit in the bottom
section of the stack.  On amd64, morestack's frame is 40 bytes, and
deferproc's frame is 56 bytes.  That fits well within the
StackGuard - StackSmall bytes at the bottom.
The linkers explore all possible call traces involving non-splitting
functions to make sure that this limit cannot be violated.
*/

const (
	// StackSystem is a number of additional bytes to add
	// to each stack below the usual guard area for OS-specific
	// purposes like signal handling.
	// Used on Windows, Plan 9, and iOS because they do not use a separate stack.
	// linux 上是0
	_StackSystem = sys.GoosWindows*512*sys.PtrSize + sys.GoosPlan9*512 + sys.GoosDarwin*sys.GoarchArm*1024 + sys.GoosDarwin*sys.GoarchArm64*1024

	// The minimum size of stack used by Go code
	_StackMin = 2048 // 一开始goroutine 默认的栈空间

	// The minimum stack size to allocate.
	// The hackery黑客 here rounds FixedStack0 up to a power of 2.
	_FixedStack0 = _StackMin + _StackSystem            // 2k   0b 1000 0000 0000
	_FixedStack1 = _FixedStack0 - 1                    // 2047 0b 0111 1111 1111
	_FixedStack2 = _FixedStack1 | (_FixedStack1 >> 1)  // 2047 0b 0111 1111 1111
	_FixedStack3 = _FixedStack2 | (_FixedStack2 >> 2)  // 2047 0b 0111 1111 1111
	_FixedStack4 = _FixedStack3 | (_FixedStack3 >> 4)  // 2047 0b 0111 1111 1111
	_FixedStack5 = _FixedStack4 | (_FixedStack4 >> 8)  // 2047 0b 0111 1111 1111
	_FixedStack6 = _FixedStack5 | (_FixedStack5 >> 16) // 2047 0b 0111 1111 1111
	_FixedStack  = _FixedStack6 + 1                    // 2k   0b 1000 0000 0000

	// Functions that need frames bigger than this use an extra
	// instruction to do the stack split check, to avoid overflow
	// in case SP - framesize wraps below zero.
	// This value can be no bigger than the size of the unmapped space at zero.
	_StackBig = 4096

	// The stack guard is a pointer this many bytes above the bottom of the stack.
	// 928
	_StackGuard = 928*sys.StackGuardMultiplier + _StackSystem

	// After a stack split check the SP is allowed to be this
	// many bytes below the stack guard. This saves an instruction
	// in the checking sequence for tiny frames.
	_StackSmall = 128

	// The maximum number of bytes that a chain of NOSPLIT functions can use.
	// 800
	_StackLimit = _StackGuard - _StackSystem - _StackSmall
)

const (
	// 栈操作 日志级别
	// stackDebug == 0: no logging
	//            == 1: logging of per-stack operations
	//            == 2: logging of per-frame operations
	//            == 3: logging of per-word updates
	//            == 4: logging of per-word reads
	stackDebug       = 0
	stackFromSystem  = 0 // 从系统分配栈内存而不是堆 allocate stacks from system memory instead of the heap
	stackFaultOnFree = 0 // old stacks are mapped noaccess to detect use after free
	stackPoisonCopy  = 0 // fill stack that should not be accessed with garbage, to detect bad dereferences during copy
	stackNoCache     = 0 // disable per-P small stack caches

	// check the BP links during traceback.
	debugCheckBP = false
)

const (
	// 0xffffffffffff
	uintptrMask = 1<<(8*sys.PtrSize) - 1

	// Goroutine preemption request. 被标记为抢占
	// Stored into g->stackguard0 to cause split stack check failure.
	// Must be greater than any real sp. 比任何实际sp都大，必然进入morestack函数
	// 0xFFFFFFFFFFFFFADE in hex.
	stackPreempt = uintptrMask & -1314

	// Thread is forking. 表示正在fork
	// Stored into g->stackguard0 to cause split stack check failure. 无法栈扩容
	// Must be greater than any real sp.
	// 0xFFFFFFFFFFFFFB2E
	stackFork = uintptrMask & -1234
)

// Global pool of spans that have free stacks. 全局空闲栈池
// Stacks are assigned an order according to size.
//     order = log_2(size/FixedStack)
// There is a free list for each order.  每个p的mcache 都有 stackcache
// 全局变量 长度是4， 全局栈缓存， 分配 < 32KB 的栈
var stackpool [_NumStackOrders]struct {
	item stackpoolItem
	_    [cpu.CacheLinePadSize - unsafe.Sizeof(stackpoolItem{})%cpu.CacheLinePadSize]byte
}

//go:notinheap
type stackpoolItem struct {
	mu   mutex  //全局的 要加锁
	span mSpanList  // 从mspan里获取内存， go的栈内存是从堆上分配的
}

// Global pool of large stack spans.
// 全局 的 大栈 缓存， 分配 > 32KB的栈
var stackLarge struct {
	lock mutex
	// 48-13 = 35
	free [heapAddrBits - pageShift]mSpanList // free lists by log_2(s.npages)
}

func stackinit() {
	if _StackCacheSize&_PageMask != 0 {
		throw("cache size must be a multiple of page size")
	}
	// 初始化 栈缓存对象，并未分配空间
	for i := range stackpool {
		stackpool[i].item.span.init()
		lockInit(&stackpool[i].item.mu, lockRankStackpool)
	}

	for i := range stackLarge.free {
		stackLarge.free[i].init()
		lockInit(&stackLarge.lock, lockRankStackLarge)
	}
}

// stacklog2 快速计算returns ⌊log_2(n)⌋.
func stacklog2(n uintptr) int {
	log2 := 0
	for n > 1 {
		n >>= 1
		log2++
	}
	return log2
}

// Allocates a stack from the free pool. Must be called with
// stackpool[order].item.mu held.
func stackpoolalloc(order uint8) gclinkptr {
	list := &stackpool[order].item.span
	// 头
	s := list.first

	lockWithRankMayAcquire(&mheap_.lock, lockRankMheap)

	if s == nil {
		// no free stacks. Allocate another span worth.
		s = mheap_.allocManual(_StackCacheSize>>_PageShift, &memstats.stacks_inuse)
		if s == nil {
			throw("out of memory")
		}
		if s.allocCount != 0 {
			throw("bad allocCount")
		}
		if s.manualFreeList.ptr() != nil {
			throw("bad manualFreeList")
		}
		// linux 上为空
		osStackAlloc(s)

		s.elemsize = _FixedStack << order
		for i := uintptr(0); i < _StackCacheSize; i += s.elemsize {
			x := gclinkptr(s.base() + i)
			x.ptr().next = s.manualFreeList
			s.manualFreeList = x
		}
		// 拿到了新的，插入链表
		list.insert(s)
	}

	x := s.manualFreeList
	if x.ptr() == nil {
		throw("span has no free stacks")
	}

	s.manualFreeList = x.ptr().next
	s.allocCount++

	if s.manualFreeList.ptr() == nil {
		// all stacks in s are allocated.
		list.remove(s)
	}

	return x
}

// Adds stack x to the free pool. Must be called with stackpool[order].item.mu held.
func stackpoolfree(x gclinkptr, order uint8) {
	s := spanOfUnchecked(uintptr(x))
	if s.state.get() != mSpanManual {
		throw("freeing stack not in a stack span")
	}
	if s.manualFreeList.ptr() == nil {
		// s will now have a free stack
		// 放回缓冲池
		stackpool[order].item.span.insert(s)
	}
	x.ptr().next = s.manualFreeList
	s.manualFreeList = x
	s.allocCount--
	if gcphase == _GCoff && s.allocCount == 0 {
		//
		// Span is completely free. Return it to the heap immediately if we're sweeping.
		//
		// If GC is active, we delay the free until the end of
		// GC to avoid the following type of situation:
		//
		// 1) GC starts, scans a SudoG but does not yet mark the SudoG.elem pointer
		// 2) The stack that pointer points to is copied
		// 3) The old stack is freed
		// 4) The containing span is marked free
		// 5) GC attempts to mark the SudoG.elem pointer. The
		//    marking fails because the pointer looks like a
		//    pointer into a free span.
		//
		// By not freeing, we prevent step #4 until GC is done.
		stackpool[order].item.span.remove(s) // 拿出来
		s.manualFreeList = 0
		osStackFree(s) // linux上为空函数
		// 放回堆里
		mheap_.freeManual(s, &memstats.stacks_inuse)
	}
}

// stackcacherefill/stackcacherelease implement a global pool of stack segments.
// The pool is required to prevent unlimited growth of per-thread caches.
// mcache.stackcache 填充 线程里的栈内存缓存
//go:systemstack
func stackcacherefill(c *mcache, order uint8) {
	if stackDebug >= 1 {
		print("stackcacherefill order=", order, "\n")
	}

	// Grab some stacks from the global cache.
	// Grab half of the allowed capacity (to prevent thrashing).
	var list gclinkptr
	var size uintptr
	lock(&stackpool[order].item.mu)
	for size < _StackCacheSize/2 {
		x := stackpoolalloc(order)  // 从堆分配一个手动管理的 mspan
		x.ptr().next = list
		list = x
		size += _FixedStack << order
	}
	unlock(&stackpool[order].item.mu)
	c.stackcache[order].list = list
	c.stackcache[order].size = size
}

//go:systemstack
func stackcacherelease(c *mcache, order uint8) {
	if stackDebug >= 1 {
		print("stackcacherelease order=", order, "\n")
	}
	x := c.stackcache[order].list
	size := c.stackcache[order].size
	lock(&stackpool[order].item.mu)
	for size > _StackCacheSize/2 {
		y := x.ptr().next
		stackpoolfree(x, order)
		x = y
		size -= _FixedStack << order
	}
	unlock(&stackpool[order].item.mu)
	c.stackcache[order].list = x
	c.stackcache[order].size = size
}

//go:systemstack
func stackcache_clear(c *mcache) {
	if stackDebug >= 1 {
		print("stackcache clear\n")
	}
	for order := uint8(0); order < _NumStackOrders; order++ {
		lock(&stackpool[order].item.mu)
		x := c.stackcache[order].list
		for x.ptr() != nil {
			y := x.ptr().next
			stackpoolfree(x, order)
			x = y
		}
		c.stackcache[order].list = 0
		c.stackcache[order].size = 0
		unlock(&stackpool[order].item.mu)
	}
}

// stackalloc allocates an n byte stack.
// 返回的不是指针是栈结构
// stackalloc must run on the system stack because it uses per-P resources and must not split the stack.
//
//go:systemstack
func stackalloc(n uint32) stack {
	// Stackalloc must be called on scheduler stack, so that we
	// never try to grow the stack during the code that stackalloc runs.
	// Doing so would cause a deadlock (issue 1547).
	thisg := getg()
	// 必须是当前线程的g0
	if thisg != thisg.m.g0 {
		throw("stackalloc not on scheduler stack")
	}

	if n&(n-1) != 0 {
		throw("stack size not a power of 2")
	}

	if stackDebug >= 1 {
		print("stackalloc ", n, "\n")
	}

	// 从系统分配栈
	if debug.efence != 0 || stackFromSystem != 0 {
		n = uint32(alignUp(uintptr(n), physPageSize))
		// 系统调用分配一块内存，不进入堆管理之中
		v := sysAlloc(uintptr(n), &memstats.stacks_sys)
		if v == nil {
			throw("out of memory (stackalloc)")
		}

		// v 是低地址
		return stack{uintptr(v), uintptr(v) + uintptr(n)}
	}

	// Small stacks are allocated with a fixed-size free-list allocator.
	// If we need a stack of a bigger size, we fall back on重新回到 allocating a dedicated span.
	var v unsafe.Pointer
	// n < 32K && n < 32K _FixedStack = 2048 _NumStackOrders = 4,  _StackCacheSize = 32K
	if n < _FixedStack<<_NumStackOrders && n < _StackCacheSize {
		// 小栈分配
		order := uint8(0)
		n2 := n
		// 2k
		for n2 > _FixedStack {
			order++
			n2 >>= 1
		}

		var x gclinkptr
		if stackNoCache != 0 || thisg.m.p == 0 || thisg.m.preemptoff != "" {
			// 不能从p里面拿
			// thisg.m.p == 0 can happen in the guts of exitsyscall or procresize.
			// Just get a stack from the global pool.
			// Also don't touch stackcache during gc as it's flushed concurrently.
			lock(&stackpool[order].item.mu)
			// 全局 stackpoll 里分配栈空间
			x = stackpoolalloc(order)
			unlock(&stackpool[order].item.mu)
		} else {
			c := thisg.m.p.ptr().mcache
			// 线程 局部 mcache的栈缓存
			x = c.stackcache[order].list
			if x.ptr() == nil {
				stackcacherefill(c, order) // 填充线程 局部 mcache的栈缓存
				x = c.stackcache[order].list
			}
			c.stackcache[order].list = x.ptr().next
			c.stackcache[order].size -= uintptr(n)
		}
		v = unsafe.Pointer(x)
	} else {  // >= 32KB
		// 大栈分配
		var s *mspan
		// n / 8K 算得页数
		npage := uintptr(n) >> _PageShift
		log2npage := stacklog2(npage)

		// Try to get a stack from the large stack cache.
		lock(&stackLarge.lock)
		//
		if !stackLarge.free[log2npage].isEmpty() {
			// 从全局大栈缓存上分配内存
			s = stackLarge.free[log2npage].first
			stackLarge.free[log2npage].remove(s)
		}
		unlock(&stackLarge.lock)

		lockWithRankMayAcquire(&mheap_.lock, lockRankMheap)

		if s == nil {
			// Allocate a new stack from the heap.
			// 从堆上分配手动管理的mspan， 还是分配 size = n 大小的栈内存
			s = mheap_.allocManual(npage, &memstats.stacks_inuse)
			if s == nil {
				throw("out of memory")
			}
			// 空函数，只针对openbsd系统 有特殊处理
			osStackAlloc(s)
			// 元素大小以栈为单位， 一般就当成一个mspan里只有一个对象
			s.elemsize = uintptr(n)
		}
		v = unsafe.Pointer(s.base())
	}

	if raceenabled {
		racemalloc(v, uintptr(n))
	}
	if msanenabled {
		msanmalloc(v, uintptr(n))
	}
	if stackDebug >= 1 {
		print("  allocated ", v, "\n")
	}

	return stack{uintptr(v), uintptr(v) + uintptr(n)}
}

// stackfree frees an n byte stack allocation at stk.
//
// stackfree must run on the system stack because it uses per-P
// resources and must not split the stack.
// 释放栈空间，与栈分配的逻辑是反向的
//go:systemstack
func stackfree(stk stack) {
	gp := getg()
	v := unsafe.Pointer(stk.lo)
	n := stk.hi - stk.lo
	if n&(n-1) != 0 {
		throw("stack not a power of 2")
	}
	if stk.lo+n < stk.hi {
		throw("bad stack size")
	}
	if stackDebug >= 1 {
		println("stackfree", v, n)
		memclrNoHeapPointers(v, n) // for testing, clobber stack data
	}
	if debug.efence != 0 || stackFromSystem != 0 { //固定是false
		// 释放
		if debug.efence != 0 || stackFaultOnFree != 0 {
			sysFault(v, n)
		} else {
			sysFree(v, n, &memstats.stacks_sys)
		}
		return
	}
	if msanenabled {
		msanfree(v, n)
	}

	if n < _FixedStack<<_NumStackOrders && n < _StackCacheSize {
		order := uint8(0)
		n2 := n
		for n2 > _FixedStack {
			order++
			n2 >>= 1
		}
		x := gclinkptr(v)
		if stackNoCache != 0 || gp.m.p == 0 || gp.m.preemptoff != "" {
			lock(&stackpool[order].item.mu)
			stackpoolfree(x, order)
			unlock(&stackpool[order].item.mu)
		} else {
			c := gp.m.p.ptr().mcache
			if c.stackcache[order].size >= _StackCacheSize {
				stackcacherelease(c, order)
			}
			x.ptr().next = c.stackcache[order].list
			c.stackcache[order].list = x
			c.stackcache[order].size += n
		}
	} else {
		s := spanOfUnchecked(uintptr(v))
		if s.state.get() != mSpanManual {
			println(hex(s.base()), v)
			throw("bad span state")
		}
		if gcphase == _GCoff {
			// Free the stack immediately if we're
			// sweeping.
			osStackFree(s)
			mheap_.freeManual(s, &memstats.stacks_inuse)
		} else {
			// If the GC is running, we can't return a
			// stack span to the heap because it could be
			// reused as a heap span, and this state
			// change would race with GC. Add it to the
			// large stack cache instead.
			log2npage := stacklog2(s.npages)
			lock(&stackLarge.lock)
			stackLarge.free[log2npage].insert(s)
			unlock(&stackLarge.lock)
		}
	}
}

// 1M 初始化时被设置为1G
var maxstacksize uintptr = 1 << 20 // enough until runtime.main sets it for real 实际

var ptrnames = []string{
	0: "scalar",
	1: "ptr",
}

// Stack frame layout 栈帧布局
//
// (x86)
// +------------------+
// | args from caller |
// +------------------+ <- frame->argp 参数指针
// |  return address  |
// +------------------+
// |  caller's BP (*) | (*) 条件是 if framepointer_enabled && varp < sp
// +------------------+ <- frame->varp 本地变量指针
// |     locals       |
// +------------------+
// |  args to callee  |
// +------------------+ <- frame->sp
//
// (arm)
// +------------------+
// | args from caller |
// +------------------+ <- frame->argp
// | caller's retaddr |
// +------------------+ <- frame->varp
// |     locals       |
// +------------------+
// |  args to callee  |
// +------------------+
// |  return address  |
// +------------------+ <- frame->sp

type adjustinfo struct {
	old   stack
	delta uintptr // ptr distance from old to new stack (newbase - oldbase)
	cache pcvalueCache

	// sghi is the highest sudog.elem on the stack.
	sghi uintptr
}

// Adjustpointer checks whether *vpp is in the old stack described by adjinfo.
// If so, it rewrites *vpp to point into the new stack.
func adjustpointer(adjinfo *adjustinfo, vpp unsafe.Pointer) {
	pp := (*uintptr)(vpp)
	p := *pp
	if stackDebug >= 4 {
		print("        ", pp, ":", hex(p), "\n")
	}
	if adjinfo.old.lo <= p && p < adjinfo.old.hi {
		*pp = p + adjinfo.delta
		if stackDebug >= 3 {
			print("        adjust ptr ", pp, ":", hex(p), " -> ", hex(*pp), "\n")
		}
	}
}

// Information from the compiler about the layout of stack frames.
type bitvector struct {
	n        int32 // # of bits
	bytedata *uint8
}

// ptrbit returns the i'th bit in bv. bitvector
// ptrbit is less efficient than iterating directly over bitvector bits,
// and should only be used in non-performance-critical code.
// See adjustpointers for an example of a high-efficiency walk of a bitvector.
func (bv *bitvector) ptrbit(i uintptr) uint8 {
	b := *(addb(bv.bytedata, i/8))
	return (b >> (i % 8)) & 1
}

// bv describes the memory starting at address scanp.
// Adjust any pointers contained therein.
func adjustpointers(scanp unsafe.Pointer, bv *bitvector, adjinfo *adjustinfo, f funcInfo) {
	minp := adjinfo.old.lo
	maxp := adjinfo.old.hi
	delta := adjinfo.delta
	num := uintptr(bv.n)
	// If this frame might contain channel receive slots, use CAS
	// to adjust pointers. If the slot hasn't been received into
	// yet, it may contain stack pointers and a concurrent send
	// could race with adjusting those pointers. (The sent value
	// itself can never contain stack pointers.)
	useCAS := uintptr(scanp) < adjinfo.sghi
	for i := uintptr(0); i < num; i += 8 {
		if stackDebug >= 4 {
			for j := uintptr(0); j < 8; j++ {
				print("        ", add(scanp, (i+j)*sys.PtrSize), ":", ptrnames[bv.ptrbit(i+j)], ":", hex(*(*uintptr)(add(scanp, (i+j)*sys.PtrSize))), " # ", i, " ", *addb(bv.bytedata, i/8), "\n")
			}
		}
		b := *(addb(bv.bytedata, i/8))
		for b != 0 {
			j := uintptr(sys.Ctz8(b))
			b &= b - 1
			pp := (*uintptr)(add(scanp, (i+j)*sys.PtrSize))
		retry:
			p := *pp
			if f.valid() && 0 < p && p < minLegalPointer && debug.invalidptr != 0 {
				// Looks like a junk value in a pointer slot.
				// Live analysis wrong?
				getg().m.traceback = 2
				print("runtime: bad pointer in frame ", funcname(f), " at ", pp, ": ", hex(p), "\n")
				throw("invalid pointer found on stack")
			}
			if minp <= p && p < maxp {
				if stackDebug >= 3 {
					print("adjust ptr ", hex(p), " ", funcname(f), "\n")
				}
				if useCAS {
					ppu := (*unsafe.Pointer)(unsafe.Pointer(pp))
					if !atomic.Casp1(ppu, unsafe.Pointer(p), unsafe.Pointer(p+delta)) {
						goto retry
					}
				} else {
					*pp = p + delta
				}
			}
		}
	}
}

// Note: the argument/return area is adjusted by the callee.
func adjustframe(frame *stkframe, arg unsafe.Pointer) bool {
	adjinfo := (*adjustinfo)(arg)
	if frame.continpc == 0 {
		// Frame is dead.
		return true
	}
	f := frame.fn
	if stackDebug >= 2 {
		print("    adjusting ", funcname(f), " frame=[", hex(frame.sp), ",", hex(frame.fp), "] pc=", hex(frame.pc), " continpc=", hex(frame.continpc), "\n")
	}
	if f.funcID == funcID_systemstack_switch {
		// A special routine at the bottom of stack of a goroutine that does a systemstack call.
		// We will allow it to be copied even though we don't
		// have full GC info for it (because it is written in asm).
		return true
	}

	locals, args, objs := getStackMap(frame, &adjinfo.cache, true)

	// Adjust local variables if stack frame has been allocated.
	if locals.n > 0 {
		size := uintptr(locals.n) * sys.PtrSize
		adjustpointers(unsafe.Pointer(frame.varp-size), &locals, adjinfo, f)
	}

	// Adjust saved base pointer if there is one.
	if sys.ArchFamily == sys.AMD64 && frame.argp-frame.varp == 2*sys.RegSize {
		if !framepointer_enabled {
			print("runtime: found space for saved base pointer, but no framepointer experiment\n")
			print("argp=", hex(frame.argp), " varp=", hex(frame.varp), "\n")
			throw("bad frame layout")
		}
		if stackDebug >= 3 {
			print("      saved bp\n")
		}
		if debugCheckBP {
			// Frame pointers should always point to the next higher frame on
			// the Go stack (or be nil, for the top frame on the stack).
			bp := *(*uintptr)(unsafe.Pointer(frame.varp))
			if bp != 0 && (bp < adjinfo.old.lo || bp >= adjinfo.old.hi) {
				println("runtime: found invalid frame pointer")
				print("bp=", hex(bp), " min=", hex(adjinfo.old.lo), " max=", hex(adjinfo.old.hi), "\n")
				throw("bad frame pointer")
			}
		}
		adjustpointer(adjinfo, unsafe.Pointer(frame.varp))
	}

	// Adjust arguments.
	if args.n > 0 {
		if stackDebug >= 3 {
			print("      args\n")
		}
		adjustpointers(unsafe.Pointer(frame.argp), &args, adjinfo, funcInfo{})
	}

	// Adjust pointers in all stack objects (whether they are live or not).
	// See comments in mgcmark.go:scanframeworker.
	if frame.varp != 0 {
		for _, obj := range objs {
			off := obj.off
			base := frame.varp // locals base pointer
			if off >= 0 {
				base = frame.argp // arguments and return values base pointer
			}
			p := base + uintptr(off)
			if p < frame.sp {
				// Object hasn't been allocated in the frame yet.
				// (Happens when the stack bounds check fails and
				// we call into morestack.)
				continue
			}
			t := obj.typ
			gcdata := t.gcdata
			var s *mspan
			if t.kind&kindGCProg != 0 {
				// See comments in mgcmark.go:scanstack
				s = materializeGCProg(t.ptrdata, gcdata)
				gcdata = (*byte)(unsafe.Pointer(s.startAddr))
			}
			for i := uintptr(0); i < t.ptrdata; i += sys.PtrSize {
				if *addb(gcdata, i/(8*sys.PtrSize))>>(i/sys.PtrSize&7)&1 != 0 {
					adjustpointer(adjinfo, unsafe.Pointer(p+i))
				}
			}
			if s != nil {
				dematerializeGCProg(s)
			}
		}
	}

	return true
}

func adjustctxt(gp *g, adjinfo *adjustinfo) {
	adjustpointer(adjinfo, unsafe.Pointer(&gp.sched.ctxt))
	if !framepointer_enabled {
		return
	}
	if debugCheckBP {
		bp := gp.sched.bp
		if bp != 0 && (bp < adjinfo.old.lo || bp >= adjinfo.old.hi) {
			println("runtime: found invalid top frame pointer")
			print("bp=", hex(bp), " min=", hex(adjinfo.old.lo), " max=", hex(adjinfo.old.hi), "\n")
			throw("bad top frame pointer")
		}
	}
	adjustpointer(adjinfo, unsafe.Pointer(&gp.sched.bp))
}

func adjustdefers(gp *g, adjinfo *adjustinfo) {
	// Adjust pointers in the Defer structs.
	// We need to do this first because we need to adjust the
	// defer.link fields so we always work on the new stack.
	adjustpointer(adjinfo, unsafe.Pointer(&gp._defer))
	for d := gp._defer; d != nil; d = d.link {
		adjustpointer(adjinfo, unsafe.Pointer(&d.fn))
		adjustpointer(adjinfo, unsafe.Pointer(&d.sp))
		adjustpointer(adjinfo, unsafe.Pointer(&d._panic))
		adjustpointer(adjinfo, unsafe.Pointer(&d.link))
		adjustpointer(adjinfo, unsafe.Pointer(&d.varp))
		adjustpointer(adjinfo, unsafe.Pointer(&d.fd))
	}

	// Adjust defer argument blocks the same way we adjust active stack frames.
	// Note: this code is after the loop above, so that if a defer record is
	// stack allocated, we work on the copy in the new stack.
	tracebackdefers(gp, adjustframe, noescape(unsafe.Pointer(adjinfo)))
}

func adjustpanics(gp *g, adjinfo *adjustinfo) {
	// Panics are on stack and already adjusted.
	// Update pointer to head of list in G.
	adjustpointer(adjinfo, unsafe.Pointer(&gp._panic))
}

func adjustsudogs(gp *g, adjinfo *adjustinfo) {
	// the data elements pointed to by a SudoG structure
	// might be in the stack.
	for s := gp.waiting; s != nil; s = s.waitlink {
		adjustpointer(adjinfo, unsafe.Pointer(&s.elem))
	}
}

// 用 b 填充 栈空间
func fillstack(stk stack, b byte) {
	for p := stk.lo; p < stk.hi; p++ {
		*(*byte)(unsafe.Pointer(p)) = b
	}
}

func findsghi(gp *g, stk stack) uintptr {
	var sghi uintptr
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		p := uintptr(sg.elem) + uintptr(sg.c.elemsize)
		if stk.lo <= p && p < stk.hi && p > sghi {
			sghi = p
		}
	}
	return sghi
}

// syncadjustsudogs adjusts gp's sudogs and copies the part of gp's
// stack they refer to while synchronizing with concurrent channel
// operations. It returns the number of bytes of stack copied.
func syncadjustsudogs(gp *g, used uintptr, adjinfo *adjustinfo) uintptr {
	if gp.waiting == nil {
		return 0
	}

	// Lock channels to prevent concurrent send/receive.
	var lastc *hchan
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		if sg.c != lastc {
			// There is a ranking cycle here between gscan bit and
			// hchan locks. Normally, we only allow acquiring hchan
			// locks and then getting a gscan bit. In this case, we
			// already have the gscan bit. We allow acquiring hchan
			// locks here as a special case, since a deadlock can't
			// happen because the G involved must already be
			// suspended. So, we get a special hchan lock rank here
			// that is lower than gscan, but doesn't allow acquiring
			// any other locks other than hchan.
			lockWithRank(&sg.c.lock, lockRankHchanLeaf)
		}
		lastc = sg.c
	}

	// Adjust sudogs.
	adjustsudogs(gp, adjinfo)

	// Copy the part of the stack the sudogs point in to
	// while holding the lock to prevent races on
	// send/receive slots.
	var sgsize uintptr
	if adjinfo.sghi != 0 {
		oldBot := adjinfo.old.hi - used
		newBot := oldBot + adjinfo.delta
		sgsize = adjinfo.sghi - oldBot
		memmove(unsafe.Pointer(newBot), unsafe.Pointer(oldBot), sgsize)
	}

	// Unlock channels.
	lastc = nil
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		if sg.c != lastc {
			unlock(&sg.c.lock)
		}
		lastc = sg.c
	}

	return sgsize
}

// Copies gp's stack to a new stack of a different size.
// Caller must have changed gp status to Gcopystack. 调用方一定要先切换状态
func copystack(gp *g, newsize uintptr) {
	// 表示正在进行系统调用
	if gp.syscallsp != 0 {
		throw("stack growth not allowed in system call")
	}

	old := gp.stack
	// 表示未分配栈空间
	if old.lo == 0 {
		throw("nil stackbase")
	}
	// [sp, hi] 已使用
	used := old.hi - gp.sched.sp

	// 1. 分配新的内存空间作为栈 allocate new stack
	new := stackalloc(uint32(newsize))

	if stackPoisonCopy != 0 {
		// 清栈
		fillstack(new, 0xfd)
	}

	if stackDebug >= 1 {
		print("copystack gp=", gp, " [", hex(old.lo), " ", hex(old.hi-used), " ", hex(old.hi), "]", " -> [", hex(new.lo), " ", hex(new.hi-used), " ", hex(new.hi), "]/", newsize, "\n")
	}

	// Compute adjustment.
	var adjinfo adjustinfo
	adjinfo.old = old
	adjinfo.delta = new.hi - old.hi

	// Adjust sudogs, synchronizing with channel ops if necessary.
	ncopy := used
	if !gp.activeStackChans {
		adjustsudogs(gp, &adjinfo)  // 2. 调整sudog 结构体的指针
	} else {
		// sudogs may be pointing in to the stack and gp has
		// released channel locks, so other goroutines could
		// be writing to gp's stack. Find the highest such
		// pointer so we can handle everything there and below
		// carefully. (This shouldn't be far from the bottom
		// of the stack, so there's little cost in handling
		// everything below it carefully.)
		adjinfo.sghi = findsghi(gp, old)

		// Synchronize with channel ops and copy the part of
		// the stack they may interact with.
		ncopy -= syncadjustsudogs(gp, used, &adjinfo)  // 2. 调整sudog 结构体的指针
	}

	// Copy the stack (or the rest of it) to the new location
	// 拷贝栈 [hi-ncopy, hi)
	memmove(unsafe.Pointer(new.hi-ncopy), unsafe.Pointer(old.hi-ncopy), ncopy)

	// Adjust remaining structures that have pointers into stacks.
	// We have to do most of these before we traceback the new
	// stack because gentraceback uses them.
	// 3. 调整栈相关的指针 , 这三个函数都会调用 adjustpointer
	adjustctxt(gp, &adjinfo)
	adjustdefers(gp, &adjinfo)
	adjustpanics(gp, &adjinfo)
	if adjinfo.sghi != 0 {
		adjinfo.sghi += adjinfo.delta
	}

	// 4. 更新栈指针 Swap out old stack for new one
	gp.stack = new
	gp.stackguard0 = new.lo + _StackGuard // NOTE: might clobber击倒 a preempt request
	gp.sched.sp = new.hi - used  //更新sp
	gp.stktopsp += adjinfo.delta

	// Adjust pointers in the new stack.
	gentraceback(^uintptr(0), ^uintptr(0), 0, gp, 0, nil, 0x7fffffff, adjustframe, noescape(unsafe.Pointer(&adjinfo)), 0)

	// free old stack
	if stackPoisonCopy != 0 {  // always false
		// 用填0xfc 填满栈空间
		fillstack(old, 0xfc)
	}
	// 5. 释放旧的栈空间
	stackfree(old)  // 小栈 通过 stackpoolfree 释放
}

// round x up to a power of 2.
// 2^n > x  传 x=0, 返回1
func round2(x int32) int32 {
	s := uint(0)
	for 1<<s < x {
		s++
	}
	return 1 << s
}

// Called from runtime·morestack when more stack is needed.
// Allocate larger stack and relocate迁移 to new stack.
// Stack growth is multiplicative乘法, for constant一致的 amortized cost 均摊成本.
//
// g->atomicstatus will be Grunning or Gscanrunning upon entry.
// If the scheduler is trying to stop this g, then it will set preemptStop.
//
// This must be nowritebarrierrec because it can be called as part of
// stack growth from other nowritebarrierrec functions, but the
// compiler doesn't check this.
// 汇编函数morestack 会调用此函数 栈扩容, 此函数不应该正常return
// morestack 会优先响应抢占
//go:nowritebarrierrec
func newstack() {
	thisg := getg()

	// TODO: double check all gp. shouldn't be getg().
	// fork时不能调整栈
	if thisg.m.morebuf.g.ptr().stackguard0 == stackFork {
		throw("stack growth after fork")
	}

	if thisg.m.morebuf.g.ptr() != thisg.m.curg {
		print("runtime: newstack called from g=", hex(thisg.m.morebuf.g), "\n"+"\tm=", thisg.m, " m->curg=", thisg.m.curg, " m->g0=", thisg.m.g0, " m->gsignal=", thisg.m.gsignal, "\n")
		morebuf := thisg.m.morebuf
		traceback(morebuf.pc, morebuf.sp, morebuf.lr, morebuf.g.ptr())
		throw("runtime: wrong goroutine in newstack")
	}

	gp := thisg.m.curg

	// 抛出异常然后退出
	if thisg.m.curg.throwsplit {
		// Update syscallsp, syscallpc in case traceback uses them.
		morebuf := thisg.m.morebuf
		gp.syscallsp = morebuf.sp
		gp.syscallpc = morebuf.pc
		pcname, pcoff := "(unknown)", uintptr(0)
		f := findfunc(gp.sched.pc)
		if f.valid() {
			pcname = funcname(f)
			pcoff = gp.sched.pc - f.entry
		}
		print("runtime: newstack at ", pcname, "+", hex(pcoff),
			" sp=", hex(gp.sched.sp), " stack=[", hex(gp.stack.lo), ", ", hex(gp.stack.hi), "]\n",
			"\tmorebuf={pc:", hex(morebuf.pc), " sp:", hex(morebuf.sp), " lr:", hex(morebuf.lr), "}\n",
			"\tsched={pc:", hex(gp.sched.pc), " sp:", hex(gp.sched.sp), " lr:", hex(gp.sched.lr), " ctxt:", gp.sched.ctxt, "}\n")

		thisg.m.traceback = 2 // Include runtime frames
		traceback(morebuf.pc, morebuf.sp, morebuf.lr, gp)
		throw("runtime: stack split at bad time")
	}

	morebuf := thisg.m.morebuf
	thisg.m.morebuf.pc = 0
	thisg.m.morebuf.lr = 0
	thisg.m.morebuf.sp = 0
	thisg.m.morebuf.g = 0

	// NOTE: stackguard0 may change underfoot, if another thread
	// is about to try to preempt gp. Read it just once and use that same
	// value now and below.
	// 是抢占请求，而不是扩栈
	preempt := atomic.Loaduintptr(&gp.stackguard0) == stackPreempt

	// Be conservative保守 about where we preempt.
	// We are interested in preempting user Go code, not runtime code.
	// If we're holding locks, mallocing, or preemption is disabled, don't
	// preempt.
	// This check is very early in newstack so that even the status change
	// from Grunning to Gwaiting and back doesn't happen in this case.
	// That status change by itself can be viewed as a small preemption,
	// because the GC might change Gwaiting to Gscanwaiting, and then
	// this goroutine has to wait for the GC to finish before continuing.
	// If the GC is in some way dependent on this goroutine (for example,
	// it needs a lock held by the goroutine), that small preemption turns
	// into a real deadlock.
	if preempt {
		// 不能抢占
		if !canPreemptM(thisg.m) {
			// Let the goroutine keep running for now.
			// gp->preempt is set, so it will be preempted next time.
			// 恢复栈gaurd位置
			gp.stackguard0 = gp.stack.lo + _StackGuard
			// 重新调度 g 执行
			gogo(&gp.sched) // never return
		}
	}

	// 没有分配栈内存
	if gp.stack.lo == 0 {
		throw("missing stack in newstack")
	}

	sp := gp.sched.sp
	if sys.ArchFamily == sys.AMD64 || sys.ArchFamily == sys.I386 || sys.ArchFamily == sys.WASM {
		// The call to morestack cost a word.
		sp -= sys.PtrSize
	}

	if stackDebug >= 1 || sp < gp.stack.lo {
		print("runtime: newstack sp=", hex(sp), " stack=[", hex(gp.stack.lo), ", ", hex(gp.stack.hi), "]\n",
			"\tmorebuf={pc:", hex(morebuf.pc), " sp:", hex(morebuf.sp), " lr:", hex(morebuf.lr), "}\n",
			"\tsched={pc:", hex(gp.sched.pc), " sp:", hex(gp.sched.sp), " lr:", hex(gp.sched.lr), " ctxt:", gp.sched.ctxt, "}\n")
	}

	if sp < gp.stack.lo {
		print("runtime: gp=", gp, ", goid=", gp.goid, ", gp->status=", hex(readgstatus(gp)), "\n ")
		print("runtime: split stack overflow: ", hex(sp), " < ", hex(gp.stack.lo), "\n")
		throw("runtime: split stack overflow")
	}

	if preempt {
		// 不能抢占g0
		if gp == thisg.m.g0 {
			throw("runtime: preempt g0")
		}

		if thisg.m.p == 0 && thisg.m.locks == 0 {
			throw("runtime: g is running but p is not")
		}

		if gp.preemptShrink {
			// We're at a synchronous safe point now, so do the pending stack shrink.
			// 到达安全点, 处理缩栈请求
			gp.preemptShrink = false
			shrinkstack(gp)
		}

		if gp.preemptStop {
			// 被动响应抢占，切到其他g
			preemptPark(gp) // never returns
		}

		// Act like goroutine called runtime.Gosched.
		// 主动让出
		gopreempt_m(gp) // never return
	}

	// Allocate a bigger segment and move the stack.
	oldsize := gp.stack.hi - gp.stack.lo
	// 翻倍
	newsize := oldsize * 2

	// Make sure we grow at least as much as needed to fit the new frame.
	// (This is just an optimization - the caller of morestack will
	// recheck the bounds on return.)
	if f := findfunc(gp.sched.pc); f.valid() {
		max := uintptr(funcMaxSPDelta(f))
		for newsize-oldsize < max+_StackGuard {
			// 太小继续翻倍
			newsize *= 2
		}
	}

	// 栈太大, 实际上old size 只能是 1/2G以下
	if newsize > maxstacksize {
		print("runtime: goroutine stack exceeds ", maxstacksize, "-byte limit\n")
		print("runtime: sp=", hex(sp), " stack=[", hex(gp.stack.lo), ", ", hex(gp.stack.hi), "]\n")
		throw("stack overflow")
	}

	// The goroutine must be executing in order to call newstack,
	// so it must be Grunning (or Gscanrunning).
	// newstack() 进入栈拷贝阶段
	casgstatus(gp, _Grunning, _Gcopystack)

	// The concurrent GC will not scan the stack while we are doing the copy since
	// the gp is in a Gcopystack status.
	copystack(gp, newsize)
	if stackDebug >= 1 {
		print("stack grow done\n")
	}
	// 退出栈拷贝
	casgstatus(gp, _Gcopystack, _Grunning)
	// 调度其他g
	gogo(&gp.sched)
}

//go:nosplit
func nilfunc() {
	*(*uint8)(nil) = 0 // 奔溃
}

// adjust Gobuf as if it executed a call to fn
// and then did an immediate gosave.
//
func gostartcallfn(gobuf *gobuf, fv *funcval) {
	var fn unsafe.Pointer
	if fv != nil {
		fn = unsafe.Pointer(fv.fn)
	} else {
		fn = unsafe.Pointer(funcPC(nilfunc))
	}
	// sys_x86.go 伪造执行上下文
	gostartcall(gobuf, fn, unsafe.Pointer(fv))
}

// isShrinkStackSafe returns whether it's safe to attempt to shrink
// gp's stack. Shrinking the stack is only safe when we have precise
// pointer maps for all frames on the stack.
func isShrinkStackSafe(gp *g) bool {
	// We can't copy the stack if we're in a syscall.
	// The syscall might have pointers into the stack and
	// often we don't have precise pointer maps for the innermost
	// frames.
	//
	// We also can't copy the stack if we're at an asynchronous
	// safe-point because we don't have precise pointer maps for
	// all frames.
	// 不在进行系统调用，不是异步安全点
	return gp.syscallsp == 0 && !gp.asyncSafePoint
}

// Maybe shrink the stack being used by gp.
// 栈收缩 如果只使用了1/4 则收缩到一半
// gp must be stopped and we must own its stack. It may be in
// _Grunning, but only if this is our own user G.
func shrinkstack(gp *g) {
	if gp.stack.lo == 0 {
		throw("missing stack in shrinkstack")
	}

	if s := readgstatus(gp); s&_Gscan == 0 {
		// We don't own the stack via _Gscan. We could still own it if this is our own user G and we're on the system stack.
		if !(gp == getg().m.curg && getg() != getg().m.curg && s == _Grunning) {
			// We don't own the stack.
			throw("bad status in shrinkstack")
		}
	}
	if !isShrinkStackSafe(gp) {
		throw("shrinkstack at bad time")
	}
	// Check for self-shrinks while in a libcall. These may have
	// pointers into the stack disguised as uintptrs, but these
	// code paths should all be nosplit.
	if gp == getg().m.curg && gp.m.libcallsp != 0 {
		throw("shrinking stack in libcall")
	}

	if debug.gcshrinkstackoff > 0 {
		return
	}

	// symtab.go
	f := findfunc(gp.startpc)
	if f.valid() && f.funcID == funcID_gcBgMarkWorker {
		// We're not allowed to shrink the gcBgMarkWorker
		// stack (see gcBgMarkWorker for explanation).
		return
	}

	oldsize := gp.stack.hi - gp.stack.lo
	newsize := oldsize / 2 // 1/2
	// Don't shrink the allocation below the minimum-sized stack allocation.
	// 不能小于2k
	if newsize < _FixedStack {
		return
	}
	// Compute how much of the stack is currently in use and only
	// shrink the stack if gp is using less than a quarter of its
	// current stack. The currently used stack includes everything
	// down to the SP plus the stack guard space that ensures
	// there's room for nosplit functions.
	avail := gp.stack.hi - gp.stack.lo
	// 预先保留一串nosplit函数的占用空间 使用不超过1/4 才缩栈
	if used := gp.stack.hi - gp.sched.sp + _StackLimit; used >= avail/4 {
		return
	}

	if stackDebug > 0 {
		print("shrinking stack ", oldsize, "->", newsize, "\n")
	}

	copystack(gp, newsize)
}

// freeStackSpans frees unused stack spans at the end of GC.
// called from gcMarkTermination
func freeStackSpans() {
	// Scan stack pools for empty stack spans.
	for order := range stackpool {
		lock(&stackpool[order].item.mu)

		list := &stackpool[order].item.span
		for s := list.first; s != nil; {
			next := s.next
			// 没有一个分配
			if s.allocCount == 0 {
				list.remove(s)
				s.manualFreeList = 0
				osStackFree(s)
				// 放回堆
				mheap_.freeManual(s, &memstats.stacks_inuse)
			}
			s = next
		}
		unlock(&stackpool[order].item.mu)
	}

	// Free large stack spans.
	lock(&stackLarge.lock)

	for i := range stackLarge.free {
		for s := stackLarge.free[i].first; s != nil; {
			next := s.next
			stackLarge.free[i].remove(s)
			osStackFree(s)
			// 放回堆
			mheap_.freeManual(s, &memstats.stacks_inuse)
			s = next
		}
	}

	unlock(&stackLarge.lock)
}

// getStackMap returns the locals and arguments live pointer maps, and
// stack object list for frame.
func getStackMap(frame *stkframe, cache *pcvalueCache, debug bool) (locals, args bitvector, objs []stackObjectRecord) {
	targetpc := frame.continpc
	if targetpc == 0 {
		// Frame is dead. Return empty bitvectors.
		return
	}

	f := frame.fn
	pcdata := int32(-1)
	if targetpc != f.entry {
		// Back up to the CALL. If we're at the function entry
		// point, we want to use the entry map (-1), even if
		// the first instruction of the function changes the
		// stack map.
		targetpc--
		pcdata = pcdatavalue(f, _PCDATA_StackMapIndex, targetpc, cache)
	}
	if pcdata == -1 {
		// We do not have a valid pcdata value but there might be a
		// stackmap for this function. It is likely that we are looking
		// at the function prologue, assume so and hope for the best.
		pcdata = 0
	}

	// Local variables.
	size := frame.varp - frame.sp
	var minsize uintptr
	switch sys.ArchFamily {
	case sys.ARM64:
		minsize = sys.SpAlign
	default:
		minsize = sys.MinFrameSize
	}
	if size > minsize {
		stackid := pcdata
		stkmap := (*stackmap)(funcdata(f, _FUNCDATA_LocalsPointerMaps))
		if stkmap == nil || stkmap.n <= 0 {
			print("runtime: frame ", funcname(f), " untyped locals ", hex(frame.varp-size), "+", hex(size), "\n")
			throw("missing stackmap")
		}
		// If nbit == 0, there's no work to do.
		if stkmap.nbit > 0 {
			if stackid < 0 || stackid >= stkmap.n {
				// don't know where we are
				print("runtime: pcdata is ", stackid, " and ", stkmap.n, " locals stack map entries for ", funcname(f), " (targetpc=", hex(targetpc), ")\n")
				throw("bad symbol table")
			}
			locals = stackmapdata(stkmap, stackid)
			if stackDebug >= 3 && debug {
				print("      locals ", stackid, "/", stkmap.n, " ", locals.n, " words ", locals.bytedata, "\n")
			}
		} else if stackDebug >= 3 && debug {
			print("      no locals to adjust\n")
		}
	}

	// Arguments.
	if frame.arglen > 0 {
		if frame.argmap != nil {
			// argmap is set when the function is reflect.makeFuncStub or reflect.methodValueCall.
			// In this case, arglen specifies how much of the args section is actually live.
			// (It could be either all the args + results, or just the args.)
			args = *frame.argmap
			n := int32(frame.arglen / sys.PtrSize)
			if n < args.n {
				args.n = n // Don't use more of the arguments than arglen.
			}
		} else {
			stackmap := (*stackmap)(funcdata(f, _FUNCDATA_ArgsPointerMaps))
			if stackmap == nil || stackmap.n <= 0 {
				print("runtime: frame ", funcname(f), " untyped args ", hex(frame.argp), "+", hex(frame.arglen), "\n")
				throw("missing stackmap")
			}
			if pcdata < 0 || pcdata >= stackmap.n {
				// don't know where we are
				print("runtime: pcdata is ", pcdata, " and ", stackmap.n, " args stack map entries for ", funcname(f), " (targetpc=", hex(targetpc), ")\n")
				throw("bad symbol table")
			}
			if stackmap.nbit > 0 {
				args = stackmapdata(stackmap, pcdata)
			}
		}
	}

	// stack objects.
	p := funcdata(f, _FUNCDATA_StackObjects)
	if p != nil {
		n := *(*uintptr)(p)
		p = add(p, sys.PtrSize)
		*(*slice)(unsafe.Pointer(&objs)) = slice{array: noescape(p), len: int(n), cap: int(n)}
		// Note: the noescape above is needed to keep
		// getStackMap from "leaking param content:
		// frame".  That leak propagates up to getgcmask, then
		// GCMask, then verifyGCInfo, which converts the stack
		// gcinfo tests into heap gcinfo tests :(
	}

	return
}

// A stackObjectRecord is generated by the compiler for each stack object in a stack frame.
// This record must match the generator code in cmd/compile/internal/gc/ssa.go:emitStackObjects.
type stackObjectRecord struct {
	// offset in frame
	// if negative, offset from varp 变量在下面
	// if non-negative, offset from argp 参数在上面
	off int
	typ *_type
}

// This is exported as ABI0 via linkname so obj can call it.
// 导出的二进制接口
//go:nosplit
//go:linkname morestackc
func morestackc() {
	throw("attempt to execute system stack code on user stack")
}
