// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// defined constants
const (
	// G status
	//
	// Beyond indicating the general state of a G, the G status
	// acts like a lock on the goroutine's stack 锁住栈 (and hence此后 its
	// ability to execute user code).
	//
	// If you add to this list, add to the list
	// of "okay during garbage collection" status
	// in mgcmark.go too.
	//
	// TODO(austin): The _Gscan bit could be much lighter-weight. 更轻量
	// For example, we could choose not to run _Gscanrunnable
	// goroutines found in the run queue, rather than CAS-looping
	// until they become _Grunnable. 我们可以不选择_Gscanrunable协程运行，而不是循环cas直到 Gstatus = _Grunnable
	// And transitions like _Gscanwaiting -> _Gscanrunnable are actually okay because
	// they don't affect stack ownership 栈的所有者.

	// _Gidle means this goroutine was just allocated and has not
	// yet been initialized. 还未初始化
	_Gidle = iota // 0

	// _Grunnable means this goroutine is on a run queue. It is
	// not currently executing user code. The stack is not owned.
	//在运行队列，还未执行代码，并未持有栈
	_Grunnable // 1


	// _Grunning means this goroutine may execute user code. The
	// stack is owned by this goroutine. It is not on a run queue.
	// It is assigned an M and a P (g.m and g.m.p are valid).
	_Grunning // 2

	// _Gsyscall means this goroutine is executing a system call.
	// It is not executing user code. The stack is owned by this
	// goroutine. It is not on a run queue. It is assigned an M. no P
	_Gsyscall // 3

	// _Gwaiting means this goroutine is blocked in the runtime. 阻塞在runtime，例如chan接收
	// It is not executing user code. It is not on a run queue,
	// but should be recorded somewhere 应该在什么地方被引用了，例如chan waitq (e.g., a channel wait
	// queue) so it can be ready()d when necessary.

	//一般不持有栈，除非 chan操作会读/写栈上的数据(是加锁的)
	// The stack is not owned *except* that a channel operation may read or
	// write parts of the stack under the appropriate channel
	// lock. Otherwise, it is not safe to access the stack after a
	// goroutine enters _Gwaiting (e.g., it may get moved).
	//这个状态访问协程栈是不安全的
	_Gwaiting // 4

	// _Gmoribund_unused is currently unused, but hardcoded in gdb
	// scripts.
	_Gmoribund_unused // 5

	// _Gdead means this goroutine is currently unused. It may be
	// just exited, on a free list, or just being initialized 正在被初始化. It
	// is not executing user code. It may or may not have a stack
	// allocated.
	// The G and its stack (if any) are owned by the M (that is exiting the G or that obtained the G from the free list).
	_Gdead // 6

	// _Genqueue_unused is currently unused.
	_Genqueue_unused // 7

	// _Gcopystack means this goroutine's stack is being moved. It
	// is not executing user code and is not on a run queue. The
	// stack is owned by the goroutine that put it in _Gcopystack.
	// 栈迁移，
	_Gcopystack // 8

	// _Gpreempted means this goroutine stopped itself for因为 a suspendG函数的 preemption.
	// It is like _Gwaiting, but nothing is yet responsible for ready()ing it. 没有什么会负责恢复其执行
	// Some suspendG must CAS the status to _Gwaiting to take responsibility for
	// ready()ing this G. 必须将状态转为 _GWaiting, 才能保证之后继续执行
	_Gpreempted // 9

	// _Gscan combined with one of the above states other than 组合以上状态，除了_Grunning特别对待
	// _Grunning indicates that GC is scanning the stack在扫描栈.
	// The goroutine is not executing user code and the stack is owned
	// by the goroutine that set the _Gscan bit.
	//
	// _Gscanrunning is different: it is used to briefly block 短暂阻塞
	// state transitions while当 GC signals通知 the G to scan its own
	// stack. This is otherwise不像 like _Grunning.
	//
	// atomicstatus&~Gscan gives the state the goroutine will
	// return to 返回变为 (when the scan completes).
	_Gscan          = 0x1000
	_Gscanrunnable  = _Gscan + _Grunnable  // 0x1001
	_Gscanrunning   = _Gscan + _Grunning   // 0x1002
	_Gscansyscall   = _Gscan + _Gsyscall   // 0x1003
	_Gscanwaiting   = _Gscan + _Gwaiting   // 0x1004
	_Gscanpreempted = _Gscan + _Gpreempted // 0x1009
)

const (
	// P status

	// _Pidle means a P is not being used to run user code or the
	// scheduler. 既没有运行用户程序，也没有指定调度逻辑，
	// Typically, it's on the idle P list and available to the scheduler, 一般在空闲P列表，以被调度程序复用
	// but it may just be transitioning between other states. 也可能恰好是两种状态转换的中间态
	//
	// The P is owned by the idle list or by whatever is
	// transitioning its state. Its run queue is empty. P的运行队列是空的
	_Pidle = iota

	// _Prunning means a P is owned by an M and is being used to
	// run user code or the scheduler.
	// Only the M that owns this P is allowed to change the P's status from _Prunning.
	// The M may transition the P to _Pidle (if it has no more work to do),
	// _Psyscall (when entering a syscall), or _Pgcstop (to halt for the GC).
	// The M may also hand ownership of the P off directly to another M (e.g., to schedule a locked G).
	_Prunning

	// _Psyscall means a P is not running user code. It has
	// affinity亲密性 to an M in a syscall but is not owned by it and
	// may be stolen by another M 被其他M偷去. This is similar to _Pidle but
	// uses lightweight transitions and maintains M affinity.
	//
	// Leaving _Psyscall must be done with a CAS, either to steal
	// or retake the P.
	// 注意cas的ABA问题，Note that there's an ABA hazard: even if an M successfully CASes its original P back to _Prunning
	// after a syscall, it must understand the P may have been
	// used by another M in the interim临时的.
	_Psyscall

	// _Pgcstop means a P is halted for STW and owned by the M that stopped the world.
	// The M that stopped the world continues to use its P, even in _Pgcstop. Transitioning
	// from _Prunning to _Pgcstop causes an M to release its P and
	// park.
	//
	// The P retains保持 its run queue and startTheWorld will restart
	// the scheduler on Ps with non-empty run queues.
	_Pgcstop

	// _Pdead means a P is no longer used (GOMAXPROCS shrank shrink过去时). We
	// reuse Ps if GOMAXPROCS increases. A dead P is mostly
	// stripped脱去 of its resources, though a few things remain 虽然仍然保留了一些东西
	// (e.g., trace buffers).
	_Pdead
)

