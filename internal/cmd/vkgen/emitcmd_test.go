package main

import (
	"bytes"
	"testing"
)

// A float goes in the Frame's float slots, numbered among the floats alone;
// uintptr(x) truncated it and shifted every later float into the wrong register.
func TestEmitCallFloats(t *testing.T) {
	cb := fieldDef{Name: "commandBuffer", Type: "CommandBuffer"}
	f32 := func(n string) fieldDef { return fieldDef{Name: n, Type: "float32"} }
	for _, tc := range []struct {
		f    vkFn
		want string
	}{
		{vkFn{VarName: "pCmdSetDepthBias", Params: []fieldDef{cb, f32("depthBiasConstantFactor"), f32("depthBiasClamp"), f32("depthBiasSlopeFactor")}},
			"\tfr := new(ccall.Frame)\n" +
				"\tfr.Float32(0, depthBiasConstantFactor)\n" +
				"\tfr.Float32(1, depthBiasClamp)\n" +
				"\tfr.Float32(2, depthBiasSlopeFactor)\n" +
				"\tfr.Call1(pCmdSetDepthBias, uintptr(commandBuffer))\n"},
		{vkFn{VarName: "pMixed", Params: []fieldDef{f32("a"), {Name: "n", Type: "uint32"}, {Name: "b", Type: "float64"}, {Name: "p", Type: "*Rect2D"}}},
			"\tfr := new(ccall.Frame)\n" +
				"\tfr.Float32(0, a)\n" +
				"\tfr.Float64(1, b)\n" +
				"\tfr.Call2(pMixed, uintptr(n), uintptr(unsafe.Pointer(p)))\n" +
				"\truntime.KeepAlive(p)\n"},
		{vkFn{VarName: "pFloatRet", Params: []fieldDef{cb}, Ret: "float32"},
			"\tfr := new(ccall.Frame)\n" +
				"\tfr.Call1(pFloatRet, uintptr(commandBuffer))\n" +
				"\treturn fr.Float32Result()\n"},
		{vkFn{VarName: "pCmdDraw", Params: []fieldDef{cb, {Name: "vertexCount", Type: "uint32"}}},
			"\tnew(ccall.Frame).Call2(pCmdDraw, uintptr(commandBuffer), uintptr(vertexCount))\n"},
		{vkFn{VarName: "pGetError", Params: []fieldDef{cb}, Ret: "Result"},
			"\tr1 := new(ccall.Frame).Call1(pGetError, uintptr(commandBuffer))\n\treturn Result(r1)\n"},
	} {
		var b bytes.Buffer
		emitCall(&b, tc.f)
		if got := b.String(); got != tc.want {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", tc.f.VarName, got, tc.want)
		}
	}
}
