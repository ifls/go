// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Memory allocator. 内存分配器
//
// This was originally based on基于 tcmalloc, but has diverged分歧 quite a bit.
// TODO http://goog-perftools.sourceforge.net/doc/tcmalloc.html

// The main allocator works in runs of pages.
// Small allocation sizes (up to and including <= 32 kB) are rounded to one of about大约 70 size classes,
// each of which has its own free set 空闲集合 of objects of exactly that size.
// Any free page of memory can be split into a set of objects of one size class, which are then managed using a free bitmap. 空闲位图
//
// The allocator's data structures are:
//
//	fixalloc: a free-list allocator for fixed-size off-heap堆外 objects,
//		used to manage storage used by the allocator. 管理分配器所占用的内存
//	mheap: the malloc heap, managed at page (8192-byte) granularity尺寸.
//	mspan: a run of一连串的 in-use pages managed by the mheap.
//	mcentral: collects收集 all spans of a given size class.
//	mcache: a per-P cache of mspans with free space.
//	mstats: allocation statistics.
//
// Allocating a small object proceeds up a hierarchy of caches:
//
//	1. Round the size up to one of the small size classes
//	   and look in the corresponding mspan in this P's mcache.
//	   Scan the mspan's free bitmap to find a free slot.
//	   If there is a free slot, allocate it.
//	   This can all be done without acquiring a lock.
//
//	2. If the mspan has no free slots, obtain a new mspan
//	   from the mcentral's list of mspans of the required size
//	   class that have free space.
//	   Obtaining a whole span amortizes the cost of locking
//	   the mcentral.
//
//	3. If the mcentral's mspan list is empty, obtain a run
//	   of pages from the mheap to use for the mspan.
//
//	4. If the mheap is empty or has no page runs large enough,
//	   allocate a new group of pages (at least 1MB) from the
//	   operating system. Allocating a large run of pages
//	   amortizes the cost of talking to the operating system.
//
// Sweeping an mspan and freeing objects on it proceeds up a similar
// hierarchy:
//
//	1. If the mspan is being swept in response to allocation, it is returned to the mcache to satisfy the allocation.
//
//	2. Otherwise, if the mspan still has allocated objects in it, it is placed on the mcentral free list for the mspan's size class.
//
//	3. Otherwise, if all objects in the mspan are free, the mspan's pages are returned to the mheap and the mspan is now dead.
//
// Allocating and freeing a large object大对象 uses the mheap directly, bypassing the mcache and mcentral.
//
// If mspan.needzero is false, then free object slots in the mspan are already zeroed.
// Otherwise if needzero is true, objects are zeroed as they are allocated.
// There are various benefits to delaying zeroing this way:
//
//	1. Stack frame allocation can avoid zeroing altogether完全.
//
//	2. It exhibits better temporal locality, since the program is probably about to write to the memory.
//
//	3. We don't zero pages that never get reused.

// Virtual memory layout 虚拟内存布局
//
// The heap consists of a set of arenas, which每一个 are 64MB on 64-bit and 4MB on 32-bit (heapArenaBytes).
// Each arena's start address is also aligned to the arena size. 起始地址 % 64M
//
// Each arena has an associated heapArena object that stores the metadata for that arena:
// the heap bitmap for all words in the arena 堆位图
// and
// the span map for all pages in the arena. span位图
//
// heapArena objects are themselves allocated off-heap. 堆外分配
//
// Since arenas are aligned, the address space地址空间 can be viewed as a series of arena frames.
// The arena map (mheap_.arenas) maps from arena frame number (arena index) to *heapArena,
// or nil for parts of the address space not backed by the Go heap. linux 起始地址从 1^47开始, 前面的arenas[i] == nil

// The arena map is structured as a two-level array consisting of a "L1" arena map and many "L2" arena maps;
// however, since arenas are large, on many architectures, the arena map consists of a single, large L2 map. 在许多架构上，只是一个大的二级映射关系
//
// The arena map covers覆盖 the entire完整 possible address space, allowing the Go heap to use any part of the address space. 大多数平台除amd64都是从0开始的
// The allocator attempts to keep arenas contiguous连续 so that large spans (and hence large objects) can cross arenas. 大的span 大的对象 超过arena分配空间

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

