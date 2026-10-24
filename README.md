# vulkan

A Vulkan 1.4 binding for Go, generated from the Khronos registry and called
through [purego](https://github.com/ebitengine/purego). No cgo.

- **The whole API.** Core 1.4 plus every extension the registry supports: 841
  commands, 1457 structs and unions, 477 extensions. A new Vulkan release is a
  regeneration, not an edit.
- **Structs are the C structs.** Same field order, padding and size, so a call
  passes an address with no marshalling. A generated test holds every struct and
  union to the real headers: size, alignment and every member offset.
- **Calls allocate nothing.** Every command goes through `internal/ccall`, which
  reaches the runtime's C call path without purego's per-call allocation:
  about 23 ns a call on an Apple M4 Max, against 71 ns and an allocation
  through purego's `SyscallN`.
- **No C toolchain.** `CGO_ENABLED=0` builds and cross-compiles work.

```sh
go get github.com/Ahsan-Sarbaz/vulkan
```

## Use

Vulkan dispatches in three tiers, and so does loading. Hand `Init` the loader's
`vkGetInstanceProcAddr`, then resolve the instance and device levels once they
exist:

```go
lib, err := purego.Dlopen("libvulkan.so.1", purego.RTLD_NOW|purego.RTLD_GLOBAL)
if err != nil {
	return err
}
gipa, err := purego.Dlsym(lib, "vkGetInstanceProcAddr")
if err != nil {
	return err
}

vulkan.Init(gipa)
// ... vulkan.CreateInstance(...)
vulkan.InitInstance(instance)
// ... vulkan.CreateDevice(...)
vulkan.InitDevice(device)
if missing := vulkan.MissingBelow(1.3); len(missing) > 0 {
	return fmt.Errorf("driver lacks %v", missing)
}
```

On macOS the library is `libvulkan.1.dylib` (the Vulkan SDK or MoltenVK). On
Windows load `vulkan-1.dll` with `syscall.LoadLibrary` and `GetProcAddress`. A
windowing library that already loaded Vulkan, such as GLFW, can hand over its
own `vkGetInstanceProcAddr` instead.

`Supported`, `MustSupport` and `MissingBelow` report what has resolved so far,
so ask them after the level in question is initialised. Calling an entry point
that did not resolve panics naming it.

The entry points are package state, so they serve one instance at a time. The
first device resolves straight to its driver through `vkGetDeviceProcAddr`. A
second device switches every device-level entry point to the loader's
trampolines, which dispatch on the handle and so serve all devices, at the cost
of one indirection a call.

### Shape of the API

- Names drop the `vk` prefix: `vkCreateDevice` is `CreateDevice`,
  `VkDeviceCreateInfo` is `DeviceCreateInfo`, `VK_FORMAT_R8G8B8A8_UNORM` is
  `FORMAT_R8G8B8A8_UNORM`. Members keep their names, capitalised: `PNext`.
- Constants are typed, so a `Format` cannot go where an `ImageLayout` belongs.
- A `FlagBits` type is an alias of its `Flags` type, so bits combine with no
  conversion: `Usage: IMAGE_USAGE_SAMPLED_BIT | IMAGE_USAGE_TRANSFER_DST_BIT`.
- The names a type had before promotion to core are aliases:
  `PhysicalDeviceFeatures2KHR` is `PhysicalDeviceFeatures2`.
- C strings are `*byte` and fixed-size names are `[N]byte`, NUL-terminated.
- A union is opaque storage with one accessor per member:
  `v.Float32()[0] = 1` on a `ClearColorValue`.
- A bitfield is an accessor pair, since Go has none:
  `inst.SetMask(0xFF)`, `inst.Mask()` on an `AccelerationStructureInstanceKHR`.
- Platform types keep their ABI: an `xcb_window_t` is `uint32`, an `HWND` is
  `unsafe.Pointer`, an `LPCWSTR` is `*uint16`, and a pointer to an opaque
  struct such as `xcb_connection_t*` or `ANativeWindow*` is `unsafe.Pointer`.
- Video-codec enums that structs embed by value (`StdVideoH264ProfileIdc` and
  the like) are `int32` types. Their values come from the Vulkan Video
  headers, which the registry does not include; the codec parameter structs
  are reached through `unsafe.Pointer`.

## Pointer rules

This is not cgo, so cgo's pointer checks do not run. The generated wrappers keep
every pointer argument alive for the length of the call, and everything it
points to with it. What they cannot know about is a pointer Vulkan keeps after
the call returns, such as the `PUserData` of a debug messenger or of allocation
callbacks. Keep what that points to outside the Go heap, or pin it with
`runtime.Pinner`, for as long as Vulkan may use it.

## Platforms

amd64 and arm64 on Linux, Windows and macOS. With `CGO_ENABLED=0`, FreeBSD does
not build at purego v0.11.1: its `internal/fakecgo` is rejected by Go 1.27.

Two platforms are left out. GGP (Stadia) types are defined only in an
unpublished SDK, so their sizes cannot be checked, and SCI belongs to Vulkan SC.

## Regenerating

```sh
go generate ./...                    # regenerate from the vendored vk.xml
go run ./internal/cmd/vkgen -fetch   # fetch the latest registry, then regenerate
go test -tags vklayout .             # check every layout and bitfield against the SDK headers
```

The layout check needs cgo and the Vulkan SDK headers, on Linux or macOS
(`CGO_CFLAGS=-I...` if they are not in a default include path). Each struct is
compiled under its extension's spec version, so an SDK older than the registry
skips what it has never heard of and says how many it skipped. Every platform's
structs are checked on any machine: the window-system types are declared to C
with the same sizes this binding gives them.

## License

MIT, see [LICENSE](LICENSE).

`internal/cmd/vkgen/vk.xml` is the Vulkan API Registry, Copyright 2015-2026 The
Khronos Group Inc., licensed `Apache-2.0 OR MIT` and used here under MIT. Its
notice is in the file's header.
