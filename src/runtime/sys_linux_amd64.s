// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//
// System calls and other sys.stuff for AMD64, Linux
// 实现了内部接口调用的系统调用， 对真实系统调用返回的参数不做任何处理，不会包装成库的形式

#include "go_asm.h"
#include "go_tls.h"
#include "textflag.h"

#define AT_FDCWD -100

//优化实现的系统调用
#define SYS_read		0  //读
#define SYS_write		1   //写
// 2 SYS_open 使用 SYS_openat代替
#define SYS_close		3  //关闭fd
//mmap 分配内存的系统调用号
#define SYS_mmap		9  //映射文件或者设备到内存，以便程序访问
#define SYS_munmap		11 // 解除映射
#define SYS_brk 		12 // 设置程序数据段结尾地址
#define SYS_rt_sigaction	13  // 检查或者改变信号处理函数
#define SYS_rt_sigprocmask	14  // 检查和概念 阻塞的信号
#define SYS_rt_sigreturn	15
#define SYS_pipe		22  // create pipe
#define SYS_sched_yield 	24  // 当前线程让出处理器
#define SYS_mincore		27  // 查询指定虚拟内存页是不是在物理内存中
#define SYS_madvise		28  // 给内核设置 内存使用的建议
#define SYS_nanosleep		35  // 高精度 暂停线程执行，可能会被信号中断唤醒
#define SYS_setittimer		38  // 设置定时器
#define SYS_getpid		39  // 获取pid， 和tid有什么区别， 怎么获取tid？？
#define SYS_socket		41  // 创建 一个 通信终端
#define SYS_connect		42  // 在 socket上 初始化一个连接
#define SYS_clone		56  // 创建一个子进程
#define SYS_exit		60  // 主动 退出调用进程
#define SYS_kill		62  // 向进程发信号
#define SYS_uname		63  // 获取内核的名字和信息
#define SYS_fcntl		72  // 操作 文件描述符 fd 一个不亚于 open的大函数
#define SYS_sigaltstack 	131 // 设置或者获取 信号栈 上下文
#define SYS_mlock		149  // 内存锁
#define SYS_arch_prctl		158  // 设置 架构特定的线程状态
#define SYS_gettid		186  // 获取线程id， 对应上面的疑问
#define SYS_futex		202  // 快速 用户空间 锁 fast user-space locking
#define SYS_sched_getaffinity	204  // set and get a thread's CPU affinity(亲密性) mask
#define SYS_epoll_create	213  // open an epoll file descriptor
#define SYS_exit_group		231  // 退出进程的所有线程， 也就是一个进程， 不返回
#define SYS_epoll_ctl		233  // control interface for an epoll file descriptor
#define SYS_tgkill		234  // 向一个线程组 发 信号 send a signal to a thread
#define SYS_openat		257  // open的拓展版
#define SYS_faccessat		269  // file access at check user's permissions for a file
#define SYS_epoll_pwait		281  // wait for an I/O event on an epoll file descriptor
#define SYS_epoll_create1	291 // 打开一个 epoll fd 同上
#define SYS_pipe2		293  // 同上， 入参多了一个标志参数

//退出进程
#include <linux/unistd.h>
void exit_group(int status);
//func exit(code int32)
TEXT runtime·exit(SB),NOSPLIT,$0-4
	MOVL	code+0(FP), DI
	MOVL	$SYS_exit_group, AX
	SYSCALL
	RET

// 退出调用线程
// func exitThread(wait *uint32)
TEXT runtime·exitThread(SB),NOSPLIT,$0-8
	MOVQ	wait+0(FP), AX
	// We're done using the stack.
	MOVL	$0, (AX)
	MOVL	$0, DI	// exit code
	MOVL	$SYS_exit, AX
	SYSCALL
	// We may not even have a stack any more.
	INT	$3
	JMP	0(PC)

// func open(name *byte, mode, perm int32) int32
TEXT runtime·open(SB),NOSPLIT,$0-20
	// This uses openat instead of open, because Android O blocks open.
	MOVL	$AT_FDCWD, DI // AT_FDCWD, so this acts like open
	MOVQ	name+0(FP), SI
	MOVL	mode+8(FP), DX
	MOVL	perm+12(FP), R10
	MOVL	$SYS_openat, AX
	SYSCALL
	CMPQ	AX, $0xfffffffffffff001
	JLS	2(PC)
	MOVL	$-1, AX
	MOVL	AX, ret+16(FP)
	RET

//为什么参数大小是12B？
//func closefd(fd int32) int32
TEXT runtime·closefd(SB),NOSPLIT,$0-12
	MOVL	fd+0(FP), DI
	MOVL	$SYS_close, AX
	SYSCALL
	CMPQ	AX, $0xfffffffffffff001
	JLS	2(PC)
	MOVL	$-1, AX
	MOVL	AX, ret+8(FP)
	RET

// func write1(fd uintptr, p unsafe.Pointer, n int32) int32
TEXT runtime·write1(SB),NOSPLIT,$0-28
	MOVQ	fd+0(FP), DI
	MOVQ	p+8(FP), SI
	MOVL	n+16(FP), DX
	MOVL	$SYS_write, AX
	SYSCALL
	MOVL	AX, ret+24(FP)
	RET

