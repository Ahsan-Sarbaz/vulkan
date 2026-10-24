// Package vulkan is a Vulkan binding: generated from the Khronos registry
// and called through purego, with no cgo.
//
// A struct here is the C struct: same field order, same padding, same size. So
// passing one is passing its address, with nothing copied and nothing allocated.
// That is the difference from a marshalling binding, where the Go struct is a
// separate shape and every call rebuilds the C one field by field.
//
// Every command reaches the allocation-free call path in internal/ccall. Vulkan
// takes no struct by value anywhere in the registry and nothing with more than
// fifteen parameters, and a float goes in ccall's float slots, so none of it
// needs purego's reflected path.
//
// # Loading
//
// Vulkan dispatches in three tiers and so does this. Init takes the loader's
// vkGetInstanceProcAddr and resolves the entry points that exist before an
// instance does; InitInstance resolves the instance level once CreateInstance
// has returned; InitDevice resolves the device level through
// vkGetDeviceProcAddr, which is worth doing rather than keeping the instance
// answers, because those go through the loader's dispatch trampoline on every
// call.
//
//	vulkan.Init(procAddr)
//	vulkan.InitInstance(instance)
//	vulkan.InitDevice(device)
//
// An entry point that did not resolve panics naming it when called. Check with
// Supported, MustSupport or MissingBelow at startup.
//
// The entry points are package state, so they serve one instance at a time: a
// later InitInstance replaces the earlier one's. Devices are different, since a
// program may well open two. The first InitDevice resolves straight to that
// device's driver; a second, different device switches every device-level entry
// point to the loader's trampolines, which dispatch on the handle passed and so
// serve every device, at the cost of one indirection a call. Destroying the
// directly resolved device through DestroyDevice releases it, so a program that
// replaces its one device -- after a device loss, say -- resolves the new one
// directly too.
//
// # Layout
//
// Every struct here has to match the C one exactly, and a mismatch is silent
// corruption rather than a compile error. layout_test.go is generated alongside
// and checks all of them against the real headers; it is behind the vklayout
// tag because it needs cgo and the Vulkan SDK, which nothing else here does.
//
//	go test -tags vklayout .
//
//go:generate go run ./internal/cmd/vkgen
package vulkan

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"unsafe"

	"github.com/Ahsan-Sarbaz/vulkan/internal/ccall"
)

// The dispatch tiers, in resolution order.
const (
	levelGlobal uint8 = iota
	levelInstance
	levelDevice
)

var (
	getInstanceProcAddr uintptr
	getDeviceProcAddr   uintptr
	// instance is kept because vkGetInstanceProcAddr answers for almost
	// nothing with a null instance: only the global entry points. Every later
	// lookup, including the one for vkGetDeviceProcAddr itself, needs it.
	instance uintptr
	// device is the one device whose entry points resolved directly, or
	// shared once a second device made them trampolines.
	device   uintptr
	resolved = map[string]bool{}
	// initialised records which levels have been resolved at least once, so
	// MissingBelow asks only about tiers that have had their chance.
	initialised [3]bool
)

// shared marks device-level entry points resolved through the loader's
// trampolines, which serve any device.
const shared = ^uintptr(0)

// Init records the loader's vkGetInstanceProcAddr and resolves the entry points
// that do not need an instance. It returns the names of any that did not.
func Init(procAddr uintptr) []string {
	getInstanceProcAddr = procAddr
	if procAddr == 0 {
		return []string{"vkGetInstanceProcAddr"}
	}
	return resolve(levelGlobal, 0)
}

// InitInstance resolves the instance-level entry points. Call it once
// CreateInstance has returned.
func InitInstance(inst Instance) []string {
	instance, device = uintptr(inst), 0
	return resolve(levelInstance, instance)
}