const (
	debugMalloc = false

	// 16
	maxTinySize = _TinySize
	// 2
	tinySizeClass = _TinySizeClass
	// 2^15 32KB
	maxSmallSize = _MaxSmallSize

	// 13
	pageShift = _PageShift
	// 8K
	pageSize = _PageSize
	// 8K-1 0b 0111 1111 1111
	pageMask = _PageMask
	// By construction通过构造, single page spans of the smallest object class have the most objects per span.
	// 1k
	maxObjsPerSpan = pageSize / 8
	// true
	concurrentSweep = _ConcurrentSweep

	_PageSize = 1 << _PageShift // 2^13 8K
	_PageMask = _PageSize - 1   // 8K-1 0b 0111 1111 1111

	// _64bit = 1 on 64-bit systems, 0 on 32-bit systems
	// 1
	_64bit = 1 << (^uintptr(0) >> 63) / 2

	// Tiny allocator parameters, see "Tiny allocator" comment in malloc.go.
	_TinySize      = 16
	_TinySizeClass = int8(2)

	// 2^14 16K
	_FixAllocChunk = 16 << 10 // Chunk size for FixAlloc

	// Per-P, per order stack segment cache size. 23KB
	// 32K
	_StackCacheSize = 32 * 1024

	// Number of orders that get caching. Order 0 is FixedStack
	// and each successive order is twice as large.
	// We want to cache 2KB, 4KB, 8KB, and 16KB stacks. Larger stacks
	// will be allocated directly.
	// Since FixedStack is different on different systems, we
	// must vary NumStackOrders to keep the same maximum cached size.
	//   OS               | FixedStack | NumStackOrders
	//   -----------------+------------+---------------
	//   linux/darwin/bsd | 2KB        | 4
	//   windows/32       | 4KB        | 3
	//   windows/64       | 8KB        | 2
	//   plan9            | 4KB        | 3
	// 4
	_NumStackOrders = 4 - sys.PtrSize/4*sys.GoosWindows - 1*sys.GoosPlan9

	// heapAddrBits is the number of bits in a heap address. On
	// amd64, addresses are sign-extended beyond heapAddrBits. On
	// other arches, they are zero-extended.
	//
	// On most 64-bit platforms, we limit this to 48 bits based on a
	// combination of hardware and OS limitations.
	//
	// amd64 hardware limits addresses to 48 bits, sign-extended
	// to 64 bits. Addresses where the top 16 bits are not either
	// all 0 or all 1 are "non-canonical" and invalid. Because of
	// these "negative" addresses, we offset addresses by 1<<47
	// (arenaBaseOffset) on amd64 before computing indexes into
	// the heap arenas index. In 2017, amd64 hardware added
	// support for 57 bit addresses; however, currently only Linux
	// supports this extension and the kernel will never choose an
	// address above 1<<47 unless mmap is called with a hint
	// address above 1<<47 (which we never do).
	//
	// arm64 hardware (as of ARMv8) limits user addresses to 48
	// bits, in the range [0, 1<<48).
	//
	// ppc64, mips64, and s390x support arbitrary 64 bit addresses
	// in hardware. On Linux, Go leans on stricter OS limits. Based
	// on Linux's processor.h, the user address space is limited as
	// follows on 64-bit architectures:
	//
	// Architecture  Name              Maximum Value (exclusive)
	// ---------------------------------------------------------------------
	// amd64         TASK_SIZE_MAX     0x007ffffffff000 (47 bit addresses)
	// arm64         TASK_SIZE_64      0x01000000000000 (48 bit addresses)
	// ppc64{,le}    TASK_SIZE_USER64  0x00400000000000 (46 bit addresses)
	// mips64{,le}   TASK_SIZE64       0x00010000000000 (40 bit addresses)
	// s390x         TASK_SIZE         1<<64 (64 bit addresses)
	//
	// These limits may increase over time, but are currently at
	// most 48 bits except on s390x. On all architectures, Linux
	// starts placing mmap'd regions at addresses that are
	// significantly below 48 bits, so even if it's possible to
	// exceed Go's 48 bit limit, it's extremely unlikely in
	// practice.
	//
	// On 32-bit platforms, we accept the full 32-bit address
	// space because doing so is cheap.
	// mips32 only has access to the low 2GB of virtual memory, so
	// we further limit it to 31 bits.
	//
	// On darwin/arm64, although 64-bit pointers are presumably
	// available, pointers are truncated to 33 bits. Furthermore,
	// only the top 4 GiB of the address space are actually available
	// to the application, but we allow the whole 33 bits anyway for
	// simplicity.
	// TODO(mknyszek): Consider limiting it to 32 bits and using
	// arenaBaseOffset to offset into the top 4 GiB.
	//
	// WebAssembly currently has a limit of 4GB linear memory.
	// 48 0x30 0b110000
	heapAddrBits = (_64bit*(1-sys.GoarchWasm)*(1-sys.GoosDarwin*sys.GoarchArm64))*48 + (1-_64bit+sys.GoarchWasm)*(32-(sys.GoarchMips+sys.GoarchMipsle)) + 33*sys.GoosDarwin*sys.GoarchArm64

	// maxAlloc is the maximum size of an allocation.
	// On 64-bit, it's theoretically possible to allocate 1<<heapAddrBits bytes.
	// On 32-bit, however, this is one less than 1<<32 because the
	// number of bytes in the address space doesn't actually fit in a uintptr.
	// 2^48
	maxAlloc = (1 << heapAddrBits) - (1-_64bit)*1

	// The number of bits in a heap address, the size of heap
	// arenas, and the L1 and L2 arena map sizes are related by
	//
	//   (1 << addr bits) = arena size * L1 entries * L2 entries
	//
	// Currently, we balance these as follows:
	//
	//       Platform  Addr bits  Arena size  L1 entries   L2 entries
	// --------------  ---------  ----------  ----------  -----------
	//       */64-bit         48        64MB           1    4M (32MB)
	// windows/64-bit         48         4MB          64    1M  (8MB)
	//       */32-bit         32         4MB           1  1024  (4KB)
	//     */mips(le)         31         4MB           1   512  (2KB)

	// heapArenaBytes is the size of a heap arena. The heap
	// consists of mappings of size heapArenaBytes, aligned to
	// heapArenaBytes. The initial heap mapping is one arena.
	//
	// This is currently 64MB on 64-bit non-Windows and 4MB on
	// 32-bit and on Windows.
	// We use smaller arenas on Windows because all committed memory is charged to the process,
	// even if it's not touched. Hence, for processes with small
	// heaps, the mapped arena space needs to be commensurate.
	// This is particularly important with the race detector,
	// since it significantly amplifies the cost of committed
	// memory.
	// 64M = 2^26
	heapArenaBytes = 1 << logHeapArenaBytes

	// logHeapArenaBytes is log_2 of heapArenaBytes. For clarity,
	// prefer using heapArenaBytes where possible (we need the
	// constant to compute some other constants).
	// 26
	logHeapArenaBytes = (6+20)*(_64bit*(1-sys.GoosWindows)*(1-sys.GoarchWasm)) + (2+20)*(_64bit*sys.GoosWindows) + (2+20)*(1-_64bit) + (2+20)*sys.GoarchWasm

	// heapArenaBitmapBytes is the size of each heap arena's bitmap.
	// 2M
	heapArenaBitmapBytes = heapArenaBytes / (sys.PtrSize * 8 / 2)

	// 64M / 8K = 8K 一个arena 8k个页面
	pagesPerArena = heapArenaBytes / pageSize

	// arenaL1Bits is the number of bits of the arena number covered by the first level arena map.
	//
	// This number should be small, since the first level arena
	// map requires PtrSize*(1<<arenaL1Bits) of space in the
	// binary's BSS. It can be zero, in which case the first level
	// index is effectively unused. There is a performance benefit
	// to this, since the generated code can be more efficient,
	// but comes at the cost of having a large L2 mapping.
	//
	// We use the L1 map on 64-bit Windows because the arena size
	// is small, but the address space is still 48 bits, and
	// there's a high cost to having a large L2.
	// 0
	arenaL1Bits = 6 * (_64bit * sys.GoosWindows)

	// arenaL2Bits is the number of bits of the arena number
	// covered by the second level arena index.
	//
	// The size of each arena map allocation is proportional to
	// 1<<arenaL2Bits, so it's important that this not be too
	// large. 48 bits leads to 32MB arena index allocations, which
	// is about the practical threshold.
	// 48 - 26 - 0 = 22
	arenaL2Bits = heapAddrBits - logHeapArenaBytes - arenaL1Bits

	// arenaL1Shift is the number of bits to shift an arena frame
	// number by to compute an index into the first level arena map.
	// 22
	arenaL1Shift = arenaL2Bits

	// arenaBits is the total bits in a combined arena map index.
	// This is split between the index into the L1 arena map and
	// the L2 arena map.
	// 0 + 22
	arenaBits = arenaL1Bits + arenaL2Bits

	// arenaBaseOffset is the pointer value that corresponds to
	// index 0 in the heap arena map.
	//
	// On amd64, the address space is 48 bits, sign extended to 64
	// bits. This offset lets us handle "negative" addresses (or
	// high addresses if viewed as unsigned). 有符号拓展， 可以允许
	//
	// On aix/ppc64, this offset allows to keep the heapAddrBits to
	// 48. Otherwize, it would be 60 in order to handle mmap addresses
	// (in range 0x0a00000000000000 - 0x0afffffffffffff). But in this
	// case, the memory reserved in (s *pageAlloc).init for chunks
	// is causing important slowdowns.
	//
	// On other platforms, the user address space is contiguous
	// and starts at 0, so no offset is necessary.
	arenaBaseOffset = 0xffff800000000000*sys.GoarchAmd64 + 0x0a00000000000000*sys.GoosAix

	// Max number of threads to run garbage collection. 运行垃圾回收的最大线程数量
	// 2, 3, and 4 are all plausible可信的 maximums depending on the hardware details of the machine.
	// The garbage collector scales well伸缩性好 to 32 cpus.
	_MaxGcproc = 32

	// minLegalPointer is the smallest possible legal pointer.
	// This is the smallest possible architectural page size,
	// since we assume that the first page is never mapped. 第一页从来不使用
	//
	// This should agree with一致 minZeroPage in the compiler.
	minLegalPointer uintptr = 4096
)

// physPageSize is the size in bytes of the OS's physical pages. 物理页字节数
// Mapping and unmapping operations must be done at multiples of  映射内存必须是物理页的整数倍
// physPageSize.
//
// This must be set by the OS init code (typically in osinit) before mallocinit.
var physPageSize uintptr

