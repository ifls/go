// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
	"unsafe"
)

// Map is like a Go map[interface{}]interface{} but is safe for concurrent use
// by multiple goroutines without additional locking or coordination. 协调
// Loads, stores, and deletes run in amortized constant time. 摊还分析 常量时间
//
// The Map type is specialized 特定. Most code should use a plain Go map instead,
// with separate locking or coordination 加锁map也可以实现, for better type safety and to make it
// easier to maintain other invariants along with the map content.
//
// 此map是特殊优化的
// The Map type is optimized for two common use cases: (1) when the entry for a given
// key is only ever written once but read many times, as in caches that only grow, 多读, 只增长
// or (2) when multiple goroutines read, write, and overwrite entries for disjoint 读写不同子集 的key sets of keys.
// In these two cases, use of a Map may significantly reduce lock contention compared to a Go map paired with a separate Mutex or RWMutex.
//
// The zero Map is empty and ready for use.
// A Map must not be copied after first use.
type Map struct {
	mu Mutex

	// read contains the portion of the map's contents that are safe for
	// concurrent access (with or without mu held). 有部分内容, 并且是并发访问安全的
	//
	// The read field itself is always safe to load, but must only be stored with mu held. 更新整个entry对象和加entry对象,才算写, 修改entry里的值, 不算做写
	//
	// Entries stored in read may be updated concurrently without mu, but updating
	// a previously-expunged之前删除的 entry requires that the entry be copied to the dirty
	// map and unexpunged去掉删除标志 with mu held.
	read atomic.Value // readOnly

	// dirty contains the portion部分 of the map's contents that require mu to be held.
	// To ensure that the dirty map can be promoted to the read map quickly,
	// it also includes all of the non-expunged entries in the read map.
	//
	// Expunged entries are not stored in the dirty map. An expunged entry in the
	// clean map must be unexpunged and added to the dirty map before a new value
	// can be stored to it.
	//
	// If the dirty map is nil, the next write to the map will initialize it by
	// making a shallow copy of the clean map, omitting stale entries.
	dirty map[interface{}]*entry

	// misses counts the number of loads since the read map was last updated that
	// needed to lock mu to determine whether the key was present.
	//
	// Once enough misses have occurred to cover the cost of copying the dirty
	// map, the dirty map will be promoted to the read map (in the unamended
	// state) and the next store to the map will make a new dirty copy.
	misses int
}

// readOnly is an immutable struct stored atomically in the Map.read field.
// Map.read = &readOnly{}
type readOnly struct {
	m       map[interface{}]*entry
	amended bool // 当dirty中包含read没有的数据时为true，比如新增一条数据  true if the dirty map contains some key not in m.
	// amended == true 也表明了dirty map != nil, 也表明dirty里有read里没有的数据
}

// expunged is an arbitrary pointer that marks entries which have been deleted from the dirty map. 分配的内存是唯一的,
// 可以做进程内部的唯一id
var expunged = unsafe.Pointer(new(interface{})) // 已删除的标记值

// An entry is a slot in the map corresponding to a particular key.
type entry struct {
	// p points to the interface{} value stored for the entry.
	//
	// If p == nil, the entry has been deleted and m.dirty == nil.
	//
	// If p == expunged, the entry has been deleted, m.dirty != nil, and the entry
	// is missing from m.dirty.
	//
	// Otherwise, the entry is valid and recorded in m.read.m[key] and, if m.dirty
	// != nil, in m.dirty[key].
	//
	// An entry can be deleted by atomic replacement with nil: when m.dirty is
	// next created, it will atomically replace nil with expunged and leave
	// m.dirty[key] unset.
	//
	// An entry's associated value can be updated by atomic replacement, provided
	// p != expunged. If p == expunged, an entry's associated value can be updated
	// only after first setting m.dirty[key] = e so that lookups using the dirty
	// map find the entry.
	p unsafe.Pointer // *interface{}
	// 如果直接用指针, 外面就无法对类型进行判断和转换了
}

func newEntry(i interface{}) *entry {
	return &entry{p: unsafe.Pointer(&i)}
}

// Load returns the value stored in the map for a key, or nil if no
// value is present.
// The ok result indicates whether value was found in the map.
func (m *Map) Load(key interface{}) (value interface{}, ok bool) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]

	// 这里可能被更新了此key 对应的entry, 所以下面加锁, 还要再从read里读一次
	if !ok && read.amended { // amend == true, 表示有新元素在dirty
		m.mu.Lock()
		// Avoid reporting a spurious miss if m.dirty got promoted while we were
		// blocked on m.mu. (If further loads of the same key will not miss, it's
		// not worth copying the dirty map for this key.)
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]
		if !ok && read.amended { // 加锁, double-check
			e, ok = m.dirty[key]
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			m.missLocked() // 访问到dirty, 就++miss, miss过多, 会将dirty 提升为read
		}
		m.mu.Unlock()
	}
	if !ok {
		return nil, false
	}
	return e.load()
}