//func read(fd int32, p unsafe.Pointer, n int32) int32
TEXT runtime·read(SB),NOSPLIT,$0-28
	MOVL	fd+0(FP), DI
	MOVQ	p+8(FP), SI
	MOVL	n+16(FP), DX
	MOVL	$SYS_read, AX
	SYSCALL
	MOVL	AX, ret+24(FP)
	RET

#include <unistd.h>

/* On Alpha, IA-64, MIPS, SuperH, and SPARC/SPARC64; see NOTES */
struct fd_pair {
   long fd[2];
};
struct fd_pair pipe();

/* On all other architectures */
int pipe(int pipefd[2]);

#define _GNU_SOURCE             /* See feature_test_macros(7) */
#include <fcntl.h>              /* Obtain O_* constant definitions */
#include <unistd.h>

int pipe2(int pipefd[2], int flags);
//os_linux.go
// func pipe() (r, w int32, errno int32)
TEXT runtime·pipe(SB),NOSPLIT,$0-12
	LEAQ	r+0(FP), DI
	MOVL	$SYS_pipe, AX
	SYSCALL
	MOVL	AX, errno+8(FP)
	RET

// func pipe2(flags int32) (r, w int32, errno int32)
TEXT runtime·pipe2(SB),NOSPLIT,$0-20
	LEAQ	r+8(FP), DI
	MOVL	flags+0(FP), SI
	MOVL	$SYS_pipe2, AX
	SYSCALL
	MOVL	AX, errno+16(FP)
	RET

#include <time.h>
struct timespec {
   time_t tv_sec;        /* seconds */
   long   tv_nsec;       /* nanoseconds */
};
int nanosleep(const struct timespec *req, struct timespec *rem);
//func usleep(usec uint32)
TEXT runtime·usleep(SB),NOSPLIT,$16
	MOVL	$0, DX
	MOVL	usec+0(FP), AX
	MOVL	$1000000, CX
	DIVL	CX
	MOVQ	AX, 0(SP)
	MOVL	$1000, AX	// usec to nsec
	MULL	DX
	MOVQ	AX, 8(SP)

	// nanosleep(&ts, 0)
	MOVQ	SP, DI
	MOVL	$0, SI
	MOVL	$SYS_nanosleep, AX
	SYSCALL
	RET

#define _GNU_SOURCE
#include <unistd.h>
pid_t gettid(void);
//linux.go
//func gettid() uint32
TEXT runtime·gettid(SB),NOSPLIT,$0-4
	MOVL	$SYS_gettid, AX
	SYSCALL
	MOVL	AX, ret+0(FP)
	RET

#include <unistd.h>
pid_t getpid(void);
// func raise(sig uint32)
TEXT runtime·raise(SB),NOSPLIT,$0
	MOVL	$SYS_getpid, AX
	SYSCALL
	MOVL	AX, R12
	MOVL	$SYS_gettid, AX
	SYSCALL
	MOVL	AX, SI	// arg 2 tid
	MOVL	R12, DI	// arg 1 pid
	MOVL	sig+0(FP), DX	// arg 3
	MOVL	$SYS_tgkill, AX  // getpid 要发信号？？
	SYSCALL
	RET

#include <signal.h>
int kill(pid_t pid, int sig);
//func raiseproc(sig uint32)
TEXT runtime·raiseproc(SB),NOSPLIT,$0
	MOVL	$SYS_getpid, AX
	SYSCALL
	MOVL	AX, DI	// arg 1 pid
	MOVL	sig+0(FP), SI	// arg 2
	MOVL	$SYS_kill, AX
	SYSCALL
	RET

//func getpid() int
TEXT ·getpid(SB),NOSPLIT,$0-8
	MOVL	$SYS_getpid, AX
	SYSCALL
	MOVQ	AX, ret+0(FP)
	RET

int tkill(pid_t tid, int sig);
int tgkill(pid_t tgid, pid_t tid, int sig);
// func tgkill(tgid, tid, sig int)
TEXT ·tgkill(SB),NOSPLIT,$0
	MOVQ	tgid+0(FP), DI
	MOVQ	tid+8(FP), SI
	MOVQ	sig+16(FP), DX
	MOVL	$SYS_tgkill, AX
	SYSCALL
	RET

#include <sys/time.h>
struct itimerval {
   struct timeval it_interval; /* 第一次与第二次的间隔 Interval for periodic timer */
   struct timeval it_value;    /* 第一次的偏移 Time until next expiration */
};

struct timeval {
   time_t      tv_sec;         /* seconds */
   suseconds_t tv_usec;        /* microseconds */
};
int getitimer(int which, struct itimerval *curr_value);
int setitimer(int which, const struct itimerval *restrict new_value, struct itimerval *restrict old_value);
//func setitimer(mode int32, new, old *itimerval)
TEXT runtime·setitimer(SB),NOSPLIT,$0-24
	MOVL	mode+0(FP), DI
	MOVQ	new+8(FP), SI
	MOVQ	old+16(FP), DX
	MOVL	$SYS_setittimer, AX
	SYSCALL
	RET

//func mincore(addr unsafe.Pointer, n uintptr, dst *byte) int32
TEXT runtime·mincore(SB),NOSPLIT,$0-28
	MOVQ	addr+0(FP), DI
	MOVQ	n+8(FP), SI
	MOVQ	dst+16(FP), DX
	MOVL	$SYS_mincore, AX
	SYSCALL
	MOVL	AX, ret+24(FP)
	RET