// physHugePageSize is the size in bytes of the OS's default physical huge
// page size whose allocation is opaque不透明 to the application. It is assumed
// and verified to be a power of two. 2^n B
//
// If set, this must be set by the OS init code (typically in osinit) before
// mallocinit. However, setting it at all is optional, and leaving the default
// value is always safe (though potentially less efficient).
//
// Since physHugePageSize is always assumed to be a power of two,
// physHugePageShift is defined as physHugePageSize == 1 << physHugePageShift.
// The purpose of physHugePageShift is to avoid doing divisions in
// performance critical functions.
var (
	physHugePageSize  uintptr
	physHugePageShift uint // 根据size 计算偏移
)

// OS memory management abstraction layer
// 系统内存管理抽象层
// Regions of the address space managed by the runtime may be in one of four
// states at any given time: 系统内存区域4种状态
// 1) None - Unreserved and unmapped, the default state of any region. 初始状态，什么都没有
// 2) Reserved - Owned by the runtime, but accessing it would cause a fault. 持有但是无法访问，还未分配物理内存
//               Does not count against the process' memory footprint足迹.
// 3) Prepared - Reserved, intended not to be backed by physical memory (though 不确定是否有物理地址，但是应该有，中间状态
//               an OS may implement this lazily). Can transition efficiently to
//               Ready. Accessing memory in such a region is undefined (may
//               fault, may give back unexpected zeroes, etc.).
// 4) Ready - may be accessed safely.  内存可以使用
//
// This set of states is more than is strictly necessary to support all the
// currently supported platforms. One could get by with just None, Reserved, and
// Ready. However, the Prepared state gives us flexibility for performance
// purposes. For example, on POSIX-y operating systems, Reserved is usually a
// private anonymous mmap'd region with PROT_NONE set, and to transition
// to Ready would require setting PROT_READ|PROT_WRITE. However the
// underspecification of Prepared lets us use just MADV_FREE to transition from
// Ready to Prepared. Thus with the Prepared state we can set the permission
// bits just once early on, we can efficiently tell the OS that it's free to
// take pages away from us when we don't strictly need them.
//
// For each OS there is a common set of helpers defined that transition
// memory regions between these states. The helpers are as follows:
// ->ready
// sysAlloc transitions an OS-chosen region of memory from None to Ready.
// More specifically, it obtains a large chunk of zeroed memory from the
// operating system, typically on the order of a hundred kilobytes
// or a megabyte. This memory is always immediately available for use.
// ->node
// sysFree transitions a memory region from any state to None. Therefore, it
// returns memory unconditionally. It is used if an out-of-memory error has been
// detected midway through an allocation or to carve out an aligned section of
// the address space. It is okay if sysFree is a no-op only if sysReserve always
// returns a memory region aligned to the heap allocator's alignment
// restrictions.
// ->reserverd
// sysReserve transitions a memory region from None to Reserved. It reserves
// address space in such a way that it would cause a fatal fault upon access
// (either via permissions or not committing the memory). Such a reservation is
// thus never backed by physical memory.
// If the pointer passed to it is non-nil, the caller wants the
// reservation there, but sysReserve can still choose another
// location if that one is unavailable.
// NOTE: sysReserve returns OS-aligned memory, but the heap allocator
// may use larger alignment, so the caller must be careful to realign the
// memory obtained by sysReserve.
// ->prepared
// sysMap transitions a memory region from Reserved to Prepared. It ensures the
// memory region can be efficiently transitioned to Ready.
// ->ready
// sysUsed transitions a memory region from Prepared to Ready. It notifies the
// operating system that the memory region is needed and ensures that the region
// may be safely accessed. This is typically a no-op on systems that don't have
// an explicit commit step and hard over-commit limits, but is critical on
// Windows, for example.
// ->prepared
// sysUnused transitions a memory region from Ready to Prepared. It notifies the
// operating system that the physical pages backing this memory region are no
// longer needed and can be reused for other purposes. The contents of a
// sysUnused memory region are considered forfeit and the region must not be
// accessed again until sysUsed is called.
// ->reserverd
// sysFault transitions a memory region from Ready or Prepared to Reserved. It
// marks a region such that it will always fault if accessed. Used only for
// debugging the runtime.

