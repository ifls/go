// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Garbage collector: marking and scanning

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

const (
	fixedRootFinalizers = iota
	fixedRootFreeGStacks
	fixedRootCount

	// rootBlockBytes is the number of bytes to scan per data or
	// BSS root.
	rootBlockBytes = 256 << 10

	// maxObletBytes is the maximum bytes of an object to scan at
	// once. Larger objects will be split up into "oblets" of at
	// most this size. Since we can scan 1–2 MB/ms, 128 KB bounds
	// scan preemption at ~100 µs.
	//
	// This must be > _MaxSmallSize so that the object base is the
	// span base.
	maxObletBytes = 128 << 10

	// drainCheckThreshold specifies how many units of work to do
	// between self-preemption checks in gcDrain. Assuming a scan
	// rate of 1 MB/ms, this is ~100 µs. Lower values have higher
	// overhead in the scan loop (the scheduler check may perform
	// a syscall, so its overhead is nontrivial). Higher values
	// make the system less responsive to incoming work.
	drainCheckThreshold = 100000

	// pagesPerSpanRoot indicates how many pages to scan from a span root
	// at a time. Used by special root marking.
	//
	// Higher values improve throughput by increasing locality, but
	// increase the minimum latency of a marking operation.
	//
	// Must be a multiple of the pageInUse bitmap element size and
	// must also evenly divide pagesPerArena.
	pagesPerSpanRoot = 512

	// go115NewMarkrootSpans is a feature flag that indicates whether
	// to use the new bitmap-based markrootSpans implementation.
	go115NewMarkrootSpans = true
)

// gcMarkRootPrepare queues root scanning jobs (stacks, globals, and
// some miscellany) and initializes scanning-related state.
//
// The world must be stopped.
func gcMarkRootPrepare() {
	work.nFlushCacheRoots = 0

	// Compute how many data and BSS root blocks there are.
	nBlocks := func(bytes uintptr) int {
		return int(divRoundUp(bytes, rootBlockBytes))
	}

	work.nDataRoots = 0
	work.nBSSRoots = 0

	// Scan globals.
	for _, datap := range activeModules() {
		nDataRoots := nBlocks(datap.edata - datap.data)
		if nDataRoots > work.nDataRoots {
			work.nDataRoots = nDataRoots
		}
	}

	for _, datap := range activeModules() {
		nBSSRoots := nBlocks(datap.ebss - datap.bss)
		if nBSSRoots > work.nBSSRoots {
			work.nBSSRoots = nBSSRoots
		}
	}

	// Scan span roots for finalizer specials.
	//
	// We depend on addfinalizer to mark objects that get
	// finalizers after root marking.
	if go115NewMarkrootSpans {
		// We're going to scan the whole heap (that was available at the time the
		// mark phase started, i.e. markArenas) for in-use spans which have specials.
		//
		// Break up the work into arenas, and further into chunks.
		//
		// Snapshot allArenas as markArenas. This snapshot is safe because allArenas
		// is append-only.
		mheap_.markArenas = mheap_.allArenas[:len(mheap_.allArenas):len(mheap_.allArenas)]
		work.nSpanRoots = len(mheap_.markArenas) * (pagesPerArena / pagesPerSpanRoot)
	} else {
		// We're only interested in scanning the in-use spans,
		// which will all be swept at this point. More spans
		// may be added to this list during concurrent GC, but
		// we only care about spans that were allocated before
		// this mark phase.
		work.nSpanRoots = mheap_.sweepSpans[mheap_.sweepgen/2%2].numBlocks()
	}

	// Scan stacks.
	//
	// Gs may be created after this point, but it's okay that we
	// ignore them because they begin life without any roots, so
	// there's nothing to scan, and any roots they create during
	// the concurrent phase will be scanned during mark
	// termination.
	work.nStackRoots = int(atomic.Loaduintptr(&allglen))

	work.markrootNext = 0
	work.markrootJobs = uint32(fixedRootCount + work.nFlushCacheRoots + work.nDataRoots + work.nBSSRoots + work.nSpanRoots + work.nStackRoots)
}

// gcMarkRootCheck checks that all roots have been scanned. It is
// purely for debugging.
func gcMarkRootCheck() {
	if work.markrootNext < work.markrootJobs {
		print(work.markrootNext, " of ", work.markrootJobs, " markroot jobs done\n")
		throw("left over markroot jobs")
	}

	lock(&allglock)
	// Check that stacks have been scanned.
	var gp *g
	for i := 0; i < work.nStackRoots; i++ {
		gp = allgs[i]
		if !gp.gcscandone {
			goto fail
		}
	}
	unlock(&allglock)
	return

fail:
	println("gp", gp, "goid", gp.goid,
		"status", readgstatus(gp),
		"gcscandone", gp.gcscandone)
	unlock(&allglock) // Avoid self-deadlock with traceback.
	throw("scan missed a g")
}

// ptrmask for an allocation containing a single pointer.
var oneptrmask = [...]uint8{1}

// markroot scans the i'th root.
//
// Preemption must be disabled (because this uses a gcWork).
//
// nowritebarrier is only advisory here.
//
//go:nowritebarrier
func markroot(gcw *gcWork, i uint32) {
	// TODO(austin): This is a bit ridiculous. Compute and store
	// the bases in gcMarkRootPrepare instead of the counts.
	baseFlushCache := uint32(fixedRootCount)
	baseData := baseFlushCache + uint32(work.nFlushCacheRoots)
	baseBSS := baseData + uint32(work.nDataRoots)
	baseSpans := baseBSS + uint32(work.nBSSRoots)
	baseStacks := baseSpans + uint32(work.nSpanRoots)
	end := baseStacks + uint32(work.nStackRoots)

	// Note: if you add a case here, please also update heapdump.go:dumproots.
	switch {
	case baseFlushCache <= i && i < baseData:
		flushmcache(int(i - baseFlushCache))

	case baseData <= i && i < baseBSS:
		for _, datap := range activeModules() {
			markrootBlock(datap.data, datap.edata-datap.data, datap.gcdatamask.bytedata, gcw, int(i-baseData))
		}

	case baseBSS <= i && i < baseSpans:
		for _, datap := range activeModules() {
			markrootBlock(datap.bss, datap.ebss-datap.bss, datap.gcbssmask.bytedata, gcw, int(i-baseBSS))
		}

	case i == fixedRootFinalizers:
		for fb := allfin; fb != nil; fb = fb.alllink {
			cnt := uintptr(atomic.Load(&fb.cnt))
			scanblock(uintptr(unsafe.Pointer(&fb.fin[0])), cnt*unsafe.Sizeof(fb.fin[0]), &finptrmask[0], gcw, nil)
		}

	case i == fixedRootFreeGStacks:
		// Switch to the system stack so we can call
		// stackfree.
		systemstack(markrootFreeGStacks)

	case baseSpans <= i && i < baseStacks:
		// mark mspan.specials
		markrootSpans(gcw, int(i-baseSpans))

	default:
		// the rest is scanning goroutine stacks
		var gp *g
		if baseStacks <= i && i < end {
			gp = allgs[i-baseStacks]
		} else {
			throw("markroot: bad index")
		}

		// remember when we've first observed the G blocked
		// needed only to output in traceback
		status := readgstatus(gp) // We are not in a scan state
		if (status == _Gwaiting || status == _Gsyscall) && gp.waitsince == 0 {
			gp.waitsince = work.tstart
		}

		// scanstack must be done on the system stack in case
		// we're trying to scan our own stack.
		systemstack(func() {
			// If this is a self-scan, put the user G in
			// _Gwaiting to prevent self-deadlock. It may
			// already be in _Gwaiting if this is a mark
			// worker or we're in mark termination.
			userG := getg().m.curg
			selfScan := gp == userG && readgstatus(userG) == _Grunning
			if selfScan {
				//自我扫描，进入等待状态
				casgstatus(userG, _Grunning, _Gwaiting)
				userG.waitreason = waitReasonGarbageCollectionScan
			}

			// TODO: suspendG blocks (and spins) until gp
			// stops, which may take a while for
			// running goroutines. Consider doing this in
			// two phases where the first is non-blocking:
			// we scan the stacks we can and ask running
			// goroutines to scan themselves; and the
			// second blocks.
			stopped := suspendG(gp)
			if stopped.dead {
				gp.gcscandone = true
				return
			}
			if gp.gcscandone {
				throw("g already scanned")
			}
			scanstack(gp, gcw)
			gp.gcscandone = true
			resumeG(stopped)

			if selfScan {
				//扫描完成恢复运行状态
				casgstatus(userG, _Gwaiting, _Grunning)
			}
		})
	}
}