// func walltime1() (sec int64, nsec int32)
// non-zero frame-size means bp is saved and restored
// 获取 墙上时间
TEXT runtime·walltime1(SB),NOSPLIT,$8-12
	// We don't know how much stack space the VDSO code will need,
	// so switch to g0.
	// In particular, a kernel configured with CONFIG_OPTIMIZE_INLINING=n
	// and hardening can use a full page of stack space in gettime_sym
	// due to stack probes inserted to avoid stack/heap collisions.
	// See issue #20427.

	MOVQ	SP, BP	// Save old SP; BP unchanged by C code.

	get_tls(CX)
	MOVQ	g(CX), AX
	MOVQ	g_m(AX), BX // BX unchanged by C code.

	// Set vdsoPC and vdsoSP for SIGPROF traceback.
	LEAQ	sec+0(FP), DX
	MOVQ	-8(DX), CX
	MOVQ	CX, m_vdsoPC(BX)
	MOVQ	DX, m_vdsoSP(BX)

	CMPQ	AX, m_curg(BX)	// Only switch if on curg.
	JNE	noswitch

	MOVQ	m_g0(BX), DX
	MOVQ	(g_sched+gobuf_sp)(DX), SP	// Set SP to g0 stack

noswitch:
	SUBQ	$16, SP		// Space for results
	ANDQ	$~15, SP	// Align for C code

	MOVQ	runtime·vdsoClockgettimeSym(SB), AX
	CMPQ	AX, $0
	JEQ	fallback
	MOVL	$0, DI // CLOCK_REALTIME
	LEAQ	0(SP), SI
	CALL	AX
	MOVQ	0(SP), AX	// sec
	MOVQ	8(SP), DX	// nsec
	MOVQ	BP, SP		// Restore real SP
	MOVQ	$0, m_vdsoSP(BX)
	MOVQ	AX, sec+0(FP)
	MOVL	DX, nsec+8(FP)
	RET
fallback:
	LEAQ	0(SP), DI
	MOVQ	$0, SI
	MOVQ	runtime·vdsoGettimeofdaySym(SB), AX
	CALL	AX
	MOVQ	0(SP), AX	// sec
	MOVL	8(SP), DX	// usec
	IMULQ	$1000, DX
	MOVQ	BP, SP		// Restore real SP
	MOVQ	$0, m_vdsoSP(BX)
	MOVQ	AX, sec+0(FP)
	MOVL	DX, nsec+8(FP)
	RET

// func nanotime1() int64
// non-zero frame-size means bp is saved and restored
// 避免syscall指令
TEXT runtime·nanotime1(SB),NOSPLIT,$8-8
	// Switch to g0 stack. See comment above in runtime·walltime.

	MOVQ	SP, BP	// Save old SP; BP unchanged by C code.

	get_tls(CX)
	MOVQ	g(CX), AX
	MOVQ	g_m(AX), BX // BX unchanged by C code.

	// Set vdsoPC and vdsoSP for SIGPROF traceback.
	LEAQ	ret+0(FP), DX
	MOVQ	-8(DX), CX
	MOVQ	CX, m_vdsoPC(BX)
	MOVQ	DX, m_vdsoSP(BX)

	CMPQ	AX, m_curg(BX)	// Only switch if on curg.
	JNE	noswitch

	MOVQ	m_g0(BX), DX
	MOVQ	(g_sched+gobuf_sp)(DX), SP	// Set SP to g0 stack

noswitch:
	SUBQ	$16, SP		// Space for results
	ANDQ	$~15, SP	// Align for C code

	MOVQ	runtime·vdsoClockgettimeSym(SB), AX
	CMPQ	AX, $0
	JEQ	fallback
	MOVL	$1, DI // CLOCK_MONOTONIC
	LEAQ	0(SP), SI
	CALL	AX
	MOVQ	0(SP), AX	// sec
	MOVQ	8(SP), DX	// nsec
	MOVQ	BP, SP		// Restore real SP
	MOVQ	$0, m_vdsoSP(BX)
	// sec is in AX, nsec in DX
	// return nsec in AX
	IMULQ	$1000000000, AX
	ADDQ	DX, AX
	MOVQ	AX, ret+0(FP)
	RET
fallback:
	LEAQ	0(SP), DI
	MOVQ	$0, SI
	MOVQ	runtime·vdsoGettimeofdaySym(SB), AX
	CALL	AX
	MOVQ	0(SP), AX	// sec
	MOVL	8(SP), DX	// usec
	MOVQ	BP, SP		// Restore real SP
	MOVQ	$0, m_vdsoSP(BX)
	IMULQ	$1000, DX
	// sec is in AX, nsec in DX
	// return nsec in AX
	IMULQ	$1000000000, AX
	ADDQ	DX, AX
	MOVQ	AX, ret+0(FP)
	RET

