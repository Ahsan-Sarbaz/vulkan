//go:build ccall_asmprobe

// The asmcgocall comparison, behind a tag because the Go 1.27 linker refuses the
// linkname it needs.
//
// runtime.asmcgocall is not one of the runtime symbols a pull linkname may name
// any more, so linking a test binary that references it fails with "invalid
// reference to runtime.asmcgocall" -- which took the whole package's tests down
// with it, over a comparison that exists to justify *not* using asmcgocall.
//
// It is still worth being able to run, because the argument in the package
// comment is a measurement rather than an opinion, and a future runtime could
// make it wrong. So it lives here, and needs the check turned off to link:
//
//	go test -tags ccall_asmprobe -ldflags=-checklinkname=0 ./internal/ccall/

package ccall

import (
	"testing"
	"time"
	"unsafe"

	_ "unsafe" // for go:linkname

	"github.com/ebitengine/purego"
)

//go:linkname asmcgocall runtime.asmcgocall
//go:noescape
func asmcgocall(fn, arg unsafe.Pointer) int32

// callAsm is Call1 without the syscall transition. Faster, and not used: see the
// package comment and TestAsmcgocallIsInterrupted.
func callAsm(fn, a1 uintptr) uintptr {
	args := syscallArgs{fn: fn, a1: a1}
	asmcgocall(fnPtr(syscallXABI0), unsafe.Pointer(&args))
	return args.a1
}

// The same agreement TestRawCallMatchesSyscallN asserts, for the path this file
// exists to compare against.
func TestAsmCallMatchesSyscallN(t *testing.T) {
	fn := sym(t, "strlen")
	for _, s := range []string{"", "a", "ccall", "a rather longer string than the others"} {
		b := append([]byte(s), 0)
		arg := uintptr(unsafe.Pointer(&b[0]))
		want := uintptr(len(s))
		if got, _, _ := purego.SyscallN(fn, arg); got != want {
			t.Errorf("SyscallN(%q) = %d, want %d", s, got, want)
		}
		if got := callAsm(fn, arg); got != want {
			t.Errorf("asmcgocall(%q) = %d, want %d", s, got, want)
		}
	}
}

// Why the fast path is cgocall and not asmcgocall, demonstrated rather than
// asserted. Skipping entersyscall leaves the goroutine runnable as far as the
// scheduler knows, so sysmon preempts it by sending SIGURG to a thread that is
// inside C -- and the C call takes the signal.
func TestAsmcgocallIsInterrupted(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps for about a quarter second")
	}
	fn := sym(t, "usleep")
	const sleep = 200 * time.Millisecond
	us := uintptr(sleep.Microseconds())

	timeCall := func(call func(fn, arg unsafe.Pointer) int32) time.Duration {
		args := syscallArgs{fn: fn, a1: us}
		start := time.Now()
		call(fnPtr(syscallXABI0), unsafe.Pointer(&args))
		return time.Since(start)
	}
	viaCgocall := timeCall(cgocall)
	viaAsm := timeCall(asmcgocall)
	t.Logf("usleep(%v): cgocall %v, asmcgocall %v", sleep, viaCgocall, viaAsm)

	if viaCgocall < sleep {
		t.Errorf("cgocall returned after %v, short of the %v it asked for", viaCgocall, sleep)
	}
	if viaAsm >= sleep {
		t.Errorf("asmcgocall ran the full %v; the argument for cgocall may no longer hold", viaAsm)
	}
}

func BenchmarkCallAsm(b *testing.B) { benchVariant(b, callAsm) }