// Mutual exclusion locks.  In the uncontended case,
// as fast as spin locks (just a few user-level instructions),
// but on the contention path they sleep in the kernel.
// A zeroed Mutex is unlocked (no need to initialize each lock).
// Initialization is helpful for static lock ranking, but not required.
type mutex struct {
	// Empty struct if lock ranking is disabled, otherwise includes the lock rank
	lockRankStruct
	// Futex-based impl基于futex的实现 treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union过去时一个union, but unions break precise GC.
	key uintptr
}

// sleep and wakeup on one-time一次 events.
// before any calls to notesleep or notewakeup,
// must call noteclear to initialize the Note.
// then, exactly one thread can call notesleep
// and exactly one thread can call notewakeup (once).
// once notewakeup has been called, the notesleep
// will return.  future notesleep will return immediately.
// subsequent noteclear must be called only after
// previous notesleep has returned, e.g. it's disallowed
// to call noteclear straight after notewakeup. notewakeup 之后不能直接调用noteclear
//
// notetsleep (timeoutsleep) is like notesleep but wakes up after
// a given number of nanoseconds even if the event
// has not yet happened.  if a goroutine uses notetsleep to
// wake up early, it must wait to call noteclear until it
// can be sure that no other goroutine is calling
// notewakeup.
//
// notesleep/notetsleep are generally called on g0,
// notetsleepg is similar to notetsleep but is called on user g 用户g.
// lock_futex.go
type note struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

//闭包
type funcval struct {
	fn uintptr	//函数指针
	//函数数据跟在下面，通过指针引用
	// variable-size, fn-specific data here
}

//带方法接口
type iface struct {
	tab  *itab
	data unsafe.Pointer
}

//interface{} 内部实现结构
type eface struct {
	_type *_type
	data  unsafe.Pointer
}

func efaceOf(ep *interface{}) *eface {
	return (*eface)(unsafe.Pointer(ep))
}

// The guintptr, muintptr, and puintptr are all used to bypass绕过 write barriers.
// It is particularly特别 important to avoid write barriers when the current P has
// been released, because the GC thinks the world is stopped, and an
// unexpected write barrier would not be synchronized with the GC,
// which can lead to a half-executed write barrier that has marked the object
// but not queued it. If the GC skips the object and completes before the
// queuing can occur, it will incorrectly free the object.
//
// We tried using special assignment functions invoked only when not
// holding a running P, but then some updates to a particular memory
// word went through write barriers and some did not. This breaks the
// write barrier shadow checking mode, and it is also scary: better to have
// a word that is completely ignored by the GC than to have one for which
// only a few updates are ignored.
//
// Gs and Ps are always reachable via true pointers in the
// allgs and allp lists or (during allocation before they reach those lists)
// from stack variables.
//
// Ms are always reachable via true pointers either from allm or
// freem. Unlike Gs and Ps we do free Ms, so it's important that
// nothing ever hold an muintptr across a safe point.

// A guintptr holds a goroutine pointer, but typed as a uintptr
// to bypass write barriers. It is used in the Gobuf goroutine state
// and in scheduling lists that are manipulated without a P.
//
// The Gobuf.g goroutine pointer is almost always updated by assembly code.
// In one of the few places it is updated by Go code - func save - it must be
// treated as a uintptr to avoid a write barrier being emitted at a bad time.
// Instead of figuring out how to emit the write barriers missing in the
// assembly manipulation, we change the type of the field to uintptr,
// so that it does not require write barriers at all.
//
// Goroutine structs are published in the allg list and never freed.
// That will keep the goroutine structs from being collected.
// There is never a time that Gobuf.g's contain the only references
// to a goroutine: the publishing of the goroutine in allg comes first.
// Goroutine pointers are also kept in non-GC-visible places like TLS,
// so I can't see them ever moving. If we did want to start moving data
// in the GC, we'd need to allocate the goroutine structs from an
// alternate arena. Using guintptr doesn't make that problem any worse.
type guintptr uintptr

//跳过栈溢出检查
//go:nosplit
func (gp guintptr) ptr() *g { return (*g)(unsafe.Pointer(gp)) }

//go:nosplit
func (gp *guintptr) set(g *g) { *gp = guintptr(unsafe.Pointer(g)) }

//go:nosplit
func (gp *guintptr) cas(old, new guintptr) bool {
	return atomic.Casuintptr((*uintptr)(unsafe.Pointer(gp)), uintptr(old), uintptr(new))
}

// setGNoWB performs *gp = new without a write barrier.
// For times when it's impractical不实际 to use a guintptr.
//go:nosplit
//go:nowritebarrier
func setGNoWB(gp **g, new *g) {
	(*guintptr)(unsafe.Pointer(gp)).set(new)
}