#include <signal.h>
/* Prototype for the glibc wrapper function */
int sigprocmask(int how, const sigset_t *restrict set, sigset_t *restrict oldset);
// 改变调用线程的信号掩码
// how 参数改变函数的不同行为 -> SIG_BLOCK | SIG_NOBLOCK | SIG_SETMASK
// sigset_t 实际类型的 uint32
//func rtsigprocmask(how int32, new, old *sigset, size int32)
TEXT runtime·rtsigprocmask(SB),NOSPLIT,$0-28
	MOVL	how+0(FP), DI
	MOVQ	new+8(FP), SI
	MOVQ	old+16(FP), DX
	MOVL	size+24(FP), R10
	MOVL	$SYS_rt_sigprocmask, AX
	SYSCALL
	CMPQ	AX, $0xfffffffffffff001
	JLS	2(PC)
	MOVL	$0xf1, 0xf1  // crash
	RET

// #include <signal.h>
/*
siginfo_t {
   int      si_signo;     /* Signal number */
   int      si_errno;     /* An errno value */
   int      si_code;      /* Signal code */
   int      si_trapno;    /* Trap number that caused
							 hardware-generated signal
							 (unused on most architectures) */
   pid_t    si_pid;       /* Sending process ID */
   uid_t    si_uid;       /* Real user ID of sending process */
   int      si_status;    /* Exit value or signal */
   clock_t  si_utime;     /* User time consumed */
   clock_t  si_stime;     /* System time consumed */
   union sigval si_value; /* Signal value */
   int      si_int;       /* POSIX.1b signal */
   void    *si_ptr;       /* POSIX.1b signal */
   int      si_overrun;   /* Timer overrun count;
							 POSIX.1b timers */
   int      si_timerid;   /* Timer ID; POSIX.1b timers */
   void    *si_addr;      /* Memory location which caused fault */
   long     si_band;      /* Band event (was int in
							 glibc 2.3.2 and earlier) */
   int      si_fd;        /* File descriptor */
   short    si_addr_lsb;  /* Least significant bit of address
							 (since Linux 2.6.32) */
   void    *si_lower;     /* Lower bound when address violation
							 occurred (since Linux 3.19) */
   void    *si_upper;     /* Upper bound when address violation
							 occurred (since Linux 3.19) */
   int      si_pkey;      /* Protection key on PTE that caused
							 fault (since Linux 4.6) */
   void    *si_call_addr; /* Address of system call instruction
							 (since Linux 3.5) */
   int      si_syscall;   /* Number of attempted system call
							 (since Linux 3.5) */
   unsigned int si_arch;  /* Architecture of attempted system call
							 (since Linux 3.5) */
}
struct sigaction {
   void     (*sa_handler)(int);  // 指定处理函数(处理函数只知道信号数)SIG_DFL, SIG_IGN, 或者一个函数指针，三者之一
   void     (*sa_sigaction)(int sig, siginfo_t *info, void *ucontext);  // ucontext 实际是一个 ucontext_t* 指针
   sigset_t   sa_mask;   // 实际类型是uint32 指定会被阻塞的 信号的掩码
   int        sa_flags;  // SA_NOCLDSTOP | SA_NOCLDWAIT 设置SA_SIGINFO 标记表示, sa_sigaction 代替 sa_handler, 用于获取信号详细再处理
   void     (*sa_restorer)(void);
};
int sigaction(int signum, const struct sigaction *restrict act, struct sigaction *restrict oldact);
// act 不是NULL，会设置新函数，oldact不失NULL，会返回新的函数
// signum 是信号量枚举定义，9是SIGKILL
func rt_sigaction(sig uintptr, new, old *sigactiont, size uintptr) int32
TEXT runtime·rt_sigaction(SB),NOSPLIT,$0-36
	MOVQ	sig+0(FP), DI
	MOVQ	new+8(FP), SI
	MOVQ	old+16(FP), DX
	MOVQ	size+24(FP), R10
	MOVL	$SYS_rt_sigaction, AX
	SYSCALL
	MOVL	AX, ret+32(FP)
	RET

//func callCgoSigaction(sig uintptr, new, old *sigactiont) int32
// cgo_sigaction.go
// Call the function stored in _cgo_sigaction using the GCC calling convention.
TEXT runtime·callCgoSigaction(SB),NOSPLIT,$16
	MOVQ	sig+0(FP), DI
	MOVQ	new+8(FP), SI
	MOVQ	old+16(FP), DX
	MOVQ	_cgo_sigaction(SB), AX
	MOVQ	SP, BX	// callee-saved
	ANDQ	$~15, SP	// alignment as per amd64 psABI
	CALL	AX
	MOVQ	BX, SP
	MOVL	AX, ret+24(FP)
	RET

// func sigfwd(fn uintptr, sig uint32, info *siginfo, ctx unsafe.Pointer)
// signal_unix.go
TEXT runtime·sigfwd(SB),NOSPLIT,$0-32
	MOVQ	fn+0(FP),    AX
	MOVL	sig+8(FP),   DI
	MOVQ	info+16(FP), SI
	MOVQ	ctx+24(FP),  DX
	PUSHQ	BP
	MOVQ	SP, BP
	ANDQ	$~15, SP     // alignment for x86_64 ABI
	CALL	AX
	MOVQ	BP, SP
	POPQ	BP
	RET

