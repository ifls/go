// +build !goexperiment.staticlockranking

package sync

import "unsafe"

// Approximation of notifyList in runtime/sema.go. Size and alignment must agree. 大小和字段对齐必须一致 init函数会检查
type notifyList struct {
	wait   uint32
	notify uint32
	lock   uintptr // key field of the mutex
	head   unsafe.Pointer
	tail   unsafe.Pointer
}
