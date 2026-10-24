//go:build !windows

package ccall

import (
	"math"
	"runtime"
	"sync"
	"testing"
	"unsafe"

	"github.com/ebitengine/purego"
)

// libc is where these find a C function to call. Any exported symbol with a
// known answer would do; strlen is the one every platform has.
func libc() string {
	switch runtime.GOOS {
	case "darwin":
		return "/usr/lib/libSystem.B.dylib"
	case "linux":
		return "libc.so.6"
	case "freebsd", "netbsd":
		return "libc.so"
	}
	return ""
}

// libm is where the float functions are: libSystem on darwin, its own library elsewhere.
func libm() string {
	switch runtime.GOOS {
	case "darwin":
		return libc()
	case "linux":
		return "libm.so.6"
	case "freebsd", "netbsd":
		return "libm.so"
	}
	return ""
}

func sym(t testing.TB, name string) uintptr { t.Helper(); return symIn(t, libc(), name) }

func symIn(t testing.TB, lib, name string) uintptr {
	t.Helper()
	if lib == "" {
		t.Skipf("no libc path known for %s", runtime.GOOS)
	}
	h, err := purego.Dlopen(lib, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil || h == 0 {
		t.Skipf("cannot open %s: %v", lib, err)
	}
	p, err := purego.Dlsym(h, name)
	if err != nil || p == 0 {
		t.Skipf("cannot resolve %s: %v", name, err)
	}
	return p
}

//go:linkname pooledSyscallN github.com/ebitengine/purego.syscall_SyscallN
//go:noescape
func pooledSyscallN(fn uintptr, ints, floats []uintptr, r8 uintptr) *syscallArgs

//go:linkname pooledArgs github.com/ebitengine/purego.thePool
var pooledArgs sync.Pool

// pooledType is purego's own type word for its block: the pool asserts it on
// Get, so a block goes back as purego's type, not this package's twin.
var pooledType = func() unsafe.Pointer {
	v := pooledArgs.Get()
	pooledArgs.Put(v)
	return (*[2]unsafe.Pointer)(unsafe.Pointer(&v))[0]
}()

func callPooled(fn, a1 uintptr) uintptr {
	var ints [32]uintptr
	ints[0] = a1
	s := pooledSyscallN(fn, ints[:], ints[:8], 0)
	r := s.a1
	var v any
	*(*[2]unsafe.Pointer)(unsafe.Pointer(&v)) = [2]unsafe.Pointer{pooledType, unsafe.Pointer(s)}
	pooledArgs.Put(v)
	return r
}

// The generated bindings call CallN rather than purego.SyscallN, which is a
// linkname into purego's internals. This is what says the two still agree: an
// upgrade that renames or reshapes syscall_SyscallN fails here rather than in
// a renderer.
func TestRawCallMatchesSyscallN(t *testing.T) {
	fn := sym(t, "strlen")
	for _, s := range []string{"", "a", "ccall", "a rather longer string than the others"} {
		b := append([]byte(s), 0)
		arg := uintptr(unsafe.Pointer(&b[0]))
		want := uintptr(len(s))

		got, _, _ := purego.SyscallN(fn, arg)
		if got != want {
			t.Errorf("SyscallN(%q) = %d, want %d", s, got, want)
		}
		if got := callPooled(fn, arg); got != want {
			t.Errorf("pooled(%q) = %d, want %d", s, got, want)
		}
		if got := new(Frame).Call1(fn, arg); got != want {
			t.Errorf("Frame.Call1(%q) = %d, want %d", s, got, want)
		}
		runtime.KeepAlive(b)
	}
}

// The trampoline writes its float results to f1..f4 and reads a16..a32, so a
// block shorter than purego's own has it writing past the end of the caller's
// frame. strlen never notices; a double result in the wrong slot does, and so
// does the tail.
func TestArgsBlockMatchesPuregoLayout(t *testing.T) {
	fn := sym(t, "atof")
	b := []byte("2.5\x00")
	var g struct {
		args syscallArgs
		tail [16]uintptr
	}
	g.args = syscallArgs{fn: fn, a1: uintptr(unsafe.Pointer(&b[0]))}
	cgocall(fnPtr(syscallXABI0), unsafe.Pointer(&g.args))
	runtime.KeepAlive(b)
	if got := math.Float64frombits(uint64(g.args.floats[0])); got != 2.5 {
		t.Errorf("atof result in f1 = %v, want 2.5", got)
	}
	for i, w := range g.tail {
		if w != 0 {
			t.Fatalf("trampoline wrote %#x past the block, at word %d", w, i)
		}
	}
}

// The C ABI numbers float arguments apart from integer ones: ldexp(x, e) takes
// x in the first float register and e in the first integer one. Copying the
// integers into the float slots, as CallN once did, overwrote x with e's bits.
func TestFloatArgsNumberApartFromInts(t *testing.T) {
	lib := libm()
	ldexp, ldexpf := symIn(t, lib, "ldexp"), symIn(t, lib, "ldexpf")
	fmaf, pow := symIn(t, lib, "fmaf"), symIn(t, lib, "pow")

	f := new(Frame)
	f.Float64(0, 1.5)
	f.Call1(ldexp, 3)
	if got := f.Float64Result(); got != 12 {
		t.Errorf("ldexp(1.5, 3) = %v, want 12", got)
	}
	f = new(Frame)
	f.Float32(0, -0.75)
	f.Call1(ldexpf, 4)
	if got := f.Float32Result(); got != -12 {
		t.Errorf("ldexpf(-0.75, 4) = %v, want -12", got)
	}
	f = new(Frame)
	f.Float32(0, 1.5)
	f.Float32(1, 2)
	f.Float32(2, 0.25)
	f.Call0(fmaf)
	if got := f.Float32Result(); got != 3.25 {
		t.Errorf("fmaf(1.5, 2, 0.25) = %v, want 3.25", got)
	}
	f = new(Frame)
	f.Float64(0, 3)
	f.Float64(1, 2)
	f.Call0(pow)
	if got := f.Float64Result(); got != 9 {
		t.Errorf("pow(3, 2) = %v, want 9", got)
	}
}

//go:noinline
func memsetAtDepth(fn uintptr, depth int) uint64 {
	if depth > 0 {
		return memsetAtDepth(fn, depth-1)
	}
	var out uint64
	new(Frame).Call3(fn, uintptr(unsafe.Pointer(&out)), 0xab, 8)
	return out
}

// The bindings' out-parameter shape: a stack cell whose address is already a
// uintptr when CallN starts. Sweeping the depth puts CallN's frame across a
// stack-growth boundary; C must still write the cell the caller reads.
func TestOutCellSurvivesStackGrowth(t *testing.T) {
	fn := sym(t, "memset")
	done := make(chan uint64)
	for d := range 2000 {
		go func() { done <- memsetAtDepth(fn, d) }()
		if got := <-done; got != 0xabababababababab {
			t.Fatalf("depth %d: out cell = %#x after the call, want 0xabab...", d, got)
		}
	}
}

// The whole reason CallN exists: purego.SyscallN is variadic and carries
// go:uintptrescapes, so its argument slice escapes and every call with an
// argument allocates. A renderer makes thousands of calls a frame and this
// tree's steady state is zero allocations.
func TestRawCallDoesNotAllocate(t *testing.T) {
	fn := sym(t, "strlen")
	b := []byte("ccall\x00")
	arg := uintptr(unsafe.Pointer(&b[0]))

	if n := testing.AllocsPerRun(1000, func() { new(Frame).Call1(fn, arg) }); n != 0 {
		t.Errorf("Frame.Call1 allocates %v per call, want 0", n)
	}
	ldexp := symIn(t, libm(), "ldexp")
	float := func() {
		f := new(Frame)
		f.Float64(0, 1.5)
		f.Call1(ldexp, 3)
		_ = f.Float64Result()
	}
	if n := testing.AllocsPerRun(1000, float); n != 0 {
		t.Errorf("a float call allocates %v per call, want 0", n)
	}
	if n := testing.AllocsPerRun(1000, func() { purego.SyscallN(fn, arg) }); n == 0 {
		t.Log("purego.SyscallN no longer allocates; this package could be dropped")
	}
}

func benchVariant(b *testing.B, call func(fn, arg uintptr) uintptr) {
	fn := sym(b, "strlen")
	buf := []byte("ccall\x00")
	arg := uintptr(unsafe.Pointer(&buf[0]))
	b.ReportAllocs()
	for b.Loop() {
		call(fn, arg)
	}
	runtime.KeepAlive(buf)
}

func BenchmarkCallSyscallN(b *testing.B) {
	benchVariant(b, func(fn, arg uintptr) uintptr { r, _, _ := purego.SyscallN(fn, arg); return r })
}
func BenchmarkCallPooled(b *testing.B) { benchVariant(b, callPooled) }
func BenchmarkCallStack(b *testing.B) {
	benchVariant(b, func(fn, arg uintptr) uintptr { return new(Frame).Call1(fn, arg) })
}