type puintptr uintptr

//go:nosplit
func (pp puintptr) ptr() *p { return (*p)(unsafe.Pointer(pp)) }

//go:nosplit
func (pp *puintptr) set(p *p) { *pp = puintptr(unsafe.Pointer(p)) }

// muintptr is a *m that is not tracked by the garbage collector.
//
// Because we do free Ms, there are some additional constrains on
// muintptrs:
//
// 1. Never hold an muintptr locally across a safe point.
//
// 2. Any muintptr in the heap must be owned by the M itself so it can
//    ensure it is not in use when the last true *m is released.
type muintptr uintptr

//go:nosplit
func (mp muintptr) ptr() *m { return (*m)(unsafe.Pointer(mp)) }

//go:nosplit
func (mp *muintptr) set(m *m) { *mp = muintptr(unsafe.Pointer(m)) }

// setMNoWB performs *mp = new without a write barrier.
// For times when it's impractical to use an muintptr.
//go:nosplit
//go:nowritebarrier
func setMNoWB(mp **m, new *m) {
	(*muintptr)(unsafe.Pointer(mp)).set(new)
}

type gobuf struct {
	// The offsets of sp, pc, and g are known to (hard-coded in) libmach.
	//
	// ctxt is unusual不一样 with respect to GC: it may be a
	// heap-allocated funcval, so GC needs to track it, but it
	// needs to be set and cleared from assembly, where it's
	// difficult to have write barriers. However, ctxt is really a
	// saved, live register, and we only ever exchange it between
	// the real register and the gobuf. Hence, we treat it as a
	// root during stack scanning, which means assembly that saves
	// and restores it doesn't need write barriers. It's still
	// typed as a pointer so that any other writes from Go get
	// write barriers.
	sp   uintptr
	pc   uintptr
	g    guintptr
	ctxt unsafe.Pointer
	//保持返回值
	ret  sys.Uintreg	//uint64
	lr   uintptr
	bp   uintptr // basepointer for GOEXPERIMENT=framepointer
}

// sudog represents a g in a wait list 等待链表, such as for sending/receiving
// on a channel.
//
// sudog is necessary because the g ↔ synchronization object relation
// is many-to-many. A g can be on many wait lists, so there may be
// many sudogs for one g; and many gs may be waiting on the same
// synchronization object, so there may be many sudogs for one object.
//
// sudogs are allocated from a special pool. Use acquireSudog and
// releaseSudog to allocate and free them.
type sudog struct {
	// The following fields are protected by the hchan.lock of the
	// channel this sudog is blocking on. shrinkstack depends on
	// this for sudogs involved in channel ops.

	g *g

	next     *sudog
	prev     *sudog
	elem     unsafe.Pointer // data element (may point to stack)

	// The following fields are never accessed concurrently. 下面的字段不会被并发访问
	// For channels, waitlink is only accessed by g.
	// For semaphores, all fields (including the ones above)
	// are only accessed when holding a semaRoot信号根 lock.

	acquiretime int64
	releasetime int64
	ticket      uint32

	// isSelect indicates g is participating in a select, so
	// g.selectDone must be CAS'd to win the wake-up race.
	isSelect bool	//select{}中被阻塞

	parent      *sudog // semaRoot binary tree
	waitlink    *sudog // g.waiting list or semaRoot
	waittail    *sudog // semaRoot
	c           *hchan // channel
}

type libcall struct {
	fn   uintptr
	n    uintptr // number of parameters
	args uintptr // parameters 地址
	r1   uintptr // return values 地址
	r2   uintptr
	err  uintptr // error number 错误号
}

// describes how to handle callback
type wincallbackcontext struct {
	gobody       unsafe.Pointer // go function to call
	argsize      uintptr        // callback arguments size (in bytes)
	restorestack uintptr        // adjust stack on return by (in bytes) (386 only)
	cleanstack   bool
}

// Stack describes a Go execution stack.
// The bounds of the stack are exactly [lo, hi),
// with no implicit隐藏的 data structures on either side.
type stack struct {
	lo uintptr		//栈顶在下
	hi uintptr
}

// heldLockInfo gives info on a held lock and the rank of that lock
type heldLockInfo struct {
	lockAddr uintptr
	rank     lockRank	// int
}

