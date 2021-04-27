// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector (GC). 垃圾收集器
//
// The GC runs concurrently with mutator threads 与用户线程并发, is type accurate 类型正确(aka precise), allows multiple
// GC thread to run in parallel 并行.
// It is a concurrent mark and sweep that uses a write barrier. 并发标记和清除，在使用写屏障的前提下
// It is non-generational非分代 and non-compacting 无压缩.
// Allocation is done using size segregated隔离的 per P allocation areas to minimize fragmentation碎片 while eliminating locks in the common case.
//
// The algorithm decomposes into 分解为 several steps.
// This is a high level description of the algorithm being used. For an overview of GC a good
// place to start is Richard Jones' gchandbook.org.
//
// The algorithm's intellectual智力 heritage遗产 includes Dijkstra's on-the-fly  algorithm, see
// Edsger W. Dijkstra, Leslie Lamport, A. J. Martin, C. S. Scholten, and E. F. M. Steffens. 1978.
// On-the-fly garbage collection: an exercise in cooperation.论文 Commun. ACM 21, 11 (November 1978),
// 966-975.
// For journal quality proofs证明 that these steps are complete, correct, and terminate see
// Hudson, R., and Moss, J.E.B. Copying Garbage Collection without stopping the world.
// Concurrency and Computation: Practice and Experience 15(3-5), 2003.
//
// 1. GC performs sweep termination. 第一阶段 清扫终止
//
//    a. Stop the world. This causes all Ps to reach a GC safe-point. 让所有p 到达一个 垃圾回收 安全点
//
//    b. Sweep any unswept spans. There will only be unswept spans if
//    this GC cycle was forced before the expected time.  清理，
//
// 2. GC performs the mark phase. 第二阶段 标记阶段
//
//    a. Prepare for the mark phase by setting gcphase to _GCmark
//    (from _GCoff), enabling the write barrier, enabling mutator
//    assists, and enqueueing root mark jobs. No objects may be
//    scanned until all Ps have enabled the write barrier, which is
//    accomplished using STW. 改阶段，开写屏障 开用户辅助，入队根标记工作。 stw后，allp开写屏障才能扫描对象
//
//    b. Start the world. From this point, GC work is done by mark 恢复，
//    workers started by the scheduler and by assists performed as
//    part of allocation.
//    The write barrier shades染色 both the overwritten pointer and the new pointer value for any pointer
//    writes (see mbarrier.go for details). Newly allocated objects
//    are immediately marked black. 新分配的对象立即标记为黑色
//
//    c. GC performs root marking jobs. This includes scanning all 执行根标记工作， 扫描栈，全局变量，堆外运行时数据结构里指向堆的指针
//    stacks, shading all globals, and shading any heap pointers in
//    off-heap runtime data structures. Scanning a stack stops a 扫描栈停止g，染色栈上所有指针，然后恢复g的执行
//    goroutine, shades any pointers found on its stack, and then
//    resumes the goroutine.
//
//    d. GC drains the work queue of grey objects, scanning each grey gcDrain() 耗空灰色队列，递归式
//    object to black and shading all pointers found in the object
//    (which in turn may add those pointers to the work queue).
//
//    e. Because GC work is spread across 遍布 local caches, GC uses a gc使用分布式终止算法检测何时没有根标记任务or黑色对象(参考gcMarkDone()函数)
//    distributed termination algorithm to detect when there are no
//    more root marking jobs or grey objects (see gcMarkDone). At this 此时，GC 切换到标记终止阶段
//    point, GC transitions to mark termination.
//
// 3. GC performs mark termination. 第3阶段 gc执行标记终止
//
//    a. Stop the world. stw
//
//    b. Set gcphase to _GCmarktermination, and disable workers and 更新阶段，禁用标记工作者和用户程序辅助
//    assists.
//
//    c. Perform housekeeping like flushing mcaches. 执行家政服务像冲洗mcache
//
// 4. GC performs the sweep phase. 第4阶段 执行清除阶段
//
//    a. Prepare for the sweep phase by setting gcphase to _GCoff, 修改阶段，建立sweep状态，禁用写屏障
//    setting up sweep state and disabling the write barrier.
//
//    b. Start the world. From this point on, newly allocated objects 恢复执行，此时新分配的对象式白色，分配sweep内存块
//    are white, and allocating sweeps spans before use if necessary.
//
//    c. GC does concurrent sweeping in the background and in response 并发sweep 后台，并且响应分配请求
//    to allocation. See description below.
//
// 5. When sufficient大量 allocation has taken place, replay重复1步骤 the sequence
// starting with 1 above. See discussion of GC rate below.

// Concurrent sweep. 并发sweep
//
// The sweep phase proceeds开始 concurrently并发 with normal program execution执行.
// The heap is swept span-by-span一个接一个 both lazily (when a goroutine needs another span 需要时sweep) 懒惰和并发两种方法
// and concurrently in a background goroutine (this helps programs that are not CPU bound 处理器受限).
// At the end of STW mark termination all spans are marked as "needs sweeping". 标记终止阶段stw结尾，所有span标记为 需要扫描
//
// The background sweeper goroutine simply sweeps spans one-by-one. 后台sweeper g只是一个接一个地扫描
//
// To avoid requesting more OS memory while there are unswept spans, when a
// goroutine needs another span, it first attempts to reclaim先回收 that much memory
// by sweeping. When a goroutine needs to allocate a new small-object span, it
// sweeps small-object spans for the same object size until it frees at least
// one object. 释放至少一个对象的mspan 都能用
// When a goroutine needs to allocate large-object span from heap, it sweeps spans until it frees at least that many pages into heap.
// There is one case where this may not suffice: if a goroutine sweeps and frees two
// nonadjacent one-page spans to the heap, it will allocate a new two-page
// span, but there can still be other one-page unswept spans which could be
// combined into a two-page span.
//
// It's critical to ensure that no operations proceed开始 on unswept spans (that would corrupt破坏
// mark bits in GC bitmap).
// During GC all mcaches are flushed into the central cache, so they are empty.
// When a goroutine grabs a new span into mcache, it sweeps it.

// When a goroutine explicitly显式 frees an object or sets a finalizer, it ensures that
// the span is swept (either by sweeping it, or by waiting for the concurrent sweep to finish).

// The finalizer goroutine is kicked off开除 only when all spans are swept.
// When the next GC starts, it sweeps all not-yet-swept spans (if any). 当下一次gc开始，sweep 所有还未扫描的mspan

// GC rate.
// Next GC is after we've allocated an extra amount of memory proportional to
// the amount already in use. The proportion is controlled by GOGC environment variable
// (100 by default). If GOGC=100 and we're using 4M, we'll GC again when we get to 8M
// (this mark is tracked in next_gc variable). This keeps the GC cost in linear
// proportion to the allocation cost. Adjusting GOGC just changes the linear constant
// (and also the amount of extra memory used).

// Oblets
//
// In order to prevent long pauses while scanning large objects and to
// improve parallelism, the garbage collector breaks up scan jobs for
// objects larger than maxObletBytes into "oblets" of at most
// maxObletBytes.
// When scanning encounters the beginning of a large
// object, it scans only the first oblet and enqueues the remaining
// oblets as new scan jobs.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"unsafe"
)

const (
	_DebugGC         = 0
	_ConcurrentSweep = true
	// 4k
	_FinBlockSize = 4 * 1024

	// debugScanConservative enables debug logging for stack
	// frames that are scanned conservatively.
	debugScanConservative = false

	// sweepMinHeapDistance is a lower bound on the heap distance
	// (in bytes) reserved for concurrent sweeping between GC
	// cycles.
	// 1m
	sweepMinHeapDistance = 1024 * 1024
)

// heapminimum is the minimum heap size at which to trigger GC.
// For small heaps, this overrides the usual GOGC*live set rule.
//
// When there is a very small live set but a lot of allocation, simply
// collecting when the heap reaches GOGC*live results in many GC
// cycles and high total per-GC overhead. This minimum amortizes this
// per-GC overhead while keeping the heap reasonably small.
//
// During initialization this is set to 4MB*GOGC/100. In the case of
// GOGC==0, this will set heapminimum to 0, resulting in constant
// collection even when the heap size is small, which is useful for
// debugging.
// 2^22次方
var heapminimum uint64 = defaultHeapMinimum

// defaultHeapMinimum is the value of heapminimum for GOGC==100.
const defaultHeapMinimum = 4 << 20

// Initialized from $GOGC.  GOGC=off means no GC.
var gcpercent int32

// gc组件的初始化
// 初始化变量
func gcinit() {
	if unsafe.Sizeof(workbuf{}) != _WorkbufSize {
		throw("size of Workbuf is suboptimal")
	}

	// No sweep on the first cycle.
	mheap_.sweepdone = 1

	// Set a reasonable initial GC trigger.
	memstats.triggerRatio = 7 / 8.0

	// Fake a heap_marked value so it looks like a trigger at
	// heapminimum is the appropriate growth from heap_marked.
	// This will go into computing the initial GC goal.
	memstats.heap_marked = uint64(float64(heapminimum) / (1 + memstats.triggerRatio))

	// Set gcpercent from the environment. This will also compute
	// and set the GC trigger and goal.
	// 环境变量 GOGC 设置gc比例
	_ = setGCPercent(readgogc())

	work.startSema = 1
	work.markDoneSema = 1
	// 初始化锁
	lockInit(&work.sweepWaiters.lock, lockRankSweepWaiters)
	lockInit(&work.assistQueue.lock, lockRankAssistQueue)
	lockInit(&work.wbufSpans.lock, lockRankWbufSpans)
}

func readgogc() int32 {
	p := gogetenv("GOGC")
	if p == "off" {
		return -1
	}
	if n, ok := atoi32(p); ok {
		return n
	}
	return 100
}

// gcenable is called after the bulk of the runtime initialization,
// just before we're about to start letting user code run.
// It kicks off独立执行 the background sweeper goroutine, the background
// scavenger清道夫 goroutine, and enables GC.
func gcenable() {
	// Kick off sweeping and scavenging.
	c := make(chan int, 2)
	go bgsweep(c)
	go bgscavenge(c)
	<-c
	<-c
	memstats.enablegc = true // now that runtime is initialized, GC is okay
}

//go:linkname setGCPercent runtime/debug.setGCPercent
func setGCPercent(in int32) (out int32) {
	// Run on the system stack since we grab the heap lock.
	systemstack(func() {
		lock(&mheap_.lock)
		out = gcpercent
		if in < 0 {
			in = -1
		}
		gcpercent = in
		heapminimum = defaultHeapMinimum * uint64(gcpercent) / 100
		// Update pacing in response to gcpercent change.
		gcSetTriggerRatio(memstats.triggerRatio)
		unlock(&mheap_.lock)
	})

	// If we just disabled GC, wait for any concurrent GC mark to
	// finish so we always return with no GC running.
	if in < 0 {
		// 阻塞直到第n论标记完成
		gcWaitOnMark(atomic.Load(&work.cycles))
	}

	return out
}

