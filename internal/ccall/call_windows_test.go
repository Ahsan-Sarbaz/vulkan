//go:build windows

package ccall

import (
	"bytes"
	"math"
	"syscall"
	"testing"
	"unsafe"
)

// Floats go in the slots after the integers. sprintf reads them from the
// integer registers and the stack, being variadic; _dclass from XMM0.
func TestFloatsFollowIntegers(t *testing.T) {
	sprintf := syscall.MustLoadDLL("msvcrt.dll").MustFindProc("sprintf").Addr()
	for _, c := range []struct {
		format string
		ints   []uintptr
		floats []float64
		want   string
	}{
		{"%d %.1f", []uintptr{7}, []float64{2.5}, "7 2.5"},
		{"%d %d %d %.2f %.3f", []uintptr{1, 2, 3}, []float64{1.5, 2.25}, "1 2 3 1.50 2.250"},
	} {
		var buf [64]byte
		format := append([]byte(c.format), 0)
		fr := new(Frame)
		for i, x := range c.floats {
			fr.Float64(i, x)
		}
		b, f := uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&format[0]))
		switch len(c.ints) {
		case 1:
			fr.Call3(sprintf, b, f, c.ints[0])
		case 3:
			fr.Call5(sprintf, b, f, c.ints[0], c.ints[1], c.ints[2])
		}
		if got := string(buf[:bytes.IndexByte(buf[:], 0)]); got != c.want {
			t.Errorf("sprintf(%q) = %q, want %q", c.format, got, c.want)
		}
	}

	dclass := syscall.MustLoadDLL("ucrtbase.dll").MustFindProc("_dclass").Addr()
	for _, c := range []struct {
		x    float64
		want int16
	}{{0, 0}, {1, -1}, {math.Inf(1), 1}, {math.NaN(), 2}} {
		fr := new(Frame)
		fr.Float64(0, c.x)
		if got := int16(fr.Call0(dclass)); got != c.want {
			t.Errorf("_dclass(%v) = %d, want %d", c.x, got, c.want)
		}
	}

	if n := testing.AllocsPerRun(100, func() {
		fr := new(Frame)
		fr.Float64(0, 1)
		fr.Call0(dclass)
	}); n != 0 {
		t.Errorf("a float call allocates %v times", n)
	}
}
