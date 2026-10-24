//go:build darwin || linux

// The rows, each the same Vulkan call made a different way:
//
//	ours                 this binding
//	cgo-raw              hand-written cgo passing void*, as a binding over Go memory would
//	cgo-typed            hand-written cgo with typed pointers: no runtime pointer check, the cgo floor
//	purego-SyscallN      purego's public variadic call
//	purego-RegisterFunc  purego's reflect-built typed function
//	goki                 github.com/goki/vulkan (cgo, c-for-go; vulkan-go/vulkan is the same code)
//	go-vk                github.com/bbredesen/go-vk (cgo)
//	vkng-loader          github.com/vkngwrapper/core/v3/loader, its raw layer (cgo)
//	vkng                 github.com/vkngwrapper/core/v3, its object API
//
// Each binding is used as its documentation says, so a row includes whatever
// it costs to get a usable Go value back. Every row but vkng drives the same
// instance, device and command buffer; vkng needs its own.
//
// Run from this directory, with the Vulkan headers on the include path:
//
//	CGO_CFLAGS=-I$VULKAN_SDK/include go test -run '^$' -bench . -count 10
//
// On macOS goki links MoltenVK directly, so add
// CGO_LDFLAGS="-L$VULKAN_SDK/lib -Wl,-rpath,$VULKAN_SDK/lib", and pick a driver
// with VK_DRIVER_FILES if more than one is installed. VULKAN_LIBRARY names the
// loader if it is not on the default search path. BENCH_TRAMPOLINE=1 runs this
// binding on the loader's trampolines instead of the driver's own entry points.

package benchmarks

import (
	"os"
	"testing"
	"unsafe"

	vk "github.com/Ahsan-Sarbaz/vulkan"
	govk "github.com/bbredesen/go-vk"
	"github.com/ebitengine/purego"
	gk "github.com/goki/vulkan"
	vkng "github.com/vkngwrapper/core/v3"
	"github.com/vkngwrapper/core/v3/common"
	"github.com/vkngwrapper/core/v3/core1_0"
	"github.com/vkngwrapper/core/v3/loader"
)

// Commands recorded between resets. The reset runs with the timer stopped.
const resetEvery = 8192

var (
	// raw driver pointers for the cgo / purego floors
	fnStencil, fnScissor, fnQueue, fnProps, fnCreateBuf, fnDestroyBuf uintptr

	// vkngwrapper raw loader layer, on the shared handles
	ngl loader.Loader

	// vkngwrapper high-level API, on its own instance and device
	ngInst core1_0.CoreInstanceDriver
	ngDev  core1_0.CoreDeviceDriver
	ngPD   core1_0.PhysicalDevice
	ngCB   core1_0.CommandBuffer

	// purego.RegisterFunc
	rfStencil func(cb uintptr, mask, ref uint32)
	rfScissor func(cb uintptr, first, n uint32, r *vk.Rect2D)
	rfQueue   func(dev uintptr, fam, idx uint32, out *vk.Queue)
	rfProps   func(pd uintptr, out *vk.PhysicalDeviceProperties)
	rfCreate  func(dev uintptr, ci *vk.BufferCreateInfo, alloc uintptr, out *vk.Buffer) int32
	rfDestroy func(dev uintptr, buf uint64, alloc uintptr)
)

func TestMain(m *testing.M) {
	setup()

	fnStencil = deviceProc("vkCmdSetStencilReference")
	fnScissor = deviceProc("vkCmdSetScissor")
	fnQueue = deviceProc("vkGetDeviceQueue")
	fnCreateBuf = deviceProc("vkCreateBuffer")
	fnDestroyBuf = deviceProc("vkDestroyBuffer")
	fnProps = instanceProc("vkGetPhysicalDeviceProperties")
	purego.RegisterFunc(&rfStencil, fnStencil)
	purego.RegisterFunc(&rfScissor, fnScissor)
	purego.RegisterFunc(&rfQueue, fnQueue)
	purego.RegisterFunc(&rfProps, fnProps)
	purego.RegisterFunc(&rfCreate, fnCreateBuf)
	purego.RegisterFunc(&rfDestroy, fnDestroyBuf)

	// goki/vulkan: global table from vkGetInstanceProcAddr(instance, ...), so
	// device commands are the loader's trampolines.
	gk.SetGetInstanceProcAddr(unsafe.Pointer(gipa))
	if err := gk.Init(); err != nil {
		panic(err)
	}
	if err := gk.InitInstance(*(*gk.Instance)(unsafe.Pointer(&instance))); err != nil {
		panic(err)
	}

	// go-vk: dlsym on the library, so also the loader's exported trampolines.
	govk.OverrideDefaultVulkanLibrary(libvulkan)

	// vkngwrapper loader layer: device commands from vkGetDeviceProcAddr (direct).
	base, err := loader.CreateLoaderFromProcAddr(unsafe.Pointer(gipa))
	if err != nil {
		panic(err)
	}
	il, err := base.CreateInstanceLoader(loader.VkInstance(instance))
	if err != nil {
		panic(err)
	}
	ngl, err = il.CreateDeviceLoader(loader.VkDevice(device))
	if err != nil {
		panic(err)
	}

	setupVkng()
	os.Exit(m.Run())
}