// markrootBlock scans the shard'th shard of the block of memory [b0,
// b0+n0), with the given pointer mask.
//
//go:nowritebarrier
func markrootBlock(b0, n0 uintptr, ptrmask0 *uint8, gcw *gcWork, shard int) {
	if rootBlockBytes%(8*sys.PtrSize) != 0 {
		// This is necessary to pick byte offsets in ptrmask0.
		throw("rootBlockBytes must be a multiple of 8*ptrSize")
	}

	// Note that if b0 is toward the end of the address space,
	// then b0 + rootBlockBytes might wrap around.
	// These tests are written to avoid any possible overflow.
	off := uintptr(shard) * rootBlockBytes
	if off >= n0 {
		return
	}
	b := b0 + off
	ptrmask := (*uint8)(add(unsafe.Pointer(ptrmask0), uintptr(shard)*(rootBlockBytes/(8*sys.PtrSize))))
	n := uintptr(rootBlockBytes)
	if off+n > n0 {
		n = n0 - off
	}

	// Scan this shard.
	scanblock(b, n, ptrmask, gcw, nil)
}

// markrootFreeGStacks frees stacks of dead Gs.
// 释放deadG的栈空间
// This does not free stacks of dead Gs cached on Ps, but having a few
// cached stacks around isn't a problem.
func markrootFreeGStacks() {
	// Take list of dead Gs with stacks.
	lock(&sched.gFree.lock)
	list := sched.gFree.stack
	sched.gFree.stack = gList{}
	unlock(&sched.gFree.lock)
	if list.empty() {
		return
	}

	// Free stacks.
	q := gQueue{list.head, list.head}
	for gp := list.head.ptr(); gp != nil; gp = gp.schedlink.ptr() {
		//g is dead 释放栈空间
		stackfree(gp.stack)
		gp.stack.lo = 0
		gp.stack.hi = 0
		// Manipulate the queue directly since the Gs are
		// already all linked the right way.
		q.tail.set(gp)
	}

	// Put Gs back on the free list.
	lock(&sched.gFree.lock)
	sched.gFree.noStack.pushAll(q)
	unlock(&sched.gFree.lock)
}

// markrootSpans marks roots for one shard of markArenas.
//
//go:nowritebarrier
func markrootSpans(gcw *gcWork, shard int) {
	if !go115NewMarkrootSpans {
		oldMarkrootSpans(gcw, shard)
		return
	}
	// Objects with finalizers have two GC-related invariants:
	//
	// 1) Everything reachable from the object must be marked.
	// This ensures that when we pass the object to its finalizer,
	// everything the finalizer can reach will be retained.
	//
	// 2) Finalizer specials (which are not in the garbage
	// collected heap) are roots. In practice, this means the fn
	// field must be scanned.
	sg := mheap_.sweepgen

	// Find the arena and page index into that arena for this shard.
	ai := mheap_.markArenas[shard/(pagesPerArena/pagesPerSpanRoot)]
	ha := mheap_.arenas[ai.l1()][ai.l2()]
	arenaPage := uint(uintptr(shard) * pagesPerSpanRoot % pagesPerArena)

	// Construct slice of bitmap which we'll iterate over.
	specialsbits := ha.pageSpecials[arenaPage/8:]
	specialsbits = specialsbits[:pagesPerSpanRoot/8]
	for i := range specialsbits {
		// Find set bits, which correspond to spans with specials.
		specials := atomic.Load8(&specialsbits[i])
		if specials == 0 {
			continue
		}
		for j := uint(0); j < 8; j++ {
			if specials&(1<<j) == 0 {
				continue
			}
			// Find the span for this bit.
			//
			// This value is guaranteed to be non-nil because having
			// specials implies that the span is in-use, and since we're
			// currently marking we can be sure that we don't have to worry
			// about the span being freed and re-used.
			s := ha.spans[arenaPage+uint(i)*8+j]

			// The state must be mSpanInUse if the specials bit is set, so
			// sanity check that.
			if state := s.state.get(); state != mSpanInUse {
				print("s.state = ", state, "\n")
				throw("non in-use span found with specials bit set")
			}
			// Check that this span was swept (it may be cached or uncached).
			if !useCheckmark && !(s.sweepgen == sg || s.sweepgen == sg+3) {
				// sweepgen was updated (+2) during non-checkmark GC pass
				print("sweep ", s.sweepgen, " ", sg, "\n")
				throw("gc: unswept span")
			}

			// Lock the specials to prevent a special from being
			// removed from the list while we're traversing it.
			lock(&s.speciallock)
			for sp := s.specials; sp != nil; sp = sp.next {
				if sp.kind != _KindSpecialFinalizer {
					continue
				}
				// don't mark finalized object, but scan it so we
				// retain everything it points to.
				spf := (*specialfinalizer)(unsafe.Pointer(sp))
				// A finalizer can be set for an inner byte of an object, find object beginning.
				p := s.base() + uintptr(spf.special.offset)/s.elemsize*s.elemsize

				// Mark everything that can be reached from
				// the object (but *not* the object itself or
				// we'll never collect it).
				scanobject(p, gcw)

				// The special itself is a root.
				scanblock(uintptr(unsafe.Pointer(&spf.fn)), sys.PtrSize, &oneptrmask[0], gcw, nil)
			}
			unlock(&s.speciallock)
		}
	}
}

