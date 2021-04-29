// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"unsafe"
)

// This file contains the implementation of Go's map type.
//
// A map is just a hash table. 就只是一个 哈希表 实现
// The data is arranged into an array数组 of buckets.
// Each bucket contains up to 8 key/elem pairs.
// The low-order bits地位字节 of the hash are used to select a bucket 用于数组中定位桶.
// Each bucket contains a few high-order bits of each hash to distinguish the entries within a single bucket. 高位字节用于区分一个桶内的key
//
// If more than 8 keys hash to a bucket, we chain on extra buckets. 多出的key, 链接到额外的桶上
//
// When the hashtable grows, we allocate a new array
// of buckets twice as big分配两倍大.
// Buckets are incrementally增量 copied from the old bucket array to the new bucket array.
// map 实现没有 缩容的逻辑??
// 迭代器
// Map iterators walk through the array of buckets 迭代数组 and return the keys in walk order (bucket #, then overflow chain order, then bucket index).
// To maintain iteration semantics, we never move keys within their bucket (if we did, keys might be returned 0 or 2 times). ????
// When growing the table, iterators remain仍然 iterating through the
// old table 扩容时迭代旧table and must而且必须检查新表 check the new table if the bucket
// they are iterating through has been moved ("evacuated") to the new table.  先旧后新

// Picking loadFactor:
// too large and we have lots of overflow buckets,  太大, 使用的溢出桶太多, 操作更慢
// too small and we waste a lot of space更费空间.
// I wrote a simple program to check some stats for different loads: (64-bit, 8 byte keys and elems)
//  loadFactor    %overflow  bytes/entry     hitprobe    missprobe
//        4.00         2.13        20.77         3.00         4.00
//        4.50         4.05        17.30         3.25         4.50
//        5.00         6.85        14.77         3.50         5.00
//        5.50        10.55        12.94         3.75         5.50
//        6.00        15.27        11.67         4.00         6.00
//        6.50        20.90        10.79         4.25         6.50
//        7.00        27.14        10.15         4.50         7.00
//        7.50        34.03         9.73         4.75         7.50
//        8.00        41.10         9.40         5.00         8.00
//
// %overflow   = percentage of buckets which have an overflow bucket
// bytes/entry = overhead bytes used per key/elem pair
// hitprobe    = # of entries to check when looking up a present key 查找存在的key，需要检查多少条目
// missprobe   = # of entries to check when looking up an absent key 查找不存在的key，需要检查多少条目
//
// Keep in mind this data is for maximally loaded tables 记住这是负载最大的情况，一开始，没什么负载, i.e. just
// before the table grows. Typical tables will be somewhat less loaded.

import (
	"runtime/internal/atomic"
	"runtime/internal/math"
	"runtime/internal/sys"
	"unsafe"
)

const (
	// Maximum number of key/elem pairs a bucket can hold.
	bucketCntBits = 3
	// 8
	bucketCnt = 1 << bucketCntBits // 8对kv

	// Maximum average load of a bucket that triggers growth is 6.5.
	// Represent as loadFactorNum/loadFactorDen, to allow integer math.
	// 计算负载因子
	loadFactorNum = 13 // 拆开2个值，以允许整数运算
	loadFactorDen = 2

	// Maximum key or elem size to keep inline (instead of mallocing per element).
	// Must fit in a uint8.
	// Fast versions cannot handle big elems - the cutoff size for
	// fast versions in cmd/compile/internal/gc/walk.go must be at most this elem.
	// key或者value的 大小, 高于这个值, 不会使用优化版本
	maxKeySize  = 128
	maxElemSize = 128

	// data offset should be the size of the bmap struct, but needs to be aligned对齐校准 correctly.
	// For amd64p32 this means 64-bit alignment
	// even though pointers are 32 bit.
	// = 8 应该等于bmap结构体的大小
	dataOffset = unsafe.Offsetof(struct {
		b bmap
		v int64
	}{}.v)

	// Possible tophash values. We reserve a few possibilities for special marks.
	// Each bucket (including its overflow buckets, if any) will have either all or none of its
	// entries in the evacuated* states (except during the evacuate() method, which only happens
	// during map writes and thus no one else can observe the map during that time).
	emptyRest      = 0 // this cell is empty, 并且整个bmap链表当前和之后的cell完全为空, 优化查找, 避免查找大量的空cell and there are no more non-empty cells at higher indexes or overflows.
	emptyOne       = 1 // this cell is empty 当前为空
	evacuatedX     = 2 // 迁移完毕. key在前半部分 key/elem is valid.  Entry has been evacuated to first half of larger table.
	evacuatedY     = 3 // 迁移完毕. key在后半部分 same as above, but evacuated to second half of larger table.
	evacuatedEmpty = 4 // 表示已迁移, 所以当前是空的 cell is empty, bucket is evacuated.
	// 更小的hash [2-4]标记 表示正在迁移
	minTopHash = 5 // 正常cell的最小 tophash minimum tophash for a normal filled cell.

	// map.flags
	iterator     = 1 // 有迭代器在使用当前的bucket there may be an iterator using buckets
	oldIterator  = 2 // there may be an iterator using oldbuckets
	hashWriting  = 4 // a goroutine is writing to the map
	// 不扩容迁移, 因为map不断的put和delete，出现了很多空格，这些空格会导致bmap很长，但是中间有很多空的地方，扫描时间变长。所以第一种扩容实际是一种整理，将数据整理到前面一起
	sameSizeGrow = 8 //  the current map growth is to a new map of the same size  什么情况会触发原地整理??

	// sentinel哨兵 bucket ID for iterator checkss
	noCheck = 1<<(8*sys.PtrSize) - 1 // 2^64-1 0x ff ff ff ff ff ff ff ff
)

// isEmpty reports whether the given tophash array entry represents an empty bucket entry.
// entry is empty  x is tophash
func isEmpty(x uint8) bool {
	return x <= emptyOne
}

// A header for a Go map.
type hmap struct {
	// Note: the format of the hmap is also encoded in cmd/compile/internal/gc/reflect.go.
	// Make sure this stays in sync with the compiler's definition.
	count int   // 元素数量 # live cells == size of map.  Must be first (used by * len() * builtin)
	flags uint8 // 状态标志  it| oldit | writing | sameSizeGrow 四种

	// loadFactor := count/(2^B)
	B         uint8  // 桶长度所占位数，2^B表示hash桶的数量 不包括溢出桶 log_2 of # of buckets (can hold up to loadFactor * 2^B items)
	noverflow uint16 // overflow的桶的近似数量 approximate number of overflow buckets; see incrnoverflow for details
	hash0     uint32 // 随机种子 hash seed

	buckets    unsafe.Pointer // 当前在用的桶  [2^B个桶]bucketsize array of 2^B Buckets. may be nil if count==0.
	oldbuckets unsafe.Pointer // 扩容时执行之前的桶 previous bucket array of half the size, non-nil only when growing
	nevacuate  uintptr        // 迁移计数器，指示进度 progress counter for evacuation (buckets less than< this have been evacuated) 没有等于

	extra *mapextra // optional fields,
}