type g struct {
	// Stack parameters.
	// stack describes the actual stack memory: [stack.lo, stack.hi).
	// stackguard0 is the stack pointer compared in the Go stack growth prologue.
	// It is stack.lo+StackGuard normally, but can be StackPreempt to trigger a preemption.
	// stackguard1 is the stack pointer compared in the C stack growth prologue.
	// It is stack.lo+StackGuard on g0 and gsignal stacks.
	// It is ~0 on other goroutine stacks, to trigger a call to morestackc (and crash).
	stack       stack   // 独立栈空间 offset known to runtime/cgo
	stackguard0 uintptr // 一般是 stack.lo 因为向下增长 offset known to liblink
	stackguard1 uintptr // offset known to liblink

	_panic       *_panic // innermost最里面的 panic - offset known to liblink
	_defer       *_defer // innermost defer
	m            *m      // current m; offset known to arm liblink
	sched        gobuf   // 保存上下文

	//  系统调用时，额外保存上下文地址
	syscallsp    uintptr        // if status==Gsyscall, syscallsp = sched.sp to use during gc
	syscallpc    uintptr        // if status==Gsyscall, syscallpc = sched.pc to use during gc

	stktopsp     uintptr        // 期望sp在栈顶，用户回溯检查 expected sp at top of stack, to check in traceback
	param        unsafe.Pointer // passed parameter on wakeup	chan send/recv = sudog 唤醒时,被其他地方传递来的参数

	atomicstatus uint32		// g的状态, 原子性访问
	stackLock    uint32 	// 全局都搜不到 sigprof/scang lock; TODO: fold in to atomicstatus
	goid         int64		// 协程唯一id
	schedlink    guintptr	// 连接gList 或者gQueue 的下一个g， 只能在gList | gQueue中选一个
	waitsince    int64      // 阻塞开始时间 approx time when the g become blocked
	waitreason   waitReason // 进入阻塞状态的原因 if status==Gwaiting

	preempt       bool // 抢占信号 preemption signal, duplicates stackguard0 = stackpreempt
	preemptStop   bool // transition to _Gpreempted on preemption; otherwise, just deschedule
	preemptShrink bool // 表示是否在一个安全点缩栈 shrink stack at synchronous safe point

	// asyncSafePoint is set if g is stopped at an asynchronous
	// safe point. This means there are frames on the stack
	// without precise pointer information.
	// preemt.go 异步抢占时标记
	asyncSafePoint bool

	paniconfault bool // panic (instead of crash) on unexpected fault address
	gcscandone   bool // g has scanned stack; protected by _Gscan bit in status
	throwsplit   bool // true 表示一定不能扩栈 must not split stack
	// activeStackChans indicates that there are unlocked channels
	// pointing into this goroutine's stack. If true, stack
	// copying needs to acquire channel locks to protect these
	// areas of the stack.
	activeStackChans bool

	raceignore     int8     // ignore race detection events
	sysblocktraced bool     // StartTrace has emitted发出 EvGoInSyscall about this goroutine
	sysexitticks   int64    // cputicks when syscall has returned (for tracing)
	traceseq       uint64   // trace event sequencer
	tracelastp     puintptr // last P emitted an event for this goroutine

	lockedm        muintptr	//g锁定在此m上运行

	writebuf       []byte

	//panic 时保存sp和pc, 收到信号时保存信号码和错误码
	sigcode0       uintptr	//信号代码
	sigcode1       uintptr
	sig            uint32
	sigpc          uintptr		//信号处理时的pc

	gopc           uintptr         // 创建此协程的go语句的代码地址 pc of go statement that created this goroutine
	ancestors      *[]ancestorInfo // 创建此g的g信息 ancestor information goroutine(s) that created this goroutine (only used if debug.tracebackancestors)

	startpc        uintptr         // 函数起始执行地址 pc of goroutine function
	racectx        uintptr			//race 竞争检测相关
	waiting        *sudog         // 执行等待链表里的sudug，相互反指 sudog structures this g is waiting on (that have a valid elem ptr); in lock order
	cgoCtxt        []uintptr      // cgo traceback context
	labels         unsafe.Pointer // profiler labels
	timer          *timer         // sleep 当前协程， sleep时 使用时才获取/创建 cached timer for time.Sleep
	selectDone     uint32         // select语句结束后 置0 are we participating in a select and did someone win the race?

	// Per-G GC state

	// gcAssistBytes is this G's GC assist credit in terms of
	// bytes allocated. If this is positive, then the G has credit
	// to allocate gcAssistBytes bytes without assisting. If this
	// is negative, then the G must correct this by performing
	// scan work. We track this in bytes to make it fast to update
	// and check for debt in the malloc hot path. The assist ratio
	// determines how this corresponds to scan work debt.
	gcAssistBytes int64			//记录分配了多少字节，gc时要还
}