// oldMarkrootSpans marks roots for one shard of work.spans.
//
// For go115NewMarkrootSpans = false.
//
//go:nowritebarrier
func oldMarkrootSpans(gcw *gcWork, shard int) {
	// Objects with finalizers have two GC-related invariants:
	//
	// 1) Everything reachable from the object must be marked.
	// This ensures that when we pass the object to its finalizer,
	// everything the finalizer can reach will be retained.
	//
	// 2) Finalizer specials (which are not in the garbage
	// collected heap) are roots. In practice, this means the fn
	// field must be scanned.
	//
	// TODO(austin): There are several ideas for making this more
	// efficient in issue #11485.

	sg := mheap_.sweepgen
	spans := mheap_.sweepSpans[mheap_.sweepgen/2%2].block(shard)
	// Note that work.spans may not include spans that were
	// allocated between entering the scan phase and now. We may
	// also race with spans being added into sweepSpans when they're
	// just created, and as a result we may see nil pointers in the
	// spans slice. This is okay because any objects with finalizers
	// in those spans must have been allocated and given finalizers
	// after we entered the scan phase, so addfinalizer will have
	// ensured the above invariants for them.
	for i := 0; i < len(spans); i++ {
		// sweepBuf.block requires that we read pointers from the block atomically.
		// It also requires that we ignore nil pointers.
		s := (*mspan)(atomic.Loadp(unsafe.Pointer(&spans[i])))

		// This is racing with spans being initialized, so
		// check the state carefully.
		if s == nil || s.state.get() != mSpanInUse {
			continue
		}
		// Check that this span was swept (it may be cached or uncached).
		if !useCheckmark && !(s.sweepgen == sg || s.sweepgen == sg+3) {
			// sweepgen was updated (+2) during non-checkmark GC pass
			print("sweep ", s.sweepgen, " ", sg, "\n")
			throw("gc: unswept span")
		}

		// Speculatively check if there are any specials
		// without acquiring the span lock. This may race with
		// adding the first special to a span, but in that
		// case addfinalizer will observe that the GC is
		// active (which is globally synchronized) and ensure
		// the above invariants. We may also ensure the
		// invariants, but it's okay to scan an object twice.
		if s.specials == nil {
			continue
		}

		// Lock the specials to prevent a special from being
		// removed from the list while we're traversing it.
		lock(&s.speciallock)

		for sp := s.specials; sp != nil; sp = sp.next {
			if sp.kind != _KindSpecialFinalizer {
				continue
			}
			// don't mark finalized object, but scan it so we
			// retain everything it points to.
			spf := (*specialfinalizer)(unsafe.Pointer(sp))
			// A finalizer can be set for an inner byte of an object, find object beginning.
			p := s.base() + uintptr(spf.special.offset)/s.elemsize*s.elemsize

			// Mark everything that can be reached from
			// the object (but *not* the object itself or
			// we'll never collect it).
			scanobject(p, gcw)

			// The special itself is a root.
			scanblock(uintptr(unsafe.Pointer(&spf.fn)), sys.PtrSize, &oneptrmask[0], gcw, nil)
		}

		unlock(&s.speciallock)
	}
}

// gcAssistAlloc performs GC work to make gp's assist debt positive.
// gp must be the calling user gorountine.
//
// This must be called with preemption enabled.
func gcAssistAlloc(gp *g) {
	// Don't assist in non-preemptible contexts. These are
	// generally fragile and won't allow the assist to block.
	if getg() == gp.m.g0 {
		return
	}
	if mp := getg().m; mp.locks > 0 || mp.preemptoff != "" {
		return
	}

	traced := false
retry:
	// Compute the amount of scan work we need to do to make the
	// balance positive. When the required amount of work is low,
	// we over-assist to build up credit for future allocations
	// and amortize the cost of assisting.
	debtBytes := -gp.gcAssistBytes
	scanWork := int64(gcController.assistWorkPerByte * float64(debtBytes))
	if scanWork < gcOverAssistWork {
		scanWork = gcOverAssistWork
		debtBytes = int64(gcController.assistBytesPerWork * float64(scanWork))
	}

	// Steal as much credit as we can from the background GC's
	// scan credit. This is racy and may drop the background
	// credit below 0 if two mutators steal at the same time. This
	// will just cause steals to fail until credit is accumulated
	// again, so in the long run it doesn't really matter, but we
	// do have to handle the negative credit case.
	bgScanCredit := atomic.Loadint64(&gcController.bgScanCredit)
	stolen := int64(0)
	if bgScanCredit > 0 {
		if bgScanCredit < scanWork {
			stolen = bgScanCredit
			gp.gcAssistBytes += 1 + int64(gcController.assistBytesPerWork*float64(stolen))
		} else {
			stolen = scanWork
			gp.gcAssistBytes += debtBytes
		}
		atomic.Xaddint64(&gcController.bgScanCredit, -stolen)

		scanWork -= stolen

		if scanWork == 0 {
			// We were able to steal all of the credit we
			// needed.
			if traced {
				traceGCMarkAssistDone()
			}
			return
		}
	}

	if trace.enabled && !traced {
		traced = true
		traceGCMarkAssistStart()
	}

	// Perform assist work
	systemstack(func() {
		gcAssistAlloc1(gp, scanWork)
		// The user stack may have moved, so this can't touch
		// anything on it until it returns from systemstack.
	})

	completed := gp.param != nil
	gp.param = nil
	if completed {
		gcMarkDone()
	}

	if gp.gcAssistBytes < 0 {
		// We were unable steal enough credit or perform
		// enough work to pay off the assist debt. We need to
		// do one of these before letting the mutator allocate
		// more to prevent over-allocation.
		//
		// If this is because we were preempted, reschedule
		// and try some more.
		if gp.preempt {
			Gosched()
			goto retry
		}

		// Add this G to an assist queue and park. When the GC
		// has more background credit, it will satisfy queued
		// assists before flushing to the global credit pool.
		//
		// Note that this does *not* get woken up when more
		// work is added to the work list. The theory is that
		// there wasn't enough work to do anyway, so we might
		// as well let background marking take care of the
		// work that is available.
		if !gcParkAssist() {
			goto retry
		}

		// At this point either background GC has satisfied
		// this G's assist debt, or the GC cycle is over.
	}
	if traced {
		traceGCMarkAssistDone()
	}
}

