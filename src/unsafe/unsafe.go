// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
	Package unsafe contains operations that step around绕开 the type safety of Go programs.

	Packages that import unsafe may be non-portable 不可移植 and are not protected by the
	Go 1 compatibility guidelines. 不被 go1 兼容性参考 所保护
*/
package unsafe

// ArbitraryType is here for the purposes of documentation only and is not actually part of the unsafe package.
//It represents the type of an arbitrary Go expression.
type ArbitraryType int

// Pointer represents a pointer to an arbitrary type. void*
// There are four special operations available for type Pointer that are not available for other types:
//	- A pointer value of any type can be converted to a Pointer. Pointer = &var1
//	- A Pointer can be converted to a pointer value of any type. var a *type = Pointer
//	- A uintptr can be converted to a Pointer. Pointer = uintptr
//	- A Pointer can be converted to a uintptr. uintptr = Pointer

// Pointer therefore allows a program to defeat击败 the type system and read and write
// arbitrary memory. 读写任意内存
// It should be used with extreme care. 更改极度小心使用
//
// The following patterns involving Pointer are valid. 使用模式参考
// Code not using these patterns is likely to be invalid today or to become invalid in the future.
// Even the valid patterns below come with important caveats 说明,警告.
// go vet 是很好的检测工具
// Running "go vet" can help find uses of Pointer that do not conform to these patterns, but silence from "go vet" is not a guarantee that the code is valid.
//
// (1) Conversion of a *T1 to Pointer to *T2. 指针类型转换
// T2 <= T1
// Provided that T2 is no larger than T1 and that the two share an equivalent memory layout,
// this conversion allows reinterpreting重新解释 data of one type as data of another type.
// An example is the implementation of math.Float64bits:
//
//	func Float64bits(f float64) uint64 {
//		return *(*uint64)(unsafe.Pointer(&f))
//	}
//
// (2) Conversion of a Pointer to a uintptr (but not back to Pointer).
// 	uintptr = Pointer
// Converting a Pointer to a uintptr produces产生 the memory address of the value pointed at, as an integer.
// The usual use for such a uintptr is to print it.
// 反向转换 Point = uintptr 通常不合理
// Conversion of a uintptr back to Pointer is not valid in general.
//
// A uintptr is an integer, not a reference. 是数字不是引用
// Converting a Pointer to a uintptr creates an integer value with no pointer semantics语义.
// Even if a uintptr holds the address of some object, the garbage collector will not update that uintptr's value
// if the object moves, nor will that uintptr keep the object from being reclaimed.
//
// The remaining patterns enumerate列举 the only valid conversions from uintptr to Pointer.
//
// (3) Conversion of a Pointer to a uintptr and back, with arithmetic算术运算.
//
// If p points into an allocated object, it can be advanced through the object
// by conversion to uintptr, addition加上那个 of an offset, and conversion back to Pointer.
//
//	p = unsafe.Pointer(uintptr(p) + offset)
//
// The most common use of this pattern is to access fields in a struct or elements of an array:  通常用来访问结构和数组元素
//
//	// equivalent等价形式 to f := unsafe.Pointer(&s.f)
//	f := unsafe.Pointer(uintptr(unsafe.Pointer(&s)) + unsafe.Offsetof(s.f))
//
//	// equivalent to e := unsafe.Pointer(&x[i])
//	e := unsafe.Pointer(uintptr(unsafe.Pointer(&x[0])) + i*unsafe.Sizeof(x[0]))
//
// It is valid both to add and to subtract offsets from a pointer in this way.
// It is also valid to use &^ to round pointers, usually for alignment对齐.
// In all cases, the result must continue to point into the original allocated object.
//
// Unlike in C, it is not valid to advance a pointer just beyond the end of its original allocation:
// 指向最后一个元素是不合法的
//	// INVALID: end points指向 outside allocated space.
//	var s thing
//	end = unsafe.Pointer(uintptr(unsafe.Pointer(&s)) + unsafe.Sizeof(s))
//
//	// INVALID: end points指向 outside allocated space.
//	b := make([]byte, n)
//	end = unsafe.Pointer(uintptr(unsafe.Pointer(&b[0])) + uintptr(n))
//
// Note注意 that both conversions must appear in the same expression, with only the intervening介于中间 arithmetic between them:
//
//	// INVALID: uintptr cannot be stored in variable
//	// before conversion back to Pointer.
//	u := uintptr(p)
//	p = unsafe.Pointer(u + offset)
//
// Note that the pointer must point into an allocated object, so it may not be nil.
//
//	// INVALID: conversion of nil pointer
//	u := unsafe.Pointer(nil)
//	p := unsafe.Pointer(uintptr(u) + offset)
//
// (4) Conversion of a Pointer to a uintptr when calling syscall.Syscall.
//
// The Syscall functions in package syscall pass their uintptr arguments directly 直接传递uintptr
// to the operating system, which then may, depending on the details of the call,
// reinterpret some of them as pointers. 再把他们解释为指针
// That is, the system call implementation is implicitly隐式 converting certain arguments back from uintptr to pointer.
//
// If a pointer argument must be converted to uintptr for use as an argument,
// that conversion must appear in the call expression itself:
//
//	syscall.Syscall(SYS_READ, uintptr(fd), uintptr(unsafe.Pointer(p)), uintptr(n))
//
// The compiler handles a Pointer converted to a uintptr in the argument list of a call to a function
// implemented in assembly by arranging安排 that the referenced allocated object, if any, is retained and not moved until the call completes, 持有且不移动
// even though from the types alone it would appear that the object is no longer needed during the call.
//
// For the compiler to recognize this pattern, the conversion must appear in the argument list:
//
//	// INVALID: uintptr cannot be stored in variable before implicit conversion back to Pointer during system call.
//	u := uintptr(unsafe.Pointer(p))
//	syscall.Syscall(SYS_READ, uintptr(fd), u, uintptr(n))
//
// (5) Conversion of the result of reflect.Value.Pointer or reflect.Value.UnsafeAddr from uintptr to Pointer.
//
// Package reflect's Value methods named Pointer and UnsafeAddr return type uintptr instead of unsafe.Pointer
// to keep callers from changing the result to an arbitrary type without first importing "unsafe" 不需要导入unsafe包.
// However, this means that the result is fragile易碎的 and must be converted to Pointer immediately after making the call,
// in the same expression:
//
//	p := (*int)(unsafe.Pointer(reflect.ValueOf(new(int)).Pointer()))
//
// As in the cases above 如上所示, it is invalid to store the result before the conversion:
//
//	// INVALID: uintptr cannot be stored in variable before conversion back to Pointer.
//	u := reflect.ValueOf(new(int)).Pointer()
//	p := (*int)(unsafe.Pointer(u))
//
// (6) Conversion of a reflect.SliceHeader or reflect.StringHeader Data field to or from Pointer.
//
// As in the previous case, the reflect data structures SliceHeader and StringHeader
// declare the field Data as a uintptr to keep callers from changing the result to
// an arbitrary type without first importing "unsafe".
//
// However, this means that SliceHeader and StringHeader are only valid when interpreting the content
// of an actual slice or string value.
//
//	var s string
//	hdr := (*reflect.StringHeader)(unsafe.Pointer(&s)) // case 1
//	hdr.Data = uintptr(unsafe.Pointer(p))              // case 6 (this case)
//	hdr.Len = n
//
// In this usage hdr.Data is really an alternate way to refer to the underlying pointer in the string header, not a uintptr variable itself.
//
// In general, reflect.SliceHeader and reflect.StringHeader should be used
// only as *reflect.SliceHeader and *reflect.StringHeader pointing at actual slices or strings, never as plain简单 structs.
// A program should not declare or allocate variables of these struct types.
//
//	// INVALID: a directly-declared header will not hold Data as a reference. 不算引用
//	var hdr reflect.StringHeader
//	hdr.Data = uintptr(unsafe.Pointer(p))
//	hdr.Len = n
//	s := *(*string)(unsafe.Pointer(&hdr)) // p possibly already lost 可能被gc了
//
type Pointer *ArbitraryType