// Garbage collector phase 阶段. 只是区分了标记相关阶段
// Indicates to write barrier and synchronization task to perform.
var gcphase uint32

// The compiler knows about this variable.
// If you change it, you must change builtin/runtime.go, too.
// If you change the first four bytes, you must also change the write
// barrier insertion code.
// 全局变量 垃圾回收写屏障 相关的信息
var writeBarrier struct {
	enabled bool    // 开关 compiler emits a check of this before calling write barrier
	pad     [3]byte // compiler uses 32-bit load for "enabled" field
	needed  bool    // whether we need a write barrier for current GC phase
	cgo     bool    // whether we need a write barrier for a cgo check
	alignme uint64  // guarantee alignment so that compiler can use a 32 or 64-bit load
}

// gcBlackenEnabled is 1 if mutator assists and background mark
// workers are allowed to blacken objects. This must only be set when
// gcphase == _GCmark. 用户辅助程序和后台标记工作者可以标记黑色
var gcBlackenEnabled uint32

const (
	_GCoff             = iota // GC not running; sweeping in background, write barrier disabled
	_GCmark                   // GC marking roots and workbufs: allocate black, write barrier ENABLED
	_GCmarktermination        // GC mark termination: allocate black, P's help GC, write barrier ENABLED
)

//go:nosplit
func setGCPhase(x uint32) {
	// 直接原子赋值
	atomic.Store(&gcphase, x)
	// 标记和标记终止阶段需要写屏障，off就是关闭写屏障
	writeBarrier.needed = gcphase == _GCmark || gcphase == _GCmarktermination

	writeBarrier.enabled = writeBarrier.needed || writeBarrier.cgo
}

// gcMarkWorkerMode represents the mode that a concurrent mark worker
// should operate in.
//
// Concurrent marking happens through four different mechanisms. One
// is mutator assists, which happen in response to allocations and are
// not scheduled. The other three are variations in the per-P mark
// workers and are distinguished by gcMarkWorkerMode.
// 类型
type gcMarkWorkerMode int

const (
	// gcMarkWorkerDedicatedMode indicates that the P of a mark
	// worker is dedicated to running that mark worker. The mark
	// worker should run without preemption.
	gcMarkWorkerDedicatedMode gcMarkWorkerMode = iota

	// gcMarkWorkerFractionalMode indicates that a P is currently
	// running the "fractional" mark worker. The fractional worker
	// is necessary when GOMAXPROCS*gcBackgroundUtilization is not
	// an integer. The fractional worker should run until it is
	// preempted and will be scheduled to pick up the fractional
	// part of GOMAXPROCS*gcBackgroundUtilization.
	gcMarkWorkerFractionalMode

	// gcMarkWorkerIdleMode indicates that a P is running the mark
	// worker because it has nothing else to do. The idle worker
	// should run until it is preempted and account its time
	// against gcController.idleMarkTime.
	gcMarkWorkerIdleMode
)

// gcMarkWorkerModeStrings are the strings labels of gcMarkWorkerModes
// to use in execution traces.
var gcMarkWorkerModeStrings = [...]string{
	"GC (dedicated)",
	"GC (fractional)",
	"GC (idle)",
}

// gcController implements the GC pacing步调 controller that
// determines when to trigger concurrent garbage collection and how much marking
// work to do in mutator assists and background marking. 决定when和工作量
//
// It uses a feedback control algorithm to adjust the memstats.gc_trigger
// trigger based on the heap growth and GC CPU utilization each cycle每轮.

// This algorithm optimizes for heap growth to match GOGC and for CPU
// utilization利用率 between assist and background marking to be 25% of 负载达到25%
// GOMAXPROCS. The high-level design of this algorithm is documented
// at https://golang.org/s/go15gcpacing. 小论文
//
// All fields of gcController are used only during a single mark
// cycle.
var gcController gcControllerState

type gcControllerState struct {
	// scanWork is the total scan work performed this cycle. This
	// is updated atomically during the cycle. Updates occur in
	// bounded batches, since it is both written and read
	// throughout the cycle. At the end of the cycle, this is how
	// much of the retained heap is scannable.
	//
	// Currently this is the bytes of heap scanned. For most uses,
	// this is an opaque unit of work, but for estimation the
	// definition is important.
	scanWork int64

	// bgScanCredit is the scan work credit accumulated by the
	// concurrent background scan. This credit is accumulated by
	// the background scan and stolen by mutator assists. This is
	// updated atomically. Updates occur in bounded batches, since
	// it is both written and read throughout the cycle.
	bgScanCredit int64

	// assistTime is the nanoseconds spent in mutator assists
	// during this cycle. This is updated atomically. Updates
	// occur in bounded batches, since it is both written and read
	// throughout the cycle.
	assistTime int64

	// dedicatedMarkTime is the nanoseconds spent in dedicated
	// mark workers during this cycle. This is updated atomically
	// at the end of the concurrent mark phase.
	dedicatedMarkTime int64

	// fractionalMarkTime is the nanoseconds spent in the
	// fractional mark worker during this cycle. This is updated
	// atomically throughout the cycle and will be up-to-date if
	// the fractional mark worker is not currently running.
	fractionalMarkTime int64

	// idleMarkTime is the nanoseconds spent in idle marking
	// during this cycle. This is updated atomically throughout
	// the cycle.
	idleMarkTime int64

	// markStartTime is the absolute start time in nanoseconds
	// that assists and background mark workers started.
	markStartTime int64

	// dedicatedMarkWorkersNeeded is the number of dedicated mark
	// workers that need to be started. This is computed at the
	// beginning of each cycle and decremented atomically as
	// dedicated mark workers get started.
	dedicatedMarkWorkersNeeded int64

	// assistWorkPerByte is the ratio of scan work to allocated
	// bytes that should be performed by mutator assists. This is
	// computed at the beginning of each cycle and updated every
	// time heap_scan is updated.
	assistWorkPerByte float64

	// assistBytesPerWork is 1/assistWorkPerByte.
	assistBytesPerWork float64

	// fractionalUtilizationGoal is the fraction of wall clock
	// time that should be spent in the fractional mark worker on
	// each P that isn't running a dedicated worker.
	//
	// For example, if the utilization goal is 25% and there are
	// no dedicated workers, this will be 0.25. If the goal is
	// 25%, there is one dedicated worker, and GOMAXPROCS is 5,
	// this will be 0.05 to make up the missing 5%.
	//
	// If this is zero, no fractional workers are needed.
	fractionalUtilizationGoal float64

	_ cpu.CacheLinePad
}

// startCycle resets the GC controller's state and computes estimates
// for a new GC cycle. The caller must hold worldsema.
func (c *gcControllerState) startCycle() {
	c.scanWork = 0
	c.bgScanCredit = 0
	c.assistTime = 0
	c.dedicatedMarkTime = 0
	c.fractionalMarkTime = 0
	c.idleMarkTime = 0

	// Ensure that the heap goal is at least a little larger than
	// the current live heap size. This may not be the case if GC
	// start is delayed or if the allocation that pushed heap_live
	// over gc_trigger is large or if the trigger is really close to
	// GOGC. Assist is proportional to this distance, so enforce a
	// minimum distance, even if it means going over the GOGC goal
	// by a tiny bit.
	if memstats.next_gc < memstats.heap_live+1024*1024 {
		memstats.next_gc = memstats.heap_live + 1024*1024
	}

	// Compute the background mark utilization goal. In general,
	// this may not come out exactly. We round the number of
	// dedicated workers so that the utilization is closest to
	// 25%. For small GOMAXPROCS, this would introduce too much
	// error, so we add fractional workers in that case.
	totalUtilizationGoal := float64(gomaxprocs) * gcBackgroundUtilization
	c.dedicatedMarkWorkersNeeded = int64(totalUtilizationGoal + 0.5)
	utilError := float64(c.dedicatedMarkWorkersNeeded)/totalUtilizationGoal - 1
	const maxUtilError = 0.3
	if utilError < -maxUtilError || utilError > maxUtilError {
		// Rounding put us more than 30% off our goal. With
		// gcBackgroundUtilization of 25%, this happens for
		// GOMAXPROCS<=3 or GOMAXPROCS=6. Enable fractional
		// workers to compensate.
		if float64(c.dedicatedMarkWorkersNeeded) > totalUtilizationGoal {
			// Too many dedicated workers.
			c.dedicatedMarkWorkersNeeded--
		}
		c.fractionalUtilizationGoal = (totalUtilizationGoal - float64(c.dedicatedMarkWorkersNeeded)) / float64(gomaxprocs)
	} else {
		c.fractionalUtilizationGoal = 0
	}

	// In STW mode, we just want dedicated workers.
	if debug.gcstoptheworld > 0 {
		c.dedicatedMarkWorkersNeeded = int64(gomaxprocs)
		c.fractionalUtilizationGoal = 0
	}

	// Clear per-P state
	for _, p := range allp {
		p.gcAssistTime = 0
		p.gcFractionalMarkTime = 0
	}

	// Compute initial values for controls that are updated
	// throughout the cycle.
	c.revise()

	if debug.gcpacertrace > 0 {
		print("pacer: assist ratio=", c.assistWorkPerByte,
			" (scan ", memstats.heap_scan>>20, " MB in ",
			work.initialHeapLive>>20, "->",
			memstats.next_gc>>20, " MB)",
			" workers=", c.dedicatedMarkWorkersNeeded,
			"+", c.fractionalUtilizationGoal, "\n")
	}
}