// gcAssistAlloc1 is the part of一部分 gcAssistAlloc that runs on the system
// stack. This is a separate单独地 function to make it easier to see that
// we're not capturing anything from the user stack, since the user
// stack用户栈 may move会被移动 while we're in this function.
//
// gcAssistAlloc1 indicates whether this assist completed the mark
// phase by setting gp.param to non-nil. This can't be communicated on
// the stack since it may move.
//
//go:systemstack
func gcAssistAlloc1(gp *g, scanWork int64) {
	// Clear the flag indicating that this assist completed the
	// mark phase.
	gp.param = nil

	if atomic.Load(&gcBlackenEnabled) == 0 {
		// The gcBlackenEnabled check in malloc races with the
		// store that clears it but an atomic check in every malloc
		// would be a performance hit.
		// Instead we recheck it here on the non-preemptable system
		// stack to determine if we should perform an assist.

		// GC is done, so ignore any remaining debt.
		gp.gcAssistBytes = 0
		return
	}
	// Track time spent in this assist. Since we're on the
	// system stack, this is non-preemptible, so we can
	// just measure start and end time.
	startTime := nanotime()

	decnwait := atomic.Xadd(&work.nwait, -1)
	if decnwait == work.nproc {
		println("runtime: work.nwait =", decnwait, "work.nproc=", work.nproc)
		throw("nwait > work.nprocs")
	}

	// gcDrainN requires the caller to be preemptible.
	// 进入等待状态执行gcBrainN
	casgstatus(gp, _Grunning, _Gwaiting)
	gp.waitreason = waitReasonGCAssistMarking

	// drain own cached work first in the hopes that it
	// will be more cache friendly.
	gcw := &getg().m.p.ptr().gcw
	workDone := gcDrainN(gcw, scanWork)
	//执行完退出
	casgstatus(gp, _Gwaiting, _Grunning)

	// Record that we did this much scan work.
	//
	// Back out the number of bytes of assist credit that
	// this scan work counts for. The "1+" is a poor man's
	// round-up, to ensure this adds credit even if
	// assistBytesPerWork is very low.
	gp.gcAssistBytes += 1 + int64(gcController.assistBytesPerWork*float64(workDone))

	// If this is the last worker and we ran out of work,
	// signal a completion point.
	incnwait := atomic.Xadd(&work.nwait, +1)
	if incnwait > work.nproc {
		println("runtime: work.nwait=", incnwait,
			"work.nproc=", work.nproc)
		throw("work.nwait > work.nproc")
	}

	if incnwait == work.nproc && !gcMarkWorkAvailable(nil) {
		// This has reached a background completion point. Set
		// gp.param to a non-nil value to indicate this. It
		// doesn't matter what we set it to (it just has to be
		// a valid pointer).
		gp.param = unsafe.Pointer(gp)
	}
	duration := nanotime() - startTime
	_p_ := gp.m.p.ptr()
	_p_.gcAssistTime += duration
	if _p_.gcAssistTime > gcAssistTimeSlack {
		atomic.Xaddint64(&gcController.assistTime, _p_.gcAssistTime)
		_p_.gcAssistTime = 0
	}
}

// gcWakeAllAssists wakes all currently blocked assists. This is used
// at the end of a GC cycle. gcBlackenEnabled must be false to prevent
// new assists from going to sleep after this point.
func gcWakeAllAssists() {
	lock(&work.assistQueue.lock)
	list := work.assistQueue.q.popList()
	injectglist(&list)
	unlock(&work.assistQueue.lock)
}

// gcParkAssist puts the current goroutine on the assist queue and parks.
//
// gcParkAssist reports whether the assist is now satisfied. If it
// returns false, the caller must retry the assist.
//
//go:nowritebarrier
func gcParkAssist() bool {
	lock(&work.assistQueue.lock)
	// If the GC cycle finished while we were getting the lock,
	// exit the assist. The cycle can't finish while we hold the
	// lock.
	if atomic.Load(&gcBlackenEnabled) == 0 {
		unlock(&work.assistQueue.lock)
		return true
	}

	gp := getg()
	oldList := work.assistQueue.q
	work.assistQueue.q.pushBack(gp)

	// Recheck for background credit now that this G is in
	// the queue, but can still back out. This avoids a
	// race in case background marking has flushed more
	// credit since we checked above.
	if atomic.Loadint64(&gcController.bgScanCredit) > 0 {
		work.assistQueue.q = oldList
		if oldList.tail != 0 {
			oldList.tail.ptr().schedlink.set(nil)
		}
		unlock(&work.assistQueue.lock)
		return false
	}
	// Park.
	goparkunlock(&work.assistQueue.lock, waitReasonGCAssistWait, traceEvGoBlockGC, 2)
	return true
}

// gcFlushBgCredit flushes scanWork units of background scan work
// credit. This first satisfies blocked assists on the
// work.assistQueue and then flushes any remaining credit to
// gcController.bgScanCredit.
//
// Write barriers are disallowed because this is used by gcDrain after
// it has ensured that all work is drained and this must preserve that
// condition.
//
//go:nowritebarrierrec
func gcFlushBgCredit(scanWork int64) {
	if work.assistQueue.q.empty() {
		// Fast path; there are no blocked assists. There's a
		// small window here where an assist may add itself to
		// the blocked queue and park. If that happens, we'll
		// just get it on the next flush.
		atomic.Xaddint64(&gcController.bgScanCredit, scanWork)
		return
	}

	scanBytes := int64(float64(scanWork) * gcController.assistBytesPerWork)

	lock(&work.assistQueue.lock)
	for !work.assistQueue.q.empty() && scanBytes > 0 {
		gp := work.assistQueue.q.pop()
		// Note that gp.gcAssistBytes is negative because gp
		// is in debt. Think carefully about the signs below.
		if scanBytes+gp.gcAssistBytes >= 0 {
			// Satisfy this entire assist debt.
			scanBytes += gp.gcAssistBytes
			gp.gcAssistBytes = 0
			// It's important that we *not* put gp in
			// runnext. Otherwise, it's possible for user
			// code to exploit the GC worker's high
			// scheduler priority to get itself always run
			// before other goroutines and always in the
			// fresh quantum started by GC.
			ready(gp, 0, false)
		} else {
			// Partially satisfy this assist.
			gp.gcAssistBytes += scanBytes
			scanBytes = 0
			// As a heuristic, we move this assist to the
			// back of the queue so that large assists
			// can't clog up the assist queue and
			// substantially delay small assists.
			work.assistQueue.q.pushBack(gp)
			break
		}
	}

	if scanBytes > 0 {
		// Convert from scan bytes back to work.
		scanWork = int64(float64(scanBytes) * gcController.assistWorkPerByte)
		atomic.Xaddint64(&gcController.bgScanCredit, scanWork)
	}
	unlock(&work.assistQueue.lock)
}