// schedinit() 中调用
func mallocinit() {
	// size_2  是16B 检查sizeclass.go是否正确生成
	if class_to_size[_TinySizeClass] != _TinySize {
		throw("bad TinySizeClass")
	}

	testdefersizes()

	// 必须是2的幂次 2M
	if heapArenaBitmapBytes&(heapArenaBitmapBytes-1) != 0 {
		// heapBits expects需要 modular arithmetic模运算 on bitmap addresses to work.
		throw("heapArenaBitmapBytes not a power of 2")
	}

	// Copy拷贝到统计 class sizes out for statistics table.
	for i := range class_to_size {
		memstats.by_size[i].size = uint32(class_to_size[i])
	}

	// Check physPageSize. 初始化失败
	if physPageSize == 0 {
		// The OS init code failed to fetch the physical page size.
		throw("failed to get system page size")
	}
	// (minPhysPageSize, maxPhysPageSize)
	if physPageSize > maxPhysPageSize {
		print("system page size (", physPageSize, ") is larger than maximum page size (", maxPhysPageSize, ")\n")
		throw("bad system page size")
	}
	if physPageSize < minPhysPageSize {
		print("system page size (", physPageSize, ") is smaller than minimum page size (", minPhysPageSize, ")\n")
		throw("bad system page size")
	}
	if physPageSize&(physPageSize-1) != 0 {
		print("system page size (", physPageSize, ") must be a power of 2\n")
		throw("bad system page size")
	}

	// 物理大页大小 检查必须是 2的幂
	if physHugePageSize&(physHugePageSize-1) != 0 {
		print("system huge page size (", physHugePageSize, ") must be a power of 2\n")
		throw("bad system huge page size")
	}
	if physHugePageSize > maxPhysHugePageSize {
		// physHugePageSize is greater than the maximum supported huge page size.
		// Don't throw here, like in the other cases, since a system configured
		// in this way isn't wrong, we just don't have the code to support them.
		// Instead, silently set the huge page size to zero.
		physHugePageSize = 0
	}
	if physHugePageSize != 0 {
		// Since physHugePageSize is a power of 2, it suffices to increase
		// physHugePageShift until 1<<physHugePageShift == physHugePageSize.
		for 1<<physHugePageShift != physHugePageSize {
			// 计算位移量
			physHugePageShift++
		}
	}

	// 8k % 2^9 = 16
	if pagesPerArena%pagesPerSpanRoot != 0 {
		print("pagesPerArena (", pagesPerArena, ") is not divisible by pagesPerSpanRoot (", pagesPerSpanRoot, ")\n")
		throw("bad pagesPerSpanRoot")
	}
	// 8k % 2^9 = 16
	if pagesPerArena%pagesPerReclaimerChunk != 0 {
		print("pagesPerArena (", pagesPerArena, ") is not divisible by pagesPerReclaimerChunk (", pagesPerReclaimerChunk, ")\n")
		throw("bad pagesPerReclaimerChunk")
	}

	// Initialize the heap.
	mheap_.init()

	// 分配第一个mcache
	mcache0 = allocmcache()

	// 设置锁的rank
	lockInit(&gcBitsArenas.lock, lockRankGcBitsArenas)
	lockInit(&proflock, lockRankProf)
	lockInit(&globalAlloc.mutex, lockRankGlobalAlloc)

	// 创建arena hint 链表
	// Create initial arena growth hints.
	if sys.PtrSize == 8 {
		// On a 64-bit machine, we pick the following hints because:
		//
		// 1. Starting from the middle of the address space
		// makes it easier to grow out a contiguous range
		// without running in to some other mapping.
		//
		// 2. This makes Go heap addresses more easily recognizable when debugging.
		//
		// 3. Stack scanning in gccgo is still conservative保守, so it's important that addresses be distinguishable可区分的 from other data.
		//
		// Starting at 0x00c0 means that the valid memory addresses
		// will begin 0x00c0, 0x00c1, ...
		// In little-endian, that's c0 00, c1 00, ... None of those are valid
		// UTF-8 sequences, and they are otherwise as far away from
		// ff (likely a common byte) as possible.
		// If that fails, we try other 0xXXc0 addresses.
		// An earlier attempt to use 0x11f8 caused out of memory errors on OS X during thread allocations.
		// 0x00c0 causes conflicts with AddressSanitizer which reserves all memory up to 0x0100.
		// These choices reduce the odds of a conservative garbage collector
		// not collecting memory because some non-pointer block of memory
		// had a bit pattern that matched a memory address.
		//
		// However, on arm64, we ignore all this advice above and slam the
		// allocation at 0x40 << 32 because when using 4k pages with 3-level
		// translation buffers, the user address space is limited to 39 bits
		// On darwin/arm64, the address space is even smaller.
		//
		// On AIX, mmaps starts at 0x0A00000000000000 for 64-bit.
		// processes.
		for i := 0x7f; i >= 0; i-- {
			var p uintptr
			switch {
			case GOARCH == "arm64" && GOOS == "darwin":
				p = uintptr(i)<<40 | uintptrMask&(0x0013<<28)
			case GOARCH == "arm64":
				p = uintptr(i)<<40 | uintptrMask&(0x0040<<32)
			case GOOS == "aix":
				if i == 0 {
					// We don't use addresses directly after 0x0A00000000000000
					// to avoid collisions with others mmaps done by non-go programs.
					continue
				}
				p = uintptr(i)<<40 | uintptrMask&(0xa0<<52)
			case raceenabled:
				// The TSAN runtime requires the heap
				// to be in the range [0x00c000000000,
				// 0x00e000000000).
				p = uintptr(i)<<32 | uintptrMask&(0x00c0<<32)
				if p >= uintptrMask&0x00e000000000 {
					continue
				}
			default:
				// 0x 7f        00        00        00        00        00
				// 0b 0111 1111 0000 0000 0000 0000 0000 0000 0000 0000 0000 0000
				// 0b 0000 0000 1100 0000 0000 0000 0000 0000 0000 0000 0000 0000
				p = uintptr(i)<<40 | uintptrMask&(0x00c0<<32)
			}

			// 堆外对象
			hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
			// 指向堆内地址
			hint.addr = p
			// 交换
			hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
		}
	} else {
		// On a 32-bit machine, we're much more concerned
		// about keeping the usable heap contiguous.
		// Hence:
		//
		// 1. We reserve space for all heapArenas up front so
		// they don't get interleaved with the heap. They're
		// ~258MB, so this isn't too bad. (We could reserve a
		// smaller amount of space up front if this is a
		// problem.)
		//
		// 2. We hint the heap to start right above the end of
		// the binary so we have the best chance of keeping it
		// contiguous.
		//
		// 3. We try to stake out a reasonably large initial
		// heap reservation.

		const arenaMetaSize = (1 << arenaBits) * unsafe.Sizeof(heapArena{})
		meta := uintptr(sysReserve(nil, arenaMetaSize))
		if meta != 0 {
			mheap_.heapArenaAlloc.init(meta, arenaMetaSize)
		}

		// We want to start the arena low, but if we're linked
		// against C code, it's possible global constructors
		// have called malloc and adjusted the process' brk.
		// Query the brk so we can avoid trying to map the
		// region over it (which will cause the kernel to put
		// the region somewhere else, likely at a high
		// address).
		// 系统调用
		procBrk := sbrk0()

		// If we ask for the end of the data segment but the
		// operating system requires a little more space
		// before we can start allocating, it will give out a
		// slightly higher pointer. Except QEMU, which is
		// buggy, as usual: it won't adjust the pointer
		// upward. So adjust it upward a little bit ourselves:
		// 1/4 MB to get away from the running binary image.
		p := firstmoduledata.end
		if p < procBrk {
			p = procBrk
		}
		if mheap_.heapArenaAlloc.next <= p && p < mheap_.heapArenaAlloc.end {
			p = mheap_.heapArenaAlloc.end
		}
		p = alignUp(p+(256<<10), heapArenaBytes)
		// Because we're worried about fragmentation on
		// 32-bit, we try to make a large initial reservation.
		arenaSizes := []uintptr{
			512 << 20,
			256 << 20,
			128 << 20,
		}
		for _, arenaSize := range arenaSizes {
			a, size := sysReserveAligned(unsafe.Pointer(p), arenaSize, heapArenaBytes)
			if a != nil {
				mheap_.arena.init(uintptr(a), size)
				p = mheap_.arena.end // For hint below
				break
			}
		}
		hint := (*arenaHint)(mheap_.arenaHintAlloc.alloc())
		hint.addr = p
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
	}
}