// mapextra holds fields that are not present on all maps. 不是所有map都需要这些字段
type mapextra struct {
	// If both key and elem do not contain pointers 不包含指针and are inline, then we mark bucket 此类型会被标记为无指针，避免了在这种map上的gc扫描
	// type as containing no pointers. This avoids scanning避免gc扫描 such maps.

	// However, bmap.overflow is a pointer. In order to keep overflow buckets
	// alive 避免被gc清除, we store pointers to all overflow buckets in hmap.extra.overflow and hmap.extra.oldoverflow. 保存所有桶的指针
	// overflow and oldoverflow are only used if key and elem do not contain pointers. 只有key和val都不含指针的时候才用
	// overflow contains overflow buckets for hmap.buckets.
	// oldoverflow contains overflow buckets for hmap.oldbuckets.
	// The indirection间接 allows to store a pointer to the slice in hiter. 间接支持了 map 迭代器
	overflow    *[]*bmap // 执行 buckets 的 溢出桶 todo 这里是如何阻止垃圾回收的??
	oldoverflow *[]*bmap // 指向 oldbuckets 的 overflow桶

	// nextOverflow holds a pointer to a free overflow bucket.
	nextOverflow *bmap // 指向未使用的第一个溢出桶
}

// A bucket for a Go map.
type bmap struct {
	// tophash generally contains the top byte of the hash value for each key in this bucket. 桶中每个key的首个高位字节
	// If tophash[0] < minTopHash, tophash[0] is a bucket evacuation state instead. 如果topHash[0] < 5 表示桶正在迁移
	tophash [bucketCnt]uint8 // 8个字节的数组
	// Followed by bucketCnt keys and then bucketCnt elems. 后面跟8个key, 8个val 大小的内存空间

	// NOTE: packing all the keys together and then all the elems together makes the
	// code a bit more complicated than alternating key/elem/key/elem/...
	// but it allows us to eliminate padding 空白which would be needed for, e.g., map[int64]int8.
	// Followed by an overflow pointer. 再跟溢出桶指针

	// keys   [8]keysize  // key 和val 本身可能存的是真正的key或value的指针, 以控制 keysize/elemsize 不会很大, 不会超过 128
	// values [8]elemsize
	// overflow *bucket
}

// A hash iteration structure.
// If you modify hiter, also change cmd/compile/internal/gc/reflect.go to indicate
// the layout of this structure.
// 结构体大小是 96 = 8 * 9 + 4 + 4 + 8 * 2
type hiter struct {
	key         unsafe.Pointer // Must be in first position.  Write nil to indicate iteration end nil表示迭代器结束 (see cmd/internal/gc/range.go).
	elem        unsafe.Pointer // Must be in second position (see cmd/internal/gc/range.go).
	t           *maptype // map的类型
	h           *hmap   // map的指针
	buckets     unsafe.Pointer // bucket ptr at hash_iter initialization time
	bptr        *bmap          // current bucket
	overflow    *[]*bmap       // keeps overflow buckets of hmap.buckets alive 防止gc回收
	oldoverflow *[]*bmap       // keeps overflow buckets of hmap.oldbuckets alive
	startBucket uintptr        // bucket iteration started at
	offset      uint8          // 桶内的固定起始偏移, 引入随机性 intra-bucket offset to start from during iteration (should be big enough to hold bucketCnt-1)  bmap内的偏移
	wrapped     bool           // already wrapped around from end of bucket array to beginning
	B           uint8
	i           uint8          // 桶内偏移, offset+i是真正的第一个起始偏移
	bucket      uintptr
	checkBucket uintptr
}

// bucketShift returns 1<<b, optimized for code generation.
// 1 << (b % 64) 避免移动64位以上，1都移没了
func bucketShift(b uint8) uintptr {
	// Masking the shift amount allows overflow checks to be elided.省略
	return uintptr(1) << (b & (sys.PtrSize*8 - 1))
}

// bucketMask returns 1<<b - 1, optimized for code generation.
// if b == 3, 0000 1000 -> 0000 0111
func bucketMask(b uint8) uintptr {
	return bucketShift(b) - 1
}

// tophash calculates the tophash value for hash.
func tophash(hash uintptr) uint8 {
	// 高8位bit, top byte of hash
	top := uint8(hash >> (sys.PtrSize*8 - 8))
	if top < minTopHash { // top < 5
		top += minTopHash
	}
	// 0xf4633f636f1f0a0b -> 56 = 0x 00 00 00 00 00 00 00 f4
	return top
}

// 已迁移完毕 empty -> evacuatedEmpty 才算迁移完
func evacuated(b *bmap) bool {
	h := b.tophash[0]
	// evacuatedX = 2, evacuatedY=3, evacuatedEmpty=4
	return h > emptyOne && h < minTopHash
}

// return b.overflow
func (b *bmap) overflow(t *maptype) *bmap {
	return *(**bmap)(add(unsafe.Pointer(b), uintptr(t.bucketsize)-sys.PtrSize)) // 减掉一个指针, 是overflow 指针的偏移地址
}

// b.overflow = ovf
func (b *bmap) setoverflow(t *maptype, ovf *bmap) {
	*(**bmap)(add(unsafe.Pointer(b), uintptr(t.bucketsize)-sys.PtrSize)) = ovf
}

// bmap大小偏移后就是key 的起始地址
func (b *bmap) keys() unsafe.Pointer {
	return add(unsafe.Pointer(b), dataOffset)
}

// incrnoverflow increments h.noverflow.
// noverflow counts the number of overflow buckets.
// This is used to trigger same-size map growth. 用于触发原大小扩容整理
// See also tooManyOverflowBuckets.
// To keep hmap small, noverflow is a uint16. 优化空间, 牺牲时间
// When there are few buckets, noverflow is an exact count.
// When there are many buckets, noverflow is an approximate count.
func (h *hmap) incrnoverflow() {
	// We trigger same-size map growth if there are
	// as many overflow buckets as buckets.
	// We need to be able to count to 1<<h.B.
	// 精确
	if h.B < 16 {
		h.noverflow++  // 只有2B, 16位
		return
	}
	// Increment with probability 1/(1<<(h.B-15)).
	// When we reach 1<<15 - 1, we will have approximately
	// as many overflow buckets as buckets.
	mask := uint32(1)<<(h.B-15) - 1
	// Example: if h.B == 18, then mask == 7,
	// and fastrand & 7 == 0 with probability 1/8.
	// fastrand % 8 == 0 概率是8分之一
	if fastrand()&mask == 0 { // 2 ^ (B - 15) 分之 1 的概率
		h.noverflow++
	}
}