// scanstack scans gp's stack, greying all pointers found on the stack.
//
// scanstack will also shrink the stack if it is safe to do so. If it
// is not, it schedules a stack shrink for the next synchronous safe
// point.
//
// scanstack is marked go:systemstack because it must not be preempted
// while using a workbuf.
// 扫描，判断栈是否要收缩
//go:nowritebarrier
//go:systemstack
func scanstack(gp *g, gcw *gcWork) {
	if readgstatus(gp)&_Gscan == 0 {
		print("runtime:scanstack: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", hex(readgstatus(gp)), "\n")
		throw("scanstack - bad status")
	}

	switch readgstatus(gp) &^ _Gscan {
	default:
		print("runtime: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
		throw("mark - bad status")
	case _Gdead:
		return
	case _Grunning:
		print("runtime: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
		throw("scanstack: goroutine not stopped")
	case _Grunnable, _Gsyscall, _Gwaiting:
		// ok
	}

	if gp == getg() {
		throw("can't scan our own stack")
	}

	if isShrinkStackSafe(gp) {
		// Shrink the stack if not much of it is being used.
		shrinkstack(gp)
	} else {
		// Otherwise, shrink the stack at the next sync safe point.
		gp.preemptShrink = true
	}

	var state stackScanState
	state.stack = gp.stack

	if stackTraceDebug {
		println("stack trace goroutine", gp.goid)
	}

	if debugScanConservative && gp.asyncSafePoint {
		print("scanning async preempted goroutine ", gp.goid, " stack [", hex(gp.stack.lo), ",", hex(gp.stack.hi), ")\n")
	}

	// Scan the saved context register. This is effectively a live
	// register that gets moved back and forth between the
	// register and sched.ctxt without a write barrier.
	if gp.sched.ctxt != nil {
		scanblock(uintptr(unsafe.Pointer(&gp.sched.ctxt)), sys.PtrSize, &oneptrmask[0], gcw, &state)
	}

	// Scan the stack. Accumulate a list of stack objects.
	scanframe := func(frame *stkframe, unused unsafe.Pointer) bool {
		scanframeworker(frame, &state, gcw)
		return true
	}
	gentraceback(^uintptr(0), ^uintptr(0), 0, gp, 0, nil, 0x7fffffff, scanframe, nil, 0)

	// Find additional pointers that point into the stack from the heap.
	// Currently this includes defers and panics. See also function copystack.

	// Find and trace all defer arguments.
	tracebackdefers(gp, scanframe, nil)

	// Find and trace other pointers in defer records.
	for d := gp._defer; d != nil; d = d.link {
		if d.fn != nil {
			// tracebackdefers above does not scan the func value, which could
			// be a stack allocated closure. See issue 30453.
			scanblock(uintptr(unsafe.Pointer(&d.fn)), sys.PtrSize, &oneptrmask[0], gcw, &state)
		}
		if d.link != nil {
			// The link field of a stack-allocated defer record might point
			// to a heap-allocated defer record. Keep that heap record live.
			scanblock(uintptr(unsafe.Pointer(&d.link)), sys.PtrSize, &oneptrmask[0], gcw, &state)
		}
		// Retain defers records themselves.
		// Defer records might not be reachable from the G through regular heap
		// tracing because the defer linked list might weave between the stack and the heap.
		if d.heap {
			scanblock(uintptr(unsafe.Pointer(&d)), sys.PtrSize, &oneptrmask[0], gcw, &state)
		}
	}
	if gp._panic != nil {
		// Panics are always stack allocated.
		state.putPtr(uintptr(unsafe.Pointer(gp._panic)), false)
	}

	// Find and scan all reachable stack objects.
	//
	// The state's pointer queue prioritizes precise pointers over
	// conservative pointers so that we'll prefer scanning stack
	// objects precisely.
	state.buildIndex()
	for {
		p, conservative := state.getPtr()
		if p == 0 {
			break
		}
		obj := state.findObject(p)
		if obj == nil {
			continue
		}
		t := obj.typ
		if t == nil {
			// We've already scanned this object.
			continue
		}
		obj.setType(nil) // Don't scan it again.
		if stackTraceDebug {
			printlock()
			print("  live stkobj at", hex(state.stack.lo+uintptr(obj.off)), "of type", t.string())
			if conservative {
				print(" (conservative)")
			}
			println()
			printunlock()
		}
		gcdata := t.gcdata
		var s *mspan
		if t.kind&kindGCProg != 0 {
			// This path is pretty unlikely, an object large enough
			// to have a GC program allocated on the stack.
			// We need some space to unpack the program into a straight
			// bitmask, which we allocate/free here.
			// TODO: it would be nice if there were a way to run a GC
			// program without having to store all its bits. We'd have
			// to change from a Lempel-Ziv style program to something else.
			// Or we can forbid putting objects on stacks if they require
			// a gc program (see issue 27447).
			s = materializeGCProg(t.ptrdata, gcdata)
			gcdata = (*byte)(unsafe.Pointer(s.startAddr))
		}

		b := state.stack.lo + uintptr(obj.off)
		if conservative {
			scanConservative(b, t.ptrdata, gcdata, gcw, &state)
		} else {
			scanblock(b, t.ptrdata, gcdata, gcw, &state)
		}

		if s != nil {
			dematerializeGCProg(s)
		}
	}

	// Deallocate object buffers.
	// (Pointer buffers were all deallocated in the loop above.)
	for state.head != nil {
		x := state.head
		state.head = x.next
		if stackTraceDebug {
			for _, obj := range x.obj[:x.nobj] {
				if obj.typ == nil { // reachable
					continue
				}
				println("  dead stkobj at", hex(gp.stack.lo+uintptr(obj.off)), "of type", obj.typ.string())
				// Note: not necessarily really dead - only reachable-from-ptr dead.
			}
		}
		x.nobj = 0
		putempty((*workbuf)(unsafe.Pointer(x)))
	}
	if state.buf != nil || state.cbuf != nil || state.freeBuf != nil {
		throw("remaining pointer buffers")
	}
}

// Scan a stack frame: local variables and function arguments/results.
//go:nowritebarrier
func scanframeworker(frame *stkframe, state *stackScanState, gcw *gcWork) {
	if _DebugGC > 1 && frame.continpc != 0 {
		print("scanframe ", funcname(frame.fn), "\n")
	}

	isAsyncPreempt := frame.fn.valid() && frame.fn.funcID == funcID_asyncPreempt
	isDebugCall := frame.fn.valid() && frame.fn.funcID == funcID_debugCallV1
	if state.conservative || isAsyncPreempt || isDebugCall {
		if debugScanConservative {
			println("conservatively scanning function", funcname(frame.fn), "at PC", hex(frame.continpc))
		}

		// Conservatively scan the frame. Unlike the precise
		// case, this includes the outgoing argument space
		// since we may have stopped while this function was
		// setting up a call.
		//
		// TODO: We could narrow this down if the compiler
		// produced a single map per function of stack slots
		// and registers that ever contain a pointer.
		if frame.varp != 0 {
			size := frame.varp - frame.sp
			if size > 0 {
				scanConservative(frame.sp, size, nil, gcw, state)
			}
		}

		// Scan arguments to this frame.
		if frame.arglen != 0 {
			// TODO: We could pass the entry argument map
			// to narrow this down further.
			scanConservative(frame.argp, frame.arglen, nil, gcw, state)
		}

		if isAsyncPreempt || isDebugCall {
			// This function's frame contained the
			// registers for the asynchronously stopped
			// parent frame. Scan the parent
			// conservatively.
			state.conservative = true
		} else {
			// We only wanted to scan those two frames
			// conservatively. Clear the flag for future
			// frames.
			state.conservative = false
		}
		return
	}

	locals, args, objs := getStackMap(frame, &state.cache, false)

	// Scan local variables if stack frame has been allocated.
	if locals.n > 0 {
		size := uintptr(locals.n) * sys.PtrSize
		scanblock(frame.varp-size, size, locals.bytedata, gcw, state)
	}

	// Scan arguments.
	if args.n > 0 {
		scanblock(frame.argp, uintptr(args.n)*sys.PtrSize, args.bytedata, gcw, state)
	}

	// Add all stack objects to the stack object list.
	if frame.varp != 0 {
		// varp is 0 for defers, where there are no locals.
		// In that case, there can't be a pointer to its args, either.
		// (And all args would be scanned above anyway.)
		for _, obj := range objs {
			off := obj.off
			base := frame.varp // locals base pointer
			if off >= 0 {
				base = frame.argp // arguments and return values base pointer
			}
			ptr := base + uintptr(off)
			if ptr < frame.sp {
				// object hasn't been allocated in the frame yet.
				continue
			}
			if stackTraceDebug {
				println("stkobj at", hex(ptr), "of type", obj.typ.string())
			}
			state.addObject(ptr, obj.typ)
		}
	}
}

type gcDrainFlags int

const (
	gcDrainUntilPreempt gcDrainFlags = 1 << iota
	gcDrainFlushBgCredit
	gcDrainIdle
	gcDrainFractional
)

// gcDrain scans roots and objects in work buffers, blackening grey
// objects until it is unable to get more work. It may return before
// GC is done; it's the caller's responsibility to balance work from
// other Ps.
//
// If flags&gcDrainUntilPreempt != 0, gcDrain returns when g.preempt
// is set.
//
// If flags&gcDrainIdle != 0, gcDrain returns when there is other work
// to do.
//
// If flags&gcDrainFractional != 0, gcDrain self-preempts when
// pollFractionalWorkerExit() returns true. This implies
// gcDrainNoBlock.
//
// If flags&gcDrainFlushBgCredit != 0, gcDrain flushes scan work
// credit to gcController.bgScanCredit every gcCreditSlack units of
// scan work.
// 扫描跟对象，黑化灰色对象，直到没有
// gcDrain will always return if there is a pending STW.
//
//go:nowritebarrier
func gcDrain(gcw *gcWork, flags gcDrainFlags) {
	if !writeBarrier.needed {
		throw("gcDrain phase incorrect")
	}

	gp := getg().m.curg
	preemptible := flags&gcDrainUntilPreempt != 0
	flushBgCredit := flags&gcDrainFlushBgCredit != 0
	idle := flags&gcDrainIdle != 0

	initScanWork := gcw.scanWork

	// checkWork is the scan work before performing the next
	// self-preempt check.
	checkWork := int64(1<<63 - 1)
	var check func() bool
	if flags&(gcDrainIdle|gcDrainFractional) != 0 {
		checkWork = initScanWork + drainCheckThreshold
		if idle {
			check = pollWork
		} else if flags&gcDrainFractional != 0 {
			check = pollFractionalWorkerExit
		}
	}

	// Drain root marking jobs.
	if work.markrootNext < work.markrootJobs {
		// Stop if we're preemptible or if someone wants to STW.
		for !(gp.preempt && (preemptible || atomic.Load(&sched.gcwaiting) != 0)) {
			job := atomic.Xadd(&work.markrootNext, +1) - 1
			if job >= work.markrootJobs {
				break
			}
			markroot(gcw, job)
			if check != nil && check() {
				goto done
			}
		}
	}

	// Drain heap marking jobs.
	// Stop if we're preemptible or if someone wants to STW.
	for !(gp.preempt && (preemptible || atomic.Load(&sched.gcwaiting) != 0)) {
		// Try to keep work available on the global queue. We used to
		// check if there were waiting workers, but it's better to
		// just keep work available than to make workers wait. In the
		// worst case, we'll do O(log(_WorkbufSize)) unnecessary
		// balances.
		if work.full == 0 {
			gcw.balance()
		}

		b := gcw.tryGetFast()
		if b == 0 {
			b = gcw.tryGet()
			if b == 0 {
				// Flush the write barrier
				// buffer; this may create
				// more work.
				wbBufFlush(nil, 0)
				b = gcw.tryGet()
			}
		}
		if b == 0 {
			// Unable to get work.
			break
		}
		scanobject(b, gcw)

		// Flush background scan work credit to the global
		// account if we've accumulated enough locally so
		// mutator assists can draw on it.
		if gcw.scanWork >= gcCreditSlack {
			atomic.Xaddint64(&gcController.scanWork, gcw.scanWork)
			if flushBgCredit {
				gcFlushBgCredit(gcw.scanWork - initScanWork)
				initScanWork = 0
			}
			checkWork -= gcw.scanWork
			gcw.scanWork = 0

			if checkWork <= 0 {
				checkWork += drainCheckThreshold
				if check != nil && check() {
					break
				}
			}
		}
	}

done:
	// Flush remaining scan work credit.
	if gcw.scanWork > 0 {
		atomic.Xaddint64(&gcController.scanWork, gcw.scanWork)
		if flushBgCredit {
			gcFlushBgCredit(gcw.scanWork - initScanWork)
		}
		gcw.scanWork = 0
	}
}

// 将黑色对象变为灰色
// gcDrainN blackens grey objects until it has performed roughly
// scanWork units of scan work or the G is preempted. This is
// best-effort, so it may perform less work if it fails to get a work
// buffer. Otherwise, it will perform at least n units of work, but
// may perform more because scanning is always done in whole object
// increments. It returns the amount of scan work performed.
//
// The caller goroutine must be in a preemptible state (e.g.,
// _Gwaiting) to prevent deadlocks during stack scanning. As a
// consequence, this must be called on the system stack.
//
//go:nowritebarrier
//go:systemstack
func gcDrainN(gcw *gcWork, scanWork int64) int64 {
	if !writeBarrier.needed {
		throw("gcDrainN phase incorrect")
	}

	// There may already be scan work on the gcw, which we don't
	// want to claim was done by this call.
	workFlushed := -gcw.scanWork

	gp := getg().m.curg
	for !gp.preempt && workFlushed+gcw.scanWork < scanWork {
		// See gcDrain comment.
		if work.full == 0 {
			gcw.balance()
		}

		// This might be a good place to add prefetch code...
		// if(wbuf.nobj > 4) {
		//         PREFETCH(wbuf->obj[wbuf.nobj - 3];
		//  }
		//
		b := gcw.tryGetFast()
		if b == 0 {
			b = gcw.tryGet()
			if b == 0 {
				// Flush the write barrier buffer;
				// this may create more work.
				wbBufFlush(nil, 0)
				b = gcw.tryGet()
			}
		}

		if b == 0 {
			// Try to do a root job.
			//
			// TODO: Assists should get credit for this
			// work.
			if work.markrootNext < work.markrootJobs {
				job := atomic.Xadd(&work.markrootNext, +1) - 1
				if job < work.markrootJobs {
					markroot(gcw, job)
					continue
				}
			}
			// No heap or root jobs.
			break
		}
		scanobject(b, gcw)

		// Flush background scan work credit.
		if gcw.scanWork >= gcCreditSlack {
			atomic.Xaddint64(&gcController.scanWork, gcw.scanWork)
			workFlushed += gcw.scanWork
			gcw.scanWork = 0
		}
	}

	// Unlike gcDrain, there's no need to flush remaining work
	// here because this never flushes to bgScanCredit and
	// gcw.dispose will flush any remaining work to scanWork.

	return workFlushed + gcw.scanWork
}

// scanblock scans b as scanobject would, but using an explicit
// pointer bitmap instead of the heap bitmap.
//
// This is used to scan non-heap roots, so it does not update
// gcw.bytesMarked or gcw.scanWork.
//
// If stk != nil, possible stack pointers are also reported to stk.putPtr.
//go:nowritebarrier
func scanblock(b0, n0 uintptr, ptrmask *uint8, gcw *gcWork, stk *stackScanState) {
	// Use local copies of original parameters, so that a stack trace
	// due to one of the throws below shows the original block
	// base and extent.
	b := b0
	n := n0

	for i := uintptr(0); i < n; {
		// Find bits for the next word.
		bits := uint32(*addb(ptrmask, i/(sys.PtrSize*8)))
		if bits == 0 {
			i += sys.PtrSize * 8
			continue
		}
		for j := 0; j < 8 && i < n; j++ {
			if bits&1 != 0 {
				// Same work as in scanobject; see comments there.
				p := *(*uintptr)(unsafe.Pointer(b + i))
				if p != 0 {
					if obj, span, objIndex := findObject(p, b, i); obj != 0 {
						greyobject(obj, b, i, span, gcw, objIndex)
					} else if stk != nil && p >= stk.stack.lo && p < stk.stack.hi {
						stk.putPtr(p, false)
					}
				}
			}
			bits >>= 1
			i += sys.PtrSize
		}
	}
}

// scanobject scans the object starting at b, adding pointers to gcw.
// b must point to the beginning of a heap object or an oblet.
// scanobject consults the GC bitmap for the pointer mask and the
// spans for the size of the object.
//
//go:nowritebarrier
func scanobject(b uintptr, gcw *gcWork) {
	// Find the bits for b and the size of the object at b.
	//
	// b is either the beginning of an object, in which case this
	// is the size of the object to scan, or it points to an
	// oblet, in which case we compute the size to scan below.
	hbits := heapBitsForAddr(b)
	s := spanOfUnchecked(b)
	n := s.elemsize
	if n == 0 {
		throw("scanobject n == 0")
	}

	if n > maxObletBytes {
		// Large object. Break into oblets for better
		// parallelism and lower latency.
		if b == s.base() {
			// It's possible this is a noscan object (not
			// from greyobject, but from other code
			// paths), in which case we must *not* enqueue
			// oblets since their bitmaps will be
			// uninitialized.
			if s.spanclass.noscan() {
				// Bypass the whole scan.
				gcw.bytesMarked += uint64(n)
				return
			}

			// Enqueue the other oblets to scan later.
			// Some oblets may be in b's scalar tail, but
			// these will be marked as "no more pointers",
			// so we'll drop out immediately when we go to
			// scan those.
			for oblet := b + maxObletBytes; oblet < s.base()+s.elemsize; oblet += maxObletBytes {
				if !gcw.putFast(oblet) {
					gcw.put(oblet)
				}
			}
		}

		// Compute the size of the oblet. Since this object
		// must be a large object, s.base() is the beginning
		// of the object.
		n = s.base() + s.elemsize - b
		if n > maxObletBytes {
			n = maxObletBytes
		}
	}

	var i uintptr
	for i = 0; i < n; i += sys.PtrSize {
		// Find bits for this word.
		if i != 0 {
			// Avoid needless hbits.next() on last iteration.
			hbits = hbits.next()
		}
		// Load bits once. See CL 22712 and issue 16973 for discussion.
		bits := hbits.bits()
		// During checkmarking, 1-word objects store the checkmark
		// in the type bit for the one word. The only one-word objects
		// are pointers, or else they'd be merged with other non-pointer
		// data into larger allocations.
		if i != 1*sys.PtrSize && bits&bitScan == 0 {
			break // no more pointers in this object
		}
		if bits&bitPointer == 0 {
			continue // not a pointer
		}

		// Work here is duplicated in scanblock and above.
		// If you make changes here, make changes there too.
		obj := *(*uintptr)(unsafe.Pointer(b + i))

		// At this point we have extracted the next potential pointer.
		// Quickly filter out nil and pointers back to the current object.
		if obj != 0 && obj-b >= n {
			// Test if obj points into the Go heap and, if so,
			// mark the object.
			//
			// Note that it's possible for findObject to
			// fail if obj points to a just-allocated heap
			// object because of a race with growing the
			// heap. In this case, we know the object was
			// just allocated and hence will be marked by
			// allocation itself.
			if obj, span, objIndex := findObject(obj, b, i); obj != 0 {
				greyobject(obj, b, i, span, gcw, objIndex)
			}
		}
	}
	gcw.bytesMarked += uint64(n)
	gcw.scanWork += int64(i)
}

// scanConservative scans block [b, b+n) conservatively, treating any
// pointer-like value in the block as a pointer.
//
// If ptrmask != nil, only words that are marked in ptrmask are
// considered as potential pointers.
//
// If state != nil, it's assumed that [b, b+n) is a block in the stack
// and may contain pointers to stack objects.
func scanConservative(b, n uintptr, ptrmask *uint8, gcw *gcWork, state *stackScanState) {
	if debugScanConservative {
		printlock()
		print("conservatively scanning [", hex(b), ",", hex(b+n), ")\n")
		hexdumpWords(b, b+n, func(p uintptr) byte {
			if ptrmask != nil {
				word := (p - b) / sys.PtrSize
				bits := *addb(ptrmask, word/8)
				if (bits>>(word%8))&1 == 0 {
					return '$'
				}
			}

			val := *(*uintptr)(unsafe.Pointer(p))
			if state != nil && state.stack.lo <= val && val < state.stack.hi {
				return '@'
			}

			span := spanOfHeap(val)
			if span == nil {
				return ' '
			}
			idx := span.objIndex(val)
			if span.isFree(idx) {
				return ' '
			}
			return '*'
		})
		printunlock()
	}

	for i := uintptr(0); i < n; i += sys.PtrSize {
		if ptrmask != nil {
			word := i / sys.PtrSize
			bits := *addb(ptrmask, word/8)
			if bits == 0 {
				// Skip 8 words (the loop increment will do the 8th)
				//
				// This must be the first time we've
				// seen this word of ptrmask, so i
				// must be 8-word-aligned, but check
				// our reasoning just in case.
				if i%(sys.PtrSize*8) != 0 {
					throw("misaligned mask")
				}
				i += sys.PtrSize*8 - sys.PtrSize
				continue
			}
			if (bits>>(word%8))&1 == 0 {
				continue
			}
		}

		val := *(*uintptr)(unsafe.Pointer(b + i))

		// Check if val points into the stack.
		if state != nil && state.stack.lo <= val && val < state.stack.hi {
			// val may point to a stack object. This
			// object may be dead from last cycle and
			// hence may contain pointers to unallocated
			// objects, but unlike heap objects we can't
			// tell if it's already dead. Hence, if all
			// pointers to this object are from
			// conservative scanning, we have to scan it
			// defensively, too.
			state.putPtr(val, true)
			continue
		}

		// Check if val points to a heap span.
		span := spanOfHeap(val)
		if span == nil {
			continue
		}

		// Check if val points to an allocated object.
		idx := span.objIndex(val)
		if span.isFree(idx) {
			continue
		}

		// val points to an allocated object. Mark it.
		obj := span.base() + idx*span.elemsize
		greyobject(obj, b, i, span, gcw, idx)
	}
}

// Shade the object if it isn't already.
// The object is not nil and known to be in the heap.
// Preemption must be disabled.
//go:nowritebarrier
func shade(b uintptr) {
	if obj, span, objIndex := findObject(b, 0, 0); obj != 0 {
		gcw := &getg().m.p.ptr().gcw
		greyobject(obj, 0, 0, span, gcw, objIndex)
	}
}

// obj is the start of an object with mark mbits.
// If it isn't already marked, mark it and enqueue into gcw.
// base and off are for debugging only and could be removed.
//
// See also wbBufFlush1, which partially duplicates this logic.
//
//go:nowritebarrierrec
func greyobject(obj, base, off uintptr, span *mspan, gcw *gcWork, objIndex uintptr) {
	// obj should be start of allocation, and so must be at least pointer-aligned.
	if obj&(sys.PtrSize-1) != 0 {
		throw("greyobject: obj not pointer-aligned")
	}
	mbits := span.markBitsForIndex(objIndex)

	if useCheckmark {
		if !mbits.isMarked() {
			printlock()
			print("runtime:greyobject: checkmarks finds unexpected unmarked object obj=", hex(obj), "\n")
			print("runtime: found obj at *(", hex(base), "+", hex(off), ")\n")

			// Dump the source (base) object
			gcDumpObject("base", base, off)

			// Dump the object
			gcDumpObject("obj", obj, ^uintptr(0))

			getg().m.traceback = 2
			throw("checkmark found unmarked object")
		}
		hbits := heapBitsForAddr(obj)
		if hbits.isCheckmarked(span.elemsize) {
			return
		}
		hbits.setCheckmarked(span.elemsize)
		if !hbits.isCheckmarked(span.elemsize) {
			throw("setCheckmarked and isCheckmarked disagree")
		}
	} else {
		if debug.gccheckmark > 0 && span.isFree(objIndex) {
			print("runtime: marking free object ", hex(obj), " found at *(", hex(base), "+", hex(off), ")\n")
			gcDumpObject("base", base, off)
			gcDumpObject("obj", obj, ^uintptr(0))
			getg().m.traceback = 2
			throw("marking free object")
		}

		// If marked we have nothing to do.
		if mbits.isMarked() {
			return
		}
		mbits.setMarked()

		// Mark span.
		arena, pageIdx, pageMask := pageIndexOf(span.base())
		if arena.pageMarks[pageIdx]&pageMask == 0 {
			atomic.Or8(&arena.pageMarks[pageIdx], pageMask)
		}

		// If this is a noscan object, fast-track it to black
		// instead of greying it.
		if span.spanclass.noscan() {
			gcw.bytesMarked += uint64(span.elemsize)
			return
		}
	}

	// Queue the obj for scanning. The PREFETCH(obj) logic has been removed but
	// seems like a nice optimization that can be added back in.
	// There needs to be time between the PREFETCH and the use.
	// Previously we put the obj in an 8 element buffer that is drained at a rate
	// to give the PREFETCH time to do its work.
	// Use of PREFETCHNTA might be more appropriate than PREFETCH
	if !gcw.putFast(obj) {
		gcw.put(obj)
	}
}

// gcDumpObject dumps the contents of obj for debugging and marks the
// field at byte offset off in obj.
func gcDumpObject(label string, obj, off uintptr) {
	s := spanOf(obj)
	print(label, "=", hex(obj))
	if s == nil {
		print(" s=nil\n")
		return
	}
	print(" s.base()=", hex(s.base()), " s.limit=", hex(s.limit), " s.spanclass=", s.spanclass, " s.elemsize=", s.elemsize, " s.state=")
	if state := s.state.get(); 0 <= state && int(state) < len(mSpanStateNames) {
		print(mSpanStateNames[state], "\n")
	} else {
		print("unknown(", state, ")\n")
	}

	skipped := false
	size := s.elemsize
	if s.state.get() == mSpanManual && size == 0 {
		// We're printing something from a stack frame. We
		// don't know how big it is, so just show up to an
		// including off.
		size = off + sys.PtrSize
	}
	for i := uintptr(0); i < size; i += sys.PtrSize {
		// For big objects, just print the beginning (because
		// that usually hints at the object's type) and the
		// fields around off.
		if !(i < 128*sys.PtrSize || off-16*sys.PtrSize < i && i < off+16*sys.PtrSize) {
			skipped = true
			continue
		}
		if skipped {
			print(" ...\n")
			skipped = false
		}
		print(" *(", label, "+", i, ") = ", hex(*(*uintptr)(unsafe.Pointer(obj + i))))
		if i == off {
			print(" <==")
		}
		print("\n")
	}
	if skipped {
		print(" ...\n")
	}
}

// gcmarknewobject marks a newly allocated object black. obj must
// not contain any non-nil pointers.
//
// This is nosplit so it can manipulate a gcWork without preemption.
//
//go:nowritebarrier
//go:nosplit
func gcmarknewobject(obj, size, scanSize uintptr) {
	if useCheckmark { // The world should be stopped so this should not happen.
		throw("gcmarknewobject called while doing checkmark")
	}
	markBitsForAddr(obj).setMarked()
	gcw := &getg().m.p.ptr().gcw
	gcw.bytesMarked += uint64(size)
	gcw.scanWork += int64(scanSize)
}

// gcMarkTinyAllocs greys all active tiny alloc blocks.
//
// The world must be stopped.
func gcMarkTinyAllocs() {
	for _, p := range allp {
		c := p.mcache
		if c == nil || c.tiny == 0 {
			continue
		}
		_, span, objIndex := findObject(c.tiny, 0, 0)
		gcw := &p.gcw
		greyobject(c.tiny, 0, 0, span, gcw, objIndex)
	}
}

// Checkmarking

// To help debug the concurrent GC we remark with the world
// stopped ensuring that any object encountered has their normal
// mark bit set. To do this we use an orthogonal bit
// pattern to indicate the object is marked. The following pattern
// uses the upper two bits in the object's boundary nibble.
// 01: scalar  not marked
// 10: pointer not marked
// 11: pointer     marked
// 00: scalar      marked
// Xoring with 01 will flip the pattern from marked to unmarked and vica versa.
// The higher bit is 1 for pointers and 0 for scalars, whether the object
// is marked or not.
// The first nibble no longer holds the typeDead pattern indicating that the
// there are no more pointers in the object. This information is held
// in the second nibble.

// If useCheckmark is true, marking of an object uses the
// checkmark bits (encoding above) instead of the standard
// mark bits.
var useCheckmark = false

//go:nowritebarrier
func initCheckmarks() {
	useCheckmark = true
	for _, s := range mheap_.allspans {
		if s.state.get() == mSpanInUse {
			heapBitsForAddr(s.base()).initCheckmarkSpan(s.layout())
		}
	}
}

func clearCheckmarks() {
	useCheckmark = false
	for _, s := range mheap_.allspans {
		if s.state.get() == mSpanInUse {
			heapBitsForAddr(s.base()).clearCheckmarkSpan(s.layout())
		}
	}
}