func setupVkng() {
	global, err := vkng.CreateDriverFromProcAddr(unsafe.Pointer(gipa))
	if err != nil {
		panic(err)
	}
	ici := core1_0.InstanceCreateInfo{APIVersion: common.Vulkan1_2}
	dci := core1_0.DeviceCreateInfo{
		QueueCreateInfos: []core1_0.DeviceQueueCreateInfo{{QueueFamilyIndex: 0, QueuePriorities: []float32{1}}},
	}
	if portability {
		ici.Flags = core1_0.InstanceCreateFlags(1) // VK_INSTANCE_CREATE_ENUMERATE_PORTABILITY_BIT_KHR
		ici.EnabledExtensionNames = []string{"VK_KHR_portability_enumeration"}
		dci.EnabledExtensionNames = []string{"VK_KHR_portability_subset"}
	}
	ngInst, _, err = global.CreateInstance(nil, ici)
	if err != nil {
		panic(err)
	}
	pds, _, err := ngInst.EnumeratePhysicalDevices()
	if err != nil {
		panic(err)
	}
	ngPD = pds[0]
	ngDev, _, err = ngInst.CreateDevice(ngPD, nil, dci)
	if err != nil {
		panic(err)
	}
	pool, _, err := ngDev.CreateCommandPool(nil, core1_0.CommandPoolCreateInfo{Flags: core1_0.CommandPoolCreateResetBuffer})
	if err != nil {
		panic(err)
	}
	cbs, _, err := ngDev.AllocateCommandBuffers(core1_0.CommandBufferAllocateInfo{
		CommandPool: pool, Level: core1_0.CommandBufferLevelPrimary, CommandBufferCount: 1,
	})
	if err != nil {
		panic(err)
	}
	ngCB = cbs[0]
	ngBegin()
}

func ngBegin() {
	if _, err := ngDev.BeginCommandBuffer(ngCB, core1_0.CommandBufferBeginInfo{Flags: core1_0.CommandBufferUsageOneTimeSubmit}); err != nil {
		panic(err)
	}
}

func ngReset() {
	ngDev.EndCommandBuffer(ngCB)
	ngDev.ResetCommandBuffer(ngCB, 0)
	ngBegin()
}

// record runs body b.N times, resetting the command buffer every resetEvery
// calls with the timer stopped.
func record(b *testing.B, reset func(), body func()) {
	b.ReportAllocs()
	reset()
	b.ResetTimer()
	k := 0
	for i := 0; i < b.N; i++ {
		body()
		if k++; k == resetEvery {
			b.StopTimer()
			reset()
			k = 0
			b.StartTimer()
		}
	}
}

func run(b *testing.B, body func()) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body()
	}
}

func gkCB() gk.CommandBuffer  { c := cmdBuf; return *(*gk.CommandBuffer)(unsafe.Pointer(&c)) }
func gkDev() gk.Device        { d := device; return *(*gk.Device)(unsafe.Pointer(&d)) }
func gkPD() gk.PhysicalDevice { p := physDev; return *(*gk.PhysicalDevice)(unsafe.Pointer(&p)) }
func cb() uintptr             { return uintptr(cmdBuf) }
func dev() uintptr            { return uintptr(device) }

// ---------------------------------------------------------------------------
// 1. vkCmdSetStencilReference: three scalar args, recorded. The shape of most
//    per-draw commands (CmdDraw, CmdBindPipeline, CmdSetStencil*).

