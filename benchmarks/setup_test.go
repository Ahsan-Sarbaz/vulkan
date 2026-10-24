//go:build darwin || linux

package benchmarks

import (
	"fmt"
	"os"
	"runtime"

	vk "github.com/Ahsan-Sarbaz/vulkan"
	"github.com/ebitengine/purego"
)

// One instance and device, created through this binding. The other bindings
// are handed the same handles, so every row drives the same driver objects
// and the driver's share of each number is the same.
var (
	libvulkan  string
	gipa       uintptr
	instance   vk.Instance
	physDev    vk.PhysicalDevice
	device     vk.Device
	cmdPool    vk.CommandPool
	cmdBuf     vk.CommandBuffer
	trampoline bool
)

// portability is true where the driver may be a portability implementation
// (MoltenVK, KosmicKrisp) that the loader hides unless asked for it.
var portability = runtime.GOOS == "darwin"

// libvulkanCandidates lists where to look for the loader. dyld no longer
// searches /usr/local/lib for a bare name, so macOS names the usual installs.
func libvulkanCandidates() []string {
	if p := os.Getenv("VULKAN_LIBRARY"); p != "" {
		return []string{p}
	}
	if runtime.GOOS != "darwin" {
		return []string{"libvulkan.so.1"}
	}
	c := []string{"libvulkan.1.dylib"}
	if sdk := os.Getenv("VULKAN_SDK"); sdk != "" {
		c = append(c, sdk+"/lib/libvulkan.1.dylib")
	}
	return append(c, "/usr/local/lib/libvulkan.1.dylib", "/opt/homebrew/lib/libvulkan.1.dylib")
}

func cstr(s string) *byte { b := append([]byte(s), 0); return &b[0] }

func check(r vk.Result, what string) {
	if r != vk.SUCCESS {
		panic(fmt.Sprintf("%s: %d", what, r))
	}
}

func setup() {
	var lib uintptr
	var err error
	for _, libvulkan = range libvulkanCandidates() {
		if lib, err = purego.Dlopen(libvulkan, purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
			break
		}
	}
	if err != nil {
		panic(err)
	}
	gipa, err = purego.Dlsym(lib, "vkGetInstanceProcAddr")
	if err != nil {
		panic(err)
	}
	vk.Init(gipa)

	app := vk.ApplicationInfo{SType: vk.STRUCTURE_TYPE_APPLICATION_INFO, ApiVersion: 1<<22 | 3<<12}
	ici := vk.InstanceCreateInfo{
		SType:            vk.STRUCTURE_TYPE_INSTANCE_CREATE_INFO,
		PApplicationInfo: &app,
	}
	if portability {
		exts := []*byte{cstr("VK_KHR_portability_enumeration")}
		ici.Flags = vk.INSTANCE_CREATE_ENUMERATE_PORTABILITY_BIT_KHR
		ici.EnabledExtensionCount = 1
		ici.PpEnabledExtensionNames = &exts[0]
	}
	check(vk.CreateInstance(&ici, nil, &instance), "vkCreateInstance")
	vk.InitInstance(instance)

	var n uint32
	check(vk.EnumeratePhysicalDevices(instance, &n, nil), "vkEnumeratePhysicalDevices")
	if n == 0 {
		panic("no Vulkan device")
	}
	pds := make([]vk.PhysicalDevice, n)
	check(vk.EnumeratePhysicalDevices(instance, &n, &pds[0]), "vkEnumeratePhysicalDevices")
	physDev = pds[0]

	device = newDevice()
	vk.InitDevice(device)
	if os.Getenv("BENCH_TRAMPOLINE") == "1" {
		// A second device moves every device-level entry point onto the
		// loader's trampolines: the path goki and go-vk always take.
		vk.InitDevice(newDevice())
		trampoline = true
	}

	pci := vk.CommandPoolCreateInfo{
		SType: vk.STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
		Flags: vk.COMMAND_POOL_CREATE_RESET_COMMAND_BUFFER_BIT,
	}
	check(vk.CreateCommandPool(device, &pci, nil, &cmdPool), "vkCreateCommandPool")
	cai := vk.CommandBufferAllocateInfo{
		SType:              vk.STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
		CommandPool:        cmdPool,
		Level:              vk.COMMAND_BUFFER_LEVEL_PRIMARY,
		CommandBufferCount: 1,
	}
	check(vk.AllocateCommandBuffers(device, &cai, &cmdBuf), "vkAllocateCommandBuffers")
	beginCB()

	var props vk.PhysicalDeviceProperties
	vk.GetPhysicalDeviceProperties(physDev, &props)
	fmt.Fprintf(os.Stderr, "device: %s, trampolines: %v\n", cName(props.DeviceName[:]), trampoline)
}

// newDevice makes a device with one queue from family 0.
func newDevice() vk.Device {
	prio := float32(1)
	qci := vk.DeviceQueueCreateInfo{
		SType:            vk.STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
		QueueCount:       1,
		PQueuePriorities: &prio,
	}
	dci := vk.DeviceCreateInfo{
		SType:                vk.STRUCTURE_TYPE_DEVICE_CREATE_INFO,
		QueueCreateInfoCount: 1,
		PQueueCreateInfos:    &qci,
	}
	if portability {
		exts := []*byte{cstr("VK_KHR_portability_subset")}
		dci.EnabledExtensionCount = 1
		dci.PpEnabledExtensionNames = &exts[0]
	}
	var d vk.Device
	check(vk.CreateDevice(physDev, &dci, nil, &d), "vkCreateDevice")
	return d
}

func beginCB() {
	bi := vk.CommandBufferBeginInfo{
		SType: vk.STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
		Flags: vk.COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT,
	}
	check(vk.BeginCommandBuffer(cmdBuf, &bi), "vkBeginCommandBuffer")
}

// resetCB throws away what was recorded, so a long run does not grow the
// command buffer without bound.
func resetCB() {
	vk.EndCommandBuffer(cmdBuf)
	check(vk.ResetCommandBuffer(cmdBuf, 0), "vkResetCommandBuffer")
	beginCB()
}

func cName(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// deviceProc and instanceProc resolve the driver's own pointers, the ones this
// binding holds in direct mode, for the cgo and purego rows.
func deviceProc(name string) uintptr {
	r := vk.GetDeviceProcAddr(device, cstr(name))
	if r == 0 {
		panic("no " + name)
	}
	return r
}

func instanceProc(name string) uintptr {
	r := vk.GetInstanceProcAddr(instance, cstr(name))
	if r == 0 {
		panic("no " + name)
	}
	return r
}