type m struct {
	g0      *g     // 执行调度器的h goroutine with scheduling stack
	morebuf gobuf  // 扩栈时保存原来的上下文 gobuf arg to morestack
	divmod  uint32 // div/mod denominator分母 for arm - known to liblink

	// Fields not known to debuggers.
	procid        uint64       // 线程id for debuggers, but offset not hard-coded
	gsignal       *g           // 负责信号处理的g signal-handling g
	goSigStack    gsignalStack // 信号处理栈 Go-allocated signal handling stack
	sigmask       sigset       // 信号掩码 storage for saved signal mask
	tls           [6]uintptr   // 线程本地存储 thread-local storage (for x86 extern register)
	mstartfn      func()		//第一个执行的函数
	curg          *g       // 当前执行的用户g current running goroutine
	caughtsig     guintptr // 致命信号时运行的g，用于捕获错误信息，goroutine running during fatal signal
	p             puintptr // P attached p for executing go code (nil if not executing go code)
	nextp         puintptr	// 恢复执行后，要运行的下一个p
	oldp          puintptr // 上一个P, 进入系统调用时释放的p the p that was attached before executing a syscall
	id            int64   //M id
	mallocing     int32		//标记正在分配内存，防未完成分配的重入
	throwing      int32
	preemptoff    string // 关闭抢占 if != "", keep curg running on this m
	locks         int32
	dying         int32
	profilehz     int32
	spinning      bool // 没事找事做的自旋状态 m is out of work and is actively looking for work
	blocked       bool // m is blocked on a note 或者说sema，或者linux的futex
	newSigstack   bool // minit on C thread called sigaltstack
	printlock     int8
	incgo         bool   //正在执行cgo调用 m is executing a cgo call
	freeWait      uint32 // if == 0, safe to free g0 and delete m (atomic)
	fastrand      [2]uint32	//随机种子
	needextram    bool
	traceback     uint8

	ncgocall      uint64      // 次数 number of cgo calls in total
	ncgo          int32       // number of cgo calls currently in progress
	cgoCallersUse uint32      // if non-zero, cgoCallers in use temporarily
	cgoCallers    *cgoCallers // cgo调用奔溃的回调 cgo traceback if crashing in cgo call

	park          note		// 暂停线程，等待wake wake/sleep/clear
	alllink       *m 		// allm链表里 链接之后的m on allm
	schedlink     muintptr	//连接schet的空闲链表里的上下节点
	lockedg       guintptr		//锁定在此m运行的g
	createstack   [32]uintptr // 线程栈stack that created this thread.
	lockedExt     uint32      // 外部锁定线程的数量 tracking for external LockOSThread
	lockedInt     uint32      // runtime 内部锁住线程的数量 tracking for internal lockOSThread

	nextwaitm     muintptr    // next m waiting for lock
	//保存gopark调用参数
	waitunlockf   func(*g, unsafe.Pointer) bool	//gopark()相关
	waitlock      unsafe.Pointer
	waittraceev   byte		//traceGoPark
	waittraceskip int		//traceGoPark
	startingtrace bool		//only trace.go
	syscalltick   uint32
	freelink      *m // on sched.freem

	// these are here because they are too large to be on the stack
	// of low-level NOSPLIT functions.
	//放在栈上占空间
	libcall   libcall		//linux not used
	libcallpc uintptr // for cpu profiler
	libcallsp uintptr
	libcallg  guintptr
	syscall   libcall // stores syscall parameters on windows

	//linux VDSO virtual dynamic shared object
	vdsoSP uintptr // SP for traceback while in VDSO call (0 if not in call)
	vdsoPC uintptr // sigprof里使用 PC for traceback while in VDSO call

	// preemptGen counts the number of completed preemption
	// signals. This is used to detect when a preemption is
	// requested, but fails. Accessed atomically.
	preemptGen uint32		//记录抢占完成次数

	// Whether this is a pending preemption signal on this M.
	// Accessed atomically.
	signalPending uint32	//发抢占信号置1，抢占处理完置0

	//nothing
	dlogPerM //struct{}
	mOS		//struct{}

	// lockrank_on.go Up to 10 locks held by this m, maintained by the lock ranking code.
	locksHeldLen int	//持有锁的数量，最多达10个
	locksHeld    [10]heldLockInfo
}

type p struct {
	id          int32		//pid
	status      uint32 // one of pidle/prunning/...
	link        puintptr	//用于链接 runnable p 链表
	schedtick   uint32     // 调度次数 incremented on every scheduler call
	syscalltick uint32     // 系统调用次数 incremented on every system call
	sysmontick  sysmontick // 上一次被监控观察的计数 last tick observed by sysmon
	m           muintptr   // *m back-link to associated m (nil if idle)
	mcache      *mcache    // Per-P 缓存
	pcache      pageCache	//页缓存
	raceprocctx uintptr

	//缓存defer结构体，复用
	deferpool    [5][]*_defer // pool of available defer structs of different sizes (see panic.go)
	deferpoolbuf [5][32]*_defer

	// Cache of goroutine ids, amortizes摊分分期 accesses to runtime·sched.goidgen.
	goidcache    uint64		//避免每次都访问goidgen生成唯一goid
	goidcacheend uint64

	// Queue of runnable goroutines. Accessed without lock.
	runqhead uint32	//运行队列头
	runqtail uint32
	runq     [256]guintptr  //运行队列
	// runnext, if non-nil, is a runnable G that was ready'd by
	// the current G and should be run next instead of what's in
	// runq if there's time remaining in the running G's time
	// slice. It will inherit the time left in the current time
	// slice. If a set of goroutines is locked in a
	// communicate-and-wait pattern, this schedules that set as a
	// unit and eliminates the (potentially large) scheduling
	// latency that otherwise arises from adding the ready'd
	// goroutines to the end of the run queue.
	runnext guintptr	//下一个要运行的

	// 全局缓存 Available G's (status == Gdead)
	gFree struct {
		gList	//*g列表
		n int32
	}

	sudogcache []*sudog		//缓存sudog
	sudogbuf   [128]*sudog

	// Cache of mspan objects from the heap. mspan结构体缓存
	mspancache struct {
		// We need an explicit length here because this field is used
		// in allocation codepaths where write barriers are not allowed,
		// and eliminating the write barrier/keeping it eliminated from
		// slice updates is tricky, moreso than just managing the length
		// ourselves.
		len int
		buf [128]*mspan
	}

	tracebuf traceBufPtr	//uintptr

	// traceSweep indicates the sweep events should be traced.
	// This is used to defer the sweep start event until a span
	// has actually been swept.
	traceSweep bool //跟踪垃圾清除事件
	// traceSwept and traceReclaimed track the number of bytes
	// swept and reclaimed by sweeping in the current sweep loop.
	traceSwept, traceReclaimed uintptr

	palloc persistentAlloc // per-P to avoid mutex

	_ uint32 // Alignment for atomic fields below

	// The when field of the first entry on the timer heap.
	// This is updated using atomic functions.
	// This is 0 if the timer heap is empty.
	timer0When uint64		//timer0计时器的when

	// Per-P GC state
	gcAssistTime         int64    // 辅助gc回收时间 Nanoseconds in assistAlloc
	gcFractionalMarkTime int64    // gc碎片标记时间 Nanoseconds in fractional mark worker (atomic)
	gcBgMarkWorker       guintptr // (atomic)
	gcMarkWorkerMode     gcMarkWorkerMode

	// gcMarkWorkerStartTime is the nanotime() at which this mark
	// worker started.
	gcMarkWorkerStartTime int64

	// gcw is this P's GC work buffer cache. The work buffer is
	// filled by write barriers, drained by mutator assists, and
	// disposed on certain GC state transitions.
	gcw gcWork		//垃圾回收工作缓存

	// wbBuf is this P's GC write barrier buffer.
	//
	// TODO: Consider caching this in the running G.
	wbBuf wbBuf	//gc写屏障缓存

	runSafePointFn uint32 // if 1, run sched.safePointFn at next safe point

	// Lock for timers. We normally access the timers while running
	// on this P, but the scheduler can also do it from a different P.
	timersLock mutex		//保护计时器的锁

	// Actions to take at some time. This is used to implement the
	// standard library's time package.
	// Must hold timersLock to access.
	timers []*timer			//每个p， 一个timer数组，实现为4叉堆 储存计时器的最小4叉堆

	// Number of timers in P's heap.
	// Modified using atomic instructions.
	numTimers uint32		//定时器 数量

	// Number of timerModifiedEarlier timers on P's heap.
	// This should only be modified while holding timersLock,
	// or while the timer status is in a transient state
	// such as timerModifying.
	adjustTimers uint32		// 要调整的定时器的数量

	// Number of timerDeleted timers in P's heap.
	// Modified using atomic instructions.
	deletedTimers uint32	//timerDeleted

	// Race context used while executing timer functions.
	timerRaceCtx uintptr

	// preempt is set to indicate that this P should be enter the
	// scheduler ASAP (regardless of what G is running on it).
	preempt bool	//是否运行抢占

	pad cpu.CacheLinePad	//填满cacheline
}

