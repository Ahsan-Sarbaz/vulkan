//go:build darwin || linux

// Package benchmarks compares this binding against cgo, purego's public API and
// the other Go Vulkan bindings, on a real driver.
//
// It is its own module so that the bindings it compares against, and cgo, stay
// out of the binding's dependencies. See bench_test.go for how to run it.
package benchmarks

// Hand-written cgo: the cheapest way cgo can call a Vulkan function pointer.
// The void* forms are what a binding passing Go memory usually writes; the
// typed forms let cgo skip its runtime pointer check, and are the floor.

/*
#cgo CFLAGS: -DVK_NO_PROTOTYPES
#include <vulkan/vulkan.h>
#include <stdint.h>

static void bench_stencil(uintptr_t fn, uintptr_t cb, uint32_t mask, uint32_t ref) {
	((PFN_vkCmdSetStencilReference)fn)((VkCommandBuffer)cb, mask, ref);
}
static void bench_scissor(uintptr_t fn, uintptr_t cb, uint32_t first, uint32_t n, void* r) {
	((PFN_vkCmdSetScissor)fn)((VkCommandBuffer)cb, first, n, (const VkRect2D*)r);
}
static void bench_scissor_typed(uintptr_t fn, uintptr_t cb, uint32_t first, uint32_t n, VkRect2D* r) {
	((PFN_vkCmdSetScissor)fn)((VkCommandBuffer)cb, first, n, r);
}
static void bench_queue(uintptr_t fn, uintptr_t dev, uint32_t fam, uint32_t idx, void* out) {
	((PFN_vkGetDeviceQueue)fn)((VkDevice)dev, fam, idx, (VkQueue*)out);
}
static void bench_queue_typed(uintptr_t fn, uintptr_t dev, uint32_t fam, uint32_t idx, uintptr_t* out) {
	((PFN_vkGetDeviceQueue)fn)((VkDevice)dev, fam, idx, (VkQueue*)out);
}
static void bench_props(uintptr_t fn, uintptr_t pd, void* out) {
	((PFN_vkGetPhysicalDeviceProperties)fn)((VkPhysicalDevice)pd, (VkPhysicalDeviceProperties*)out);
}
static int32_t bench_create_buffer(uintptr_t fn, uintptr_t dev, void* ci, void* out) {
	return ((PFN_vkCreateBuffer)fn)((VkDevice)dev, (const VkBufferCreateInfo*)ci, NULL, (VkBuffer*)out);
}
static void bench_destroy_buffer(uintptr_t fn, uintptr_t dev, uint64_t buf) {
	((PFN_vkDestroyBuffer)fn)((VkDevice)dev, (VkBuffer)buf, NULL);
}
*/
import "C"

import "unsafe"

func cgoStencil(fn, cb uintptr, mask, ref uint32) {
	C.bench_stencil(C.uintptr_t(fn), C.uintptr_t(cb), C.uint32_t(mask), C.uint32_t(ref))
}

func cgoScissor(fn, cb uintptr, first, n uint32, r unsafe.Pointer) {
	C.bench_scissor(C.uintptr_t(fn), C.uintptr_t(cb), C.uint32_t(first), C.uint32_t(n), r)
}

func cgoScissorTyped(fn, cb uintptr, first, n uint32, r unsafe.Pointer) {
	C.bench_scissor_typed(C.uintptr_t(fn), C.uintptr_t(cb), C.uint32_t(first), C.uint32_t(n), (*C.VkRect2D)(r))
}

func cgoQueue(fn, dev uintptr, fam, idx uint32, out unsafe.Pointer) {
	C.bench_queue(C.uintptr_t(fn), C.uintptr_t(dev), C.uint32_t(fam), C.uint32_t(idx), out)
}

func cgoQueueTyped(fn, dev uintptr, fam, idx uint32, out *uintptr) {
	C.bench_queue_typed(C.uintptr_t(fn), C.uintptr_t(dev), C.uint32_t(fam), C.uint32_t(idx), (*C.uintptr_t)(unsafe.Pointer(out)))
}

func cgoProps(fn, pd uintptr, out unsafe.Pointer) {
	C.bench_props(C.uintptr_t(fn), C.uintptr_t(pd), out)
}

func cgoCreateBuffer(fn, dev uintptr, ci, out unsafe.Pointer) int32 {
	return int32(C.bench_create_buffer(C.uintptr_t(fn), C.uintptr_t(dev), ci, out))
}

func cgoDestroyBuffer(fn, dev uintptr, buf uint64) {
	C.bench_destroy_buffer(C.uintptr_t(fn), C.uintptr_t(dev), C.uint64_t(buf))
}