// 从溢出桶里, 分配新的bmap出来
func (h *hmap) newoverflow(t *maptype, b *bmap) *bmap {
	var ovf *bmap
	if h.extra != nil && h.extra.nextOverflow != nil {
		// We have preallocated overflow buckets available.
		// See makeBucketArray for more details.
		ovf = h.extra.nextOverflow
		if ovf.overflow(t) == nil { // 表示可以分配
			// 还没有到预分配的溢出桶的结尾，向下偏移一段
			// We're not at the end of the preallocated overflow buckets. Bump the pointer.
			// 两种技术来加快内存分配。他们分别是是”bump-the-pointer“和“TLABs（Thread-Local Allocation Buffers）”。 Bump-the-pointer技术跟踪
			// 移动到下一个bmap
			h.extra.nextOverflow = (*bmap)(add(unsafe.Pointer(ovf), uintptr(t.bucketsize)))
		} else {
			// This is the last preallocated overflow bucket. 最后一个 预先分配的 溢出桶
			// Reset the overflow pointer on this bucket,
			// which was set to a non-nil sentinel value. 之前是设置指向哨兵, 现在重设置为nil, 当做普通的溢出桶
			ovf.setoverflow(t, nil)

			h.extra.nextOverflow = nil // 置nil 表示预分配溢出桶已经用完了
		}
	} else {
		// 没有溢出桶可用, 则直接临时分配新的bmap内存
		ovf = (*bmap)(newobject(t.bucket))
	}
	// 不管是哪里拿的, 都增加溢出桶计数
	h.incrnoverflow()

	if t.bucket.ptrdata == 0 {
		// 分配overflow数组，指向已占用的溢出桶，防止被垃圾回收
		h.createOverflow()
		// 将指针添加到overflow数组, 防止被垃圾回收??, 执行 当前 bucket 之后的
		*h.extra.overflow = append(*h.extra.overflow, ovf)
	}

	// 记录下一个桶, 链表, 链接起来
	// b.oveflow = ovf
	b.setoverflow(t, ovf)
	return ovf
}

// just init
func (h *hmap) createOverflow() {
	if h.extra == nil {
		h.extra = new(mapextra)
	}
	if h.extra.overflow == nil {
		h.extra.overflow = new([]*bmap)
	}
}

func makemap64(t *maptype, hint int64, h *hmap) *hmap {
	// 非64位
	if int64(int(hint)) != hint {
		// 32位， 如果太大， 不预先分配空间
		hint = 0
	}
	return makemap(t, int(hint), h)
}

// hint表示提示大小
// makemap_small implements Go map creation for make(map[k]v) 没有hint时会用这个版本？？ and
// make(map[k]v, hint) when hint is known to be at most bucketCnt hint <= 8 编译时已知
// at compile time and the map needs to be allocated on the heap. 需要分配在堆上
func makemap_small() *hmap {
	h := new(hmap) // 不需要提前
	h.hash0 = fastrand()
	return h
}

// makemap implements Go map creation for make(map[k]v, hint).
// If the compiler has determined that the map or the first bucket
// can be created on the stack 栈上创建, h and/or bucket may be non-nil. h可以被复用

// If h != nil, the map can be created directly in h.
// If h.buckets != nil, bucket pointed to can be used as the first bucket.
// t = type.map[string]int(SB)
func makemap(t *maptype, hint int, h *hmap) *hmap {
	// hint 数量， size尺寸
	mem, overflow := math.MulUintptr(uintptr(hint), t.bucket.size) // 这个size是什么size？
	if overflow || mem > maxAlloc {
		hint = 0 // 溢出了，直接按需分配
	}
	// 计算预先需要分配多少entry空间

	// initialize Hmap
	if h == nil {
		h = new(hmap)
	}
	// 随机种子
	h.hash0 = fastrand()

	// Find the size parameter B which will hold the requested # of elements.
	// For hint < 0 overLoadFactor returns false since hint < bucketCnt.
	// 下面的代码都可以赋值的时候， 临时分配
	B := uint8(0)
	// 根据负载因子，计算需要的桶数量
	// hint/8 > 2^B
	for overLoadFactor(hint, B) {
		B++
	}
	// count/b <= 2^B
	h.B = B

	// allocate initial hash table
	// if B == 0, the buckets field is allocated lazily later (in mapassign)
	// If hint is large zeroing this memory could take a while.
	if h.B != 0 {
		var nextOverflow *bmap
		// 预先分配空间，返回首地址和溢出桶首地址
		h.buckets, nextOverflow = makeBucketArray(t, h.B, nil)
		if nextOverflow != nil {
			h.extra = new(mapextra)
			// 指向溢出桶
			h.extra.nextOverflow = nextOverflow  //保存溢出桶指针
		}
	}

	return h
}

// makeBucketArray initializes a backing底 array for map buckets.
// 1<<b is the minimum最小 number of buckets to allocate.
// dirtyalloc should either be nil or a bucket array previously 或者之前分配的桶数组
// allocated by makeBucketArray with the same t and b parameters.

// 重用
// If dirtyalloc is nil a new backing array will be alloced and
// otherwise dirtyalloc will be cleared and reused as backing array.
func makeBucketArray(t *maptype, b uint8, dirtyalloc unsafe.Pointer) (buckets unsafe.Pointer, nextOverflow *bmap) {
	// 2^(h.B + 1)
	base := bucketShift(b)
	nbuckets := base
	// For small b, overflow buckets are unlikely.
	// Avoid the overhead of the calculation.
	// 额外创建 2^(b-4) 也就是 2^B / 16 个溢出桶
	if b >= 4 {
		// Add on the estimated number of overflow buckets
		// required to insert the median number of elements
		// used with this value of b.
		nbuckets += bucketShift(b - 4)
		sz := t.bucket.size * nbuckets
		// 返回mallocgc()实际会分配的内存大小
		up := roundupsize(sz)
		if up != sz {
			nbuckets = up / t.bucket.size
		}
	}

	if dirtyalloc == nil {
		// 创建新桶数组，得到指针
		buckets = newarray(t.bucket, int(nbuckets))
	} else {  // mapclear 的时候， 才 != nil
		// dirtyalloc was previously generated by
		// the above newarray(t.bucket, int(nbuckets))
		// but may not be empty.
		buckets = dirtyalloc
		size := t.bucket.size * nbuckets

		// 清零内存
		if t.bucket.ptrdata != 0 {
			// 堆上
			memclrHasPointers(buckets, size)
		} else {
			memclrNoHeapPointers(buckets, size)
		}
	}

	// base是应该要分配的bmap数量, 如果不相等, 多出来的作为溢出桶
	if base != nbuckets {
		// We preallocated some overflow buckets.
		// To keep the overhead of tracking these overflow buckets to a minimum,
		// we use the convention that if a preallocated overflow bucket's overflow
		// pointer is nil, then there are more available by bumping the pointer.
		// We need a safe non-nil pointer for the last overflow bucket; just use buckets.
		// 溢出桶指向基本桶之后的一个桶
		nextOverflow = (*bmap)(add(buckets, base*uintptr(t.bucketsize)))

		last := (*bmap)(add(buckets, (nbuckets-1)*uintptr(t.bucketsize)))
		// 最后一个溢出桶指向第一个桶, 哨兵桶
		last.setoverflow(t, (*bmap)(buckets))
	}
	return buckets, nextOverflow
}