type schedt struct {
	// accessed atomically. keep at top to ensure alignment on 32-bit systems.
	goidgen   uint64	//唯一gid生成, 也代表生成过的g的数量
	lastpoll  uint64 // 上次网络轮询的nanotime() time of last network poll, 0 if currently polling
	pollUntil uint64 // time to which current poll is sleeping

	lock mutex

	// When increasing nmidle, nmidlelocked, nmsys, or nmfreed, be
	// sure to call checkdead().

	midle        muintptr // 空闲m列表 idle m's waiting for work
	nmidle       int32    // number of idle m's waiting for work
	nmidlelocked int32    // 锁住的因此出于idle的m的数量 number of locked m's waiting for work
	mnext        int64    // 最大mId生成 number of m's that have been created and next M ID
	maxmcount    int32    // maximum number of m's allowed (or die)
	nmsys        int32    // 系统m数量  number of system m's not counted for deadlock
	nmfreed      int64    // mexit函数里+1 释放过的m的数量 cumulative number of freed m's

	ngsys uint32 // 系统g数量 number of system goroutines; updated atomically

	pidle      puintptr // 空闲p列表 idle p's
	npidle     uint32	//空闲p的数量
	nmspinning uint32 // 记录自旋线程数量 See "Worker thread parking/unparking" comment in proc.go.

	// Global runnable queue.
	runq     gQueue	//全局运行g队列
	runqsize int32	//大小

	// disable controls selective disabling of the scheduler.
	//
	// Use schedEnableUser to control this.
	//
	// disable is protected by sched.lock.
	disable struct {
		// user disables scheduling of user goroutines.
		user     bool	// 用户禁止用户协程调用
		runnable gQueue // pending runnable Gs
		n        int32  // length of runnable
	}

	// Global cache of dead G's.
	// 全局缓存 dead-g 空闲列表
	gFree struct {
		lock    mutex
		stack   gList // Gs with stacks 持有栈
		noStack gList // Gs without stacks 未持有栈
		n       int32
	}

	// Central cache of sudog structs.
	sudoglock  mutex
	sudogcache *sudog

	// Central pool of available defer structs of different sizes.
	deferlock mutex
	deferpool [5]*_defer

	// freem is the list of m's waiting to be freed when their
	// m.exited is set. Linked through m.freelink.
	freem *m	//空闲的m的列表， 一个用于遍历，释放g0.stack


	gcwaiting  uint32 // 要执行一个gc gc is waiting to run

	stopwait   int32	//如果数量大于0就需要进行sleep
	stopnote   note		// 用于stw
	sysmonwait uint32	// 做已经sleep的标记，其他地方可以判断，然后唤醒
	sysmonnote note		// sysmon sleep在这上面， 等待其他地方恢复

	// safepointFn should be called on each P at the next GC
	// safepoint if p.runSafePointFn is set.
	//设置safepoint
	safePointFn   func(*p)
	safePointWait int32
	safePointNote note

	profilehz int32 // 采样频率 cpu profiling rate

	procresizetime int64 // 上次改p的数量的时间 nanotime() of last change to gomaxprocs
	totaltime      int64 //p数量 * 工作时间，p改变后会调整 积分 ∫gomaxprocs dt up to procresizetime
}

// Values for the flags field of a sigTabT.
const (
	_SigNotify   = 1 << iota // let signal.Notify have signal, even if from kernel
	_SigKill                 // if signal.Notify doesn't take it, exit quietly 安静 退出
	_SigThrow                // if signal.Notify doesn't take it, exit loudly 大声退出
	_SigPanic                // if the signal is from the kernel, panic
	_SigDefault              // if the signal isn't explicitly requested, don't monitor it
	_SigGoExit               // cause all runtime procs to exit (only used on Plan 9).
	_SigSetStack             // add SA_ONSTACK to libc handler
	_SigUnblock              // always unblock; see blockableSig
	_SigIgn                  // _SIG_DFL action is to ignore the signal
)