// revise updates the assist ratio during the GC cycle to account for
// improved estimates. This should be called either under STW or
// whenever memstats.heap_scan, memstats.heap_live, or
// memstats.next_gc is updated (with mheap_.lock held).
//
// It should only be called when gcBlackenEnabled != 0 (because this
// is when assists are enabled and the necessary statistics are
// available).
func (c *gcControllerState) revise() {
	gcpercent := gcpercent
	if gcpercent < 0 {
		// If GC is disabled but we're running a forced GC,
		// act like GOGC is huge for the below calculations.
		gcpercent = 100000
	}
	live := atomic.Load64(&memstats.heap_live)

	// Assume we're under the soft goal. Pace GC to complete at
	// next_gc assuming the heap is in steady-state.
	heapGoal := int64(memstats.next_gc)

	// Compute the expected scan work remaining.
	//
	// This is estimated based on the expected
	// steady-state scannable heap. For example, with
	// GOGC=100, only half of the scannable heap is
	// expected to be live, so that's what we target.
	//
	// (This is a float calculation to avoid overflowing on
	// 100*heap_scan.)
	scanWorkExpected := int64(float64(memstats.heap_scan) * 100 / float64(100+gcpercent))

	if live > memstats.next_gc || c.scanWork > scanWorkExpected {
		// We're past the soft goal, or we've already done more scan
		// work than we expected. Pace GC so that in the worst case it
		// will complete by the hard goal.
		const maxOvershoot = 1.1
		heapGoal = int64(float64(memstats.next_gc) * maxOvershoot)

		// Compute the upper bound on the scan work remaining.
		scanWorkExpected = int64(memstats.heap_scan)
	}

	// Compute the remaining scan work estimate.
	//
	// Note that we currently count allocations during GC as both
	// scannable heap (heap_scan) and scan work completed
	// (scanWork), so allocation will change this difference
	// slowly in the soft regime and not at all in the hard
	// regime.
	scanWorkRemaining := scanWorkExpected - c.scanWork
	if scanWorkRemaining < 1000 {
		// We set a somewhat arbitrary lower bound on
		// remaining scan work since if we aim a little high,
		// we can miss by a little.
		//
		// We *do* need to enforce that this is at least 1,
		// since marking is racy and double-scanning objects
		// may legitimately make the remaining scan work
		// negative, even in the hard goal regime.
		scanWorkRemaining = 1000
	}

	// Compute the heap distance remaining.
	heapRemaining := heapGoal - int64(live)
	if heapRemaining <= 0 {
		// This shouldn't happen, but if it does, avoid
		// dividing by zero or setting the assist negative.
		heapRemaining = 1
	}

	// Compute the mutator assist ratio so by the time the mutator
	// allocates the remaining heap bytes up to next_gc, it will
	// have done (or stolen) the remaining amount of scan work.
	c.assistWorkPerByte = float64(scanWorkRemaining) / float64(heapRemaining)
	c.assistBytesPerWork = float64(heapRemaining) / float64(scanWorkRemaining)
}

// endCycle computes the trigger ratio for the next cycle.
func (c *gcControllerState) endCycle() float64 {
	if work.userForced {
		// Forced GC means this cycle didn't start at the
		// trigger, so where it finished isn't good
		// information about how to adjust the trigger.
		// Just leave it where it is.
		return memstats.triggerRatio
	}

	// Proportional response gain for the trigger controller. Must
	// be in [0, 1]. Lower values smooth out transient effects but
	// take longer to respond to phase changes. Higher values
	// react to phase changes quickly, but are more affected by
	// transient changes. Values near 1 may be unstable.
	const triggerGain = 0.5

	// Compute next cycle trigger ratio. First, this computes the
	// "error" for this cycle; that is, how far off the trigger
	// was from what it should have been, accounting for both heap
	// growth and GC CPU utilization. We compute the actual heap
	// growth during this cycle and scale that by how far off from
	// the goal CPU utilization we were (to estimate the heap
	// growth if we had the desired CPU utilization). The
	// difference between this estimate and the GOGC-based goal
	// heap growth is the error.
	goalGrowthRatio := gcEffectiveGrowthRatio()
	actualGrowthRatio := float64(memstats.heap_live)/float64(memstats.heap_marked) - 1
	assistDuration := nanotime() - c.markStartTime

	// Assume background mark hit its utilization goal.
	utilization := gcBackgroundUtilization
	// Add assist utilization; avoid divide by zero.
	if assistDuration > 0 {
		utilization += float64(c.assistTime) / float64(assistDuration*int64(gomaxprocs))
	}

	triggerError := goalGrowthRatio - memstats.triggerRatio - utilization/gcGoalUtilization*(actualGrowthRatio-memstats.triggerRatio)

	// Finally, we adjust the trigger for next time by this error,
	// damped by the proportional gain.
	triggerRatio := memstats.triggerRatio + triggerGain*triggerError

	if debug.gcpacertrace > 0 {
		// Print controller state in terms of the design
		// document.
		H_m_prev := memstats.heap_marked
		h_t := memstats.triggerRatio
		H_T := memstats.gc_trigger
		h_a := actualGrowthRatio
		H_a := memstats.heap_live
		h_g := goalGrowthRatio
		H_g := int64(float64(H_m_prev) * (1 + h_g))
		u_a := utilization
		u_g := gcGoalUtilization
		W_a := c.scanWork
		print("pacer: H_m_prev=", H_m_prev,
			" h_t=", h_t, " H_T=", H_T,
			" h_a=", h_a, " H_a=", H_a,
			" h_g=", h_g, " H_g=", H_g,
			" u_a=", u_a, " u_g=", u_g,
			" W_a=", W_a,
			" goalΔ=", goalGrowthRatio-h_t,
			" actualΔ=", h_a-h_t,
			" u_a/u_g=", u_a/u_g,
			"\n")
	}

	return triggerRatio
}

// enlistWorker encourages another dedicated mark worker to start on
// another P if there are spare worker slots. It is used by putfull
// when more work is made available.
//
//go:nowritebarrier
func (c *gcControllerState) enlistWorker() {
	// If there are idle Ps, wake one so it will run an idle worker.
	// NOTE: This is suspected of causing deadlocks. See golang.org/issue/19112.
	//
	//	if atomic.Load(&sched.npidle) != 0 && atomic.Load(&sched.nmspinning) == 0 {
	//		wakep()
	//		return
	//	}

	// There are no idle Ps. If we need more dedicated workers,
	// try to preempt a running P so it will switch to a worker.
	if c.dedicatedMarkWorkersNeeded <= 0 {
		return
	}
	// Pick a random other P to preempt.
	if gomaxprocs <= 1 {
		return
	}
	gp := getg()
	if gp == nil || gp.m == nil || gp.m.p == 0 {
		return
	}
	myID := gp.m.p.ptr().id
	for tries := 0; tries < 5; tries++ {
		id := int32(fastrandn(uint32(gomaxprocs - 1)))
		if id >= myID {
			id++
		}
		p := allp[id]
		if p.status != _Prunning {
			continue
		}
		if preemptone(p) {
			return
		}
	}
}

// findRunnableGCWorker returns the background mark worker for _p_ if it
// should be run. This must only be called when gcBlackenEnabled != 0.
func (c *gcControllerState) findRunnableGCWorker(_p_ *p) *g {
	if gcBlackenEnabled == 0 {
		throw("gcControllerState.findRunnable: blackening not enabled")
	}
	if _p_.gcBgMarkWorker == 0 {
		// The mark worker associated with this P is blocked
		// performing a mark transition. We can't run it
		// because it may be on some other run or wait queue.
		return nil
	}

	if !gcMarkWorkAvailable(_p_) {
		// No work to be done right now. This can happen at
		// the end of the mark phase when there are still
		// assists tapering off. Don't bother running a worker
		// now because it'll just return immediately.
		return nil
	}

	decIfPositive := func(ptr *int64) bool {
		if *ptr > 0 {
			if atomic.Xaddint64(ptr, -1) >= 0 {
				return true
			}
			// We lost a race
			atomic.Xaddint64(ptr, +1)
		}
		return false
	}

	if decIfPositive(&c.dedicatedMarkWorkersNeeded) {
		// This P is now dedicated to marking until the end of
		// the concurrent mark phase.
		_p_.gcMarkWorkerMode = gcMarkWorkerDedicatedMode
	} else if c.fractionalUtilizationGoal == 0 {
		// No need for fractional workers.
		return nil
	} else {
		// Is this P behind on the fractional utilization
		// goal?
		//
		// This should be kept in sync with pollFractionalWorkerExit.
		delta := nanotime() - gcController.markStartTime
		if delta > 0 && float64(_p_.gcFractionalMarkTime)/float64(delta) > c.fractionalUtilizationGoal {
			// Nope. No need to run a fractional worker.
			return nil
		}
		// Run a fractional worker.
		_p_.gcMarkWorkerMode = gcMarkWorkerFractionalMode
	}

	// Run the background mark worker
	gp := _p_.gcBgMarkWorker.ptr()
	// 允许执行 标记g
	casgstatus(gp, _Gwaiting, _Grunnable)
	if trace.enabled {
		traceGoUnpark(gp, 0)
	}
	return gp
}

// pollFractionalWorkerExit reports whether a fractional部分 mark worker
// should self-preempt. It assumes it is called from the fractional
// worker. 被一些标记工作g调用
func pollFractionalWorkerExit() bool {
	// This should be kept in sync with the fractional worker
	// scheduler logic in findRunnableGCWorker.
	now := nanotime()
	delta := now - gcController.markStartTime
	if delta <= 0 {
		return true
	}
	p := getg().m.p.ptr()
	selfTime := p.gcFractionalMarkTime + (now - p.gcMarkWorkerStartTime)
	// Add some slack to the utilization goal so that the
	// fractional worker isn't behind again the instant it exits.
	return float64(selfTime)/float64(delta) > 1.2*gcController.fractionalUtilizationGoal
}