// 分配至少nB arena空间
// sysAlloc allocates heap arena space for at least n bytes.
// The returned pointer is always heapArenaBytes-aligned and backed by h.arenas metadata.
// The returned size is always a multiple of heapArenaBytes. sysAlloc returns nil on failure.
// There is no corresponding free function.
//
// sysAlloc returns a memory region in the Prepared state. This region must
// be transitioned to Ready before use.
//
// h must be locked.
// 分配1-n个arena大小的空间
func (h *mheap) sysAlloc(n uintptr) (v unsafe.Pointer, size uintptr) {
	// n 是字节数 % 64M
	n = alignUp(n, heapArenaBytes)

	// First, try the arena pre-reservation.
	// 先尝试从预留空间中分配内存
	v = h.arena.alloc(n, heapArenaBytes, &memstats.heap_sys)
	if v != nil {
		size = n
		goto mapped
	}

	// Try to grow the heap at a hint address. 链表
	// 拿不到， 再尝试从 arenaHints 地址上扩容
	for h.arenaHints != nil {
		hint := h.arenaHints
		p := hint.addr

		if hint.down {
			p -= n
		}

		if p+n < p {
			// We can't use this, so don't ask.
			v = nil
		} else if arenaIndex(p+n-1) >= 1<<arenaBits {
			// Outside addressable heap. Can't use.
			// 超过了范围
			v = nil
		} else {
			// Node -> reserved
			v = sysReserve(unsafe.Pointer(p), n)
		}

		if p == uintptr(v) {
			// Success. Update the hint.
			if !hint.down {
				p += n
			}
			hint.addr = p
			size = n
			break
		}
		// Failed. Discard this hint and try the next.
		//
		// TODO: This would be cleaner if sysReserve could be
		// told to only return the requested address. In
		// particular, this is already how Windows behaves, so
		// it would simplify things there.
		// 分配成功但是用不上, 算失败要回收
		if v != nil {
			sysFree(v, n, nil)
		}
		// next
		h.arenaHints = hint.next
		// 释放到前hint
		h.arenaHintAlloc.free(unsafe.Pointer(hint))
	}

	if size == 0 {
		if raceenabled {
			// The race detector assumes the heap lives in
			// [0x00c000000000, 0x00e000000000), but we
			// just ran out of hints in this region. Give
			// a nice failure.
			throw("too many address space collisions for -race mode")
		}

		// All of the hints failed, so we'll take any
		// (sufficiently aligned) address the kernel will give us.
		// 从hint地址拿失败了， 再从系统拿任意地址开头的
		v, size = sysReserveAligned(nil, n, heapArenaBytes)
		if v == nil {
			return nil, 0
		}

		// Create new hints for extending this region. 插入两个 方向的
		hint := (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr, hint.down = uintptr(v), true
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
		hint = (*arenaHint)(h.arenaHintAlloc.alloc())
		hint.addr = uintptr(v) + size
		hint.next, mheap_.arenaHints = mheap_.arenaHints, hint
	}

	// Check for bad pointers or pointers we can't use.
	{
		var bad string
		p := uintptr(v)
		if p+size < p {
			bad = "region exceeds uintptr range"
		} else if arenaIndex(p) >= 1<<arenaBits {
			bad = "base outside usable address space"
		} else if arenaIndex(p+size-1) >= 1<<arenaBits {
			bad = "end outside usable address space"
		}
		if bad != "" {
			// This should be impossible on most architectures,
			// but it would be really confusing to debug.
			print("runtime: memory allocated by OS [", hex(p), ", ", hex(p+size), ") not in usable address space: ", bad, "\n")
			throw("memory reservation exceeds address space limit")
		}
	}

	// 未分配在对齐的地址上
	if uintptr(v)&(heapArenaBytes-1) != 0 {
		throw("misrounded allocation in sysAlloc")
	}

	// Transition from Reserved to Prepared.
	// 尽快分配物理空间 Reserved -> Prepared
	sysMap(v, size, &memstats.heap_sys)

mapped:
	// Create arena metadata. 分别对应空间的元数据
	// 上界, 下界
	for ri := arenaIndex(uintptr(v)); ri <= arenaIndex(uintptr(v)+size-1); ri++ {
		l2 := h.arenas[ri.l1()]
		if l2 == nil {
			// Allocate an L2 arena map. // 分配l2 大切片
			l2 = (*[1 << arenaL2Bits]*heapArena)(persistentalloc(unsafe.Sizeof(*l2), sys.PtrSize, nil))
			if l2 == nil {
				throw("out of memory allocating heap arena map")
			}
			// 加入 mheap.arenas[i] = l2
			atomic.StorepNoWB(unsafe.Pointer(&h.arenas[ri.l1()]), unsafe.Pointer(l2))
		}

		// 冗余检查
		if l2[ri.l2()] != nil {
			throw("arena already initialized")
		}

		var r *heapArena
		// 分配arena结构体 堆外分配
		r = (*heapArena)(h.heapArenaAlloc.alloc(unsafe.Sizeof(*r), sys.PtrSize, &memstats.gc_sys))
		if r == nil {
			// 包装sysAlloc 向系统申请内存
			r = (*heapArena)(persistentalloc(unsafe.Sizeof(*r), sys.PtrSize, &memstats.gc_sys))
			if r == nil {
				throw("out of memory allocating heap arena metadata")
			}
		}

		// Add the arena to the arenas list. []arenaindex
		// 扩容切片， 加拷贝， 为什么不直接append？
		if len(h.allArenas) == cap(h.allArenas) {
			size := 2 * uintptr(cap(h.allArenas)) * sys.PtrSize
			if size == 0 {
				size = physPageSize
			}
			newArray := (*notInHeap)(persistentalloc(size, sys.PtrSize, &memstats.gc_sys))
			if newArray == nil {
				throw("out of memory allocating allArenas")
			}
			oldSlice := h.allArenas
			// 堆外切片
			*(*notInHeapSlice)(unsafe.Pointer(&h.allArenas)) = notInHeapSlice{newArray, len(h.allArenas), int(size / sys.PtrSize)}
			copy(h.allArenas, oldSlice)
			// Do not free the old backing array because
			// there may be concurrent readers. Since we
			// double the array each time, this can lead
			// to at most 2x waste.
		}
		h.allArenas = h.allArenas[:len(h.allArenas)+1]
		h.allArenas[len(h.allArenas)-1] = ri

		// Store atomically just in case an object from the
		// new heap arena becomes visible before the heap lock
		// is released (which shouldn't happen, but there's
		// little downside负面,下降趋势 to this).
		// 保存arena指针 arenas[i][j] = &heapArena
		atomic.StorepNoWB(unsafe.Pointer(&l2[ri.l2()]), unsafe.Pointer(r))
	}

	// Tell the race detector about the new heap memory.
	if raceenabled {
		racemapshadow(v, size)
	}

	return
}

// sysReserveAligned is like sysReserve, but the returned pointer is
// aligned to align bytes. It may reserve either n or n+align bytes,
// so it returns the size that was reserved.
func sysReserveAligned(v unsafe.Pointer, size, align uintptr) (unsafe.Pointer, uintptr) {
	// Since the alignment is rather large in uses of this
	// function, we're not likely to get it by chance, so we ask
	// for a larger region and remove the parts we don't need.
	retries := 0
retry:
	p := uintptr(sysReserve(v, size+align))
	switch {
	case p == 0:
		return nil, 0
	case p&(align-1) == 0:
		// 正好对齐
		// We got lucky and got an aligned region, so we can
		// use the whole thing.
		return unsafe.Pointer(p), size + align
	case GOOS == "windows":
		// On Windows we can't release pieces of a
		// reservation, so we release the whole thing and
		// re-reserve the aligned sub-region. This may race,
		// so we may have to try again.
		sysFree(unsafe.Pointer(p), size+align, nil)
		p = alignUp(p, align)
		p2 := sysReserve(unsafe.Pointer(p), size)
		if p != uintptr(p2) {
			// Must have raced. Try again.
			sysFree(p2, size, nil)
			if retries++; retries == 100 {
				throw("failed to allocate aligned heap memory; too many retries")
			}
			goto retry
		}
		// Success.
		return p2, size
	default:
		// Trim off the unaligned parts.
		pAligned := alignUp(p, align)
		// 释放之前的空间
		sysFree(unsafe.Pointer(p), pAligned-p, nil)
		end := pAligned + size
		endLen := (p + size + align) - end
		if endLen > 0 {
			// 释放后半部分
			sysFree(unsafe.Pointer(end), endLen, nil)
		}
		return unsafe.Pointer(pAligned), size
	}
}

// base address for all 0-byte allocations
var zerobase uintptr // 未分配空间的指针的默认值

// nextFreeFast returns the next free object if one is quickly available.
// Otherwise it returns 0.
func nextFreeFast(s *mspan) gclinkptr {
	// count trail zero 如果所有位都是0 返回64表示全都分配了，
	theBit := sys.Ctz64(s.allocCache) // Is there a free object in the allocCache?
	// bit == 1的位还有
	if theBit < 64 {
		result := s.freeindex + uintptr(theBit)
		if result < s.nelems {
			// result位被占用, freedix就是下一位
			freeidx := result + 1
			if freeidx%64 == 0 && freeidx != s.nelems {
				return 0
			}
			// 右移,让 所有位都是1, 全部都没有被占用
			s.allocCache >>= uint(theBit + 1)
			// 更新 空闲对象起始地址为下一位
			s.freeindex = freeidx
			s.allocCount++
			// result表示第几个, elumsize 元素占用字节，s.base() 基址偏移
			return gclinkptr(result*s.elemsize + s.base())
		}
	}
	return 0
}

// nextFree returns the next free object from the cached span if one is available.
// Otherwise it refills the cache with a span with an available object and
// returns that object along with a flag indicating that this was a heavy
// weight allocation. If it is a heavy weight allocation the caller must
// determine whether a new GC cycle needs to be started or if the GC is active
// whether this goroutine needs to assist the GC.
//
// Must run in a non-preemptible context since otherwise the owner of
// c could change.
func (c *mcache) nextFree(spc spanClass) (v gclinkptr, s *mspan, shouldhelpgc bool) {
	s = c.alloc[spc]
	shouldhelpgc = false
	freeIndex := s.nextFreeIndex()
	if freeIndex == s.nelems {
		// The span is full. 满了
		// allocCount ！= nelems 状态不一致
		if uintptr(s.allocCount) != s.nelems {
			println("runtime: s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
			throw("s.allocCount != s.nelems && freeIndex == s.nelems")
		}
		// 向mcentral 换一个, 必须是可分配的
		c.refill(spc)
		shouldhelpgc = true
		s = c.alloc[spc]
		// 函数里面会++
		freeIndex = s.nextFreeIndex()
	}

	if freeIndex >= s.nelems {
		throw("freeIndex is not valid")
	}

	v = gclinkptr(freeIndex*s.elemsize + s.base())
	s.allocCount++
	if uintptr(s.allocCount) > s.nelems {
		println("s.allocCount=", s.allocCount, "s.nelems=", s.nelems)
		throw("s.allocCount > s.nelems")
	}
	return
}

// Allocate an object of size bytes.
// Small objects are allocated from the per-P cache's free lists. <= 32K mcache
// Large objects (> 32 kB) are allocated straight from the heap.  > 32k mheap
// 分配对象的入口 new(T) = newobject(typ) = mallocgc(typ.size, typ, true)
// chan, slice, string 分配内存的时候 不会传类型信息 typ == nil
// needzero 为false， 可能是后面的 代码需要自行置0
func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
	// 标记收尾阶段，不能分配内存，修改内存对象关系图
	if gcphase == _GCmarktermination {
		throw("mallocgc called with gcphase == _GCmarktermination")
	}

	// 0, 不用分配空间，返回一个指向0的指针，例如 new(struct{})
	if size == 0 {
		return unsafe.Pointer(&zerobase)
	}

	// 设置sbrk=1会使用一个碎片回收器代替内存分配器和垃圾回收器。它从操作系统获取内存，并且永远也不会回收任何内存
	if debug.sbrk != 0 {
		align := uintptr(16)
		if typ != nil {
			// TODO(austin): This should be just
			//   align = uintptr(typ.align)
			// but that's only 4 on 32-bit platforms,
			// even if there's a uint64 field in typ (see #599).
			// This causes 64-bit atomic accesses to panic.
			// Hence, we use stricter alignment that matches
			// the normal allocator better.
			// 根据size%2^k 进行对齐
			if size&7 == 0 {
				align = 8
			} else if size&3 == 0 {
				align = 4
			} else if size&1 == 0 {
				align = 2
			} else {
				align = 1
			}
		}
		// 使用sbrk 类似的直接向系统要内存，再分配内存
		return persistentalloc(size, align, &memstats.other_sys)
	}

	// assistG is the G to charge for负责此次分配的g this allocation, or nil if GC is not currently active.
	var assistG *g
	// gc的标记阶段已开始
	if gcBlackenEnabled != 0 {
		// Charge the current user G for this allocation.
		// 当前g停止用户代码，切换到帮助标记
		assistG = getg()
		if assistG.m.curg != nil {
			assistG = assistG.m.curg
		}
		// Charge付费 the allocation against the G. We'll account
		// for internal fragmentation 对内部碎片负责 at the end of mallocgc.
		assistG.gcAssistBytes -= int64(size)

		if assistG.gcAssistBytes < 0 {
			// This G is in debt. Assist the GC to correct
			// this before allocating. This must happen
			// before disabling preemption.
			// g欠债，需要帮助执行gc，以偿还，直到为正， 不欠帐，才可以分配内存，
			gcAssistAlloc(assistG)
		}
	}

	// Set mp.mallocing to keep from being preempted by GC. 避免被gc抢占
	// lockm
	mp := acquirem()

	// 防一个m 重复进入分配
	if mp.mallocing != 0 {
		throw("malloc deadlock")
	}

	// 信号处理g不能分配内存，进行malloc
	if mp.gsignal == getg() {
		throw("malloc during signal")
	}
	mp.mallocing = 1

	shouldhelpgc := false
	dataSize := size

	// 拿到当前p的mcache
	var c *mcache
	if mp.p != 0 {
		c = mp.p.ptr().mcache
	} else {
		// We will be called without a P while bootstrapping自举,
		// in which case we use mcache0, which is set in mallocinit.
		// mcache0 is cleared 清除when bootstrapping is complete, by procresize (因为移动到了p0上).
		// m0一开始没有p0对应，所以 mp.p == 0 但是有分配的mcache0可用
		c = mcache0
		if c == nil {
			throw("malloc called with no P")
		}
	}

	var x unsafe.Pointer
	// 无类型 或者类型无指针
	noscan := typ == nil || typ.ptrdata == 0
	// <= 32KB
	if size <= maxSmallSize {
		// <- 16B
		if noscan && size < maxTinySize {
			// noscan表示非包含指针类型的数据类型
			// 微对象（不可以是指针类型的对象） 先尝试tiny分配器，再mcache，mcenttral,mheap
			// Tiny allocator.
			//
			// Tiny allocator combines several tiny allocation requests
			// into a single memory block. The resulting memory block
			// is freed when all subobjects are unreachable. The subobjects
			// must be noscan (don't have pointers), this ensures that
			// the amount of potentially wasted memory is bounded.
			//
			// Size of the memory block used for combining (maxTinySize) is tunable.
			// Current setting is 16 bytes, which relates to 2x worst case memory
			// wastage (when all but one subobjects are unreachable).
			// 8 bytes would result in no wastage at all, but provides less
			// opportunities for combining.
			// 32 bytes provides more opportunities for combining,
			// but can lead to 4x worst case wastage.
			// The best case winning is 8x regardless of block size.
			//
			// Objects obtained from tiny allocator must not be freed explicitly.
			// So when an object will be freed explicitly, we ensure that
			// its size >= maxTinySize.
			//
			// SetFinalizer has a special case for objects potentially coming
			// from tiny allocator, it such case it allows to set finalizers
			// for an inner byte of a memory block.
			//
			// The main targets of tiny allocator are small strings and
			// standalone escaping variables. On a json benchmark
			// the allocator reduces number of allocations by ~12% and
			// reduces heap size by ~20%.
			off := c.tinyoffset
			// Align tiny pointer for required (conservative) alignment.
			if size&7 == 0 {
				off = alignUp(off, 8)
			} else if size&3 == 0 {
				off = alignUp(off, 4)
			} else if size&1 == 0 {
				off = alignUp(off, 2)
			}

			// <= 16B && 还有小块内存 先从tiny 给小对象(主要是较小的字符串和逃逸的临时变量)分配内存
			// 只有当一个内存块里几个tiny对象都可回收时，才会被回收

			if off+size <= maxTinySize && c.tiny != 0 {
				// The object fits into existing tiny block.
				// 拿到小对象指针
				x = unsafe.Pointer(c.tiny + off)
				c.tinyoffset = off + size
				// 本地mcache小块分配次数+1
				c.local_tinyallocs++

				// 解锁
				mp.mallocing = 0
				releasem(mp)
				return x
			}

			// 找不到然后再从mcache里去找内存 mcache Allocate a new maxTinySize block.
			span := c.alloc[tinySpanClass]
			// 从mspan快速找下一个空闲对象地址
			v := nextFreeFast(span) // fastpath
			if v == 0 {
				// 失败，再次找下一个地址，会向上级去拿，必须拿的到
				v, _, shouldhelpgc = c.nextFree(tinySpanClass)  // slowpath, 从中心缓存拿
			}
			x = unsafe.Pointer(v)
			// 清零16B，小对象的内存就全清0了
			(*[2]uint64)(x)[0] = 0
			(*[2]uint64)(x)[1] = 0
			// See if we need to replace the existing tiny block with the new one based on amount of remaining free space.
			if size < c.tinyoffset || c.tiny == 0 {
				// 更新基址和基址偏移量
				c.tiny = uintptr(x)
				c.tinyoffset = size
			}
			size = maxTinySize
		} else {
			// 16 <= size <= 32K
			//小对象 mcache,mcentral,mheap
			var sizeclass uint8
			// <= 1024-8
			if size <= smallSizeMax-8 {
				// 8
				sizeclass = size_to_class8[divRoundUp(size, smallSizeDiv)]
			} else {
				// 128
				sizeclass = size_to_class128[divRoundUp(size-smallSizeMax, largeSizeDiv)]
			}
			size = uintptr(class_to_size[sizeclass])
			spc := makeSpanClass(sizeclass, noscan)
			// mcache中找mspan mcache.alloc
			span := c.alloc[spc]
			v := nextFreeFast(span)
			if v == 0 {
				v, span, shouldhelpgc = c.nextFree(spc)
			}
			x = unsafe.Pointer(v)
			if needzero && span.needzero != 0 {
				memclrNoHeapPointers(unsafe.Pointer(v), size)  // 清0内存
			}
		}
	} else {
		// 大对象
		var s *mspan
		shouldhelpgc = true  // 拿一次大对象， 必判断是否要gc
		systemstack(func() {
			// 大对象分配 alloc allocSpan grow sysAlloc
			s = largeAlloc(size, needzero, noscan)
		})
		s.freeindex = 1
		s.allocCount = 1
		x = unsafe.Pointer(s.base())
		size = s.elemsize
	}

	var scanSize uintptr
	if !noscan {
		// If allocating a defer+arg block, now that we've picked a malloc size
		// large enough to hold everything, cut the "asked for" size down to
		// just the defer header, so that the GC bitmap will record the arg block
		// as containing nothing at all (as if it were unused space at the end of
		// a malloc block caused by size rounding).
		// The defer arg areas are scanned as part of scanstack.
		if typ == deferType {  // 针对 defer 的特殊处理
			dataSize = unsafe.Sizeof(_defer{})
		}
		heapBitsSetType(uintptr(x), size, dataSize, typ)
		if dataSize > typ.size {
			// Array allocation. If there are any
			// pointers, GC has to scan to the last
			// element.
			if typ.ptrdata != 0 {
				scanSize = dataSize - typ.size + typ.ptrdata
			}
		} else {
			scanSize = typ.ptrdata
		}
		c.local_scan += scanSize
	}

	// Ensure that the stores above that initialize x to
	// type-safe memory and set the heap bits occur before
	// the caller can make x observable to the garbage
	// collector. Otherwise, on weakly ordered machines,
	// the garbage collector could follow a pointer to x,
	// but see uninitialized memory or stale heap bits.
	// 空函数 实现是直接ret
	publicationBarrier()

	// Allocate black during GC. gc期间分配黑色对象
	// All slots hold nil so no scanning is needed.
	// This may be racing with GC so do it atomically if there can be
	// a race marking the bit.
	if gcphase != _GCoff {
		gcmarknewobject(uintptr(x), size, scanSize)
	}

	if raceenabled {
		racemalloc(x, size)
	}

	if msanenabled {
		msanmalloc(x, size)
	}

	// leave reset status
	mp.mallocing = 0
	releasem(mp)

	if debug.allocfreetrace != 0 {
		tracealloc(x, size, typ)
	}

	// 每采样 512K， 就记录一次
	if rate := MemProfileRate; rate > 0 {
		if rate != 1 && size < c.next_sample {
			c.next_sample -= size
		} else {
			mp := acquirem()
			// 剖析内存分配
			profilealloc(mp, x, size)
			releasem(mp)
		}
	}

	if assistG != nil {
		// Account账户 for internal fragmentation in the assist
		// debt now that we know it.
		assistG.gcAssistBytes -= int64(size - dataSize)
	}

	// 每次分配内存都要判断是否开gc
	if shouldhelpgc {
		if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
			gcStart(t)
		}
	}

	return x
}