// Layout of in-memory per-function information prepared by linker 链接器 函数信息 字段布局
// See TODO https://golang.org/s/go12symtab.
// Keep in sync with linker (../cmd/link/internal/ld/pcln.go:/pclntab)
// and with package debug/gosym and with symtab.go in package runtime.
type _func struct {
	entry   uintptr // start pc
	nameoff int32   // function name

	args        int32  // in/out args size 输入参数和返回参数加起来
	deferreturn uint32 // offset of start of a deferreturn call instruction from entry, if any.  deferreturn pc - entry

	pcsp      int32		//栈指针
	pcfile    int32		//所在文件
	pcln      int32		//line
	npcdata   int32
	funcID    funcID  // uint8 set for certain special runtime functions
	_         [2]int8 // unused
	nfuncdata uint8   // must be last
}

// 伪Pseudo-Func that is returned for PCs that occur in inlined code. 内联代码返回地址
// A *Func can be either a *_func or a *funcinl, and they are distinguished
// by the first uintptr.
type funcinl struct {
	zero  uintptr // 0 set to 0 to distinguish from _func
	entry uintptr // 上一帧 entry of the real (the "outermost") frame.
	name  string  //name
	file  string
	line  int
}

// layout of Itab known to compilers
// allocated in non-garbage-collected memory 分配在非垃圾回收内存
// Needs to be in sync with
// ../cmd/compile/internal/gc/reflect.go:/^func.dumptabs.
type itab struct {
	inter *interfacetype
	_type *_type
	hash  uint32 // copy of _type.hash. Used for type switches.
	_     [4]byte
	fun   [1]uintptr // variable sized. fun[0]==0 means _type does not implement inter.
}

// Lock-free stack node.
// 无锁栈节点
// Also known to export_test.go.
type lfnode struct {
	next    uint64
	pushcnt uintptr
}

type forcegcstate struct {
	lock mutex
	g    *g
	idle uint32
}

// startup_random_data holds random bytes initialized at startup. These come from
// the ELF AT_RANDOM auxiliary vector (vdso_linux_amd64.go or os_linux_386.go).
var startupRandomData []byte

// extendRandom extends the random numbers in r[:n] to the whole slice r.
//n是起始位置，用随机数填满slice
// Treats n<0 as n==0.
func extendRandom(r []byte, n int) {
	if n < 0 {
		n = 0
	}
	for n < len(r) {
		// Extend random bits using hash function & time seed
		w := n
		if w > 16 {
			w = 16
		}
		h := memhash(unsafe.Pointer(&r[n-w]), uintptr(nanotime()), uintptr(w))
		for i := 0; i < sys.PtrSize && n < len(r); i++ {
			r[n] = byte(h)
			n++
			h >>= 8
		}
	}
}

// A _defer holds an entry on the list of deferred calls.
// If you add a field here, add code to clear it in freedefer and deferProcStack
// This struct must match the code in cmd/compile/internal/gc/reflect.go:deferstruct
// and cmd/compile/internal/gc/ssa.go:(*state).call.
// Some defers will be allocated on the stack and some on the heap.
// All defers are logically part of the stack, so write barriers to
// initialize them are not required. All defers must be manually scanned,
// and for heap defers, marked.
type _defer struct {
	siz     int32 // includes both arguments and results
	started bool
	heap    bool
	// openDefer indicates that this _defer is for a frame with open-coded
	// defers. We have only one defer record for the entire frame (which may
	// currently have 0, 1, or more defers active).
	openDefer bool
	sp        uintptr  // sp at time of defer
	pc        uintptr  // pc at time of defer
	fn        *funcval // can be nil for open-coded defers
	_panic    *_panic  // 导致defer执行的panic panic that is running defer
	link      *_defer	//链表

	// If openDefer is true, the fields below record values about the stack
	// frame and associated function that has the open-coded defer(s). sp
	// above will be the sp for the frame, and pc will be address of the
	// deferreturn call in the function.
	fd   unsafe.Pointer // 函数引用的变量funcdata for the function associated with the frame
	varp uintptr        // 栈帧参数 value of varp for the stack frame
	// framepc is the current pc associated with the stack frame. Together,
	// with sp above (which is the sp associated with the stack frame),
	// framepc/sp can be used as pc/sp pair to continue a stack trace via
	// gentraceback().
	framepc uintptr		//关联的栈帧
}

// A _panic holds information about an active panic.
//
// This is marked go:notinheap because _panic values must only ever
// live on the stack.
//
// The argp and link fields are stack pointers, but don't need special
// handling during stack growth: because they are pointer-typed and
// _panic values only live on the stack, regular stack pointer
// adjustment takes care of them.
//
//go:notinheap
type _panic struct {
	argp      unsafe.Pointer // 执行参数的指针 pointer to arguments of deferred call run during panic; cannot move - known to liblink
	arg       interface{}    // argument to panic
	link      *_panic        // 链表 link to earlier panic
	pc        uintptr        // where to return to in runtime if this panic is bypassed
	sp        unsafe.Pointer // where to return to in runtime if this panic is bypassed
	recovered bool           // whether this panic is over
	aborted   bool           // the panic was aborted
	goexit    bool
}

// stack traces
type stkframe struct {
	fn       funcInfo   // symtab.go function being run
	pc       uintptr    // program counter within fn
	continpc uintptr    // continue pc. program counter where execution can continue, or 0 if not
	lr       uintptr    // program counter at caller aka又称 link register
	sp       uintptr    // stack pointer at pc
	fp       uintptr    // stack pointer at caller aka又称 frame pointer
	varp     uintptr    // top of local variables 本地变量顶部
	argp     uintptr    // pointer to function arguments 函数参数底部
	arglen   uintptr    // number of bytes at argp   参数大小B
	argmap   *bitvector // force use of this argmap
}