// gcSetTriggerRatio sets the trigger ratio and updates everything
// derived from it: the absolute trigger, the heap goal, mark pacing,
// and sweep pacing.
//
// This can be called any time. If GC is the in the middle of a
// concurrent phase, it will adjust the pacing of that phase.
//
// This depends on gcpercent, memstats.heap_marked, and
// memstats.heap_live. These must be up to date.
//
// mheap_.lock must be held or the world must be stopped.
func gcSetTriggerRatio(triggerRatio float64) {
	// Compute the next GC goal, which is when the allocated heap
	// has grown by GOGC/100 over the heap marked by the last
	// cycle.
	goal := ^uint64(0)
	if gcpercent >= 0 {
		goal = memstats.heap_marked + memstats.heap_marked*uint64(gcpercent)/100
	}

	// Set the trigger ratio, capped to reasonable bounds.
	if gcpercent >= 0 {
		scalingFactor := float64(gcpercent) / 100
		// Ensure there's always a little margin so that the
		// mutator assist ratio isn't infinity.
		maxTriggerRatio := 0.95 * scalingFactor
		if triggerRatio > maxTriggerRatio {
			triggerRatio = maxTriggerRatio
		}

		// If we let triggerRatio go too low, then if the application
		// is allocating very rapidly we might end up in a situation
		// where we're allocating black during a nearly always-on GC.
		// The result of this is a growing heap and ultimately an
		// increase in RSS. By capping us at a point >0, we're essentially
		// saying that we're OK using more CPU during the GC to prevent
		// this growth in RSS.
		//
		// The current constant was chosen empirically: given a sufficiently
		// fast/scalable allocator with 48 Ps that could drive the trigger ratio
		// to <0.05, this constant causes applications to retain the same peak
		// RSS compared to not having this allocator.
		minTriggerRatio := 0.6 * scalingFactor
		if triggerRatio < minTriggerRatio {
			triggerRatio = minTriggerRatio
		}
	} else if triggerRatio < 0 {
		// gcpercent < 0, so just make sure we're not getting a negative
		// triggerRatio. This case isn't expected to happen in practice,
		// and doesn't really matter because if gcpercent < 0 then we won't
		// ever consume triggerRatio further on in this function, but let's
		// just be defensive here; the triggerRatio being negative is almost
		// certainly undesirable.
		triggerRatio = 0
	}
	memstats.triggerRatio = triggerRatio

	// Compute the absolute GC trigger from the trigger ratio.
	//
	// We trigger the next GC cycle when the allocated heap has
	// grown by the trigger ratio over the marked heap size.
	trigger := ^uint64(0)
	if gcpercent >= 0 {
		trigger = uint64(float64(memstats.heap_marked) * (1 + triggerRatio))
		// Don't trigger below the minimum heap size.
		minTrigger := heapminimum
		if !isSweepDone() {
			// Concurrent sweep happens in the heap growth
			// from heap_live to gc_trigger, so ensure
			// that concurrent sweep has some heap growth
			// in which to perform sweeping before we
			// start the next GC cycle.
			sweepMin := atomic.Load64(&memstats.heap_live) + sweepMinHeapDistance
			if sweepMin > minTrigger {
				minTrigger = sweepMin
			}
		}
		if trigger < minTrigger {
			trigger = minTrigger
		}
		if int64(trigger) < 0 {
			print("runtime: next_gc=", memstats.next_gc, " heap_marked=", memstats.heap_marked, " heap_live=", memstats.heap_live, " initialHeapLive=", work.initialHeapLive, "triggerRatio=", triggerRatio, " minTrigger=", minTrigger, "\n")
			throw("gc_trigger underflow")
		}
		if trigger > goal {
			// The trigger ratio is always less than GOGC/100, but
			// other bounds on the trigger may have raised it.
			// Push up the goal, too.
			goal = trigger
		}
	}

	// Commit to the trigger and goal.
	memstats.gc_trigger = trigger
	memstats.next_gc = goal
	if trace.enabled {
		traceNextGC()
	}

	// Update mark pacing.
	if gcphase != _GCoff {
		gcController.revise()
	}

	// Update sweep pacing.
	if isSweepDone() {
		mheap_.sweepPagesPerByte = 0
	} else {
		// Concurrent sweep needs to sweep all of the in-use
		// pages by the time the allocated heap reaches the GC
		// trigger. Compute the ratio of in-use pages to sweep
		// per byte allocated, accounting for the fact that
		// some might already be swept.
		heapLiveBasis := atomic.Load64(&memstats.heap_live)
		heapDistance := int64(trigger) - int64(heapLiveBasis)
		// Add a little margin so rounding errors and
		// concurrent sweep are less likely to leave pages
		// unswept when GC starts.
		heapDistance -= 1024 * 1024
		if heapDistance < _PageSize {
			// Avoid setting the sweep ratio extremely high
			heapDistance = _PageSize
		}
		pagesSwept := atomic.Load64(&mheap_.pagesSwept)
		pagesInUse := atomic.Load64(&mheap_.pagesInUse)
		sweepDistancePages := int64(pagesInUse) - int64(pagesSwept)
		if sweepDistancePages <= 0 {
			mheap_.sweepPagesPerByte = 0
		} else {
			mheap_.sweepPagesPerByte = float64(sweepDistancePages) / float64(heapDistance)
			mheap_.sweepHeapLiveBasis = heapLiveBasis
			// Write pagesSweptBasis last, since this
			// signals concurrent sweeps to recompute
			// their debt.
			atomic.Store64(&mheap_.pagesSweptBasis, pagesSwept)
		}
	}

	gcPaceScavenger()
}

// gcEffectiveGrowthRatio returns the current effective heap growth
// ratio (GOGC/100) based on heap_marked from the previous GC and
// next_gc for the current GC.
//
// This may differ from gcpercent/100 because of various upper and
// lower bounds on gcpercent. For example, if the heap is smaller than
// heapminimum, this can be higher than gcpercent/100.
//
// mheap_.lock must be held or the world must be stopped.
func gcEffectiveGrowthRatio() float64 {
	egogc := float64(memstats.next_gc-memstats.heap_marked) / float64(memstats.heap_marked)
	if egogc < 0 {
		// Shouldn't happen, but just in case.
		egogc = 0
	}
	return egogc
}

// gcGoalUtilization is the goal CPU utilization for
// marking as a fraction of GOMAXPROCS.
const gcGoalUtilization = 0.30

// gcBackgroundUtilization is the fixed CPU utilization for background
// marking. It must be <= gcGoalUtilization. The difference between
// gcGoalUtilization and gcBackgroundUtilization will be made up by
// mark assists. The scheduler will aim to use within 50% of this
// goal.
//
// Setting this to < gcGoalUtilization avoids saturating the trigger
// feedback controller when there are no assists, which allows it to
// better control CPU and heap growth. However, the larger the gap,
// the more mutator assists are expected to happen, which impact
// mutator latency.
const gcBackgroundUtilization = 0.25

// gcCreditSlack is the amount of scan work credit that can
// accumulate locally before updating gcController.scanWork and,
// optionally, gcController.bgScanCredit. Lower values give a more
// accurate assist ratio and make it more likely that assists will
// successfully steal background credit. Higher values reduce memory
// contention.
const gcCreditSlack = 2000

// gcAssistTimeSlack is the nanoseconds of mutator assist time that
// can accumulate on a P before updating gcController.assistTime.
const gcAssistTimeSlack = 5000

// gcOverAssistWork determines how many extra units of scan work a GC
// assist does when an assist happens. This amortizes the cost of an
// assist by pre-paying for this many bytes of future allocations.
const gcOverAssistWork = 64 << 10

// gc 任务 全局数据
var work struct {
	full  lfstack          // 头部 lock-free list of full blocks workbuf
	empty lfstack          // lock-free list of empty blocks workbuf
	pad0  cpu.CacheLinePad // prevents false-sharing between full/empty and nproc/nwait

	wbufSpans struct {
		lock mutex
		// free is a list of spans dedicated to workbufs, but
		// that don't currently contain any workbufs.
		free mSpanList
		// busy is a list of all spans containing workbufs on
		// one of the workbuf lists.
		busy mSpanList
	}

	// Restore 64-bit alignment on 32-bit.
	_ uint32

	// bytesMarked is the number of bytes marked this cycle. This
	// includes bytes blackened in scanned objects, noscan objects
	// that go straight to black, and permagrey objects scanned by
	// markroot during the concurrent scan phase. This is updated
	// atomically during the cycle. Updates may be batched
	// arbitrarily, since the value is only read at the end of the
	// cycle.
	//
	// Because of benign races during marking, this number may not
	// be the exact number of marked bytes, but it should be very
	// close.
	//
	// Put this field here because it needs 64-bit atomic access
	// (and thus 8-byte alignment even on 32-bit architectures).
	bytesMarked uint64

	markrootNext uint32 // next markroot job
	markrootJobs uint32 // number of markroot jobs

	nproc  uint32
	tstart int64
	nwait  uint32
	ndone  uint32

	// Number of roots of various root types. Set by gcMarkRootPrepare.
	nFlushCacheRoots                               int
	nDataRoots, nBSSRoots, nSpanRoots, nStackRoots int

	// Each type of GC state transition is protected by a lock.
	// Since multiple threads can simultaneously detect the state
	// transition condition, any thread that detects a transition
	// condition must acquire the appropriate transition lock,
	// re-check the transition condition and return if it no
	// longer holds or perform the transition if it does.
	// Likewise, any transition must invalidate the transition
	// condition before releasing the lock. This ensures that each
	// transition is performed by exactly one thread and threads
	// that need the transition to happen block until it has
	// happened.
	//
	// startSema protects the transition from "off" to mark or
	// mark termination.
	startSema uint32
	// markDoneSema protects transitions from mark to mark termination.
	markDoneSema uint32

	bgMarkReady note   // signal background mark worker has started
	bgMarkDone  uint32 // cas to 1 when at a background mark completion point
	// Background mark completion signaling

	// mode is the concurrency mode of the current GC cycle.
	mode gcMode // gc的三种模式

	// userForced indicates the current GC cycle was forced by an
	// explicit user call.
	userForced bool

	// totaltime is the CPU nanoseconds spent in GC since the
	// program started if debug.gctrace > 0.
	totaltime int64

	// initialHeapLive is the value of memstats.heap_live at the
	// beginning of this GC cycle.
	initialHeapLive uint64

	// assistQueue is a queue of assists that are blocked because
	// there was neither enough credit to steal or enough work to
	// do.
	assistQueue struct {
		lock mutex
		q    gQueue
	}

	// sweepWaiters is a list of blocked goroutines to wake when
	// we transition from mark termination to sweep.
	sweepWaiters struct {
		lock mutex
		list gList
	}

	// cycles is the number of completed GC cycles, where a GC
	// cycle is sweep termination, mark, mark termination, and
	// sweep. This differs from memstats.numgc, which is
	// incremented at mark termination.
	cycles uint32 // gc完成的次数

	// Timing/utilization stats for this cycle.
	stwprocs, maxprocs                 int32
	tSweepTerm, tMark, tMarkTerm, tEnd int64 // nanotime() of phase start

	pauseNS    int64 // total STW time this cycle
	pauseStart int64 // nanotime() of last STW

	// debug.gctrace heap sizes for this cycle.
	heap0, heap1, heap2, heapGoal uint64
}

