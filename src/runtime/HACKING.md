This is a living document文档 and at times it will be out of date. It is
intended to articulate清晰地描述 how programming in the Go runtime differs from
writing normal Go. It focuses on pervasive普通的 concepts rather than
details of particular interfaces.

Scheduler structures
====================

The scheduler manages three types of resources that pervade无处不在 the
runtime: Gs, Ms, and Ps. It's important to understand these even if
you're not working on the scheduler.

Gs, Ms, Ps
----------

A "G" is simply a goroutine. It's represented by type `g`. When a
goroutine exits退出后, its `g` object is returned to a pool入池 of free `g`s and
can later be reused重用 for some other goroutine.

An "M" is an OS thread that can be executing user Go code, runtime
code, a system call, or be idle. It's represented by type `m`. There
can be any number of Ms at a time since any number of threads may be
blocked in system calls. 任意数量的线程可能阻塞在系统调用

Finally, a "P" represents the resources代表资源 required to execute user Go
code, such as例如 scheduler and memory allocator 的状态state. It's represented
by type `p`. There are exactly `GOMAXPROCS` Ps. A P can be thought of
like a CPU in the OS scheduler and the contents of the `p` type like
per-CPU state被调度器看做cpu，p保存p-CPU 状态. This is a good place to put state that needs to be
sharded for efficiency效率, but doesn't need to be per-thread or
per-goroutine. 但不是每线程或者每协程共享

The scheduler's job is to match up a G (the code to execute), an M
(where to execute it), and a P (the rights权利 and resources to execute
it). When an M stops executing user Go code, for example by entering a
system call, it returns解绑 its P to the idle P pool. In order to resume
executing user Go code, for example on return from a system call, it
must acquire拿 a P from the idle pool.

All `g`, `m`, and `p` objects are heap allocated堆上分配, but are never freed 从不释放,
so their memory remains type stable 仍然是类型文档. As a result, the runtime can
avoid write barriers in the depths of the scheduler.

User stacks and system stacks
-----------------------------

Every non-dead G has a *user stack* associated with it, which is what
user Go code executes on. User stacks start small (e.g., 2K) and grow
or shrink dynamically.

