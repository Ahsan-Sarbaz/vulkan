//go:build windows

package ccall

import (
	"syscall"
	"unsafe"
)

// call passes the n integers and then, if any float was set, all eight float
// slots after them. The block's own slots hold the list, a16..a32 being free
// here, so the nosplit frame stays small enough for SyscallN's chain below it.
// The x64 convention has the caller own the argument area, so a callee reading
// fewer slots than it is given ignores the rest.
//
// The slots must be one run: a negative array length does not build.
var _ [unsafe.Offsetof(syscallArgs{}.a1) + 31*unsafe.Sizeof(uintptr(0)) - unsafe.Offsetof(syscallArgs{}.a32)]struct{}

//go:nosplit
func (f *Frame) call(n int) uintptr {
	slots := (*[32]uintptr)(unsafe.Pointer(&f.a1))
	if f.floats != [len(f.floats)]uintptr{} {
		copy(slots[n:n+len(f.floats)], f.floats[:])
		n += len(f.floats)
	}
	r, _, _ := syscall.SyscallN(f.fn, slots[:n]...)
	return r
}