// mapaccess1 returns a pointer to h[key].  Never returns nil, instead
// it will return a reference to the zero object for the elem type if
// the key is not in the map.
// NOTE: The returned pointer may keep the whole map live, so don't
// hold onto it for very long.
// val := map[key]
// 返回value指针，汇编函数会进行取地址, *引用
func mapaccess1(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapaccess1)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}

	// map为空，返回默认值
	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			t.hasher(key, 0) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0])
	}

	// 有线程写, 并发访问, 直接panic
	if h.flags&hashWriting != 0 { //允许并发读，但不允许并发读写
		// exit()直接退出
		throw("concurrent map read and map write")
	}

	// 根据key和种子计算hash
	hash := t.hasher(key, uintptr(h.hash0))
	// 桶idx 掩码  2^B - 1
	m := bucketMask(h.B)
	// 指针运算确定桶地址,  hash % 2^B
	b := (*bmap)(add(h.buckets, (hash&m)*uintptr(t.bucketsize)))

	// 如果在扩容
	if c := h.oldbuckets; c != nil {
		// 原地扩容，实际上是原地整理
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			// m / 2
			m >>= 1 //老的桶，只有一半数量的桶
		}
		// 找到老桶子里的低位置桶
		oldb := (*bmap)(add(c, (hash&m)*uintptr(t.bucketsize)))
		if !evacuated(oldb) { //如果没有迁移完，就在老桶子里找
			b = oldb
		}
	}

	// 前8位
	top := tophash(hash)
bucketloop:
	// overflow指向下一个桶
	for ; b != nil; b = b.overflow(t) {
		// 循环桶里的每个格子
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top { // 格子不对
				if b.tophash[i] == emptyRest { // 说明后面的都是空的，不用找了
					break bucketloop
				}

				continue // 找下一个cell
			}

			// 对应的格子
			// 拿到key指针
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() { // 如果key是一个大的结构体, 最好保存结构体变量内存所在的地址
				k = *((*unsafe.Pointer)(k))
			}

			// key匹配
			if t.key.equal(key, k) {
				// 拿到对应的元素指针
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
				if t.indirectelem() {
					e = *((*unsafe.Pointer)(e))
				}
				return e
			}

			// 否则继续下一个格子
		}
	}
	// 返回 零值
	return unsafe.Pointer(&zeroVal[0]) // &[1024]byte
}

// val, ok := map[key]
func mapaccess2(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, bool) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapaccess2)
		racereadpc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}

	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			t.hasher(key, 0) // see issue 23734
		}
		return unsafe.Pointer(&zeroVal[0]), false
	}

	if h.flags&hashWriting != 0 {
		throw("concurrent map read and map write")
	}

	hash := t.hasher(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(unsafe.Pointer(uintptr(h.buckets) + (hash&m)*uintptr(t.bucketsize)))
	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(unsafe.Pointer(uintptr(c) + (hash&m)*uintptr(t.bucketsize)))
		if !evacuated(oldb) {  //需要保证， 迁移都是一个bmap 完整的迁移
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}

			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				k = *((*unsafe.Pointer)(k))
			}

			if t.key.equal(key, k) {
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
				if t.indirectelem() {
					e = *((*unsafe.Pointer)(e))
				}
				return e, true
			}
		}
	}
	return unsafe.Pointer(&zeroVal[0]), false
}

// 通过key, 查找返回key和val，用于for range迭代器
// returns both key and elem. Used by map iterator
func mapaccessK(t *maptype, h *hmap, key unsafe.Pointer) (unsafe.Pointer, unsafe.Pointer) {
	if h == nil || h.count == 0 {
		return nil, nil
	}

	hash := t.hasher(key, uintptr(h.hash0))
	m := bucketMask(h.B)
	b := (*bmap)(unsafe.Pointer(uintptr(h.buckets) + (hash&m)*uintptr(t.bucketsize)))

	if c := h.oldbuckets; c != nil {
		if !h.sameSizeGrow() {
			// There used to be half as many buckets; mask down one more power of two.
			m >>= 1
		}
		oldb := (*bmap)(unsafe.Pointer(uintptr(c) + (hash&m)*uintptr(t.bucketsize)))
		if !evacuated(oldb) {
			b = oldb
		}
	}
	top := tophash(hash)
bucketloop:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				k = *((*unsafe.Pointer)(k))
			}
			if t.key.equal(key, k) {
				e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
				if t.indirectelem() {
					e = *((*unsafe.Pointer)(e))
				}
				return k, e
			}
		}
	}
	return nil, nil
}

// 包装下，未找到返回参数zero指定的零值, 而不是 zeroVal
func mapaccess1_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) unsafe.Pointer {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero
	}
	return e
}

// 包装下，未找到返回参数zero指定的值，和exist
func mapaccess2_fat(t *maptype, h *hmap, key, zero unsafe.Pointer) (unsafe.Pointer, bool) {
	e := mapaccess1(t, h, key)
	if e == unsafe.Pointer(&zeroVal[0]) {
		return zero, false
	}
	return e, true
}

// Like mapaccess, but allocates a slot for the key if it is not present in the map.
//	 返回一个存放value的地址, 赋值由编译器生成的汇编语句完成
func mapassign(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	if h == nil {
		panic(plainError("assignment to entry in nil map"))
	}

	if raceenabled {
		callerpc := getcallerpc()
		pc := funcPC(mapassign)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled {
		msanread(key, t.key.size)
	}

	// 正在写hash
	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}
	// 计算hash
	hash := t.hasher(key, uintptr(h.hash0))

	// Set hashWriting after calling t.hasher, since t.hasher may panic,
	// in which case we have not actually done a write.
	// 正在写hash
	h.flags ^= hashWriting

	// 没有预分配空间的情况 例如 makemap_small函数
	if h.buckets == nil { // 这种情况 h.B 是多少??
		h.buckets = newobject(t.bucket) // newarray(t.bucket, 1)
	}

again:
	// 桶idx
	bucket := hash & bucketMask(h.B)
	if h.growing() {
		// 对当前桶迁移，下面再写入, 这样只需要写入新桶就可以
		growWork(t, h, bucket)  // 一个bmap的数据，要么在新桶，要么在老桶，不会有中间状态
	}
	// 桶指针
	b := (*bmap)(unsafe.Pointer(uintptr(h.buckets) + bucket*uintptr(t.bucketsize)))
	top := tophash(hash)

	var inserti *uint8         // tophash插入位置, 只是用来判空, 其他没用
	var insertk unsafe.Pointer // key插入位置
	var elem unsafe.Pointer
bucketloop:
	for {
		// 遍历格子
		for i := uintptr(0); i < bucketCnt; i++ { // 优先找tophash 匹配的空key, 找不到才赋值key, 并覆盖原因的topHash
			if b.tophash[i] != top {
				if isEmpty(b.tophash[i]) && inserti == nil {  //找第一个无分配的slot， 但是依旧要继续遍历，找tophash匹配的， 看是否有重复的key，这种情况需要覆盖value
					inserti = &b.tophash[i]
					insertk = add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
					elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
				}
				if b.tophash[i] == emptyRest {
					break bucketloop
				}
				continue
			}

			// tophash[i] == top
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			if t.indirectkey() {
				k = *((*unsafe.Pointer)(k))
			}

			// 保存了其他key, 继续找下一个
			if !t.key.equal(key, k) {
				continue
			}

			// already have a mapping for key. Update it.
			if t.needkeyupdate() {
				// 拷贝值过去
				typedmemmove(t.key, k, key) // key -> k
			}
			elem = add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
			// 找到元素，直接修改，不用新增
			goto done
		}

		// 遍历链表下一个节点
		ovf := b.overflow(t)
		if ovf == nil {
			break
		}
		b = ovf
	}

	// map没找到key, 需要分配新的空间添加新的条目
	// Did not find mapping for key. Allocate new cell & add entry.

	// If we hit the max load factor or we have too many overflow buckets,
	// and we're not already in the middle of growing, start growing.
	// 不是正在扩容时, 并且负载因子超标时 or 溢出桶太多, 就需要进行扩容
	if !h.growing() && (overLoadFactor(h.count+1, h.B) || tooManyOverflowBuckets(h.noverflow, h.B)) {
		hashGrow(t, h)
		goto again // Growing the table 失效invalidates everything, so try again
	}

	if inserti == nil {
		// all current buckets are full, allocate a new one.
		// 分配一个新桶
		newb := h.newoverflow(t, b)
		// key首字节
		inserti = &newb.tophash[0]
		// key地址, 直接用第一个key的地址
		insertk = add(unsafe.Pointer(newb), dataOffset)
		// 元素地址
		elem = add(insertk, bucketCnt*uintptr(t.keysize))
	}

	// store new key/elem at insert position
	if t.indirectkey() {
		// key指向新分配的空间
		kmem := newobject(t.key)
		*(*unsafe.Pointer)(insertk) = kmem // 保存地址
		insertk = kmem
	}
	if t.indirectelem() {
		// 分配value的地址空间
		vmem := newobject(t.elem)
		*(*unsafe.Pointer)(elem) = vmem
	}

	typedmemmove(t.key, insertk, key) // 拷贝key到新分配的空间
	*inserti = top                    // 插入了 key, 保存 key的 topHash
	// 元素数量+1
	h.count++

done:
	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	h.flags &^= hashWriting // 置0, 指定位

	if t.indirectelem() {
		elem = *((*unsafe.Pointer)(elem))
	}
	// 返回value可插入的指针，此时value还未插入
	return elem
}

func mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapdelete)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
		raceReadObjectPC(t.key, key, callerpc, pc)
	}
	if msanenabled && h != nil {
		msanread(key, t.key.size)
	}

	// 判空
	if h == nil || h.count == 0 {
		if t.hashMightPanic() {
			// 0 是 seed
			t.hasher(key, 0) // see issue 23734
		}
		return
	}

	//
	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}

	hash := t.hasher(key, uintptr(h.hash0))

	// Set hashWriting after calling t.hasher, since t.hasher may panic,
	// in which case we have not actually done a write (delete).
	h.flags ^= hashWriting

	bucket := hash & bucketMask(h.B)

	// 先迁移
	if h.growing() {
		growWork(t, h, bucket)
	}

	b := (*bmap)(add(h.buckets, bucket*uintptr(t.bucketsize)))
	bOrig := b
	top := tophash(hash)
search:
	for ; b != nil; b = b.overflow(t) {
		for i := uintptr(0); i < bucketCnt; i++ {
			if b.tophash[i] != top {
				if b.tophash[i] == emptyRest {
					break search
				}
				continue
			}

			// 对应key首部
			k := add(unsafe.Pointer(b), dataOffset+i*uintptr(t.keysize))
			k2 := k
			if t.indirectkey() {
				k2 = *((*unsafe.Pointer)(k2))
			}

			// key匹配
			if !t.key.equal(key, k2) {
				continue
			}

			// 清除key
			// Only clear key if there are pointers in it.
			if t.indirectkey() {
				// 删除元素
				*(*unsafe.Pointer)(k) = nil
			} else if t.key.ptrdata != 0 {
				// 清除内存
				memclrHasPointers(k, t.key.size)
			}

			// 清除value
			e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+i*uintptr(t.elemsize))
			if t.indirectelem() {
				*(*unsafe.Pointer)(e) = nil
			} else if t.elem.ptrdata != 0 {
				memclrHasPointers(e, t.elem.size)
			} else {
				memclrNoHeapPointers(e, t.elem.size)
			}

			// 更新 tophash标志
			b.tophash[i] = emptyOne
			// If the bucket now ends in a bunch of emptyOne states,
			// change those to emptyRest states.
			// It would be nice to make this a separate function, but
			// for loops are not currently inlineable.

			// 如果下一个元素不是 emptyRest, 那么跳过
			if i == bucketCnt-1 {
				if b.overflow(t) != nil && b.overflow(t).tophash[0] != emptyRest {
					goto notLast
				}
			} else {
				if b.tophash[i+1] != emptyRest {
					goto notLast
				}
			}

			for { // 将 当前和前面的cell都标记为 emptyRest
				b.tophash[i] = emptyRest
				if i == 0 {
					if b == bOrig {
						break // beginning of initial bucket, we're done.
					}
					// Find previous bucket, continue at its last entry.
					c := b
					// 从头开始找，找到b的上一个bmap
					for b = bOrig; b.overflow(t) != c; b = b.overflow(t) {
					}
					// 指向最后的一个元素
					i = bucketCnt - 1
				} else {
					i--
				}
				if b.tophash[i] != emptyOne {
					break
				}
			}
		notLast:
			// 删除了一个元素
			h.count--
			// 退出循环
			break search
		}
	}

	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	// 清除标志
	h.flags &^= hashWriting
}

// mapiterinit initializes the hiter struct used for ranging over maps.
// The hiter struct pointed to by 'it' is allocated on the stack
// by the compilers order pass or on the heap by reflect_mapiterinit.
// Both need to have zeroed hiter since the struct contains pointers.
// 初始化 map迭代器结构，开始一次迭代
func mapiterinit(t *maptype, h *hmap, it *hiter) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(mapiterinit))
	}

	// 没有元素
	if h == nil || h.count == 0 {
		return
	}

	// 96B
	if unsafe.Sizeof(hiter{})/sys.PtrSize != 12 {
		throw("hash_iter size incorrect") // see cmd/compile/internal/gc/reflect.go
	}

	// type
	it.t = t
	// hmap
	it.h = h

	// grab snapshot of bucket state
	it.B = h.B
	it.buckets = h.buckets

	if t.bucket.ptrdata == 0 {
		// Allocate the current slice and remember pointers to both current and old.
		// This preserves all relevant overflow buckets alive even if
		// the table grows and/or overflow buckets are added to the table
		// while we are iterating.
		h.createOverflow()
		it.overflow = h.extra.overflow
		it.oldoverflow = h.extra.oldoverflow
	}

	// decide where to start
	r := uintptr(fastrand()) // 引入随机性
	if h.B > 31-bucketCntBits {
		r += uintptr(fastrand()) << 31
	}
	// 起始桶
	it.startBucket = r & bucketMask(h.B)

	// 不是从0开始, 起始偏移也引入了随机性  位移的优先级更高
	it.offset = uint8(r >> h.B & (bucketCnt - 1))

	// iterator state
	it.bucket = it.startBucket

	// Remember we have an iterator.
	// Can run concurrently并发 with another mapiterinit().
	if old := h.flags; old&(iterator|oldIterator) != iterator|oldIterator {
		atomic.Or8(&h.flags, iterator|oldIterator) // 加上迭代器标记
	}

	mapiternext(it) // 初始化就迭代一次
}

func mapiternext(it *hiter) {
	h := it.h
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(mapiternext))
	}
	if h.flags&hashWriting != 0 {
		throw("concurrent map iteration and map write")
	}

	t := it.t
	bucket := it.bucket
	b := it.bptr  // 这个字段初始化的时候没有赋值
	i := it.i
	checkBucket := it.checkBucket