// GC runs a garbage collection and blocks the caller until the
// garbage collection is complete. It may also block the entire
// program. 阻塞调用者直到完成
func GC() {
	// We consider a cycle to be: sweep termination, mark, mark
	// termination, and sweep. This function shouldn't return
	// until a full cycle has been completed, from beginning to
	// end. Hence, we always want to finish up the current cycle
	// and start a new one. That means:
	//
	// 1. In sweep termination, mark, or mark termination of cycle
	// N, wait until mark termination N completes and transitions
	// to sweep N.
	//
	// 2. In sweep N, help with sweep N.
	//
	// At this point we can begin a full cycle N+1.
	//
	// 3. Trigger cycle N+1 by starting sweep termination N+1.
	//
	// 4. Wait for mark termination N+1 to complete.
	//
	// 5. Help with sweep N+1 until it's done.
	//
	// This all has to be written to deal with the fact that the
	// GC may move ahead on its own. For example, when we block
	// until mark termination N, we may wake up in cycle N+2.

	// Wait until the current sweep termination, mark, and mark
	// termination complete.
	// 等待上一个gc周期完成
	n := atomic.Load(&work.cycles)
	gcWaitOnMark(n)

	// We're now in sweep N or later. Trigger GC cycle N+1, which
	// will first finish sweep N if necessary and then enter sweep
	// termination N+1.
	gcStart(gcTrigger{kind: gcTriggerCycle, n: n + 1})

	// Wait for mark termination N+1 to complete.
	// wait n + 1 too.
	gcWaitOnMark(n + 1)

	// Finish sweep N+1 before returning. We do this both to
	// complete the cycle and because runtime.GC() is often used
	// as part of tests and benchmarks to get the system into a
	// relatively stable and isolated state.
	// sweepone 扫描mspan，并返回扫描完的页数
	for atomic.Load(&work.cycles) == n+1 && sweepone() != ^uintptr(0) {
		sweep.nbgsweep++
		// 让出cpu 等待gc完成
		Gosched()
	}

	// Callers may assume that the heap profile reflects the
	// just-completed cycle when this returns (historically this
	// happened because this was a STW GC), but right now the
	// profile still reflects mark termination N, not N+1.
	//
	// As soon as all of the sweep frees from cycle N+1 are done,
	// we can go ahead and publish the heap profile.
	//
	// First, wait for sweeping to finish. (We know there are no
	// more spans on the sweep queue, but we may be concurrently
	// sweeping spans, so we have to wait.)
	// 等待sweeper 都退出 sweepone函数 进行 加1，减1
	for atomic.Load(&work.cycles) == n+1 && atomic.Load(&mheap_.sweepers) != 0 {
		Gosched()
	}

	// Now we're really done with sweeping, so we can publish the
	// stable heap profile 堆剖析. Only do this if we haven't already hit
	// another mark termination.
	mp := acquirem()
	cycle := atomic.Load(&work.cycles)
	if cycle == n+1 || (gcphase == _GCmark && cycle == n+2) {
		mProf_PostSweep()
	}
	releasem(mp)
}

// gcWaitOnMark blocks until GC finishes the Nth mark phase. If GC has
// already completed this mark phase, it returns immediately.
// 阻塞，直到标记完成
func gcWaitOnMark(n uint32) {
	for {
		// Disable phase transitions.
		lock(&work.sweepWaiters.lock)
		nMarks := atomic.Load(&work.cycles)
		if gcphase != _GCmark {
			// We've already completed this cycle's mark.
			nMarks++
		}
		if nMarks > n {
			// We're done.
			unlock(&work.sweepWaiters.lock)
			return
		}

		// Wait until sweep termination, mark, and mark
		// termination of cycle N complete.
		work.sweepWaiters.list.push(getg())
		goparkunlock(&work.sweepWaiters.lock, waitReasonWaitForGCCycle, traceEvGoBlock, 1)
	}
}

// gcMode indicates how concurrent a GC cycle should be.
type gcMode int

const (
	gcBackgroundMode gcMode = iota // concurrent GC and sweep
	gcForceMode                    // stop-the-world GC now, concurrent sweep
	gcForceBlockMode               // stop-the-world GC now and STW sweep (forced by user)
)

// A gcTrigger is a predicate for starting a GC cycle. Specifically,
// it is an exit condition for the _GCoff phase.
type gcTrigger struct {
	kind gcTriggerKind
	now  int64  // gcTriggerTime: current time
	n    uint32 // gcTriggerCycle: cycle number to start
}

type gcTriggerKind int

const (
	// gcTriggerHeap indicates that a cycle should be started when
	// the heap size reaches the trigger heap size computed by the
	// controller.
	gcTriggerHeap gcTriggerKind = iota

	// gcTriggerTime indicates that a cycle should be started when
	// it's been more than forcegcperiod nanoseconds since the
	// previous GC cycle.
	gcTriggerTime

	// gcTriggerCycle indicates that a cycle should be started if
	// we have not yet started cycle number gcTrigger.n (relative
	// to work.cycles).
	gcTriggerCycle
)

// test reports whether the trigger condition is satisfied, meaning
// that the exit condition for the _GCoff phase has been met. The exit
// condition should be tested when allocating.
func (t gcTrigger) test() bool {
	if !memstats.enablegc || panicking != 0 || gcphase != _GCoff {
		return false
	}
	switch t.kind {
	case gcTriggerHeap:
		// 分配内存结束后条件触发
		// Non-atomic access to heap_live for performance. If
		// we are going to trigger on this, this thread just
		// atomically wrote heap_live anyway and we'll see our
		// own write.
		return memstats.heap_live >= memstats.gc_trigger
	case gcTriggerTime:
		// sysmon forcegchelper 循环定时触发
		if gcpercent < 0 {
			return false
		}
		lastgc := int64(atomic.Load64(&memstats.last_gc_nanotime))
		return lastgc != 0 && t.now-lastgc > forcegcperiod
	case gcTriggerCycle:
		// 用户调用 GC() 主动触发
		// t.n > work.cycles, but accounting for wraparound.
		return int32(t.n-work.cycles) > 0
	}
	return true
}

// gcStart starts the GC. It transitions from _GCoff to _GCmark (if
// debug.gcstoptheworld == 0) or performs all of GC (if
// debug.gcstoptheworld != 0).
//
// This may return without performing this transition in some cases,
// such as when called on a system stack or with locks held.
func gcStart(trigger gcTrigger) {
	// Since this is called from malloc and malloc is called in
	// the guts of a number of libraries that might be holding
	// locks, don't attempt to start GC in non-preemptible or
	// potentially unstable situations.
	mp := acquirem()
	if gp := getg(); gp == mp.g0 || mp.locks > 1 || mp.preemptoff != "" {
		releasem(mp)
		return
	}
	releasem(mp)
	mp = nil

	// Pick up the remaining unswept/not being swept spans concurrently
	//
	// This shouldn't happen if we're being invoked in background
	// mode since proportional sweep should have just finished
	// sweeping everything, but rounding errors, etc, may leave a
	// few spans unswept. In forced mode, this is necessary since
	// GC can be forced at any point in the sweeping cycle.
	//
	// We check the transition condition continuously here in case
	// this G gets delayed in to the next GC cycle.
	for trigger.test() && sweepone() != ^uintptr(0) {
		sweep.nbgsweep++
	}

	// Perform GC initialization and the sweep termination
	// transition.
	semacquire(&work.startSema)
	// 加锁后再次检查 Re-check transition condition under transition lock.
	if !trigger.test() {
		semrelease(&work.startSema)
		return
	}

	// For stats, check if this GC was forced by the user.
	work.userForced = trigger.kind == gcTriggerCycle

	// In gcstoptheworld debug mode, upgrade the mode accordingly.
	// We do this after re-checking the transition condition so
	// that multiple goroutines that detect the heap trigger don't
	// start multiple STW GCs.
	mode := gcBackgroundMode
	if debug.gcstoptheworld == 1 {
		mode = gcForceMode
	} else if debug.gcstoptheworld == 2 {
		mode = gcForceBlockMode
	}

	// Ok, we're doing it! Stop everybody else
	semacquire(&gcsema)
	semacquire(&worldsema)

	if trace.enabled {
		traceGCStart()
	}

	// Check that all Ps have finished deferred mcache flushes.
	for _, p := range allp {
		if fg := atomic.Load(&p.mcache.flushGen); fg != mheap_.sweepgen {
			println("runtime: p", p.id, "flushGen", fg, "!= sweepgen", mheap_.sweepgen)
			throw("p mcache not flushed")
		}
	}

	// 开启 异步后台工作g
	gcBgMarkStartWorkers()

	systemstack(gcResetMarkState)

	work.stwprocs, work.maxprocs = gomaxprocs, gomaxprocs
	if work.stwprocs > ncpu {
		// This is used to compute CPU time of the STW phases,
		// so it can't be more than ncpu, even if GOMAXPROCS is.
		work.stwprocs = ncpu
	}
	work.heap0 = atomic.Load64(&memstats.heap_live)
	work.pauseNS = 0
	work.mode = mode

	now := nanotime()
	work.tSweepTerm = now
	work.pauseStart = now
	if trace.enabled {
		traceGCSTWStart(1)
	}
	// stw
	systemstack(stopTheWorldWithSema)
	// 完成清理 Finish sweep before we start concurrent scan.
	systemstack(func() {
		finishsweep_m()
	})

	// clearpools before we start the GC. If we wait they memory will not be
	// reclaimed until the next GC cycle.
	// 清理deferpool 和 sugogpool
	clearpools()

	work.cycles++

	gcController.startCycle()
	work.heapGoal = memstats.next_gc

	// In STW mode, 禁止调度用户g执行 disable scheduling of user Gs. This may also
	// disable scheduling of this goroutine, so it may block as
	// soon as we start the world again.
	if mode != gcBackgroundMode {
		schedEnableUser(false)
	}

	// Enter concurrent mark phase and enable
	// write barriers.
	//
	// Because the world is stopped, all Ps will
	// observe that write barriers are enabled by
	// the time we start the world and begin
	// scanning.
	//
	// Write barriers must be enabled before assists are
	// enabled because they must be enabled before
	// any non-leaf heap objects are marked. Since
	// allocations are blocked until assists can
	// happen, we want enable assists as early as
	// possible.

	// gcStart 进入标记阶段
	setGCPhase(_GCmark)

	// 准备工作
	gcBgMarkPrepare() // Must happen before assist enable.
	gcMarkRootPrepare()

	// Mark all active tinyalloc blocks. Since we're
	// allocating from these, they need to be black like
	// other allocations. The alternative is to blacken
	// the tiny block on every allocation from it, which
	// would slow down the tiny allocator.
	// 标记小块分片
	gcMarkTinyAllocs()

	// At this point all Ps have enabled the write
	// barrier, thus maintaining the no white to
	// black invariant不变量. Enable mutator assists to
	// put back-pressure on fast allocating
	// mutators.
	// 新分配对象直接标记为黑色
	atomic.Store(&gcBlackenEnabled, 1)

	// Assists and workers can start the moment we start
	// the world.
	gcController.markStartTime = now

	// In STW mode, we could block the instant systemstack
	// returns, so make sure we're not preemptible.
	mp = acquirem()

	// Concurrent mark. 并发标记
	systemstack(func() {
		now = startTheWorldWithSema(trace.enabled)
		work.pauseNS += now - work.pauseStart
		work.tMark = now
	})

	// Release the world sema before Gosched() in STW mode
	// because we will need to reacquire it later but before
	// this goroutine becomes runnable again, and we could
	// self-deadlock otherwise.
	semrelease(&worldsema)
	releasem(mp)

	// Make sure we block instead of returning to user code
	// in STW mode.
	if mode != gcBackgroundMode {
		Gosched()
	}

	semrelease(&work.startSema)
}

// gcMarkDoneFlushed counts the number of P's with flushed work.
//
// Ideally this would be a captured local in gcMarkDone, but forEachP
// escapes its callback closure, so it can't capture anything.
//
// This is protected by markDoneSema.
var gcMarkDoneFlushed uint32

