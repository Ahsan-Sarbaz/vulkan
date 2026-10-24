// Package ccall calls C functions the way cgo does, without cgo.
//
// The generated Vulkan binding in this module goes through here rather than
// through purego's public SyscallN, for two reasons.
//
// SyscallN allocates. It is variadic and carries go:uintptrescapes, so its
// argument slice escapes and every call with an argument costs one allocation.
// A renderer makes thousands of calls a frame against a zero-allocation budget.
//
// The entry point underneath it does not allocate but is slower than it needs to
// be: it takes its argument block from a sync.Pool, because the obvious
// alternative -- a local struct -- escapes too, the compiler having no body to
// analyse for runtime.cgocall.
//
// go:noescape on the linknamed cgocall is what makes a local work, and it is not
// a fib: cgocall hands the pointer to a C call that returns before it does,
// which is the same promise cgo's own generated wrappers rely on with their own
// stack-allocated argument struct. This is not a trick; it is what cgo does,
// reached a different way.
//
// # The stack must not move
//
// A binding passes a Go address as a uintptr, so the compiler treats it as a
// number: if the goroutine's stack is grown or shrunk and copied after the
// conversion and before cgocall, every pointer into it is adjusted except that
// one. C then writes an out-parameter into the freed old stack and the caller
// reads zero from the new one (a null VkRenderPass, in practice), or reads its
// create-info from memory another goroutine may already own. A stack moves only
// in a function prologue's stack check, so the rule is that nothing between the
// conversion and cgocall may have one: the CallN methods are go:nosplit, as are
// cgocall and syscall.Syscall, which relies on the same rule.
//
// That is why the caller supplies the block. A nosplit function's frame plus
// the deepest nosplit chain below it must fit in about 800 bytes; cgocall's
// chain takes about 450 and the block 336, so a CallN holding its own block is
// over the limit and the linker refuses it. A Frame declared in the function
// that converts the addresses was sized by that function's own prologue, which
// ran before the conversion. new(Frame) does not escape, so it costs no
// allocation. go:uintptrescapes would also be correct, at one allocation a call.
//
// The generated bindings are go:noinline so each Frame stays in its binding:
// inlined, every call site adds 336 bytes to its caller's frame.
//
// # Floats
//
// The C ABI on arm64 and amd64 numbers float arguments apart from integer ones,
// so f(ptr, float, int) puts the int in the second integer register, not the
// third. A binding passes its integers to CallN and sets each float first, in
// order, with Float32 or Float64; a float result is read back the same way.
// Converting a float to uintptr instead truncates it and loads the wrong
// register.
//
// # Cost
//
// One libc call on an M4 Max, from the benchmarks in this package:
//
//	purego.SyscallN     55.9 ns   1 alloc
//	its pooled entry    43.2 ns   0 allocs
//	Frame.Call1         24.2 ns   0 allocs
//	Call1 via asmcgocall 14.6 ns  0 allocs
//
// purego 0.11 put about 4 ns on every row: its darwin trampoline now reads errno
// after each call, and the block grew to 32 integer slots. On purego 0.10,
// against a cgo binding for the same C function, this ran about 1.2x on a bare
// call and 1.3x on one with arguments -- down from 2.0x and 2.2x through
// SyscallN. Cost is flat in argument count: the block is a fixed 336 bytes
// whatever the call takes.
//
// # Why not asmcgocall
//
// The last row above is rejected, and measured rather than argued about.
// Dropping cgocall for asmcgocall skips entersyscall, so the goroutine stays
// runnable as far as the scheduler knows; sysmon sees it running past its slice
// and preempts it by sending SIGURG to a thread that is inside C, and the C call
// takes the signal. A 200 ms usleep sleeps 204 ms through cgocall and returns
// after 88 microseconds through asmcgocall.
//
// A graphics driver blocks in ioctl or poll for a buffer swap, for a fence wait,
// and for mapping memory the GPU still holds. Every one of those would be a call
// the runtime is interrupting. Nine nanoseconds does not buy that.
//
// # Running the comparison
//
// The measurements above come from this package's benchmarks, but the
// asmcgocall half of them is behind the `ccall_asmprobe` tag and needs
// `-ldflags=-checklinkname=0`: since Go 1.27 the linker refuses a pull linkname
// to runtime.asmcgocall, and an ordinary `go test ./internal/ccall/` was failing
// to link over a comparison that exists to justify not using it:
//
//	go test -tags ccall_asmprobe -ldflags=-checklinkname=0 -bench . ./internal/ccall/
//
// runtime.cgocall, which this package actually calls, is still permitted.
//
// # Coupling
//
// The linknames reach into purego's internals, which is a real dependency on
// another module's unexported names. TestRawCallMatchesSyscallN calls this path
// and purego's own against the same function and fails if they ever disagree,
// TestArgsBlockMatchesPuregoLayout fails if the block is shorter than purego's,
// so an upgrade that reshapes them breaks a test rather than a renderer.
package ccall

import (
	"math"
	"unsafe"

	_ "github.com/ebitengine/purego" // for the linknames below
)