// 大对象分配 >32KB
func largeAlloc(size uintptr, needzero bool, noscan bool) *mspan {
	// print("largeAlloc size=", size, "\n")
	// 溢出为0
	if size+_PageSize < size {
		throw("out of memory")
	}

	// 页数
	npages := size >> _PageShift
	// size % pageSize
	if size&_PageMask != 0 {
		npages++
	}

	// Deduct credit for this span allocation and sweep if
	// necessary. mHeap_Alloc will also sweep npages, so this only
	// pays the debt down to npage pages.
	deductSweepCredit(npages*_PageSize, npages)
	// 0表示不切块
	spc := makeSpanClass(0, noscan)
	s := mheap_.alloc(npages, spc, needzero)
	if s == nil {
		throw("out of memory")
	}
	if go115NewMCentralImpl {
		// Put the large span in the mcentral swept list so that it's
		// visible to the background sweeper.
		mheap_.central[spc].mcentral.fullSwept(mheap_.sweepgen).push(s)
	}
	// 基址，界限
	s.limit = s.base() + size
	// 根据heapArena计算
	heapBitsForAddr(s.base()).initSpan(s)
	return s
}

// implementation of new builtin
// compiler (both frontend and SSA backend) knows the signature
// of this function
// 实现内置的new，堆上所有对象分配都在这里做入口
func newobject(typ *_type) unsafe.Pointer {
	return mallocgc(typ.size, typ, true)
}