//func sigtramp(sig uint32, info *siginfo, ctx unsafe.Pointer)
//os_linux.go
TEXT runtime·sigtramp(SB),NOSPLIT,$72
	// Save callee-saved C registers, since the caller may be a C signal handler.
	MOVQ	BX,  bx-8(SP)
	MOVQ	BP,  bp-16(SP)  // save in case GOEXPERIMENT=noframepointer is set
	MOVQ	R12, r12-24(SP)
	MOVQ	R13, r13-32(SP)
	MOVQ	R14, r14-40(SP)
	MOVQ	R15, r15-48(SP)
	// We don't save mxcsr or the x87 control word because sigtrampgo doesn't
	// modify them.

	MOVQ	DX, ctx-56(SP)
	MOVQ	SI, info-64(SP)
	MOVQ	DI, signum-72(SP)
	MOVQ	$runtime·sigtrampgo(SB), AX
	CALL AX

	MOVQ	r15-48(SP), R15
	MOVQ	r14-40(SP), R14
	MOVQ	r13-32(SP), R13
	MOVQ	r12-24(SP), R12
	MOVQ	bp-16(SP),  BP
	MOVQ	bx-8(SP),   BX
	RET

// Used instead of sigtramp in programs that use cgo.
// Arguments from kernel are in DI, SI, DX.
TEXT runtime·cgoSigtramp(SB),NOSPLIT,$0
	// If no traceback function, do usual sigtramp.
	MOVQ	runtime·cgoTraceback(SB), AX
	TESTQ	AX, AX
	JZ	sigtramp

	// If no traceback support function, which means that
	// runtime/cgo was not linked in, do usual sigtramp.
	MOVQ	_cgo_callers(SB), AX
	TESTQ	AX, AX
	JZ	sigtramp

	// Figure out if we are currently in a cgo call.
	// If not, just do usual sigtramp.
	get_tls(CX)
	MOVQ	g(CX),AX
	TESTQ	AX, AX
	JZ	sigtrampnog     // g == nil
	MOVQ	g_m(AX), AX
	TESTQ	AX, AX
	JZ	sigtramp        // g.m == nil
	MOVL	m_ncgo(AX), CX
	TESTL	CX, CX
	JZ	sigtramp        // g.m.ncgo == 0
	MOVQ	m_curg(AX), CX
	TESTQ	CX, CX
	JZ	sigtramp        // g.m.curg == nil
	MOVQ	g_syscallsp(CX), CX
	TESTQ	CX, CX
	JZ	sigtramp        // g.m.curg.syscallsp == 0
	MOVQ	m_cgoCallers(AX), R8
	TESTQ	R8, R8
	JZ	sigtramp        // g.m.cgoCallers == nil
	MOVL	m_cgoCallersUse(AX), CX
	TESTL	CX, CX
	JNZ	sigtramp	// g.m.cgoCallersUse != 0

	// Jump to a function in runtime/cgo.
	// That function, written in C, will call the user's traceback
	// function with proper unwind info, and will then call back here.
	// The first three arguments, and the fifth, are already in registers.
	// Set the two remaining arguments now.
	MOVQ	runtime·cgoTraceback(SB), CX
	MOVQ	$runtime·sigtramp(SB), R9
	MOVQ	_cgo_callers(SB), AX
	JMP	AX

sigtramp:
	JMP	runtime·sigtramp(SB)

sigtrampnog:
	// Signal arrived on a non-Go thread. If this is SIGPROF, get a
	// stack trace.
	CMPL	DI, $27 // 27 == SIGPROF
	JNZ	sigtramp

	// Lock sigprofCallersUse.
	MOVL	$0, AX
	MOVL	$1, CX
	MOVQ	$runtime·sigprofCallersUse(SB), R11
	LOCK
	CMPXCHGL	CX, 0(R11)
	JNZ	sigtramp  // Skip stack trace if already locked.

	// Jump to the traceback function in runtime/cgo.
	// It will call back to sigprofNonGo, which will ignore the
	// arguments passed in registers.
	// First three arguments to traceback function are in registers already.
	MOVQ	runtime·cgoTraceback(SB), CX
	MOVQ	$runtime·sigprofCallers(SB), R8
	MOVQ	$runtime·sigprofNonGo(SB), R9
	MOVQ	_cgo_callers(SB), AX
	JMP	AX

// For cgo unwinding to work, this function must look precisely like
// the one in glibc.  The glibc source code is:
// https://sourceware.org/git/?p=glibc.git;a=blob;f=sysdeps/unix/sysv/linux/x86_64/sigaction.c
// The code that cares about the precise instructions used is:
// https://gcc.gnu.org/viewcvs/gcc/trunk/libgcc/config/i386/linux-unwind.h?revision=219188&view=markup
// 从信号处理器返回，并清除栈帧 return from signal handler and cleanup stack frame
// func sigreturn()
TEXT runtime·sigreturn(SB),NOSPLIT,$0
	MOVQ	$SYS_rt_sigreturn, AX
	SYSCALL
	INT $3	// not reached

//cgo_mmap.go
//系统分配内存
// func sysMmap(addr unsafe.Pointer, n uintptr, prot, flags, fd int32, off uint32) (p unsafe.Pointer, err int)
TEXT runtime·sysMmap(SB),NOSPLIT,$0
	MOVQ	addr+0(FP), DI
	MOVQ	n+8(FP), SI
	MOVL	prot+16(FP), DX
	MOVL	flags+20(FP), R10
	MOVL	fd+24(FP), R8
	MOVL	off+28(FP), R9

	MOVL	$SYS_mmap, AX
	SYSCALL
	CMPQ	AX, $0xfffffffffffff001
	JLS	ok
	NOTQ	AX
	INCQ	AX
	MOVQ	$0, p+32(FP)
	MOVQ	AX, err+40(FP)
	RET