// syscallArgs is purego's argument block, and the layout its assembly reads.
// The stub loads the a slots into the integer registers, then the stack, and
// f1..f8 into the float registers: each numbered on its own, as the C ABI does.
// a16..a32 are never set here but must exist: the stub reads them as stack
// arguments and writes float results back to f1..f4, past a shorter block.
type syscallArgs struct {
	fn, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14, a15                uintptr
	a16, a17, a18, a19, a20, a21, a22, a23, a24, a25, a26, a27, a28, a29, a30, a31, a32 uintptr
	floats                                                                              [8]uintptr // purego's f1..f8
	arm64R8                                                                             uintptr
}

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

// Frame is one C call's argument block, declared by the function that converts
// the call's Go addresses: new(ccall.Frame).CallN(...). The package comment says why.
type Frame struct{ syscallArgs }

// Float32 and Float64 set the i'th float argument, counting floats only: a C
// call's integer arguments go to CallN and number separately.
func (f *Frame) Float32(i int, x float32) { f.floats[i] = uintptr(math.Float32bits(x)) }

func (f *Frame) Float64(i int, x float64) { f.floats[i] = uintptr(math.Float64bits(x)) }

// Float32Result and Float64Result read a float return after CallN.
func (f *Frame) Float32Result() float32 { return math.Float32frombits(uint32(f.floats[0])) }

func (f *Frame) Float64Result() float64 { return math.Float64frombits(uint64(f.floats[0])) }

// Call0..Call15 name an argument count so a call passes what it has rather than
// padding to fifteen. They are nosplit, like cgocall, so the stack cannot grow
// or shrink between the caller converting an address and C using it.

//go:nosplit
func (f *Frame) Call0(fn uintptr) uintptr {
	f.fn = fn
	return f.call()
}

//go:nosplit
func (f *Frame) Call1(fn, a1 uintptr) uintptr {
	f.fn = fn
	f.a1 = a1
	return f.call()
}

//go:nosplit
func (f *Frame) Call2(fn, a1, a2 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2 = a1, a2
	return f.call()
}

//go:nosplit
func (f *Frame) Call3(fn, a1, a2, a3 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3 = a1, a2, a3
	return f.call()
}

//go:nosplit
func (f *Frame) Call4(fn, a1, a2, a3, a4 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4 = a1, a2, a3, a4
	return f.call()
}

//go:nosplit
func (f *Frame) Call5(fn, a1, a2, a3, a4, a5 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4, f.a5 = a1, a2, a3, a4, a5
	return f.call()
}

//go:nosplit
func (f *Frame) Call6(fn, a1, a2, a3, a4, a5, a6 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4, f.a5, f.a6 = a1, a2, a3, a4, a5, a6
	return f.call()
}

//go:nosplit
func (f *Frame) Call7(fn, a1, a2, a3, a4, a5, a6, a7 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4, f.a5, f.a6, f.a7 = a1, a2, a3, a4, a5, a6, a7
	return f.call()
}

//go:nosplit
func (f *Frame) Call8(fn, a1, a2, a3, a4, a5, a6, a7, a8 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4, f.a5, f.a6, f.a7, f.a8 = a1, a2, a3, a4, a5, a6, a7, a8
	return f.call()
}

//go:nosplit
func (f *Frame) Call9(fn, a1, a2, a3, a4, a5, a6, a7, a8, a9 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4, f.a5, f.a6, f.a7, f.a8, f.a9 = a1, a2, a3, a4, a5, a6, a7, a8, a9
	return f.call()
}

//go:nosplit
func (f *Frame) Call10(fn, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4, f.a5, f.a6, f.a7, f.a8, f.a9, f.a10 = a1, a2, a3, a4, a5, a6, a7, a8, a9, a10
	return f.call()
}

//go:nosplit
func (f *Frame) Call11(fn, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4, f.a5, f.a6, f.a7, f.a8, f.a9, f.a10, f.a11 = a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11
	return f.call()
}

//go:nosplit
func (f *Frame) Call12(fn, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4, f.a5, f.a6, f.a7, f.a8, f.a9, f.a10, f.a11, f.a12 = a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12
	return f.call()
}

//go:nosplit
func (f *Frame) Call13(fn, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4, f.a5, f.a6, f.a7, f.a8, f.a9, f.a10, f.a11, f.a12, f.a13 = a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13
	return f.call()
}

//go:nosplit
func (f *Frame) Call14(fn, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4, f.a5, f.a6, f.a7, f.a8, f.a9, f.a10, f.a11, f.a12, f.a13, f.a14 = a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14
	return f.call()
}

//go:nosplit
func (f *Frame) Call15(fn, a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14, a15 uintptr) uintptr {
	f.fn = fn
	f.a1, f.a2, f.a3, f.a4, f.a5, f.a6, f.a7, f.a8, f.a9, f.a10, f.a11, f.a12, f.a13, f.a14, f.a15 = a1, a2, a3, a4, a5, a6, a7, a8, a9, a10, a11, a12, a13, a14, a15
	return f.call()
}

//go:nosplit
func (f *Frame) call() uintptr {
	cgocall(trampoline, unsafe.Pointer(f))
	return f.a1
}

// fnPtr turns the trampoline's address into a pointer. The laundering keeps go
// vet's unsafeptr check quiet: this is a C address that never was a Go pointer,
// the one case the rule is not protecting against.
func fnPtr(u uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&u))
}