// 读取value
func (e *entry) load() (value interface{}, ok bool) {
	p := atomic.LoadPointer(&e.p)
	if p == nil || p == expunged {
		return nil, false
	}
	return *(*interface{})(p), true
}

// Store sets the value for a key.
func (m *Map) Store(key, value interface{}) {
	read, _ := m.read.Load().(readOnly)
	// tryStore 在这里要处理expunged 标志, 只更新未删除的key
	if e, ok := read.m[key]; ok && e.tryStore(&value) { // 已包含此kv, 只需要更新value
		return
	}

	// 新增和删除 在下面加锁处理
	m.mu.Lock() // 互斥
	read, _ = m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok { // 更新v值
		if e.unexpungeLocked() { // expunge = nil, 去掉删除标志, 表示还在read里
			// The entry was previously expunged, which implies that there is a
			// non-nil dirty map and this entry is not in it.
			m.dirty[key] = e // 被删除的,现在又赋值, 条目需要再次放到dirty里, 因为之前expunge 的, 不会放到dirty里
		}
		e.storeLocked(&value) // 更新值
	} else if e, ok := m.dirty[key]; ok { // 不在read, 只在dirty, 说明之前amend过, nil map 也可以访问
		e.storeLocked(&value) // 更新value
	} else {               // read 和dirty 都不在, dirty新加一个key
		if !read.amended { // 标志dirty 有额外的key, 需要额外查询
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked()                                  // 创建dirty对象
			m.read.Store(readOnly{m: read.m, amended: true}) // + amend标志
		}
		m.dirty[key] = newEntry(value) // 暂时只加到dirty里
	}
	m.mu.Unlock()
}

// tryStore stores a value if the entry has not been expunged.
//
// If the entry is expunged, tryStore returns false and leaves the entry
// unchanged.
// 需要处理删除标志
func (e *entry) tryStore(i *interface{}) bool {
	for {
		p := atomic.LoadPointer(&e.p)
		if p == expunged { // 标记删除了, 需要走slow path, 更新dirty map
			return false
		}
		if atomic.CompareAndSwapPointer(&e.p, p, unsafe.Pointer(i)) {
			return true
		}
	}
}

// unexpungeLocked ensures that the entry is not marked as expunged.
//
// If the entry was previously expunged, it must be added to the dirty map
// before m.mu is unlocked.
// 更新value的时候, 需要干掉 删除标志
func (e *entry) unexpungeLocked() (wasExpunged bool) {
	return atomic.CompareAndSwapPointer(&e.p, expunged, nil)
}

// storeLocked unconditionally stores a value to the entry.
//
// The entry must be known not to be expunged.
func (e *entry) storeLocked(i *interface{}) {
	atomic.StorePointer(&e.p, unsafe.Pointer(i))
}

// LoadOrStore returns the existing value for the key if present.
// Otherwise, it stores and returns the given value.
// The loaded result is true if the value was loaded, false if stored.
func (m *Map) LoadOrStore(key, value interface{}) (actual interface{}, loaded bool) {
	// Avoid locking if it's a clean hit.
	read, _ := m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok {
		actual, loaded, ok := e.tryLoadOrStore(value) // 这里只处理非标记删除的情况
		if ok {
			return actual, loaded
		}
	}

	m.mu.Lock()
	read, _ = m.read.Load().(readOnly)
	if e, ok := read.m[key]; ok { // 标记删除的情况, 还是落到这里
		if e.unexpungeLocked() {
			m.dirty[key] = e // 去掉标记删除, 要落到dirty
		}
		actual, loaded, _ = e.tryLoadOrStore(value)
	} else if e, ok := m.dirty[key]; ok {
		actual, loaded, _ = e.tryLoadOrStore(value)
		m.missLocked() // 不断判断, 都当做一次写入??
	} else {
		if !read.amended {
			// We're adding the first new key to the dirty map.
			// Make sure it is allocated and mark the read-only map as incomplete.
			m.dirtyLocked()
			m.read.Store(readOnly{m: read.m, amended: true})
		}
		m.dirty[key] = newEntry(value)
		actual, loaded = value, false
	}
	m.mu.Unlock()

	return actual, loaded
}

// tryLoadOrStore atomically loads or stores a value if the entry is not
// expunged.
//
// If the entry is expunged, tryLoadOrStore leaves the entry unchanged and
// returns with ok==false.
func (e *entry) tryLoadOrStore(i interface{}) (actual interface{}, loaded, ok bool) {
	p := atomic.LoadPointer(&e.p)
	if p == expunged {
		return nil, false, false // 需要走 slow path
	}
	if p != nil {
		return *(*interface{})(p), true, true
	}

	// Copy the interface after the first load to make this method more amenable
	// to escape analysis: if we hit the "load" path or the entry is expunged, we
	// shouldn't bother heap-allocating.

	// nil 的情况, 写入数据
	ic := i
	for { // 没有do while 循环的缺点
		if atomic.CompareAndSwapPointer(&e.p, nil, unsafe.Pointer(&ic)) {
			return i, false, true
		}
		p = atomic.LoadPointer(&e.p)
		if p == expunged {
			return nil, false, false
		}
		if p != nil {
			return *(*interface{})(p), true, true
		}
	}
}