Every M has a *system stack* associated with it (also known as the M's
"g0" stack because it's implemented as a stub存根 G) and, on Unix
platforms, a *signal stack* (also known as the M's "gsignal" stack).
System and signal stacks cannot grow 不能扩容, but are large enough to execute
runtime and cgo code (8K in a pure Go binary; system-allocated in a
cgo binary cgo是系统分配).

Runtime code often temporarily switches to切换到 the system stack using
`systemstack`, `mcall`, or `asmcgocall` to perform tasks that must not
be preempted 执行不能被抢占的任务, that must not grow不能栈扩容 the user stack, or that switch切换用户线程的任务 user
goroutines. Code running on the system stack is implicitly暗含
non-preemptible and the garbage collector does not scan system stacks 垃圾回收不扫描系统栈.
While running on the system stack, the current user stack is not used
for execution.

`getg()` and `getg().m.curg`
----------------------------

To get the current user `g`, use `getg().m.curg`. curg只指向用户g

`getg()` alone returns the current `g`, but when executing on the
system or signal stacks, this will return the current M's "g0" or
"gsignal", respectively. This is usually not what you want.

To determine if you're running on the user stack or the system stack,
use `getg() == getg().m.curg`.

Error handling and reporting
============================

Errors that can reasonably be recovered from in user code should use 能恢复的错误使用panic，但系统栈和mallocgc例外
`panic` like usual. However, there are some situations where `panic`
will cause an immediate立即 fatal error, such as when called on the system
stack or when called during `mallocgc`.

Most errors in the runtime are not recoverable. For these, use
`throw`, which dumps the traceback and immediately terminates the 不可恢复使用throw
process.  In general, `throw` should be passed a string constant to
avoid allocating in perilous situations. By convention, additional
details are printed before `throw` using `print` or `println` and the
messages are prefixed with "runtime:". 额外的信息应该在throw前 print打印， 带runtime：前缀

For runtime error debugging, it's useful to run with 设置的是级别
`GOTRACEBACK=system` or `GOTRACEBACK=crash`.

Synchronization
===============

The runtime has multiple synchronization mechanisms机制. They differ in
semantics语义 and, in particular, in whether they interact with the
goroutine scheduler or the OS scheduler.

The simplest is `mutex`, which is manipulated using `lock` and
`unlock`. This should be used to protect shared structures for short
periods. Blocking on a `mutex` directly blocks the M 直接阻塞m，调度器不知道, without
interacting with the Go scheduler. This means it is safe安全 to use from
the lowest levels of the runtime, but also prevents any associated G
and P from being rescheduled 阻止调度. `rwmutex` is similar.

For one-shot只有一次的 notifications, use `note`, which provides `notesleep` and
`notewakeup`. Unlike traditional UNIX `sleep`/`wakeup`, `note`s are
race-free无竞争情况, so `notesleep` returns immediately立即返回 if the `notewakeup` has 不用等sleep之后的wakeup
already happened. A `note` can be reset重置 after use with `noteclear`,
which must not race 一定不能与sleep/wakeup 竞争使用 必须在之后with a sleep or wakeup. 
Like `mutex`, blocking on a `note` blocks the M. However, there are different ways to sleep on a
`note`:`notesleep` also prevents rescheduling of any associated G and 阻止调度任何相关连的g和p
P, while `notetsleepg` acts like a blocking system call that allows
the P to be reused to run another G 允许p执行其他G，不浪费cpu. 
This is still less efficient than blocking the G directly since it consumes an M. 仍然不如只阻塞G高效，如下所示

To interact directly with the goroutine scheduler 与goruntine调度器交互, use `gopark` and `goready`. 
`gopark` parks the current goroutine—putting it in the "waiting" state and removing it from the scheduler's run queue—and
schedules another goroutine on the current M/P. 
`goready` puts a parked goroutine back in the "runnable" state and adds it to the run
queue.

In summary,

<table>
<tr><th></th><th colspan="3">Blocks</th></tr>
<tr><th>Interface</th><th>G</th><th>M</th><th>P</th></tr>
<tr><td>(rw)mutex</td><td>Y</td><td>Y</td><td>Y</td></tr>
<tr><td>note</td><td>Y</td><td>Y</td><td>Y/N</td></tr>
<tr><td>park</td><td>Y</td><td>N</td><td>N</td></tr>
</table>

Atomics 原子性
=======

The runtime uses its own atomics package at `runtime/internal/atomic`.
This corresponds to `sync/atomic`, but functions have different names
for historical reasons and there are a few additional functions needed 也有一些额外原子函数被运行时需要
by the runtime.

In general, we think hard 认真思考 about the uses of atomics in the runtime and
try to avoid unnecessary atomic operations. 避免不必要的原子操作 
If access to a variable is sometimes protected by another synchronization mechanism, the
already-protected accesses generally don't need to be atomic. 如果被其他同步机制保护，就不需要原子操作 
There are several reasons for this:

1. Using non-atomic or atomic access where appropriate适当的 makes the code
   more self-documenting自我描述. 
   Atomic access to a variable implies暗示并发访问 there's somewhere else that may concurrently access the variable.

2. Non-atomic access allows for考虑需要 automatic race detection. 
   The runtime doesn't currently have a race detector 没有竞争监测, but it may in the future.
   Atomic access defeats the race detector, while non-atomic access
   allows the race detector to check your assumptions.
   
3. Non-atomic access may improve performance.

Of course, any non-atomic access to a shared variable should be
documented to explain how that access is protected. 共享变量非原子访问，需要解释为什么

Some common patterns that mix atomic and non-atomic access are:

* Read-mostly读多 variables where updates are protected by a lock. 
  Within the locked region, reads do not need to be atomic, but the write does 临界区内需要原子写？. 
  Outside the locked region, reads need to be atomic.

* Reads that only happen during STW, where no writes can happen during
  STW, do not need to be atomic. stw是读不需要是原子的

That said说到, the advice from the Go memory model stands: "Don't be
[too] clever." The performance of the runtime matters, but its
robustness matters more 鲁棒性，正确性更重要.

Unmanaged memory 堆外内存
================

In general, the runtime tries to use regular heap allocation. However,
in some cases the runtime must allocate objects outside of the garbage collected heap, in *unmanaged memory*. 
This is necessary if the objects are part of the memory manager itself 内存管理者自身使用的内存
or if they must be allocated in situations where the caller may not have a P. 没有P绑定 也就没mcache

There are three mechanisms for allocating unmanaged memory:

* sysAlloc obtains memory directly from the OS. 
  This comes in whole multiples of the system page size, but it can be freed with sysFree.

* persistentalloc永久分配 combines multiple smaller allocations into a single
  sysAlloc to avoid fragmentation 使用缓存. 
  However, there is no way to free persistentalloced objects (hence the name).

* fixalloc is a SLAB-style allocator that allocates objects of a fixed size. 
  fixalloced objects can be freed, but this memory can only be reused by the same fixalloc pool, 
  so it can only be reused for objects of the same type.

In general, types that are allocated using any of these should be
marked `//go:notinheap` (see below).

Objects that are allocated in unmanaged memory **must not** contain 不能包含堆上指针
heap pointers unless the following rules are also obeyed遵守:

1. Any pointers from unmanaged memory to the heap must be garbage
   collection roots. 必须是垃圾回收根 
   More specifically详细说, any pointer must either be accessible through a global variable 
   or be added as an explicit garbage collection root in `runtime.markroot`. 标记为根

2. If the memory is reused, the heap pointers must be zero-initialized 重用前必须用0初始化，防止被作为root持有其他对象
   before they become visible as GC roots. 
   Otherwise, the GC may observe stale僵硬的 heap pointers. See "Zero-initialization versus
   zeroing".

Zero-initialization versus zeroing  直接内存全清0还是赋值为0
==================================

There are two types of zeroing in the runtime, depending on whether
the memory is already initialized to a type-safe state.

If memory is not in a type-safe state, meaning it potentially可能 contains
"garbage" because it was just allocated and it is being initialized for first use, 
then it must be *zero-initialized* using `memclrNoHeapPointers` or non-pointer writes. 
This does not perform write barriers. 不执行写屏障

If memory is already in a type-safe state and is simply being set to
the zero value, this must be done using regular writes, `typedmemclr`,
or `memclrHasPointers`. This performs write barriers. 有写屏障

Runtime-only compiler directives
================================

In addition to the "//go:" directives documented in "go doc compile", noescape nosplit, linkname
the compiler supports additional directives only in the runtime. 支持额外内存编译指令

go:systemstack
--------------

`go:systemstack` indicates that a function must必须 run on the system
stack. This is checked dynamically运行时检查 by a special function prologue前言.

go:nowritebarrier
-----------------

`go:nowritebarrier` directs the compiler to emit an error编译器报错 if the following function contains any write barriers. 
(It *does not* suppress the generation of write barriers; it is simply an assertion断言.)

Usually you want `go:nowritebarrierrec`. `go:nowritebarrier` is
primarily useful in situations where it's "nice" not to have write
barriers, but not required for correctness 和正确性无关.

go:nowritebarrierrec and go:yeswritebarrierrec
----------------------------------------------

`go:nowritebarrierrec` directs the compiler to emit an error if the
following function or any function it calls recursively递归, up to a
`go:yeswritebarrierrec`, contains a write barrier.

Logically, the compiler floods the call graph starting from each
`go:nowritebarrierrec` function and produces an error if it encounters
a function containing a write barrier. This flood stops at
`go:yeswritebarrierrec` functions.

`go:nowritebarrierrec` is used in the implementation of the write
barrier to prevent infinite loops 阻止无限循环.

Both directives are used in the scheduler调度器. 
The write barrier requires an active P (`getg().m.p != nil`) and scheduler code often runs
without an active P. 
In this case, `go:nowritebarrierrec` is used on functions that release the P or may run without a P and
`go:yeswritebarrierrec` is used when code re-acquires an active P.
Since these are function-level annotations, code that releases or
acquires a P may need to be split across two functions.

go:notinheap
------------

`go:notinheap` applies to type declarations类型声明. 
It indicates that a type must never be allocated from the GC'd heap. 
Specifically, pointers to this type must always fail the `runtime.inheap` check. 
The type may be used for global variables, for stack variables栈上变量, or for objects in
堆外内存unmanaged memory (e.g., allocated with `sysAlloc`, `persistentalloc`,
`fixalloc`, or from a manually-managed手动管理的 span). Specifically:

1. `new(T)`, `make([]T)`, `append([]T, ...)` and implicit heap
   allocation of T are disallowed. 隐式堆内分配是不允许的
   (Though implicit allocations are disallowed in the runtime anyway.)

2. A pointer to a regular type (other than `unsafe.Pointer`) cannot be
   converted to a pointer to a `go:notinheap` type, even if they have
   the same underlying type.

3. Any type that contains a `go:notinheap` type is itself `go:notinheap`. 
   Structs and arrays are `go:notinheap` if their elements are. 
   Maps and channels of `go:notinheap` types are disallowed. 
   To keep things explicit, any type declaration where the
   type is implicitly `go:notinheap` must be explicitly marked
   `go:notinheap` as well.

4. Write barriers on pointers to `go:notinheap` types can be omitted.

The last point is the real benefit of `go:notinheap`. The runtime uses
it for low-level internal structures to avoid memory barriers in the
scheduler and the memory allocator where they are illegal or simply 不合法或者低效
inefficient. 
This mechanism is reasonably相当 safe and does not compromise妥协，让步
the readability of the runtime.