next:  // 上一个已经 迭代完毕, 需要寻找新的通来迭代
	if b == nil {
		// 迭代完毕
		if bucket == it.startBucket && it.wrapped {
			// end of iteration
			it.key = nil
			it.elem = nil
			return  // key 和 elum == nil, 表示迭代结束
		}

		// 扩容中,
		if h.growing() && it.B == h.B {
			// 扩容过程中迭代, 先判断是否在老桶中迭代, 再判断新
			// Iterator was started in the middle of a grow, and the grow isn't done yet.
			// If the bucket we're looking at hasn't been filled in yet (i.e. the old
			// bucket hasn't been evacuated) then we need to iterate through the old
			// bucket and only return the ones that will be migrated to this bucket.
			oldbucket := bucket & it.h.oldbucketmask()
			b = (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.bucketsize)))
			// 未迁移完毕
			if !evacuated(b) {
				// 老桶
				checkBucket = bucket
			} else {
				// 新桶
				b = (*bmap)(add(it.buckets, bucket*uintptr(t.bucketsize)))
				checkBucket = noCheck
			}
		} else {
			// 直接迭代新桶
			b = (*bmap)(add(it.buckets, bucket*uintptr(t.bucketsize)))
			checkBucket = noCheck
		}

		// 递增桶索引，换下一idx的桶
		bucket++
		// 循环从头开始
		if bucket == bucketShift(it.B) {
			bucket = 0
			// 表示回退到0过一次
			it.wrapped = true
		}
		i = 0
	}

	//
	for ; i < bucketCnt; i++ {
		offi := (i + it.offset) & (bucketCnt - 1) // 取模, 回绕
		if isEmpty(b.tophash[offi]) || b.tophash[offi] == evacuatedEmpty {
			// TODO: emptyRest is hard to use here, as we start iterating
			// in the middle of a bucket. It's feasible, just tricky.
			continue
		}

		// key
		k := add(unsafe.Pointer(b), dataOffset+uintptr(offi)*uintptr(t.keysize))
		if t.indirectkey() {
			k = *((*unsafe.Pointer)(k))
		}
		// value
		e := add(unsafe.Pointer(b), dataOffset+bucketCnt*uintptr(t.keysize)+uintptr(offi)*uintptr(t.elemsize))

		if checkBucket != noCheck && !h.sameSizeGrow() {  // 这两个条件, 说明在扩容中, 需要在旧桶中迭代
			// Special case: iterator was started during a grow to a larger size
			// and the grow is not done yet. We're working on a bucket whose
			// oldbucket has not been evacuated yet. Or at least, it wasn't
			// evacuated when we started the bucket. So we're iterating
			// through the oldbucket, skipping any keys that will go
			// to the other new bucket (each oldbucket expands to two
			// buckets during a grow).
			if t.reflexivekey() || t.key.equal(k, k) {
				// If the item in the oldbucket is not destined for
				// the current new bucket in the iteration, skip it.
				hash := t.hasher(k, uintptr(h.hash0))
				if hash&bucketMask(it.B) != checkBucket {
					continue
				}
			} else {
				// Hash isn't repeatable if k != k (NaNs).  We need a
				// repeatable and randomish choice of which direction
				// to send NaNs during evacuation. We'll use the low
				// bit of tophash to decide which way NaNs go.
				// NOTE: this case is why we need two evacuate tophash
				// values, evacuatedX and evacuatedY, that differ in
				// their low bit.
				if checkBucket>>(it.B-1) != uintptr(b.tophash[offi]&1) {
					continue
				}
			}
		}

		if (b.tophash[offi] != evacuatedX && b.tophash[offi] != evacuatedY) ||
			!(t.reflexivekey() || t.key.equal(k, k)) { // 还在老桶里
			// This is the golden data, we can return it.
			// OR
			// key!=key, so the entry can't be deleted or updated, so we can just return it.
			// That's lucky for us because when key!=key we can't look it up successfully.
			it.key = k
			if t.indirectelem() {
				e = *((*unsafe.Pointer)(e))
			}
			it.elem = e
		} else {
			// The hash table has grown since the iterator was started.
			// The golden data for this key is now somewhere else.
			// Check the current hash table for the data.
			// This code handles the case where the key
			// has been deleted, updated, or deleted and reinserted.
			// NOTE: we need to regrab the key as it has potentially been
			// updated to an equal() but not identical key (e.g. +0.0 vs -0.0).
			// 已经被疏散到新 map内存空间, 直接去新桶找
			rk, re := mapaccessK(t, h, k)
			if rk == nil {
				continue // key has been deleted
			}
			it.key = rk
			it.elem = re
		}
		// 更新迭代器
		it.bucket = bucket
		if it.bptr != b { // avoid unnecessary write barrier; see issue 14921
			it.bptr = b  // 记录当前在查找的*bmap
		}
		it.i = i + 1
		it.checkBucket = checkBucket
		return // 除了迭代结束, 就只有这里return了
	}
	// 上一个桶没找到，根据overflow指针，得到下一个桶
	b = b.overflow(t)
	// 格子偏移重置
	i = 0
	goto next
}

// mapclear deletes all keys from a map.
// 清除所有kv，不是删除map
func mapclear(t *maptype, h *hmap) {
	if raceenabled && h != nil {
		callerpc := getcallerpc()
		pc := funcPC(mapclear)
		racewritepc(unsafe.Pointer(h), callerpc, pc)
	}

	if h == nil || h.count == 0 {
		return
	}

	if h.flags&hashWriting != 0 {
		throw("concurrent map writes")
	}

	h.flags ^= hashWriting
	// 去掉标记
	h.flags &^= sameSizeGrow
	h.oldbuckets = nil  // 都清了， 老桶里的数据自然页不需要了
	h.nevacuate = 0
	h.noverflow = 0
	// 清空
	h.count = 0

	// Keep the mapextra allocation but clear any extra information.
	if h.extra != nil {
		*h.extra = mapextra{}  // 结构体置0
	}

	// makeBucketArray clears the memory pointed to by h.buckets
	// and recovers any overflow buckets by generating them
	// as if h.buckets was newly alloced.
	// 内存重置为零值
	_, nextOverflow := makeBucketArray(t, h.B, h.buckets)
	if nextOverflow != nil {
		// If overflow buckets are created then h.extra
		// will have been allocated during initial bucket creation.
		h.extra.nextOverflow = nextOverflow  //保留内存的引用， 不释放
	}

	if h.flags&hashWriting == 0 {
		throw("concurrent map writes")
	}
	h.flags &^= hashWriting
}