func BenchmarkStencilRef(b *testing.B) {
	b.Run("ours", func(b *testing.B) {
		c := cmdBuf
		record(b, resetCB, func() { vk.CmdSetStencilReference(c, vk.STENCIL_FACE_FRONT_AND_BACK, 1) })
	})
	b.Run("cgo-raw", func(b *testing.B) {
		c := cb()
		record(b, resetCB, func() { cgoStencil(fnStencil, c, 3, 1) })
	})
	b.Run("purego-SyscallN", func(b *testing.B) {
		c := cb()
		record(b, resetCB, func() { purego.SyscallN(fnStencil, c, 3, 1) })
	})
	b.Run("purego-RegisterFunc", func(b *testing.B) {
		c := cb()
		record(b, resetCB, func() { rfStencil(c, 3, 1) })
	})
	b.Run("goki", func(b *testing.B) {
		c := gkCB()
		record(b, resetCB, func() { gk.CmdSetStencilReference(c, gk.StencilFaceFlags(3), 1) })
	})
	b.Run("go-vk", func(b *testing.B) {
		c := govk.CommandBuffer(cb())
		record(b, resetCB, func() { govk.CmdSetStencilReference(c, govk.StencilFaceFlags(3), 1) })
	})
	b.Run("vkng-loader", func(b *testing.B) {
		c := loader.VkCommandBuffer(cb())
		record(b, resetCB, func() { ngl.VkCmdSetStencilReference(c, 3, 1) })
	})
	b.Run("vkng", func(b *testing.B) {
		record(b, ngReset, func() { ngDev.CmdSetStencilReference(ngCB, core1_0.StencilFaceFlags(3), 1) })
	})
}

// ---------------------------------------------------------------------------
// 2. vkCmdSetScissor with one rect: a pointer to a struct array, recorded.
//    Bindings with their own Go struct layouts must convert.

func BenchmarkScissor(b *testing.B) {
	r := []vk.Rect2D{{Extent: vk.Extent2D{Width: 640, Height: 480}}}
	b.Run("ours", func(b *testing.B) {
		c := cmdBuf
		record(b, resetCB, func() { vk.CmdSetScissor(c, 0, 1, &r[0]) })
	})
	b.Run("cgo-raw", func(b *testing.B) {
		c := cb()
		record(b, resetCB, func() { cgoScissor(fnScissor, c, 0, 1, unsafe.Pointer(&r[0])) })
	})
	b.Run("cgo-typed", func(b *testing.B) {
		c := cb()
		record(b, resetCB, func() { cgoScissorTyped(fnScissor, c, 0, 1, unsafe.Pointer(&r[0])) })
	})
	b.Run("purego-SyscallN", func(b *testing.B) {
		c := cb()
		record(b, resetCB, func() { purego.SyscallN(fnScissor, c, 0, 1, uintptr(unsafe.Pointer(&r[0]))) })
	})
	b.Run("purego-RegisterFunc", func(b *testing.B) {
		c := cb()
		record(b, resetCB, func() { rfScissor(c, 0, 1, &r[0]) })
	})
	b.Run("goki", func(b *testing.B) {
		c := gkCB()
		gr := []gk.Rect2D{{Extent: gk.Extent2D{Width: 640, Height: 480}}}
		record(b, resetCB, func() { gk.CmdSetScissor(c, 0, 1, gr) })
	})
	b.Run("go-vk", func(b *testing.B) {
		c := govk.CommandBuffer(cb())
		gr := []govk.Rect2D{{Extent: govk.Extent2D{Width: 640, Height: 480}}}
		record(b, resetCB, func() { govk.CmdSetScissor(c, 0, gr) })
	})
	b.Run("vkng-loader", func(b *testing.B) {
		c := loader.VkCommandBuffer(cb())
		record(b, resetCB, func() { ngl.VkCmdSetScissor(c, 0, 1, (*loader.VkRect2D)(unsafe.Pointer(&r[0]))) })
	})
	b.Run("vkng", func(b *testing.B) {
		gr := []core1_0.Rect2D{{Extent: core1_0.Extent2D{Width: 640, Height: 480}}}
		record(b, ngReset, func() { ngDev.CmdSetScissor(ngCB, gr...) })
	})
}

// ---------------------------------------------------------------------------
// 3. vkGetDeviceQueue: scalar args plus an out handle; nothing recorded, so
//    the driver side is a near no-op and the binding cost dominates.