// debugCachedWork enables extra checks for debugging premature mark
// termination.
//
// For debugging issue #27993.
const debugCachedWork = false

// gcWorkPauseGen is for debugging the mark completion algorithm.
// gcWork put operations spin while gcWork.pauseGen == gcWorkPauseGen.
// Only used if debugCachedWork is true.
//
// For debugging issue #27993.
var gcWorkPauseGen uint32 = 1

// gcMarkDone transitions the GC from mark to mark termination if all
// reachable objects have been marked 如果所有可达到对象都被标记了 (that is, there are no grey
// objects and can be no more in the future 没有灰色对象，将来也不会有了).
// Otherwise, it flushes all local work to the global queues where it can be discovered by
// other workers.
//
// This should be called when all local mark work has been drained干完 and
// there are no remaining workers. Specifically, when
//
//   work.nwait == work.nproc && !gcMarkWorkAvailable(p)
//
// The calling context must be preemptible.
//
// Flushing local work is important because idle Ps may have local
// work queued. This is the only way to make that work visible and
// drive GC to completion.
//
// It is explicitly okay to have write barriers in this function. If
// it does transition to mark termination, then all reachable objects
// have been marked, so the write barrier cannot shade any more
// objects.
// 分两个标记阶段
// 从mark -> marktermination阶段
func gcMarkDone() {
	// Ensure only one thread is running the ragged barrier at a
	// time.
	semacquire(&work.markDoneSema)

top:
	// Re-check transition condition under transition lock.
	//
	// It's critical that this checks the global work queues are
	// empty before performing the ragged barrier. Otherwise,
	// there could be global work that a P could take after the P
	// has passed the ragged barrier.
	if !(gcphase == _GCmark && work.nwait == work.nproc && !gcMarkWorkAvailable(nil)) {
		semrelease(&work.markDoneSema)
		return
	}

	// forEachP needs worldsema to execute, and we'll need it to
	// stop the world later, so acquire worldsema now.
	semacquire(&worldsema)

	// Flush all local buffers and collect flushedWork flags.
	gcMarkDoneFlushed = 0
	systemstack(func() {
		gp := getg().m.curg
		// Mark the user stack as preemptible so that it may be scanned.
		// Otherwise, our attempt to force all P's to a safepoint could
		// result in a deadlock as we attempt to preempt a worker that's
		// trying to preempt us (e.g. for a stack scan).
		// gaMarkDone()
		casgstatus(gp, _Grunning, _Gwaiting)
		forEachP(func(_p_ *p) {
			// Flush the write barrier buffer, since this may add
			// work to the gcWork.
			wbBufFlush1(_p_)
			// For debugging, shrink the write barrier
			// buffer so it flushes immediately.
			// wbBuf.reset will keep it at this size as
			// long as throwOnGCWork is set.
			if debugCachedWork {
				b := &_p_.wbBuf
				b.end = uintptr(unsafe.Pointer(&b.buf[wbBufEntryPointers]))
				b.debugGen = gcWorkPauseGen
			}
			// Flush the gcWork, since this may create global work
			// and set the flushedWork flag.
			//
			// TODO(austin): Break up these workbufs to
			// better distribute work.
			_p_.gcw.dispose()
			// Collect the flushedWork flag.
			if _p_.gcw.flushedWork {
				atomic.Xadd(&gcMarkDoneFlushed, 1)
				_p_.gcw.flushedWork = false
			} else if debugCachedWork {
				// For debugging, freeze the gcWork
				// until we know whether we've reached
				// completion or not. If we think
				// we've reached completion, but
				// there's a paused gcWork, then
				// that's a bug.
				_p_.gcw.pauseGen = gcWorkPauseGen
				// Capture the G's stack.
				for i := range _p_.gcw.pauseStack {
					_p_.gcw.pauseStack[i] = 0
				}
				callers(1, _p_.gcw.pauseStack[:])
			}
		})
		// 恢复执行
		casgstatus(gp, _Gwaiting, _Grunning)
	})

	if gcMarkDoneFlushed != 0 {
		if debugCachedWork {
			// Release paused gcWorks.
			atomic.Xadd(&gcWorkPauseGen, 1)
		}
		// More grey objects were discovered since the
		// previous termination check, so there may be more
		// work to do. Keep going. It's possible the
		// transition condition became true again during the
		// ragged barrier, so re-check it.
		semrelease(&worldsema)
		goto top
	}

	if debugCachedWork {
		throwOnGCWork = true
		// Release paused gcWorks. If there are any, they
		// should now observe throwOnGCWork and panic.
		atomic.Xadd(&gcWorkPauseGen, 1)
	}

	// There was no global work, no local work, and no Ps
	// communicated work since we took markDoneSema. Therefore
	// there are no grey objects and no more objects can be
	// shaded. Transition to mark termination.
	now := nanotime()
	work.tMarkTerm = now
	work.pauseStart = now
	getg().m.preemptoff = "gcing"
	if trace.enabled {
		traceGCSTWStart(0)
	}

	// stw markTermination
	systemstack(stopTheWorldWithSema)
	// The gcphase is _GCmark, it will transition to _GCmarktermination
	// below. The important thing is that the wb remains active until
	// all marking is complete. This includes writes made by the GC.

	if debugCachedWork {
		// For debugging, double check that no work was added after we
		// went around above and disable write barrier buffering.
		for _, p := range allp {
			gcw := &p.gcw
			if !gcw.empty() {
				printlock()
				print("runtime: P ", p.id, " flushedWork ", gcw.flushedWork)
				if gcw.wbuf1 == nil {
					print(" wbuf1=<nil>")
				} else {
					print(" wbuf1.n=", gcw.wbuf1.nobj)
				}
				if gcw.wbuf2 == nil {
					print(" wbuf2=<nil>")
				} else {
					print(" wbuf2.n=", gcw.wbuf2.nobj)
				}
				print("\n")
				if gcw.pauseGen == gcw.putGen {
					println("runtime: checkPut already failed at this generation")
				}
				throw("throwOnGCWork")
			}
		}
	} else {
		// For unknown reasons (see issue #27993), there is
		// sometimes work left over when we enter mark
		// termination. Detect this and resume concurrent
		// mark. This is obviously unfortunate.
		//
		// Switch to the system stack to call wbBufFlush1,
		// though in this case it doesn't matter because we're
		// non-preemptible anyway.
		restart := false
		systemstack(func() {
			for _, p := range allp {
				wbBufFlush1(p)
				if !p.gcw.empty() {
					restart = true
					break
				}
			}
		})
		if restart {
			getg().m.preemptoff = ""
			systemstack(func() {
				now := startTheWorldWithSema(true)
				work.pauseNS += now - work.pauseStart
			})
			semrelease(&worldsema)
			goto top
		}
	}

	// Disable assists and background workers. We must do
	// this before waking blocked assists.
	atomic.Store(&gcBlackenEnabled, 0)

	// Wake all blocked assists. These will run when we
	// start the world again.
	// 将 assist g唤醒 继续干活
	gcWakeAllAssists()

	// Likewise, release the transition lock. Blocked
	// workers and assists will run when we start the
	// world again.
	semrelease(&work.markDoneSema)

	// In STW mode, re-enable user goroutines. These will be
	// queued to run after we start the world.
	// 重新允许调度用户协程
	schedEnableUser(true)

	// endCycle depends on all gcWork cache stats being flushed.
	// The termination algorithm above ensured that up to
	// allocations since the ragged barrier.
	nextTriggerRatio := gcController.endCycle()

	// 开始标记终止的工作，会重启世界
	// Perform mark termination. This will restart the world.
	gcMarkTermination(nextTriggerRatio)
}