// InitDevice resolves the device-level entry points, preferring
// vkGetDeviceProcAddr so they skip the loader's trampoline. Called again for a
// second device, it switches them all to the trampolines; see the package
// documentation.
func InitDevice(dev Device) []string {
	switch d := uintptr(dev); {
	case device == 0 || device == d:
		device = d
		getDeviceProcAddr = lookupInstance(instance, "vkGetDeviceProcAddr")
		return resolve(levelDevice, d)
	case device == shared:
		return nil
	default:
		device = shared
		return resolve(levelDevice, 0)
	}
}

// forgetDevice is what DestroyDevice calls: a destroyed device no longer holds
// the directly resolved entry points, so the next InitDevice may take them.
func forgetDevice(d uintptr) {
	if device == d {
		device = 0
	}
}

func resolve(level uint8, handle uintptr) []string {
	initialised[level] = true
	var absent []string
	for _, e := range procs {
		if e.level != level {
			continue
		}
		var p uintptr
		switch {
		case level != levelDevice:
			p = lookupInstance(handle, e.name)
		case device == shared:
			p = lookupInstance(instance, e.name)
		default:
			p = lookupDevice(handle, e.name)
		}
		*e.p = p
		resolved[e.name] = p != 0
		if p == 0 {
			absent = append(absent, e.name)
		}
	}
	slices.Sort(absent)
	return absent
}

func lookupInstance(instance uintptr, name string) uintptr {
	if getInstanceProcAddr == 0 {
		return 0
	}
	b := append([]byte(name), 0)
	p := new(ccall.Frame).Call2(getInstanceProcAddr, instance, uintptr(unsafe.Pointer(&b[0])))
	runtime.KeepAlive(b)
	return p
}

// lookupDevice asks the device first and falls back to the instance, which is
// what the loader does for a function the driver exposes only one way.
func lookupDevice(device uintptr, name string) uintptr {
	if getDeviceProcAddr != 0 {
		b := append([]byte(name), 0)
		p := new(ccall.Frame).Call2(getDeviceProcAddr, device, uintptr(unsafe.Pointer(&b[0])))
		runtime.KeepAlive(b)
		if p != 0 {
			return p
		}
	}
	return lookupInstance(instance, name)
}

// MissingBelow returns every entry point introduced at or below a core Vulkan
// version that did not resolve, among the levels initialised so far, sorted.
// Empty means the driver has all of it. Asked after each of Init, InitInstance
// and InitDevice, it checks that tier.
//
// This is the check a program wants. An extension entry point that did not
// resolve is normal -- the instance simply did not enable that extension -- so
// those are never counted, and only what a version actually promises is.
func MissingBelow(version float64) []string {
	var absent []string
	for _, e := range procs {
		if e.ver > 0 && e.ver <= version && initialised[e.level] && !resolved[e.name] {
			absent = append(absent, e.name)
		}
	}
	slices.Sort(absent)
	return absent
}

// Supported reports whether an entry point has resolved. The name is the C one:
// Supported("vkCreateDevice"), not "CreateDevice". A name this package does not
// know, or one whose level has not been initialised yet, is false.
func Supported(name string) bool { return resolved[name] }

// MustSupport returns an error naming every entry point that has not resolved,
// so a program can fail at startup rather than panic mid-frame.
func MustSupport(names ...string) error {
	var absent []string
	for _, n := range names {
		if !resolved[n] {
			absent = append(absent, n)
		}
	}
	if len(absent) == 0 {
		return nil
	}
	return fmt.Errorf("vulkan: this driver has no %s", strings.Join(absent, ", "))
}

// unresolved is what a generated wrapper calls in place of an entry point that
// is still zero. Out of line, so the check costs each call one compare.
//
//go:noinline
func unresolved(name string) {
	panic("vulkan: " + name + " did not resolve; check Supported or MustSupport before calling it")
}

// ptr turns a returned address into a pointer. See internal/ccall for why the
// laundering keeps go vet's unsafeptr check quiet.
func ptr(u uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&u)) }