func BenchmarkDeviceQueue(b *testing.B) {
	b.Run("ours", func(b *testing.B) {
		var q vk.Queue
		run(b, func() { vk.GetDeviceQueue(device, 0, 0, &q) })
	})
	b.Run("cgo-raw", func(b *testing.B) {
		var q vk.Queue
		d := dev()
		run(b, func() { cgoQueue(fnQueue, d, 0, 0, unsafe.Pointer(&q)) })
	})
	b.Run("cgo-typed", func(b *testing.B) {
		var q uintptr
		d := dev()
		run(b, func() { cgoQueueTyped(fnQueue, d, 0, 0, &q) })
	})
	b.Run("purego-SyscallN", func(b *testing.B) {
		var q vk.Queue
		d := dev()
		run(b, func() { purego.SyscallN(fnQueue, d, 0, 0, uintptr(unsafe.Pointer(&q))) })
	})
	b.Run("purego-RegisterFunc", func(b *testing.B) {
		var q vk.Queue
		d := dev()
		run(b, func() { rfQueue(d, 0, 0, &q) })
	})
	b.Run("goki", func(b *testing.B) {
		var q gk.Queue
		d := gkDev()
		run(b, func() { gk.GetDeviceQueue(d, 0, 0, &q) })
	})
	b.Run("go-vk", func(b *testing.B) {
		d := govk.Device(dev())
		var q govk.Queue
		run(b, func() { q = govk.GetDeviceQueue(d, 0, 0) })
		_ = q
	})
	b.Run("vkng-loader", func(b *testing.B) {
		var q loader.VkQueue
		d := loader.VkDevice(dev())
		run(b, func() { ngl.VkGetDeviceQueue(d, 0, 0, &q) })
	})
	b.Run("vkng", func(b *testing.B) {
		var q core1_0.Queue
		run(b, func() { q = ngDev.GetQueue(0, 0) })
		_ = q
	})
}

// ---------------------------------------------------------------------------
// 4. vkGetPhysicalDeviceProperties: an 824-byte out struct with nested arrays.
//    Each binding is used the way its docs say, so the result is readable in Go.

var sinkU32 uint32

func BenchmarkPhysDeviceProps(b *testing.B) {
	b.Run("ours", func(b *testing.B) {
		var p vk.PhysicalDeviceProperties
		run(b, func() { vk.GetPhysicalDeviceProperties(physDev, &p); sinkU32 = p.Limits.MaxImageDimension2D })
	})
	b.Run("cgo-raw", func(b *testing.B) {
		var p vk.PhysicalDeviceProperties
		pd := uintptr(physDev)
		run(b, func() { cgoProps(fnProps, pd, unsafe.Pointer(&p)); sinkU32 = p.Limits.MaxImageDimension2D })
	})
	b.Run("purego-SyscallN", func(b *testing.B) {
		var p vk.PhysicalDeviceProperties
		pd := uintptr(physDev)
		run(b, func() {
			purego.SyscallN(fnProps, pd, uintptr(unsafe.Pointer(&p)))
			sinkU32 = p.Limits.MaxImageDimension2D
		})
	})
	b.Run("purego-RegisterFunc", func(b *testing.B) {
		var p vk.PhysicalDeviceProperties
		pd := uintptr(physDev)
		run(b, func() { rfProps(pd, &p); sinkU32 = p.Limits.MaxImageDimension2D })
	})
	b.Run("goki", func(b *testing.B) {
		pd := gkPD()
		run(b, func() {
			var p gk.PhysicalDeviceProperties
			gk.GetPhysicalDeviceProperties(pd, &p)
			p.Deref()
			p.Limits.Deref()
			sinkU32 = p.Limits.MaxImageDimension2D
			p.Free()
		})
	})
	b.Run("go-vk", func(b *testing.B) {
		pd := govk.PhysicalDevice(uintptr(physDev))
		run(b, func() { p := govk.GetPhysicalDeviceProperties(pd); sinkU32 = p.Limits.MaxImageDimension2D })
	})
	b.Run("vkng-loader", func(b *testing.B) {
		var p vk.PhysicalDeviceProperties
		pd := loader.VkPhysicalDevice(uintptr(physDev))
		run(b, func() {
			ngl.VkGetPhysicalDeviceProperties(pd, (*loader.VkPhysicalDeviceProperties)(unsafe.Pointer(&p)))
			sinkU32 = p.Limits.MaxImageDimension2D
		})
	})
	b.Run("vkng", func(b *testing.B) {
		run(b, func() {
			p, err := ngInst.GetPhysicalDeviceProperties(ngPD)
			if err != nil {
				b.Fatal(err)
			}
			sinkU32 = uint32(p.Limits.MaxImageDimension2D)
		})
	})
}

// ---------------------------------------------------------------------------
// 5. vkCreateBuffer + vkDestroyBuffer: an input create-info struct, an out
//    handle, then a destroy. Driver allocation is in every row.