// 完成标记
func gcMarkTermination(nextTriggerRatio float64) {
	// World is stopped.
	// Start marktermination which includes enabling the write barrier.
	// 关闭写屏障和辅助gc
	atomic.Store(&gcBlackenEnabled, 0)
	// 设置为标记终止状态
	setGCPhase(_GCmarktermination)

	work.heap1 = memstats.heap_live
	startTime := nanotime()

	mp := acquirem()
	mp.preemptoff = "gcing"
	_g_ := getg()
	_g_.m.traceback = 2
	gp := _g_.m.curg
	// 设置为wait，这个的g可以进行栈扫描
	casgstatus(gp, _Grunning, _Gwaiting)
	gp.waitreason = waitReasonGarbageCollection

	// Run gc on the g0 stack. We do this so that the g stack
	// we're currently running on will no longer change. Cuts
	// the root set down a bit (g0 stacks are not scanned, and
	// we don't need to scan gc's internal state).  We also
	// need to switch to g0 so we can shrink the stack.
	systemstack(func() {
		// 开始gc标记工作
		gcMark(startTime)
		// Must return immediately.
		// The outer function's stack may have moved
		// during gcMark (it shrinks stacks, including the
		// outer function's stack), so we must not refer
		// to any of its variables. Return back to the
		// non-system stack to pick up the new addresses
		// before continuing.
	})

	systemstack(func() {
		work.heap2 = work.bytesMarked
		if debug.gccheckmark > 0 {
			// Run a full non-parallel, stop-the-world
			// mark using checkmark bits, to check that we
			// didn't forget to mark anything during the
			// concurrent mark process.
			gcResetMarkState()
			initCheckmarks()
			gcw := &getg().m.p.ptr().gcw
			gcDrain(gcw, 0)
			wbBufFlush1(getg().m.p.ptr())
			gcw.dispose()
			clearCheckmarks()
		}

		// marking is complete so we can turn the write barrier off
		// 标记完成 关闭写屏障
		setGCPhase(_GCoff)

		// 唤醒gc清除，将在stw结束后进行清除
		gcSweep(work.mode)
	})

	_g_.m.traceback = 0
	// 恢复运行态
	casgstatus(gp, _Gwaiting, _Grunning)

	if trace.enabled {
		traceGCDone()
	}

	// all done
	mp.preemptoff = ""

	if gcphase != _GCoff {
		throw("gc done but gcphase != _GCoff")
	}

	// Record next_gc and heap_inuse for scavenger.
	memstats.last_next_gc = memstats.next_gc
	memstats.last_heap_inuse = memstats.heap_inuse

	// Update GC trigger and pacing for the next cycle.
	// 设置下次触发gc需要的堆大小的比例
	gcSetTriggerRatio(nextTriggerRatio)

	// Update timing memstats
	now := nanotime()
	sec, nsec, _ := time_now()
	unixNow := sec*1e9 + int64(nsec)
	work.pauseNS += now - work.pauseStart
	work.tEnd = now
	atomic.Store64(&memstats.last_gc_unix, uint64(unixNow)) // must be Unix time to make sense to user
	atomic.Store64(&memstats.last_gc_nanotime, uint64(now)) // monotonic time for us
	memstats.pause_ns[memstats.numgc%uint32(len(memstats.pause_ns))] = uint64(work.pauseNS)
	memstats.pause_end[memstats.numgc%uint32(len(memstats.pause_end))] = uint64(unixNow)
	memstats.pause_total_ns += uint64(work.pauseNS)

	// Update work.totaltime.
	sweepTermCpu := int64(work.stwprocs) * (work.tMark - work.tSweepTerm)
	// We report idle marking time below, but omit it from the
	// overall utilization here since it's "free".
	markCpu := gcController.assistTime + gcController.dedicatedMarkTime + gcController.fractionalMarkTime
	markTermCpu := int64(work.stwprocs) * (work.tEnd - work.tMarkTerm)
	cycleCpu := sweepTermCpu + markCpu + markTermCpu
	work.totaltime += cycleCpu

	// Compute overall GC CPU utilization.
	totalCpu := sched.totaltime + (now-sched.procresizetime)*int64(gomaxprocs)
	memstats.gc_cpu_fraction = float64(work.totaltime) / float64(totalCpu)

	// Reset sweep state.
	sweep.nbgsweep = 0
	sweep.npausesweep = 0

	if work.userForced {
		memstats.numforcedgc++
	}

	// Bump GC cycle count and wake goroutines waiting on sweep.
	lock(&work.sweepWaiters.lock)
	memstats.numgc++
	injectglist(&work.sweepWaiters.list)
	unlock(&work.sweepWaiters.lock)

	// Finish the current heap profiling cycle and start a new
	// heap profiling cycle. We do this before starting the world
	// so events don't leak into the wrong cycle.
	mProf_NextCycle()

	// 恢复世界
	systemstack(func() { startTheWorldWithSema(true) })

	// Flush the heap profile so we can start a new cycle next GC.
	// This is relatively expensive, so we don't do it with the
	// world stopped.
	mProf_Flush()

	// Prepare workbufs for freeing by the sweeper. We do this
	// asynchronously because it can take non-trivial time.
	prepareFreeWorkbufs()

	// Free stack spans. This must be done between GC cycles.
	systemstack(freeStackSpans)

	// Ensure all mcaches are flushed. Each P will flush its own
	// mcache before allocating, but idle Ps may not. Since this
	// is necessary to sweep all spans, we need to ensure all
	// mcaches are flushed before we start the next GC cycle.
	systemstack(func() {
		// 准备清扫
		forEachP(func(_p_ *p) {
			_p_.mcache.prepareForSweep()
		})
	})

	// Print gctrace before dropping worldsema. As soon as we drop
	// worldsema another cycle could start and smash the stats
	// we're trying to print.
	if debug.gctrace > 0 {
		util := int(memstats.gc_cpu_fraction * 100)

		var sbuf [24]byte
		printlock()
		print("gc ", memstats.numgc,
			" @", string(itoaDiv(sbuf[:], uint64(work.tSweepTerm-runtimeInitTime)/1e6, 3)), "s ",
			util, "%: ")
		prev := work.tSweepTerm
		for i, ns := range []int64{work.tMark, work.tMarkTerm, work.tEnd} {
			if i != 0 {
				print("+")
			}
			print(string(fmtNSAsMS(sbuf[:], uint64(ns-prev))))
			prev = ns
		}
		print(" ms clock, ")
		for i, ns := range []int64{sweepTermCpu, gcController.assistTime, gcController.dedicatedMarkTime + gcController.fractionalMarkTime, gcController.idleMarkTime, markTermCpu} {
			if i == 2 || i == 3 {
				// Separate mark time components with /.
				print("/")
			} else if i != 0 {
				print("+")
			}
			print(string(fmtNSAsMS(sbuf[:], uint64(ns))))
		}
		print(" ms cpu, ",
			work.heap0>>20, "->", work.heap1>>20, "->", work.heap2>>20, " MB, ",
			work.heapGoal>>20, " MB goal, ",
			work.maxprocs, " P")
		if work.userForced {
			print(" (forced)")
		}
		print("\n")
		printunlock()
	}

	semrelease(&worldsema)
	semrelease(&gcsema)
	// Careful: another GC cycle may start now.

	releasem(mp)
	mp = nil

	// now that gc is done, kick off finalizer thread if needed
	if !concurrentSweep {
		// give the queued finalizers, if any, a chance to run
		Gosched()
	}
}

// gcBgMarkStartWorkers prepares background mark worker goroutines.
// These goroutines will not run until the mark phase, but they must
// be started while the work is not stopped and from a regular G
// stack. The caller must hold worldsema.
func gcBgMarkStartWorkers() {
	// Background marking is performed by per-P G's. Ensure that
	// each P has a background GC G.
	for _, p := range allp {
		if p.gcBgMarkWorker == 0 {
			go gcBgMarkWorker(p)
			// 永久等待
			notetsleepg(&work.bgMarkReady, -1)
			noteclear(&work.bgMarkReady)
		}
	}
}

// gcBgMarkPrepare sets up state for background marking.
// Mutator assists must not yet be enabled.
func gcBgMarkPrepare() {
	// Background marking will stop when the work queues are empty
	// and there are no more workers (note that, since this is
	// concurrent, this may be a transient state, but mark
	// termination will clean it up). Between background workers
	// and assists, we don't really know how many workers there
	// will be, so we pretend to have an arbitrarily large number
	// of workers, almost all of which are "waiting". While a
	// worker is working it decrements nwait. If nproc == nwait,
	// there are no workers.
	work.nproc = ^uint32(0)
	work.nwait = ^uint32(0)
}

// 后台异步标记 per-P
//
func gcBgMarkWorker(_p_ *p) {
	gp := getg()

	// 函数内的类型声明
	type parkInfo struct {
		m      muintptr // Release this m on park.
		attach puintptr // If non-nil, attach to this p on park.
	}
	// We pass park to a gopark unlock function, so it can't be on
	// the stack (see gopark). Prevent deadlock from recursively
	// starting GC by disabling preemption.
	gp.m.preemptoff = "GC worker init"
	park := new(parkInfo)
	gp.m.preemptoff = ""

	// 锁定m，然后设置
	park.m.set(acquirem())
	park.attach.set(_p_)
	// Inform gcBgMarkStartWorkers that this worker is ready.
	// After this point, the background mark worker is scheduled
	// cooperatively by gcController.findRunnable. Hence, it must
	// never be preempted, as this would put it into _Grunnable
	// and put it on a run queue. Instead, when the preempt flag
	// is set, this puts itself into _Gwaiting to be woken up by
	// gcController.findRunnable at the appropriate time.

	// 前面相当于初始化
	// 解除等待，开始下一个 bg mark worker
	notewakeup(&work.bgMarkReady)

	for {
		// Go to sleep until woken by gcController.findRunnable.
		// We can't releasem yet since even the call to gopark
		// may be preempted.
		gopark(func(g *g, parkp unsafe.Pointer) bool {
			park := (*parkInfo)(parkp)

			// The worker G is no longer running, so it's
			// now safe to allow preemption.
			// 工作g不再运行，解锁m
			releasem(park.m.ptr())

			// If the worker isn't attached to its P,
			// attach now. During initialization and after
			// a phase change, the worker may have been
			// running on a different P. As soon as we
			// attach, the owner P may schedule the
			// worker, so this must be done after the G is
			// stopped.
			if park.attach != 0 {
				p := park.attach.ptr()
				park.attach.set(nil)
				// cas the worker because we may be
				// racing with a new worker starting
				// on this P.
				if !p.gcBgMarkWorker.cas(0, guintptr(unsafe.Pointer(g))) {
					// The P got a new worker.
					// Exit this worker.
					return false
				}
			}
			return true
		}, unsafe.Pointer(park), waitReasonGCWorkerIdle, traceEvGoBlock, 0)

		// Loop until the P dies and disassociates this
		// worker (the P may later be reused, in which case
		// it will get a new worker) or we failed to associate.
		if _p_.gcBgMarkWorker.ptr() != gp {
			break
		}

		// Disable preemption so we can use the gcw. If the
		// scheduler wants to preempt us, we'll stop draining,
		// dispose the gcw, and then preempt.
		// 重新 锁定m，禁止g被抢占
		park.m.set(acquirem())

		if gcBlackenEnabled == 0 {
			throw("gcBgMarkWorker: blackening not enabled")
		}

		startTime := nanotime()
		_p_.gcMarkWorkerStartTime = startTime

		decnwait := atomic.Xadd(&work.nwait, -1)
		if decnwait == work.nproc {
			println("runtime: work.nwait=", decnwait, "work.nproc=", work.nproc)
			throw("work.nwait was > work.nproc")
		}

		// 系统栈上的代码隐式不可抢占，垃圾收集器不扫描系统堆栈
		systemstack(func() {
			// Mark our goroutine preemptible so its stack
			// can be scanned. This lets two mark workers
			// scan each other (otherwise, they would
			// deadlock). We must not modify anything on
			// the G stack. However, stack shrinking is
			// disabled for mark workers, so it is safe to
			// read from the G stack.
			// gcBgMarkWorker
			casgstatus(gp, _Grunning, _Gwaiting)
			switch _p_.gcMarkWorkerMode {
			default:
				throw("gcBgMarkWorker: unexpected gcMarkWorkerMode")
			case gcMarkWorkerDedicatedMode:
				// 黑化 灰色对象
				gcDrain(&_p_.gcw, gcDrainUntilPreempt|gcDrainFlushBgCredit)
				if gp.preempt {
					// We were preempted. This is
					// a useful signal to kick
					// everything out of the run
					// queue so it can run
					// somewhere else.
					// 被抢占了，应该将所有任务移到全局队列
					lock(&sched.lock)
					for {
						gp, _ := runqget(_p_)
						if gp == nil {
							break
						}
						globrunqput(gp)
					}
					unlock(&sched.lock)
				}
				// Go back to draining, this time
				// without preemption.
				gcDrain(&_p_.gcw, gcDrainFlushBgCredit)
			case gcMarkWorkerFractionalMode:
				gcDrain(&_p_.gcw, gcDrainFractional|gcDrainUntilPreempt|gcDrainFlushBgCredit)
			case gcMarkWorkerIdleMode:
				gcDrain(&_p_.gcw, gcDrainIdle|gcDrainUntilPreempt|gcDrainFlushBgCredit)
			}
			// gcDrain之后恢复运行
			casgstatus(gp, _Gwaiting, _Grunning)
		})

		// Account for time.
		duration := nanotime() - startTime
		switch _p_.gcMarkWorkerMode {
		case gcMarkWorkerDedicatedMode:
			atomic.Xaddint64(&gcController.dedicatedMarkTime, duration)
			atomic.Xaddint64(&gcController.dedicatedMarkWorkersNeeded, 1)
		case gcMarkWorkerFractionalMode:
			atomic.Xaddint64(&gcController.fractionalMarkTime, duration)
			atomic.Xaddint64(&_p_.gcFractionalMarkTime, duration)
		case gcMarkWorkerIdleMode:
			atomic.Xaddint64(&gcController.idleMarkTime, duration)
		}

		// Was this the last worker and did we run out
		// of work?
		incnwait := atomic.Xadd(&work.nwait, +1)
		if incnwait > work.nproc {
			println("runtime: p.gcMarkWorkerMode=", _p_.gcMarkWorkerMode,
				"work.nwait=", incnwait, "work.nproc=", work.nproc)
			throw("work.nwait > work.nproc")
		}

		// If this worker reached a background mark completion
		// point, signal the main GC goroutine.
		if incnwait == work.nproc && !gcMarkWorkAvailable(nil) {
			// Make this G preemptible and disassociate it
			// as the worker for this P so
			// findRunnableGCWorker doesn't try to
			// schedule it.
			_p_.gcBgMarkWorker.set(nil)
			releasem(park.m.ptr())

			// 表示标记结束，开启下一阶段
			gcMarkDone()

			// Disable preemption and prepare to reattach
			// to the P.
			//
			// We may be running on a different P at this
			// point, so we can't reattach until this G is
			// parked.
			park.m.set(acquirem())
			park.attach.set(_p_)
		}
	}
}