ok:
	MOVQ	AX, p+32(FP)
	MOVQ	$0, err+40(FP)
	RET

// Call the function stored in _cgo_mmap using the GCC calling convention.
// This must be called on the system stack.
TEXT runtime·callCgoMmap(SB),NOSPLIT,$16
	MOVQ	addr+0(FP), DI
	MOVQ	n+8(FP), SI
	MOVL	prot+16(FP), DX
	MOVL	flags+20(FP), CX
	MOVL	fd+24(FP), R8
	MOVL	off+28(FP), R9
	MOVQ	_cgo_mmap(SB), AX
	MOVQ	SP, BX
	ANDQ	$~15, SP	// alignment as per amd64 psABI
	MOVQ	BX, 0(SP)
	CALL	AX
	MOVQ	0(SP), SP
	MOVQ	AX, ret+32(FP)
	RET

//func sysMunmap(addr unsafe.Pointer, n uintptr)
TEXT runtime·sysMunmap(SB),NOSPLIT,$0
	MOVQ	addr+0(FP), DI
	MOVQ	n+8(FP), SI
	MOVQ	$SYS_munmap, AX
	SYSCALL
	CMPQ	AX, $0xfffffffffffff001
	JLS	2(PC)
	MOVL	$0xf1, 0xf1  // crash
	RET

// Call the function stored in _cgo_munmap using the GCC calling convention.
// This must be called on the system stack.
TEXT runtime·callCgoMunmap(SB),NOSPLIT,$16-16
	MOVQ	addr+0(FP), DI
	MOVQ	n+8(FP), SI
	MOVQ	_cgo_munmap(SB), AX
	MOVQ	SP, BX
	ANDQ	$~15, SP	// alignment as per amd64 psABI
	MOVQ	BX, 0(SP)
	CALL	AX
	MOVQ	0(SP), SP
	RET

#include <sys/mman.h>
int madvise(void *addr, size_t length, int advice);
//func madvise(addr unsafe.Pointer, n uintptr, flags int32) int32
TEXT runtime·madvise(SB),NOSPLIT,$0
	MOVQ	addr+0(FP), DI
	MOVQ	n+8(FP), SI
	MOVL	flags+16(FP), DX
	MOVQ	$SYS_madvise, AX
	SYSCALL
	MOVL	AX, ret+24(FP)
	RET

#include <linux/futex.h>
#include <stdint.h>
#include <sys/time.h>
long futex(uint32_t *uaddr, int futex_op, uint32_t val, const struct timespec *timeout,   /* or: uint32_t val2 */uint32_t *uaddr2, uint32_t val3);
// os_linux.go
// int64 futex(int32 *uaddr, int32 op, int32 val,
// struct timespec *timeout, int32 *uaddr2, int32 val2);
TEXT runtime·futex(SB),NOSPLIT,$0
	MOVQ	addr+0(FP), DI
	MOVL	op+8(FP), SI
	MOVL	val+12(FP), DX
	MOVQ	ts+16(FP), R10
	MOVQ	addr2+24(FP), R8
	MOVL	val3+32(FP), R9
	MOVL	$SYS_futex, AX
	SYSCALL
	MOVL	AX, ret+40(FP)
	RET

#include <sched.h>
// 相比fork 提供更精确的执行上下文共享的控制
int clone(int (*fn)(void *), void *stack, int flags, void *arg, .../* pid_t *parent_tid, void *tls, pid_t *child_tid */ );
// int32 clone(int32 flags, void *stk, M *mp, G *gp, void (*fn)(void));
TEXT runtime·clone(SB),NOSPLIT,$0
	MOVL	flags+0(FP), DI
	MOVQ	stk+8(FP), SI
	MOVQ	$0, DX
	MOVQ	$0, R10

	// Copy mp, gp, fn off parent stack for use by child.
	// Careful: Linux system call clobbers CX and R11.
	MOVQ	mp+16(FP), R8
	MOVQ	gp+24(FP), R9
	MOVQ	fn+32(FP), R12

	MOVL	$SYS_clone, AX
	SYSCALL

	// In parent, return.
	CMPQ	AX, $0
	JEQ	3(PC)
	MOVL	AX, ret+40(FP)
	RET

	// In child, on new stack.
	MOVQ	SI, SP

	// If g or m are nil, skip Go-related setup.
	CMPQ	R8, $0    // m
	JEQ	nog
	CMPQ	R9, $0    // g
	JEQ	nog

	// Initialize m->procid to Linux tid
	MOVL	$SYS_gettid, AX
	SYSCALL
	MOVQ	AX, m_procid(R8)

	// Set FS to point at m->tls.
	LEAQ	m_tls(R8), DI
	CALL	runtime·settls(SB)

	// In child, set up new stack
	get_tls(CX)
	MOVQ	R8, g_m(R9)
	MOVQ	R9, g(CX)
	CALL	runtime·stackcheck(SB)		//检查sp指针指向栈范围内

