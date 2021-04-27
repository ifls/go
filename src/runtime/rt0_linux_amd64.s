// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include "textflag.h"


[root@iZwz91m0zmp2dhu4ym5b6zZ ~]# readelf -h ./main
ELF Header:
 Magic:   7f 45 4c 46 02 01 01 00 00 00 00 00 00 00 00 00
 Class:                             ELF64
 Data:                              2's complement, little endian
 Version:                           1 (current)
 OS/ABI:                            UNIX - System V
 ABI Version:                       0
 Type:                              EXEC (Executable file)
 Machine:                           Advanced Micro Devices X86-64
 Version:                           0x1
 Entry point address:               0x44d730
 Start of program headers:          64 (bytes into file)
 Start of section headers:          456 (bytes into file)
 Flags:                             0x0
 Size of this header:               64 (bytes)
 Size of program headers:           56 (bytes)
 Number of program headers:         7
 Size of section headers:           64 (bytes)
 Number of section headers:         25
 Section header string table index: 3
[root@iZwz91m0zmp2dhu4ym5b6zZ ~]# objdump -d main | grep 44d730
000000000044d730 <_rt0_amd64_linux>:
 44d730:       e9 2b c6 ff ff          jmpq   449d60 <_rt0_amd64>
//直接进入通用的amd64启动代码 elf里的 entry point address,
TEXT _rt0_amd64_linux(SB),NOSPLIT,$-8 // 参数及返回值大小 8B，
	JMP	_rt0_amd64(SB)

TEXT _rt0_amd64_linux_lib(SB),NOSPLIT,$0
	JMP	_rt0_amd64_lib(SB)