// ancestorInfo records details of where a goroutine was started.
type ancestorInfo struct {
	pcs  []uintptr // pcs from the stack of this goroutine
	goid int64     // goroutine id of this goroutine; original goroutine possibly dead
	gopc uintptr   // pc of go statement that created this goroutine
}

const (
	_TraceRuntimeFrames = 1 << iota // include frames for internal runtime functions.
	_TraceTrap                      // the initial PC, SP are from a trap, not a return PC from a call
	_TraceJumpStack                 // if traceback is on a systemstack, resume trace at g that called into it
)

// The maximum number of frames we print for a traceback
const _TracebackMaxFrames = 100

// A waitReason explains why a goroutine has been stopped.
// See gopark. Do not re-use waitReasons, add new ones.
type waitReason uint8

const (
	waitReasonZero                  waitReason = iota // ""
	waitReasonGCAssistMarking                         // "GC assist marking"
	waitReasonIOWait                                  // "IO wait"
	waitReasonChanReceiveNilChan                      // "chan receive (nil chan)"
	waitReasonChanSendNilChan                         // "chan send (nil chan)"
	waitReasonDumpingHeap                             // "dumping heap"
	waitReasonGarbageCollection                       // "garbage collection"
	waitReasonGarbageCollectionScan                   // "garbage collection scan"
	waitReasonPanicWait                               // "panicwait"
	waitReasonSelect                                  // "select"
	waitReasonSelectNoCases                           // "select (no cases)"
	waitReasonGCAssistWait                            // "GC assist wait"
	waitReasonGCSweepWait                             // "GC sweep wait"
	waitReasonGCScavengeWait                          // "GC scavenge wait"
	waitReasonChanReceive                             // "chan receive"
	waitReasonChanSend                                // "chan send"
	waitReasonFinalizerWait                           // "finalizer wait"
	waitReasonForceGCIdle                             // "force gc (idle)"
	waitReasonSemacquire                              // "semacquire"
	waitReasonSleep                                   // "sleep"
	waitReasonSyncCondWait                            // "sync.Cond.Wait"
	waitReasonTimerGoroutineIdle                      // "timer goroutine (idle)"
	waitReasonTraceReaderBlocked                      // "trace reader (blocked)"
	waitReasonWaitForGCCycle                          // "wait for GC cycle"
	waitReasonGCWorkerIdle                            // "GC worker (idle)"
	waitReasonPreempted                               // "preempted"
	waitReasonDebugCall                               // "debug call"
)

var waitReasonStrings = [...]string{
	waitReasonZero:                  "",
	waitReasonGCAssistMarking:       "GC assist marking",
	waitReasonIOWait:                "IO wait",
	waitReasonChanReceiveNilChan:    "chan receive (nil chan)",
	waitReasonChanSendNilChan:       "chan send (nil chan)",
	waitReasonDumpingHeap:           "dumping heap",
	waitReasonGarbageCollection:     "garbage collection",
	waitReasonGarbageCollectionScan: "garbage collection scan",
	waitReasonPanicWait:             "panicwait",
	waitReasonSelect:                "select",
	waitReasonSelectNoCases:         "select (no cases)",
	waitReasonGCAssistWait:          "GC assist wait",
	waitReasonGCSweepWait:           "GC sweep wait",
	waitReasonGCScavengeWait:        "GC scavenge wait",
	waitReasonChanReceive:           "chan receive",
	waitReasonChanSend:              "chan send",
	waitReasonFinalizerWait:         "finalizer wait",
	waitReasonForceGCIdle:           "force gc (idle)",
	waitReasonSemacquire:            "semacquire",
	waitReasonSleep:                 "sleep",
	waitReasonSyncCondWait:          "sync.Cond.Wait",
	waitReasonTimerGoroutineIdle:    "timer goroutine (idle)",
	waitReasonTraceReaderBlocked:    "trace reader (blocked)",
	waitReasonWaitForGCCycle:        "wait for GC cycle",
	waitReasonGCWorkerIdle:          "GC worker (idle)",
	waitReasonPreempted:             "preempted",
	waitReasonDebugCall:             "debug call",
}

func (w waitReason) String() string {
	if w < 0 || w >= waitReason(len(waitReasonStrings)) {
		return "unknown wait reason"
	}
	return waitReasonStrings[w]
}

var (
	//allgs []*g proc.go
	allglen    uintptr	//len(allgs)
	allm       *m	//链表
	allp       []*p  // len(allp) == gomaxprocs; may change at safe points, otherwise immutable
	allpLock   mutex // Protects P-less reads of allp and all writes
	gomaxprocs int32	//保存 p的数量
	ncpu       int32	// 保存 核数
	forcegc    forcegcstate
	sched      schedt	//调度体
	newprocs   int32 	//新p的数量

	// Information about what cpu features are available.
	// Packages outside the runtime should not use these
	// as they are not an external api. 只限runtime包内部使用
	// Set on startup in asm_{386,amd64}.s _rt0_go 哈数
	processorVersionInfo uint32	// cpu功能特性
	isIntel              bool
	lfenceBeforeRdtsc    bool	//汇编里cputicks使用

	goarm                uint8 // arm特定 set by cmd/link on arm systems
	framepointer_enabled bool  // 由链接器设置 set by cmd/link
)

// Set by the linker so the runtime can determine the buildmode.
var (
	islibrary bool // -buildmode=c-shared
	isarchive bool // -buildmode=c-archive
)