//go:linkname reflect_unsafe_New reflect.unsafe_New
func reflect_unsafe_New(typ *_type) unsafe.Pointer {
	return mallocgc(typ.size, typ, true)
}

//go:linkname reflectlite_unsafe_New internal/reflectlite.unsafe_New
func reflectlite_unsafe_New(typ *_type) unsafe.Pointer {
	return mallocgc(typ.size, typ, true)
}

// newarray allocates an array of n elements of type typ.
func newarray(typ *_type, n int) unsafe.Pointer {
	if n == 1 {
		return mallocgc(typ.size, typ, true)
	}
	mem, overflow := math.MulUintptr(typ.size, uintptr(n))
	if overflow || mem > maxAlloc || n < 0 {
		panic(plainError("runtime: allocation size out of range"))
	}
	return mallocgc(mem, typ, true)
}

//go:linkname reflect_unsafe_NewArray reflect.unsafe_NewArray
func reflect_unsafe_NewArray(typ *_type, n int) unsafe.Pointer {
	return newarray(typ, n)
}

func profilealloc(mp *m, x unsafe.Pointer, size uintptr) {
	var c *mcache
	if mp.p != 0 {
		c = mp.p.ptr().mcache
	} else {
		c = mcache0
		if c == nil {
			throw("profilealloc called with no P")
		}
	}
	c.next_sample = nextSample()
	mProf_Malloc(x, size)
}

// nextSample returns the next sampling point for heap profiling. The goal is
// to sample allocations on average every MemProfileRate bytes, but with a
// completely random distribution over the allocation timeline; this
// corresponds to a Poisson process with parameter MemProfileRate. In Poisson
// processes, the distance between two samples follows the exponential
// distribution (exp(MemProfileRate)), so the best return value is a random
// number taken from an exponential distribution whose mean is MemProfileRate.
func nextSample() uintptr {
	if GOOS == "plan9" {
		// Plan 9 doesn't support floating point in note handler.
		if g := getg(); g == g.m.gsignal {
			return nextSampleNoFP()
		}
	}

	return uintptr(fastexprand(MemProfileRate))
}

