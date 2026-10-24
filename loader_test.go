//go:build darwin || linux

package vulkan

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Fake addresses the fake loader hands out, never called: which one an entry
// point holds says which lookup produced it.
const (
	fromInstance = 0x1000
	fromDevice   = 0x2000
)

func cString(p *byte) string {
	var b []byte
	for q := unsafe.Pointer(p); *(*byte)(q) != 0; q = unsafe.Add(q, 1) {
		b = append(b, *(*byte)(q))
	}
	return string(b)
}

// saveLoader restores every entry point and the loader's state when the test
// ends, so the fakes do not leak into other tests.
func saveLoader(t *testing.T) {
	ptrs := make([]uintptr, len(procs))
	for i, e := range procs {
		ptrs[i] = *e.p
	}
	gipa, gdpa, inst, dev := getInstanceProcAddr, getDeviceProcAddr, instance, device
	res, levels := resolved, initialised
	resolved, initialised = map[string]bool{}, [3]bool{}
	t.Cleanup(func() {
		for i, e := range procs {
			*e.p = ptrs[i]
		}
		getInstanceProcAddr, getDeviceProcAddr, instance, device = gipa, gdpa, inst, dev
		resolved, initialised = res, levels
	})
}

func TestSupportedBeforeInit(t *testing.T) {
	saveLoader(t)
	if Supported("vkCreateDevice") {
		t.Error("vkCreateDevice is supported before anything resolved it")
	}
	if Supported("vkNoSuchCommand") {
		t.Error("an unknown name is supported")
	}
	if err := MustSupport("vkNoSuchCommand"); err == nil {
		t.Error("MustSupport accepted an unknown name")
	}
	if missing := MissingBelow(1.0); len(missing) != 0 {
		t.Errorf("MissingBelow(1.0) before any Init asks about levels never initialised: %v", missing)
	}
}

func TestDeviceDispatch(t *testing.T) {
	saveLoader(t)
	gdpa := purego.NewCallback(func(dev uintptr, name *byte) uintptr {
		if strings.HasPrefix(cString(name), "vkCmd") {
			return fromDevice
		}
		return 0
	})
	gipa := purego.NewCallback(func(inst uintptr, name *byte) uintptr {
		switch n := cString(name); {
		case n == "vkGetDeviceProcAddr":
			return gdpa
		case inst != 0, n == "vkCreateInstance", n == "vkGetInstanceProcAddr",
			strings.HasPrefix(n, "vkEnumerateInstance"):
			// A null instance answers for the global entry points only.
			return fromInstance
		}
		return 0
	})

	Init(gipa)
	if !Supported("vkCreateInstance") || Supported("vkCreateDevice") {
		t.Fatal("Init resolved the wrong level")
	}
	// Each tier is checked as it is initialised, not failed for the next.
	if missing := MissingBelow(1.0); len(missing) != 0 {
		t.Errorf("MissingBelow(1.0) after Init: %v", missing)
	}
	InitInstance(Instance(0x10))
	if missing := MissingBelow(1.0); len(missing) != 0 {
		t.Errorf("MissingBelow(1.0) after InitInstance: %v", missing)
	}
	InitDevice(Device(0x20))
	if pCmdDraw != fromDevice {
		t.Fatalf("first device: vkCmdDraw = %#x, want the driver's %#x", pCmdDraw, fromDevice)
	}
	if missing := MissingBelow(1.0); len(missing) != 0 {
		t.Errorf("MissingBelow(1.0) after a full init: %v", missing)
	}
	InitDevice(Device(0x20))
	if pCmdDraw != fromDevice {
		t.Fatal("re-initialising the same device left the driver's entry points")
	}
	InitDevice(Device(0x30))
	if pCmdDraw != fromInstance {
		t.Fatalf("second device: vkCmdDraw = %#x, want the loader's trampoline %#x", pCmdDraw, fromInstance)
	}
}

// A device replaced rather than added -- the first destroyed, then a new one --
// still resolves directly. That is what recovering from a device loss does.
func TestReplacedDeviceResolvesDirectly(t *testing.T) {
	saveLoader(t)
	gdpa := purego.NewCallback(func(dev uintptr, name *byte) uintptr {
		if strings.HasPrefix(cString(name), "vkCmd") {
			return fromDevice
		}
		return 0
	})
	gipa := purego.NewCallback(func(inst uintptr, name *byte) uintptr {
		if cString(name) == "vkGetDeviceProcAddr" {
			return gdpa
		}
		return fromInstance
	})
	destroyed := uintptr(0)
	destroy := purego.NewCallback(func(dev, alloc uintptr) { destroyed = dev })

	Init(gipa)
	InitInstance(Instance(0x10))
	InitDevice(Device(0x20))
	pDestroyDevice = destroy
	DestroyDevice(Device(0x20), nil)
	if destroyed != 0x20 {
		t.Fatalf("DestroyDevice reached C with %#x", destroyed)
	}
	InitDevice(Device(0x40))
	if pCmdDraw != fromDevice {
		t.Fatalf("replacement device: vkCmdDraw = %#x, want the driver's %#x", pCmdDraw, fromDevice)
	}
}

func TestUnresolvedPanicsNamingIt(t *testing.T) {
	saveLoader(t)
	pCmdDraw = 0
	defer func() {
		r, _ := recover().(string)
		if !strings.Contains(r, "vkCmdDraw") {
			t.Errorf("panic %q does not name vkCmdDraw", r)
		}
	}()
	CmdDraw(0, 3, 1, 0, 0)
}

// A call allocates nothing, with pointer arguments kept alive and a nested
// pointer inside a struct. Pointed at a real C function rather than a purego
// callback, whose own dispatch allocates.
func TestCallsDoNotAllocate(t *testing.T) {
	saveLoader(t)
	lib := "libc.so.6"
	if runtime.GOOS == "darwin" {
		lib = "/usr/lib/libSystem.B.dylib"
	}
	h, err := purego.Dlopen(lib, purego.RTLD_NOW)
	if err != nil {
		t.Fatal(err)
	}
	noop, err := purego.Dlsym(h, "getpid") // ignores whatever it is passed
	if err != nil {
		t.Fatal(err)
	}
	pCmdSetViewport, pCmdDraw, pCreateInstance = noop, noop, noop
	n := testing.AllocsPerRun(1000, func() {
		vp := [2]Viewport{{Width: 1}, {Height: 2}}
		CmdSetViewport(1, 0, 2, &vp[0])
		CmdDraw(1, 3, 1, 0, 0)
		names := [1]*byte{}
		ci := InstanceCreateInfo{SType: STRUCTURE_TYPE_INSTANCE_CREATE_INFO, PpEnabledExtensionNames: &names[0]}
		var inst Instance
		CreateInstance(&ci, nil, &inst)
	})
	if n != 0 {
		t.Errorf("%v allocations a run, want 0", n)
	}
}