func BenchmarkCreateDestroyBuffer(b *testing.B) {
	b.Run("ours", func(b *testing.B) {
		run(b, func() {
			ci := vk.BufferCreateInfo{
				SType: vk.STRUCTURE_TYPE_BUFFER_CREATE_INFO, Size: 256,
				Usage: vk.BUFFER_USAGE_VERTEX_BUFFER_BIT, SharingMode: vk.SHARING_MODE_EXCLUSIVE,
			}
			var buf vk.Buffer
			if vk.CreateBuffer(device, &ci, nil, &buf) != vk.SUCCESS {
				b.Fatal("create")
			}
			vk.DestroyBuffer(device, buf, nil)
		})
	})
	b.Run("cgo-raw", func(b *testing.B) {
		d := dev()
		run(b, func() {
			ci := vk.BufferCreateInfo{
				SType: vk.STRUCTURE_TYPE_BUFFER_CREATE_INFO, Size: 256,
				Usage: vk.BUFFER_USAGE_VERTEX_BUFFER_BIT, SharingMode: vk.SHARING_MODE_EXCLUSIVE,
			}
			var buf vk.Buffer
			if cgoCreateBuffer(fnCreateBuf, d, unsafe.Pointer(&ci), unsafe.Pointer(&buf)) != 0 {
				b.Fatal("create")
			}
			cgoDestroyBuffer(fnDestroyBuf, d, uint64(buf))
		})
	})
	b.Run("purego-RegisterFunc", func(b *testing.B) {
		d := dev()
		run(b, func() {
			ci := vk.BufferCreateInfo{
				SType: vk.STRUCTURE_TYPE_BUFFER_CREATE_INFO, Size: 256,
				Usage: vk.BUFFER_USAGE_VERTEX_BUFFER_BIT, SharingMode: vk.SHARING_MODE_EXCLUSIVE,
			}
			var buf vk.Buffer
			if rfCreate(d, &ci, 0, &buf) != 0 {
				b.Fatal("create")
			}
			rfDestroy(d, uint64(buf), 0)
		})
	})
	b.Run("goki", func(b *testing.B) {
		d := gkDev()
		run(b, func() {
			ci := gk.BufferCreateInfo{
				SType: gk.StructureTypeBufferCreateInfo, Size: 256,
				Usage: gk.BufferUsageFlags(gk.BufferUsageVertexBufferBit), SharingMode: gk.SharingModeExclusive,
			}
			var buf gk.Buffer
			if gk.CreateBuffer(d, &ci, nil, &buf) != gk.Success {
				b.Fatal("create")
			}
			ci.Free()
			gk.DestroyBuffer(d, buf, nil)
		})
	})
	b.Run("go-vk", func(b *testing.B) {
		d := govk.Device(dev())
		run(b, func() {
			ci := govk.BufferCreateInfo{Size: 256, Usage: govk.BUFFER_USAGE_VERTEX_BUFFER_BIT, SharingMode: govk.SHARING_MODE_EXCLUSIVE}
			buf, err := govk.CreateBuffer(d, &ci, nil)
			if err != govk.SUCCESS {
				b.Fatal(err)
			}
			govk.DestroyBuffer(d, buf, nil)
		})
	})
	b.Run("vkng-loader", func(b *testing.B) {
		d := loader.VkDevice(dev())
		run(b, func() {
			ci := vk.BufferCreateInfo{
				SType: vk.STRUCTURE_TYPE_BUFFER_CREATE_INFO, Size: 256,
				Usage: vk.BUFFER_USAGE_VERTEX_BUFFER_BIT, SharingMode: vk.SHARING_MODE_EXCLUSIVE,
			}
			var buf loader.VkBuffer
			if _, err := ngl.VkCreateBuffer(d, (*loader.VkBufferCreateInfo)(unsafe.Pointer(&ci)), nil, &buf); err != nil {
				b.Fatal(err)
			}
			ngl.VkDestroyBuffer(d, buf, nil)
		})
	})
	b.Run("vkng", func(b *testing.B) {
		run(b, func() {
			buf, _, err := ngDev.CreateBuffer(nil, core1_0.BufferCreateInfo{
				Size: 256, Usage: core1_0.BufferUsageVertexBuffer, SharingMode: core1_0.SharingModeExclusive,
			})
			if err != nil {
				b.Fatal(err)
			}
			ngDev.DestroyBuffer(buf, nil)
		})
	})
}