// fastexprand returns a random number from an exponential指数 distribution with
// the specified mean 均值.
func fastexprand(mean int) int32 {
	// Avoid overflow. Maximum possible step is
	// -ln(1/(1<<randomBitCount)) * mean, approximately 20 * mean.
	switch {
	case mean > 0x7000000:
		mean = 0x7000000
	case mean == 0:
		return 0
	}

	// Take a random sample of the exponential distribution exp(-mean*x).
	// The probability distribution function is mean*exp(-mean*x), so the CDF is
	// p = 1 - exp(-mean*x), so
	// q = 1 - p == exp(-mean*x)
	// log_e(q) = -mean*x
	// -log_e(q)/mean = x
	// x = -log_e(q) * mean
	// x = log_2(q) * (-log_e(2)) * mean    ; Using log_2 for efficiency
	const randomBitCount = 26
	q := fastrand()%(1<<randomBitCount) + 1
	qlog := fastlog2(float64(q)) - randomBitCount
	if qlog > 0 {
		qlog = 0
	}
	const minusLog2 = -0.6931471805599453 // -ln(2)
	return int32(qlog*(minusLog2*float64(mean))) + 1
}

// nextSampleNoFP is similar to nextSample, but uses older,
// simpler code to avoid floating point.
func nextSampleNoFP() uintptr {
	// Set first allocation sample size.
	rate := MemProfileRate
	if rate > 0x3fffffff { // make 2*rate not overflow
		rate = 0x3fffffff
	}
	if rate != 0 {
		return uintptr(fastrand() % uint32(2*rate))
	}
	return 0
}

type persistentAlloc struct {
	base *notInHeap
	off  uintptr
}

var globalAlloc struct {
	mutex
	persistentAlloc
}

// persistentChunkSize is the number of bytes we allocate when we grow
// a persistentAlloc.
const persistentChunkSize = 256 << 10

// persistentChunks is a list of all the persistent chunks we have
// allocated. The list is maintained through the first word in the
// persistent chunk. This is updated atomically.
var persistentChunks *notInHeap

// Wrapper around sysAlloc that can allocate small chunks.
// There is no associated free operation. 没有相关联的 free函数
// Intended for things like function/type/debug-related persistent data持久性的数据.
// If align is 0, uses default align (currently 8).  0 即是8
// The returned memory will be zeroed. 用0初始化
// 分配堆外空间，不需要回收的空间
// Consider marking persistentalloc'd types go:notinheap.
func persistentalloc(size, align uintptr, sysStat *uint64) unsafe.Pointer {
	var p *notInHeap
	systemstack(func() {
		p = persistentalloc1(size, align, sysStat)
	})
	return unsafe.Pointer(p)
}

// Must run on system stack because stack growth can (re)invoke it.
// See issue 9174.
//go:systemstack sysAlloc()
func persistentalloc1(size, align uintptr, sysStat *uint64) *notInHeap {
	const (
		maxBlock = 64 << 10 // VM reservation granularity is 64K on windows
	)

	if size == 0 {
		throw("persistentalloc: size == 0")
	}
	if align != 0 {
		if align&(align-1) != 0 {
			throw("persistentalloc: align is not a power of 2")
		}
		if align > _PageSize {
			throw("persistentalloc: align is too large")
		}
	} else {
		align = 8
	}

	// 2^16
	if size >= maxBlock {
		return (*notInHeap)(sysAlloc(size, sysStat))
	}

	mp := acquirem()
	var persistent *persistentAlloc
	if mp != nil && mp.p != 0 {
		// per-P
		persistent = &mp.p.ptr().palloc
	} else {
		lock(&globalAlloc.mutex)
		// 全局
		persistent = &globalAlloc.persistentAlloc
	}
	persistent.off = alignUp(persistent.off, align)
	if persistent.off+size > persistentChunkSize || persistent.base == nil {
		// 2^18
		persistent.base = (*notInHeap)(sysAlloc(persistentChunkSize, &memstats.other_sys))

		if persistent.base == nil {
			if persistent == &globalAlloc.persistentAlloc {
				unlock(&globalAlloc.mutex)
			}
			throw("runtime: cannot allocate memory")
		}

		// Add the new chunk to the persistentChunks list.
		for {
			chunks := uintptr(unsafe.Pointer(persistentChunks))
			*(*uintptr)(unsafe.Pointer(persistent.base)) = chunks
			if atomic.Casuintptr((*uintptr)(unsafe.Pointer(&persistentChunks)), chunks, uintptr(unsafe.Pointer(persistent.base))) {
				break
			}
		}
		persistent.off = alignUp(sys.PtrSize, align)
	}
	// 基址+已分配偏移
	p := persistent.base.add(persistent.off)
	persistent.off += size

	releasem(mp)
	if persistent == &globalAlloc.persistentAlloc {
		unlock(&globalAlloc.mutex)
	}

	if sysStat != &memstats.other_sys {
		mSysStatInc(sysStat, size)
		mSysStatDec(&memstats.other_sys, size)
	}
	return p
}

// inPersistentAlloc reports whether p points to memory allocated by
// persistentalloc. This must be nosplit because it is called by the
// cgo checker code, which is called by the write barrier code.
//go:nosplit
func inPersistentAlloc(p uintptr) bool {
	chunk := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&persistentChunks)))
	for chunk != 0 {
		if p >= chunk && p < chunk+persistentChunkSize {
			return true
		}
		chunk = *(*uintptr)(unsafe.Pointer(chunk))
	}
	return false
}

// linearAlloc is a simple linear allocator that pre-reserves a region
// of memory and then maps that region into the Ready state as needed. The
// caller is responsible for locking. 调用方负责锁
type linearAlloc struct {
	next   uintptr // 下一个空闲字节的位置 next free byte
	mapped uintptr // 已映射物理内存地址 one byte past end of mapped space
	end    uintptr // 高地址limit end of reserved space
}

func (l *linearAlloc) init(base, size uintptr) {
	if base+size < base {
		// Chop off the last byte. The runtime isn't prepared
		// to deal with situations where the bounds could overflow.
		// Leave that memory reserved, though, so we don't map it
		// later.
		size -= 1
	}
	l.next, l.mapped = base, base
	l.end = base + size
}

// 没有free
func (l *linearAlloc) alloc(size, align uintptr, sysStat *uint64) unsafe.Pointer {
	p := alignUp(l.next, align)
	// 越界，分配不了
	if p+size > l.end {
		return nil
	}
	l.next = p + size
	//
	if pEnd := alignUp(l.next-1, physPageSize); pEnd > l.mapped {
		// Transition from Reserved to Prepared to Ready.
		// 分配物理内存
		// reserved -> prepared
		sysMap(unsafe.Pointer(l.mapped), pEnd-l.mapped, sysStat)
		// prepared -> ready
		sysUsed(unsafe.Pointer(l.mapped), pEnd-l.mapped)
		l.mapped = pEnd
	}
	return unsafe.Pointer(p)
}

// notInHeap is off-heap memory allocated by a lower-level allocator like sysAlloc or persistentAlloc.
// 不受 mheap 分配管理的内存
// In general, it's better to use real types marked as go:notinheap, 一般使用 go:notinheap就可以达到非堆分配的目的
// but this serves as a generic type for situations where that isn't 这作用于通用类型的场景,
// possible (like in the allocators).
//
// TODO: Use this as the return type of sysAlloc, persistentAlloc, etc?
//
//go:notinheap
type notInHeap struct{}

func (p *notInHeap) add(bytes uintptr) *notInHeap {
	return (*notInHeap)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + bytes))
}