// gcMarkWorkAvailable reports whether executing a mark worker
// on p is potentially useful. p may be nil, in which case it only
// checks the global sources of work.
func gcMarkWorkAvailable(p *p) bool {
	if p != nil && !p.gcw.empty() {
		return true
	}
	if !work.full.empty() {
		return true // global work available
	}
	if work.markrootNext < work.markrootJobs {
		return true // root scan work available
	}
	return false
}

// gcMark runs the mark (or, for concurrent GC, mark termination)
// All gcWork caches must be empty.
// STW is in effect生效 at this point.
// 完成标记工作， 必须要marktermination阶段 被marktermination() call
func gcMark(start_time int64) {
	if debug.allocfreetrace > 0 {
		tracegc()
	}

	if gcphase != _GCmarktermination {
		throw("in gcMark expecting to see gcphase as _GCmarktermination")
	}
	work.tstart = start_time

	// Check that there's no marking work remaining.
	if work.full != 0 || work.markrootNext < work.markrootJobs {
		print("runtime: full=", hex(work.full), " next=", work.markrootNext, " jobs=", work.markrootJobs, " nDataRoots=", work.nDataRoots, " nBSSRoots=", work.nBSSRoots, " nSpanRoots=", work.nSpanRoots, " nStackRoots=", work.nStackRoots, "\n")
		panic("non-empty mark queue after concurrent mark")
	}

	if debug.gccheckmark > 0 {
		// This is expensive when there's a large number of
		// Gs, so only do it if checkmark is also enabled.
		gcMarkRootCheck()
	}
	if work.full != 0 {
		throw("work.full != 0")
	}

	// Clear out buffers and double-check that all gcWork caches
	// are empty. This should be ensured by gcMarkDone before we
	// enter mark termination.
	//
	// TODO: We could clear out buffers just before mark if this
	// has a non-negligible impact on STW time.
	for _, p := range allp {
		// The write barrier may have buffered pointers since
		// the gcMarkDone barrier. However, since the barrier
		// ensured all reachable objects were marked, all of
		// these must be pointers to black objects. Hence we
		// can just discard the write barrier buffer.
		if debug.gccheckmark > 0 || throwOnGCWork {
			// For debugging, flush the buffer and make
			// sure it really was all marked.
			wbBufFlush1(p)
		} else {
			p.wbBuf.reset()
		}

		gcw := &p.gcw
		if !gcw.empty() {
			printlock()
			print("runtime: P ", p.id, " flushedWork ", gcw.flushedWork)
			if gcw.wbuf1 == nil {
				print(" wbuf1=<nil>")
			} else {
				print(" wbuf1.n=", gcw.wbuf1.nobj)
			}
			if gcw.wbuf2 == nil {
				print(" wbuf2=<nil>")
			} else {
				print(" wbuf2.n=", gcw.wbuf2.nobj)
			}
			print("\n")
			throw("P has cached GC work at end of mark termination")
		}
		// There may still be cached empty buffers, which we
		// need to flush since we're going to free them. Also,
		// there may be non-zero stats because we allocated
		// black after the gcMarkDone barrier.
		gcw.dispose()
	}

	throwOnGCWork = false

	cachestats()

	// Update the marked heap stat.
	memstats.heap_marked = work.bytesMarked

	// Update other GC heap size stats. This must happen after
	// cachestats (which flushes local statistics to these) and
	// flushallmcaches (which modifies heap_live).
	memstats.heap_live = work.bytesMarked
	memstats.heap_scan = uint64(gcController.scanWork)

	if trace.enabled {
		traceHeapAlloc()
	}
}

// gcSweep must be called on the system stack because it acquires the heap
// lock. See mheap for details.
//
// The world must be stopped.
// 进行扫描工作
//go:systemstack
func gcSweep(mode gcMode) {
	// 标记结束才能清除
	if gcphase != _GCoff {
		throw("gcSweep being done but phase is not GCoff")
	}

	// 堆锁
	lock(&mheap_.lock)
	// 增加sweepgen, 这样sweepSpans中两个队列角色会交换, 所有span都会变为"待清扫"的span
	mheap_.sweepgen += 2
	mheap_.sweepdone = 0
	if !go115NewMCentralImpl && mheap_.sweepSpans[mheap_.sweepgen/2%2].index != 0 {
		// We should have drained this list during the last
		// sweep phase. We certainly need to start this phase
		// with an empty swept list.
		throw("non-empty swept list")
	}
	mheap_.pagesSwept = 0
	mheap_.sweepArenas = mheap_.allArenas
	mheap_.reclaimIndex = 0
	mheap_.reclaimCredit = 0
	unlock(&mheap_.lock)

	if go115NewMCentralImpl {
		sweep.centralIndex.clear()
	}

	// 如果非并发清扫，或者stw清扫，那么在这里开始工作
	if !_ConcurrentSweep || mode == gcForceBlockMode {
		// Special case synchronous sweep.
		// Record that no proportional sweeping has to happen.
		lock(&mheap_.lock)
		mheap_.sweepPagesPerByte = 0
		unlock(&mheap_.lock)
		// Sweep all spans eagerly急切.
		// 循环扫描，直到全部ok
		for sweepone() != ^uintptr(0) {
			sweep.npausesweep++
		}
		// Free workbufs eagerly.
		prepareFreeWorkbufs()
		// 释放工作缓存
		for freeSomeWbufs(false) {
		}
		// All "free" events for this mark/sweep cycle have
		// now happened, so we can make this profile cycle
		// available immediately.
		mProf_NextCycle()
		mProf_Flush()
		return
	}

	// 否则开Background sweep.
	lock(&sweep.lock)
	if sweep.parked {
		sweep.parked = false
		// 唤醒后台清扫任务
		ready(sweep.g, 0, true)
	}
	unlock(&sweep.lock)
}

// gcResetMarkState resets global state prior to在标记之前 marking (concurrent
// or STW) and resets the stack scan state of all Gs. 重置 g的栈扫描状态
//
// This is safe to do without the world stopped because any Gs created
// during or after this will start out in the reset state.
//
// gcResetMarkState must be called on the system stack because it acquires
// the heap lock. See mheap for details.
//
//go:systemstack
func gcResetMarkState() {
	// This may be called during a concurrent phase, so make sure
	// allgs doesn't change.
	lock(&allglock)
	// 初始化g相关状态
	for _, gp := range allgs {
		gp.gcscandone = false // set to true in gcphasework
		gp.gcAssistBytes = 0
	}
	unlock(&allglock)

	// Clear page marks. This is just 1MB per 64GB of heap, so the
	// time here is pretty trivial.
	lock(&mheap_.lock)
	arenas := mheap_.allArenas
	unlock(&mheap_.lock)
	// 清除所有arena上的页标记数组
	for _, ai := range arenas {
		ha := mheap_.arenas[ai.l1()][ai.l2()]
		for i := range ha.pageMarks {
			ha.pageMarks[i] = 0
		}
	}

	work.bytesMarked = 0
	work.initialHeapLive = atomic.Load64(&memstats.heap_live)
}

// Hooks for other packages

var poolcleanup func()

//go:linkname sync_runtime_registerPoolCleanup sync.runtime_registerPoolCleanup
func sync_runtime_registerPoolCleanup(f func()) {
	poolcleanup = f
}

func clearpools() {
	// clear sync.Pools
	if poolcleanup != nil {
		poolcleanup()
	}

	// Clear central sudog cache.
	// Leave per-P caches alone, they have strictly bounded size.
	// Disconnect cached list before dropping it on the floor,
	// so that a dangling ref to one entry does not pin all of them.
	lock(&sched.sudoglock)
	var sg, sgnext *sudog
	for sg = sched.sudogcache; sg != nil; sg = sgnext {
		sgnext = sg.next
		sg.next = nil
	}
	sched.sudogcache = nil
	unlock(&sched.sudoglock)

	// Clear central defer pools.
	// Leave per-P pools alone, they have strictly bounded size.
	lock(&sched.deferlock)
	for i := range sched.deferpool {
		// disconnect cached list before dropping it on the floor,
		// so that a dangling ref to one entry does not pin all of them.
		var d, dlink *_defer
		for d = sched.deferpool[i]; d != nil; d = dlink {
			dlink = d.link
			d.link = nil
		}
		sched.deferpool[i] = nil
	}
	unlock(&sched.deferlock)
}

// Timing

// itoaDiv formats val/(10**dec) into buf.
func itoaDiv(buf []byte, val uint64, dec int) []byte {
	i := len(buf) - 1
	idec := i - dec
	for val >= 10 || i >= idec {
		buf[i] = byte(val%10 + '0')
		i--
		if i == idec {
			buf[i] = '.'
			i--
		}
		val /= 10
	}
	buf[i] = byte(val + '0')
	return buf[i:]
}

// fmtNSAsMS nicely formats ns nanoseconds as milliseconds.
func fmtNSAsMS(buf []byte, ns uint64) []byte {
	if ns >= 10e6 {
		// Format as whole milliseconds.
		return itoaDiv(buf, ns/1e6, 0)
	}
	// Format two digits of precision, with at most three decimal places.
	x := ns / 1e3
	if x == 0 {
		buf[0] = '0'
		return buf[:1]
	}
	dec := 3
	for x >= 100 {
		x /= 10
		dec--
	}
	return itoaDiv(buf, x, dec)
}