nog:
	// Call fn
	CALL	R12

	// It shouldn't return. If it does, exit that thread.
	MOVL	$111, DI
	MOVL	$SYS_exit, AX
	SYSCALL
	JMP	-3(PC)	// keep exiting

#include <signal.h>
int sigaltstack(const stack_t *restrict ss, stack_t *restrict old_ss);
//func sigaltstack(new, old *stackt)
//os_linux.go
TEXT runtime·sigaltstack(SB),NOSPLIT,$-8
	MOVQ	new+0(FP), DI
	MOVQ	old+8(FP), SI
	MOVQ	$SYS_sigaltstack, AX
	SYSCALL
	CMPQ	AX, $0xfffffffffffff001
	JLS	2(PC)
	MOVL	$0xf1, 0xf1  // crash
	RET

#include <asm/prctl.h>
#include <sys/prctl.h>

int arch_prctl(int code, unsigned long addr);
int arch_prctl(int code, unsigned long *addr);
// set tls base to DI 从Di开始的地址 设置 线程
// func settls() stubs_amd64.go
TEXT runtime·settls(SB),NOSPLIT,$32
#ifdef GOOS_android
	// Android stores the TLS offset in runtime·tls_g.
	SUBQ	runtime·tls_g(SB), DI
#else
	ADDQ	$8, DI	// ELF wants to use -8(FS)
#endif
	MOVQ	DI, SI
	MOVQ	$0x1002, DI	// ARCH_SET_FS
	MOVQ	$SYS_arch_prctl, AX		//设置线程状态
	SYSCALL
	CMPQ	AX, $0xfffffffffffff001
	JLS	2(PC)
	MOVL	$0xf1, 0xf1  // crash
	RET

#include <sched.h>
int sched_yield(void);
//os_linux.go func osyield()
TEXT runtime·osyield(SB),NOSPLIT,$0
	MOVL	$SYS_sched_yield, AX	//让出cpu
	SYSCALL
	RET

#define _GNU_SOURCE             /* See feature_test_macros(7) */
#include <sched.h>
int sched_setaffinity(pid_t pid, size_t cpusetsize, const cpu_set_t *mask);
int sched_getaffinity(pid_t pid, size_t cpusetsize, cpu_set_t *mask);
//os_linux.go
//func sched_getaffinity(pid, len uintptr, buf *byte) int32
TEXT runtime·sched_getaffinity(SB),NOSPLIT,$0
	MOVQ	pid+0(FP), DI
	MOVQ	len+8(FP), SI
	MOVQ	buf+16(FP), DX
	MOVL	$SYS_sched_getaffinity, AX
	SYSCALL
	MOVL	AX, ret+24(FP)
	RET

#include <sys/epoll.h>
int epoll_create(int size);
int epoll_create1(int flags);
//netpoll_epoll.go
// int32 runtime·epollcreate(int32 size);
TEXT runtime·epollcreate(SB),NOSPLIT,$0
	MOVL    size+0(FP), DI
	MOVL    $SYS_epoll_create, AX
	SYSCALL
	MOVL	AX, ret+8(FP)
	RET

// int32 runtime·epollcreate1(int32 flags);
TEXT runtime·epollcreate1(SB),NOSPLIT,$0
	MOVL	flags+0(FP), DI
	MOVL	$SYS_epoll_create1, AX
	SYSCALL
	MOVL	AX, ret+8(FP)
	RET

#include <sys/epoll.h>
int epoll_ctl(int epfd, int op, int fd, struct epoll_event *event);
// func epollctl(epfd, op, fd int32, ev *epollEvent) int
TEXT runtime·epollctl(SB),NOSPLIT,$0
	MOVL	epfd+0(FP), DI
	MOVL	op+4(FP), SI
	MOVL	fd+8(FP), DX
	MOVQ	ev+16(FP), R10
	MOVL	$SYS_epoll_ctl, AX
	SYSCALL
	MOVL	AX, ret+24(FP)
	RET

#include <sys/epoll.h>
typedef union epoll_data {
   void    *ptr;
   int      fd;
   uint32_t u32;
   uint64_t u64;
} epoll_data_t;

struct epoll_event {
   uint32_t     events;    /* Epoll events */
   epoll_data_t data;      /* User data variable */
};
int epoll_wait(int epfd, struct epoll_event *events, int maxevents, int timeout);
int epoll_pwait(int epfd, struct epoll_event *events, int maxevents, int timeout, const sigset_t *sigmask);
int epoll_pwait2(int epfd, struct epoll_event *events, int maxevents, const struct timespec *timeout, const sigset_t *sigmask);
// maxevents 是 想接受的时间的最大数量, 必须 >0
// timeout 执行 wait 最多阻塞的
// int32 runtime·epollwait(int32 epfd, EpollEvent *ev, int32 nev, int32 timeout);
TEXT runtime·epollwait(SB),NOSPLIT,$0
	// This uses pwait instead of wait, because Android O blocks wait.
	MOVL	epfd+0(FP), DI
	MOVQ	ev+8(FP), SI
	MOVL	nev+16(FP), DX
	MOVL	timeout+20(FP), R10
	MOVQ	$0, R8
	MOVL	$SYS_epoll_pwait, AX
	SYSCALL
	MOVL	AX, ret+24(FP)
	RET

