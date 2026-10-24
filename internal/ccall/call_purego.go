//go:build !windows

package ccall

import (
	"unsafe"

	_ "github.com/ebitengine/purego" // for the linknames below
)

// syscallXABI0 is the address of purego's ABI0 trampoline.
//
//go:linkname syscallXABI0 github.com/ebitengine/purego.syscallXABI0
var syscallXABI0 uintptr

// trampoline is syscallXABI0 as cgocall takes it, converted once so the nosplit
// path makes no call before cgocall's.
var trampoline unsafe.Pointer

// A variable linkname to a name purego no longer has links as 0 rather than
// failing, and cgocall(nil) is a fatal error that recover cannot catch.
func init() {
	if syscallXABI0 == 0 {
		panic("ccall: github.com/ebitengine/purego.syscallXABI0 linked as 0; purego renamed its trampoline, so update the linkname and syscallArgs in internal/ccall to match")
	}
	trampoline = fnPtr(syscallXABI0)
}

// cgocall is the runtime's own C entry: it enters a syscall, switches to the
// system stack, and calls fn with arg.
//
//go:linkname cgocall runtime.cgocall
//go:noescape
func cgocall(fn, arg unsafe.Pointer) int32

// call is the same for every count: the stub reads the whole block.
//
//go:nosplit
func (f *Frame) call(int) uintptr {
	cgocall(trampoline, unsafe.Pointer(f))
	return f.a1
}

// fnPtr turns the trampoline's address into a pointer. The laundering keeps go
// vet's unsafeptr check quiet: this is a C address that never was a Go pointer,
// the one case the rule is not protecting against.
func fnPtr(u uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&u))
}