// 扩容的入口, 不在迁移状态， 才允许新的扩容，
// 插入不允许并发，如果当前插入需要扩容，会等扩容完成后再插入，不会导致 写入快过 疏散的情况
func hashGrow(t *maptype, h *hmap) {
	// If we've hit the load factor, get bigger.
	// Otherwise, there are too many overflow buckets,
	// so keep the same number of buckets and "grow" laterally.
	bigger := uint8(1)
	if !overLoadFactor(h.count+1, h.B) {
		// 非过载，不扩容
		bigger = 0
		h.flags |= sameSizeGrow
	}

	oldbuckets := h.buckets
	// 分配新空间, nextOverflow == true 表示有溢出桶
	newbuckets, nextOverflow := makeBucketArray(t, h.B+bigger, nil)

	flags := h.flags &^ (iterator | oldIterator)
	if h.flags&iterator != 0 {
		flags |= oldIterator  //
	}

	// commit the grow (atomic wrt gc)
	h.B += bigger
	h.flags = flags
	h.oldbuckets = oldbuckets
	h.buckets = newbuckets
	h.nevacuate = 0
	h.noverflow = 0

	if h.extra != nil && h.extra.overflow != nil {
		// Promote current overflow buckets to the old generation.
		if h.extra.oldoverflow != nil {  // 不等于 nil， 说明还没疏散完
			throw("oldoverflow is not nil")
		}
		h.extra.oldoverflow = h.extra.overflow
		h.extra.overflow = nil
	}

	if nextOverflow != nil {
		if h.extra == nil {
			h.extra = new(mapextra)
		}
		h.extra.nextOverflow = nextOverflow
	}

	// 实际的拷贝工作由growWork() 和 evacuate() 增量完成
	// the actual copying of the hash table data is done incrementally
	// by growWork() and evacuate().
}

// overLoadFactor reports whether count items placed in 1<<B buckets is over loadFactor.
// count > 8 && count >  13 * (2^B / 2)
func overLoadFactor(count int, B uint8) bool {
	return count > bucketCnt && uintptr(count) > loadFactorNum*(bucketShift(B)/loadFactorDen)
}

// tooManyOverflowBuckets reports whether noverflow buckets is too many for a map with 1<<B buckets.
// 判断溢出桶的数量是不是太多
// Note that most of these overflow buckets must be in sparse稀疏 use;
// if use was dense, then we'd have already triggered regular map growth. 触发正常的扩容
func tooManyOverflowBuckets(noverflow uint16, B uint8) bool {
	// If the threshold is too low, we do extraneous work.
	// If the threshold is too high, maps that grow and shrink can hold on to lots of unused memory.
	// "too many" means (approximately) as many overflow buckets as regular buckets.
	// See incrnoverflow for more details.
	if B > 15 {
		B = 15
	}
	// The compiler doesn't see here that B < 16; mask B to generate shorter shift code.
	return noverflow >= uint16(1)<<(B&15) // 1<< 15 32K // 溢出桶超过基础桶的一倍
}

// growing reports whether h is growing. The growth may be to the same size or bigger.
// 是否 还是在迁移当中
func (h *hmap) growing() bool {
	return h.oldbuckets != nil
}

// sameSizeGrow reports whether the current growth is to a map of the same size.
func (h *hmap) sameSizeGrow() bool {
	return h.flags&sameSizeGrow != 0
}

// noldbuckets calculates the number of buckets prior to the current map growth.
// 老桶的
func (h *hmap) noldbuckets() uintptr {
	oldB := h.B
	if !h.sameSizeGrow() {
		oldB--
	}
	return bucketShift(oldB)
}

// oldbucketmask provides a mask that can be applied to calculate n % noldbuckets().
func (h *hmap) oldbucketmask() uintptr {
	return h.noldbuckets() - 1
}

// 继续进行扩容工作， 调用前必须打上写标志
func growWork(t *maptype, h *hmap, bucket uintptr) { // bucket 是桶 id
	// make sure we evacuate the oldbucket corresponding
	// to the bucket we're about to use
	// 迁移指定老桶
	evacuate(t, h, bucket&h.oldbucketmask())

	// evacuate one more oldbucket to make progress on growing
	// 迁移更多老桶
	if h.growing() { // 调用方 会判断条件, 但是上一行迁移了一个桶, 需要额外再判断一次
		evacuate(t, h, h.nevacuate)
	}
}

// 是否已迁移
func bucketEvacuated(t *maptype, h *hmap, bucket uintptr) bool {
	b := (*bmap)(add(h.oldbuckets, bucket*uintptr(t.bucketsize)))
	return evacuated(b)
}

// evacDst is an evacuation destination.
type evacDst struct {
	b *bmap          // current destination bucket
	i int            // key/elem index into b
	k unsafe.Pointer // pointer to current key storage
	e unsafe.Pointer // pointer to current elem storage
}

// 继续进行扩容，将元素疏散到新的桶里，以单个桶为单位，一个一个
func evacuate(t *maptype, h *hmap, oldbucket uintptr) {
	b := (*bmap)(add(h.oldbuckets, oldbucket*uintptr(t.bucketsize)))
	newbit := h.noldbuckets()

	// 未迁移完
	if !evacuated(b) {
		// TODO: reuse overflow buckets instead of using new ones, if there
		// is no iterator using the old buckets.  (If !oldIterator.)

		// xy contains the x and y (low and high) evacuation destinations.
		var xy [2]evacDst
		x := &xy[0]
		// 新桶, 原偏移量
		x.b = (*bmap)(add(h.buckets, oldbucket*uintptr(t.bucketsize)))
		x.k = add(unsafe.Pointer(x.b), dataOffset)
		x.e = add(x.k, bucketCnt*uintptr(t.keysize))

		if !h.sameSizeGrow() {
			// Only calculate y pointers if we're growing bigger.
			// Otherwise GC can see bad pointers.
			y := &xy[1]
			// 新桶, 高位偏移量
			y.b = (*bmap)(add(h.buckets, (oldbucket+newbit)*uintptr(t.bucketsize)))
			y.k = add(unsafe.Pointer(y.b), dataOffset)
			y.e = add(y.k, bucketCnt*uintptr(t.keysize))
		}

		for ; b != nil; b = b.overflow(t) { // 整个链表上的, 都会一次迁移过去
			k := add(unsafe.Pointer(b), dataOffset)
			e := add(k, bucketCnt*uintptr(t.keysize))
			for i := 0; i < bucketCnt; i, k, e = i+1, add(k, uintptr(t.keysize)), add(e, uintptr(t.elemsize)) {
				top := b.tophash[i]
				if isEmpty(top) {
					b.tophash[i] = evacuatedEmpty // 空槽, 也要标记迁移完成
					continue
				}
				if top < minTopHash {
					throw("bad map state")
				}
				k2 := k
				if t.indirectkey() {
					k2 = *((*unsafe.Pointer)(k2))
				}

				var useY uint8
				if !h.sameSizeGrow() {
					// Compute hash to make our evacuation decision (whether we need
					// to send this key/elem to bucket x or bucket y).
					hash := t.hasher(k2, uintptr(h.hash0))
					if h.flags&iterator != 0 && !t.reflexivekey() && !t.key.equal(k2, k2) {
						// If key != key (NaNs), then the hash could be (and probably
						// will be) entirely different from the old hash. Moreover,
						// it isn't reproducible. Reproducibility is required in the
						// presence of iterators, as our evacuation decision must
						// match whatever decision the iterator made.
						// Fortunately, we have the freedom to send these keys either
						// way. Also, tophash is meaningless for these kinds of keys.
						// We let the low bit of tophash drive the evacuation decision.
						// We recompute a new random tophash for the next level so
						// these keys will get evenly distributed across all buckets
						// after multiple grows.
						useY = top & 1
						top = tophash(hash)
					} else {
						// 自反的, 在这里
						if hash&newbit != 0 { // 地位不为0的 迁移到 新偏移量
							useY = 1
						}
					}
				}

				if evacuatedX+1 != evacuatedY || evacuatedX^1 != evacuatedY { // 为什么要对常量做这种check??
					throw("bad evacuatedN")
				}

				b.tophash[i] = evacuatedX + useY // evacuatedX + 1 == evacuatedY
				dst := &xy[useY]                 // evacuation destination

				if dst.i == bucketCnt { // 整个链表上. 会超过 8个slot, 需要reset 起始 index
					// 满了，需要扩容新桶
					dst.b = h.newoverflow(t, dst.b)
					dst.i = 0
					dst.k = add(unsafe.Pointer(dst.b), dataOffset)
					dst.e = add(dst.k, bucketCnt*uintptr(t.keysize))
				}

				// 更新新位置的 top, key, vale
				dst.b.tophash[dst.i&(bucketCnt-1)] = top // mask dst.i as an optimization, to avoid a bounds check
				if t.indirectkey() {
					*(*unsafe.Pointer)(dst.k) = k2 // copy pointer
				} else {
					typedmemmove(t.key, dst.k, k) // copy elem
				}

				if t.indirectelem() {
					*(*unsafe.Pointer)(dst.e) = *(*unsafe.Pointer)(e)
				} else {
					typedmemmove(t.elem, dst.e, e)
				}

				dst.i++
				// These updates might push these pointers past the end of the
				// key or elem arrays.  That's ok, as we have the overflow pointer
				// at the end of the bucket to protect against pointing past the
				// end of the bucket.
				dst.k = add(dst.k, uintptr(t.keysize))
				dst.e = add(dst.e, uintptr(t.elemsize))
			}
		}
		// Unlink the overflow buckets & clear key/elem to help GC.
		if h.flags&oldIterator == 0 && t.bucket.ptrdata != 0 {
			b := add(h.oldbuckets, oldbucket*uintptr(t.bucketsize))
			// Preserve b.tophash because the evacuation
			// state is maintained there.
			ptr := add(b, dataOffset)
			n := uintptr(t.bucketsize) - dataOffset
			memclrHasPointers(ptr, n) // 写入 写屏障缓冲区, 再置零值
		}
	}

	if oldbucket == h.nevacuate {
		advanceEvacuationMark(h, t, newbit)
	}
}