// Sizeof takes an expression表达式 x of any type and returns the size in bytes of a hypothetical假设 variable v as if v was declared via var v = x.
// The size does not include any memory possibly referenced by x. 不包含x引用的其他内存空间的尺寸
// For instance例如, if x is a slice, Sizeof returns the size of the slice descriptor是sliceheader的长度, not the size of the memory referenced by the slice.
// The return value of Sizeof is a Go constant. 编译时确定
//任意类型的变量
func Sizeof(x ArbitraryType) uintptr

// Offsetof returns the offset within the struct结构体 of the field represented by x, which must be of the form structValue.field.
// In other words也就是说, it returns the number of bytes between the start of the struct and the start of the field.
// The return value of Offsetof is a Go constant. 编译时确定
func Offsetof(x ArbitraryType) uintptr

// Alignof takes an expression x of any type and returns the required alignment of a hypothetical假想的 variable v as if v was declared via var v = x.

// It is the largest value m such that the address of v is always zero mod m.   v % m = 0  m最大

// It is the same as the value returned by reflect.TypeOf(x).Align(). 和反射相同

// As a special case, if a variable s is of struct type and f is a field within that struct, then Alignof(s.f) will return the required alignment
// of a field of that type within a struct. 返回结构体内部相对的对齐值
// This case is the same as the value returned by reflect.TypeOf(s.f).FieldAlign(). 字段对齐值
// The return value of Alignof is a Go constant. 编译时确定
func Alignof(x ArbitraryType) uintptr