#include <unistd.h>
#include <fcntl.h>
int fcntl(int fd, int cmd, ... /* arg */ );
//  func closeonexec(fd int32) netpoll_epoll.go
// void runtime·closeonexec(int32 fd);
TEXT runtime·closeonexec(SB),NOSPLIT,$0
	MOVL    fd+0(FP), DI  // fd
	MOVQ    $2, SI  // F_SETFD
	MOVQ    $1, DX  // FD_CLOEXEC
	MOVL	$SYS_fcntl, AX
	SYSCALL
	RET

// os_linux.go
// func runtime·setNonblock(int32 fd)
TEXT runtime·setNonblock(SB),NOSPLIT,$0-4
	MOVL    fd+0(FP), DI  // fd
	MOVQ    $3, SI  // F_GETFL
	MOVQ    $0, DX
	MOVL	$SYS_fcntl, AX
	SYSCALL
	MOVL	fd+0(FP), DI // fd
	MOVQ	$4, SI // F_SETFL
	MOVQ	$0x800, DX // O_NONBLOCK
	ORL	AX, DX
	MOVL	$SYS_fcntl, AX
	SYSCALL
	RET

#include <unistd.h>

int access(const char *pathname, int mode);

#include <fcntl.h>           /* Definition of AT_* constants */
#include <unistd.h>

int faccessat(int dirfd, const char *pathname, int mode, int flags);
			   /* But see C library/kernel differences, below */

int faccessat2(int dirfd, const char *pathname, int mode, int flags);

//stubs_linux.go
// int access(const char *name, int mode)
TEXT runtime·access(SB),NOSPLIT,$0
	// This uses faccessat instead of access, because Android O blocks access.
	MOVL	$AT_FDCWD, DI // AT_FDCWD, so this acts like access
	MOVQ	name+0(FP), SI
	MOVL	mode+8(FP), DX
	MOVL	$0, R10
	MOVL	$SYS_faccessat, AX
	SYSCALL
	MOVL	AX, ret+16(FP)
	RET

#include <sys/socket.h>
struct sockaddr {
	__uint8_t       sa_len;         /* total length */
	sa_family_t     sa_family;      /* [XSI] address family */
	char            sa_data[14];    /* [XSI] addr value (actually larger) */
};
typedef uint32 socklen_t
int connect(int sockfd, const struct sockaddr *addr, socklen_t addrlen);
// int connect(int fd, const struct sockaddr *addr, socklen_t addrlen)
TEXT runtime·connect(SB),NOSPLIT,$0-28
	MOVL	fd+0(FP), DI
	MOVQ	addr+8(FP), SI
	MOVL	len+16(FP), DX
	MOVL	$SYS_connect, AX
	SYSCALL
	MOVL	AX, ret+24(FP)
	RET

#include <sys/socket.h>
int socket(int domain, int type, int protocol);
// domain -> AF_INET | AF_INET6 | AF_UNIX | AF_PACKET | AF_LOCAL
// type -> SOCK_STREAM | SOCK_DGRAM | SOCK_RAW
// protocol -> 参考 cat /etc/protocols
// int socket(int domain, int type, int protocol)
TEXT runtime·socket(SB),NOSPLIT,$0-20
	MOVL	domain+0(FP), DI
	MOVL	typ+4(FP), SI
	MOVL	prot+8(FP), DX
	MOVL	$SYS_socket, AX
	SYSCALL
	MOVL	AX, ret+16(FP)
	RET

// brk 的实际含义就是设置新的 数据段结尾地址
// int brk(void *addr); // 设置断结尾地址，需要自己维护地址加减(c lib形式) 系统调用形式是直接返回 地址指针
// void *sbrk(intptr_t increment); //(c lib)使用brk系统调用实现， 返回之前的地址，而不是增加后的
// func sbrk0() uintptr  以0增量调用sbrk（）可用于查找程序中断(program break)的当前位置 参考链接 https://stackoverflow.com/questions/6338162/what-is-program-break-where-does-it-start-from-0x00
// go 分配内存不使用brk，那使用的是什么??
TEXT runtime·sbrk0(SB),NOSPLIT,$0-8
	// Implemented as brk(NULL).
	MOVQ	$0, DI
	MOVL	$SYS_brk, AX
	SYSCALL
	MOVQ	AX, ret+0(FP)  // 返回新的 数据段结尾地址
	RET

#include <sys/utsname.h>
int uname(struct utsname *buf);
//os_linux_x86.go
// func uname(utsname *new_utsname) int
TEXT ·uname(SB),NOSPLIT,$0-16
	MOVQ    utsname+0(FP), DI
	MOVL    $SYS_uname, AX
	SYSCALL
	MOVQ	AX, ret+8(FP)
	RET

#include <sys/mman.h>

int mlock(const void *addr, size_t len);
int mlock2(const void *addr, size_t len, unsigned int flags);
int munlock(const void *addr, size_t len);

int mlockall(int flags);
int munlockall(void);
//os_linux_x86.go
// func mlock(addr, len uintptr) int
TEXT ·mlock(SB),NOSPLIT,$0-24
	MOVQ    addr+0(FP), DI
	MOVQ    len+8(FP), SI
	MOVL    $SYS_mlock, AX
	SYSCALL
	MOVQ	AX, ret+16(FP)
	RET
