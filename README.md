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

`Supported` and `MustSupport` report what has resolved so far. `MissingBelow`
looks only at the levels initialised so far, so asking it after each `Init` call
checks that tier. Calling an entry point that did not resolve panics naming it.

The entry points are package state, so they serve one instance at a time. The
first device resolves straight to its driver through `vkGetDeviceProcAddr`. A
second device switches every device-level entry point to the loader's
trampolines, which dispatch on the handle and so serve all devices, at the cost
of one indirection a call. A device destroyed with `DestroyDevice` gives up its
place, so replacing your one device -- after a device loss, say -- keeps the
direct path.

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

## Performance

The same Vulkan call made through this binding, hand-written cgo, purego's
public API and the other Go Vulkan bindings, against one MoltenVK device on an
Apple M4 Max. Nanoseconds a call, median of ten runs, allocations in brackets
where there are any. Each binding is used as its documentation says, so a row
includes what it takes to get a usable Go value back.

| call | this | cgo, typed | cgo, `void*` | goki | go-vk | vkngwrapper loader | vkngwrapper | purego `SyscallN` | purego `RegisterFunc` |
|---|---|---|---|---|---|---|---|---|---|
| `CmdSetStencilReference` | 29.6 | 21.3 | 21.3 | 23.2 | 29.7 | 24.4 | 26.5 | 63.4 (1) | 166 (4) |
| `CmdSetScissor`, one rect | 32.2 | 24.0 | 32.4 | 562 (8) | 35.4 | 27.5 | 40.1 | 66.5 (1) | 189 (4) |
| `GetDeviceQueue` | 27.7 | 20.2 | 29.4 | 30.2 | 31.8 | 30.9 | 39.4 (1) | 63.9 (1) | 187 (4) |
| `GetPhysicalDeviceProperties` | 36.3 | | 37.5 | 1127 (9) | 514 (5) | 32.6 | 237 (5) | 70.0 (1) | 159 (2) |
| `CreateBuffer` + `DestroyBuffer` | 80.1 | | 110 (2) | 284 (4) | 88.1 | 123 (2) | 138 (2) | | 439 (10) |

What it shows:

- A bare call costs about 8 ns more than the cheapest cgo can do: the fixed
  argument block and purego's trampoline, which reads errno on darwin.
- cgo checks every `unsafe.Pointer` argument at run time, and that check costs
  about as much, so against cgo as a binding over Go memory writes it -- the
  `void*` column -- a call with a pointer comes out level or ahead. cgo also
  moves what such a pointer addresses to the heap; this binding does not.
- Nothing here allocates. Bindings with their own Go struct layouts convert on
  every call, and on a struct or a slice that is 6 to 30 times the call.
- The loader's trampolines, which serve a second device, cost about a
  nanosecond a call on this driver.

At ten thousand calls a frame the gap to the cgo floor is under a tenth of a
millisecond. The driver's own work is usually most of each call.

The code is in [`benchmarks`](benchmarks), a module of its own so that the
bindings it compares against stay out of this one's dependencies. It needs cgo
and the Vulkan headers:

```sh
cd benchmarks
CGO_CFLAGS=-I$VULKAN_SDK/include go test -run '^$' -bench . -count 10
```

`bench_test.go` says what each row is and the settings macOS needs.

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