// LoadAndDelete deletes the value for a key, returning the previous value if any.
// The loaded result reports whether the key was present.
// go1.15 欧长坤, 实现的
func (m *Map) LoadAndDelete(key interface{}) (value interface{}, loaded bool) {
	read, _ := m.read.Load().(readOnly)
	e, ok := read.m[key]
	if !ok && read.amended {
		// 互斥, 加锁
		m.mu.Lock()
		read, _ = m.read.Load().(readOnly)
		e, ok = read.m[key]
		if !ok && read.amended { // double check 防止值已改变, 但是多个g同时进来的情况
			e, ok = m.dirty[key]
			// Regardless of whether the entry was present, record a miss: this key
			// will take the slow path until the dirty map is promoted to the read
			// map.
			m.missLocked() // 需要到dirty里删除
		}
		m.mu.Unlock()
	}
	if ok {
		return e.delete() // 删除value的值, 置为nil, 也即是同时删除read和dirty里的entry 的值
	}
	return nil, false
}

// Delete deletes the value for a key.
func (m *Map) Delete(key interface{}) {
	m.LoadAndDelete(key)
}

func (e *entry) delete() (value interface{}, ok bool) {
	for {
		p := atomic.LoadPointer(&e.p)
		if p == nil || p == expunged {
			return nil, false // 已删除了, 此次未删除成功
		}
		if atomic.CompareAndSwapPointer(&e.p, p, nil) {
			return *(*interface{})(p), true
		}
	}
}

// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration.
//
// Range does not necessarily correspond to any consistent snapshot of the Map's
// contents: no key will be visited more than once, but if the value for any key
// is stored or deleted concurrently, Range may reflect any mapping for that key
// from any point during the Range call.
//
// Range may be O(N) with the number of elements in the map even if f returns
// false after a constant number of calls.
func (m *Map) Range(f func(key, value interface{}) bool) {
	// We need to be able to iterate over all of the keys that were already
	// present at the start of the call to Range.
	// If read.amended is false, then read.m satisfies that property without
	// requiring us to hold m.mu for a long time.
	read, _ := m.read.Load().(readOnly)
	if read.amended {
		// m.dirty contains keys not in read.m. Fortunately, Range is already O(N)
		// (assuming the caller does not break out early), so a call to Range
		// amortizes an entire copy of the map: we can promote the dirty copy
		// immediately!
		m.mu.Lock()
		read, _ = m.read.Load().(readOnly)
		if read.amended {
			read = readOnly{m: m.dirty} // dirty 直接提升到read
			m.read.Store(read)
			m.dirty = nil
			m.misses = 0
		}
		m.mu.Unlock()
	}

	// 只range add里面的, 新增的key, 不会遍历到
	// read 会被替换, 但是 read.m不会被替换成其他对象
	for k, e := range read.m {
		v, ok := e.load()
		if !ok {
			continue
		}
		if !f(k, v) {
			break
		}
	}
}

func (m *Map) missLocked() {
	m.misses++
	if m.misses < len(m.dirty) {
		return
	}
	// dirty 要包含全部未删除的数据, 以便直接提升为read
	m.read.Store(readOnly{m: m.dirty})
	m.dirty = nil
	m.misses = 0
}

// 保证 dirty 有
func (m *Map) dirtyLocked() {
	if m.dirty != nil {
		return
	}

	read, _ := m.read.Load().(readOnly)
	m.dirty = make(map[interface{}]*entry, len(read.m))
	for k, e := range read.m {
		// 非标记删除的, 都会存到dirty里
		if !e.tryExpungeLocked() { // 原来 的 nil 都会被标记为删除
			m.dirty[k] = e
		}
	}
}

// 生成dirty 时会 标记 expunged, 为什么要标记删除? 不直接置为nil? 是为了区分真的不存在 和read里不存在, 在dirty里存在, 两种情况, 更新value 的时候, 要根据这个标志判断,
// 是否也要更新dirty
// nil -> 删除标志
// 返回是否被标记删除
func (e *entry) tryExpungeLocked() (isExpunged bool) {
	p := atomic.LoadPointer(&e.p)
	for p == nil {
		if atomic.CompareAndSwapPointer(&e.p, nil, expunged) {
			return true
		}
		p = atomic.LoadPointer(&e.p)
	}
	return p == expunged
}
