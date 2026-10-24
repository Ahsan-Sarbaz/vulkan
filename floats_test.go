//go:build darwin || linux

package vulkan

import (
	"testing"

	"github.com/ebitengine/purego"
)

// A float parameter reaches C in the float registers, in order, with the
// integers numbered apart. Each entry point is pointed at a Go callback that
// records what C would have received.
func TestFloatParamsReachC(t *testing.T) {
	type got struct {
		h, m    uintptr
		a, b, c float32
	}
	var g got
	defer func(bias, bounds, prio uintptr) {
		pCmdSetDepthBias, pCmdSetDepthBounds, pSetDeviceMemoryPriorityEXT = bias, bounds, prio
	}(pCmdSetDepthBias, pCmdSetDepthBounds, pSetDeviceMemoryPriorityEXT)
	pCmdSetDepthBias = purego.NewCallback(func(cb uintptr, a, b, c float32) { g = got{h: cb, a: a, b: b, c: c} })
	pCmdSetDepthBounds = purego.NewCallback(func(cb uintptr, a, b float32) { g = got{h: cb, a: a, b: b} })
	pSetDeviceMemoryPriorityEXT = purego.NewCallback(func(d, m uintptr, p float32) { g = got{h: d, m: m, a: p} })

	CmdSetDepthBias(0x1234, 1.5, -2.25, 0.75)
	if want := (got{h: 0x1234, a: 1.5, b: -2.25, c: 0.75}); g != want {
		t.Errorf("CmdSetDepthBias: C got %+v, want %+v", g, want)
	}
	CmdSetDepthBounds(0x5678, 0.125, 0.875)
	if want := (got{h: 0x5678, a: 0.125, b: 0.875}); g != want {
		t.Errorf("CmdSetDepthBounds: C got %+v, want %+v", g, want)
	}
	SetDeviceMemoryPriorityEXT(0x9abc, 0xdef0, 0.5)
	if want := (got{h: 0x9abc, m: 0xdef0, a: 0.5}); g != want {
		t.Errorf("SetDeviceMemoryPriorityEXT: C got %+v, want %+v", g, want)
	}
}