// 疏散完一个桶bmap的链表
func advanceEvacuationMark(h *hmap, t *maptype, newbit uintptr) {
	h.nevacuate++
	// Experiments suggest that 1024 is overkill by at least an order of magnitude.
	// Put it in there as a safeguard anyway, to ensure O(1) behavior.
	stop := h.nevacuate + 1024 // 每次 最多增量1024
	if stop > newbit {
		stop = newbit
	}

	// 判断指定索引的桶是不是已疏散完了
	for h.nevacuate != stop && bucketEvacuated(t, h, h.nevacuate) {
		h.nevacuate++
	}

	// 全部疏散完了
	if h.nevacuate == newbit { // newbit == # of oldbuckets
		// Growing is all done. Free old main bucket array.
		h.oldbuckets = nil // 迁移完成, 置nil, 作为完成标记
		// Can discard old overflow buckets as well.
		// If they are still referenced by an iterator,
		// then the iterator holds a pointers to the slice.
		if h.extra != nil {
			h.extra.oldoverflow = nil  //老的溢出桶， 也必须置nil
		}
		h.flags &^= sameSizeGrow // 置0 此标志， 表示可以扩容
	}
}

// Reflect stubs. Called from ../reflect/asm_*.s

//go:linkname reflect_makemap reflect.makemap
func reflect_makemap(t *maptype, cap int) *hmap {
	// Check invariants and reflects math.
	if t.key.equal == nil {
		throw("runtime.reflect_makemap: unsupported map key type")
	}
	if t.key.size > maxKeySize && (!t.indirectkey() || t.keysize != uint8(sys.PtrSize)) ||
		t.key.size <= maxKeySize && (t.indirectkey() || t.keysize != uint8(t.key.size)) {
		throw("key size wrong")
	}
	if t.elem.size > maxElemSize && (!t.indirectelem() || t.elemsize != uint8(sys.PtrSize)) ||
		t.elem.size <= maxElemSize && (t.indirectelem() || t.elemsize != uint8(t.elem.size)) {
		throw("elem size wrong")
	}
	if t.key.align > bucketCnt {
		throw("key align too big")
	}
	if t.elem.align > bucketCnt {
		throw("elem align too big")
	}
	if t.key.size%uintptr(t.key.align) != 0 {
		throw("key size not a multiple of key align")
	}
	if t.elem.size%uintptr(t.elem.align) != 0 {
		throw("elem size not a multiple of elem align")
	}
	if bucketCnt < 8 {
		throw("bucketsize too small for proper alignment")
	}
	if dataOffset%uintptr(t.key.align) != 0 {
		throw("need padding in bucket (key)")
	}
	if dataOffset%uintptr(t.elem.align) != 0 {
		throw("need padding in bucket (elem)")
	}

	return makemap(t, cap, nil)
}

//go:linkname reflect_mapaccess reflect.mapaccess
func reflect_mapaccess(t *maptype, h *hmap, key unsafe.Pointer) unsafe.Pointer {
	elem, ok := mapaccess2(t, h, key)
	if !ok {
		// reflect wants nil for a missing element
		elem = nil
	}
	return elem
}

//go:linkname reflect_mapassign reflect.mapassign
func reflect_mapassign(t *maptype, h *hmap, key unsafe.Pointer, elem unsafe.Pointer) {
	p := mapassign(t, h, key)
	typedmemmove(t.elem, p, elem)
}

//go:linkname reflect_mapdelete reflect.mapdelete
func reflect_mapdelete(t *maptype, h *hmap, key unsafe.Pointer) {
	mapdelete(t, h, key)
}

//go:linkname reflect_mapiterinit reflect.mapiterinit
func reflect_mapiterinit(t *maptype, h *hmap) *hiter {
	it := new(hiter)
	mapiterinit(t, h, it)
	return it
}

//go:linkname reflect_mapiternext reflect.mapiternext
func reflect_mapiternext(it *hiter) {
	mapiternext(it)
}

//go:linkname reflect_mapiterkey reflect.mapiterkey
func reflect_mapiterkey(it *hiter) unsafe.Pointer {
	return it.key
}

//go:linkname reflect_mapiterelem reflect.mapiterelem
func reflect_mapiterelem(it *hiter) unsafe.Pointer {
	return it.elem
}

//go:linkname reflect_maplen reflect.maplen
func reflect_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(reflect_maplen))
	}
	return h.count
}

//go:linkname reflectlite_maplen internal/reflectlite.maplen
func reflectlite_maplen(h *hmap) int {
	if h == nil {
		return 0
	}
	if raceenabled {
		callerpc := getcallerpc()
		racereadpc(unsafe.Pointer(h), callerpc, funcPC(reflect_maplen))
	}
	return h.count
}

const maxZero = 1024 // must match value in cmd/compile/internal/gc/walk.go:zeroValSize
var zeroVal [maxZero]byte
