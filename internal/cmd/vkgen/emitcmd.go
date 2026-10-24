package main

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// emitConsts writes every enumerant, grouped by the type it belongs to and
// typed as that type, so a VkFormat cannot be passed where a VkImageLayout
// belongs. A group whose type the selected API does not reach is left out, and
// so is an alias whose target is, or it would not compile.
func (e *emitter) emitConsts() []byte {
	var b bytes.Buffer
	header(&b, e.pkg)

	groups := make([]string, 0, len(e.fs.enumConsts))
	for g := range e.fs.enumConsts {
		if g == "" || e.need[g] {
			groups = append(groups, g)
		}
	}
	sort.Strings(groups)

	defined := map[string]bool{}
	for _, g := range groups {
		for _, d := range e.fs.enumConsts[g] {
			if d.Alias != "" {
				defined[d.Name] = true
			} else if _, ok := goLiteral(d.Value); ok {
				defined[d.Name] = true
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, g := range groups {
			for _, d := range e.fs.enumConsts[g] {
				if d.Alias != "" && defined[d.Name] && !defined[d.Alias] {
					delete(defined, d.Name)
					changed = true
				}
			}
		}
	}

	for _, g := range groups {
		var defs []constDef
		for _, d := range e.fs.enumConsts[g] {
			if defined[d.Name] {
				defs = append(defs, d)
			}
		}
		if len(defs) == 0 {
			continue
		}
		sort.SliceStable(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
		typ := ""
		if g != "" {
			typ = " " + trimVk(g)
			fmt.Fprintf(&b, "// %s values.\nconst (\n", trimVk(g))
		} else {
			b.WriteString("// API constants.\nconst (\n")
		}
		for _, d := range defs {
			if d.Alias != "" {
				fmt.Fprintf(&b, "\t%s = %s\n", trimConst(d.Name), trimConst(d.Alias))
				continue
			}
			v, _ := goLiteral(d.Value)
			// Every line carries the type. A Go const block only repeats the
			// previous line when the expression is omitted entirely, so putting
			// it on the first alone would leave the rest untyped -- which
			// compiles, and silently gives up the checking this is for.
			t := typ
			if strings.HasPrefix(v, "\"") {
				t = ""
			}
			fmt.Fprintf(&b, "\t%s%s = %s\n", trimConst(d.Name), t, v)
		}
		b.WriteString(")\n\n")
	}
	return b.Bytes()
}

// vkFn is a command resolved to Go types.
type vkFn struct {
	CName   string
	GoName  string
	VarName string
	Params  []fieldDef
	Ret     string
	Origin  string
	// CoreVersion is the Vulkan version that introduced this command, or 0 when
	// it came from an extension. What MissingBelow filters on.
	CoreVersion float64
	// Level decides which loader resolves it: the global entry points come from
	// vkGetInstanceProcAddr with a nil instance, the rest from an instance or a
	// device. A device-level function resolved through the instance still works
	// but goes through the loader's dispatch trampoline on every call.
	Level string // "global", "instance", "device"
}

func (e *emitter) resolveCmd(c xcommand) (vkFn, error) {
	f := vkFn{
		CName:  c.Proto.pname,
		GoName: trimCmd(c.Proto.pname),
		Origin: e.fs.versionOf[c.Proto.pname],
	}
	f.VarName = "p" + f.GoName
	if v, ok := strings.CutPrefix(f.Origin, "Vulkan "); ok {
		f.CoreVersion, _ = strconv.ParseFloat(v, 64)
	}
	if c.Proto.ctype != "void" {
		t, err := e.conv.goType(c.Proto.ctype)
		if err != nil {
			return f, fmt.Errorf("%s: return: %w", f.CName, err)
		}
		f.Ret = t
	}
	for _, p := range c.Param {
		if p.API != "" && !apiMatches(p.API, e.api) {
			continue
		}
		t, err := e.conv.goType(p.ctype)
		if err != nil {
			return f, fmt.Errorf("%s: param %s: %w", f.CName, p.pname, err)
		}
		if p.array != "" {
			// A C array parameter is a pointer to its first element, so the
			// outermost bound goes and the rest stay part of the element type.
			dims, err := e.arrayDims(p.array)
			if err != nil {
				return f, fmt.Errorf("%s: param %s: %w", f.CName, p.pname, err)
			}
			_, inner, _ := strings.Cut(dims, "]")
			t = "*" + inner + t
		}
		f.Params = append(f.Params, fieldDef{Name: paramName(p.pname), Type: t})
	}
	f.Level = commandLevel(f)
	return f, nil
}

// commandLevel reads the dispatch level off the first parameter, which is what
// the Vulkan loader does too.
//
// vkGetInstanceProcAddr takes an instance and is still global: the spec has it
// answer with a null one, so it resolves before any instance exists.
func commandLevel(f vkFn) string {
	switch f.CName {
	case "vkCreateInstance", "vkEnumerateInstanceExtensionProperties",
		"vkEnumerateInstanceLayerProperties", "vkEnumerateInstanceVersion",
		"vkGetInstanceProcAddr":
		return "global"
	}
	if len(f.Params) == 0 {
		return "global"
	}
	switch f.Params[0].Type {
	case "Device", "Queue", "CommandBuffer":
		return "device"
	}
	return "instance"
}

func (e *emitter) emitCommands(fns []vkFn) []byte {
	var b bytes.Buffer
	header(&b, e.pkg)
	b.WriteString("import (\n\t\"runtime\"\n\t\"unsafe\"\n\n\t\"github.com/Ahsan-Sarbaz/vulkan/internal/ccall\"\n)\n\nvar _ unsafe.Pointer\n\n")
	for _, f := range fns {
		fmt.Fprintf(&b, "// %s is %s, from %s.\n", f.GoName, f.CName, f.Origin)
		// Not inlined: each inlined copy would add its own ccall.Frame to the caller.
		b.WriteString("//\n//go:noinline\n")
		b.WriteString(f.signature())
		fmt.Fprintf(&b, " {\n\tif %s == 0 {\n\t\tunresolved(%q)\n\t}\n", f.VarName, f.CName)
		emitCall(&b, f)
		b.WriteString("}\n\n")
	}
	return b.Bytes()
}

// emitCall writes a call through internal/ccall. Integers and pointers go to
// CallN; each float is set on the Frame first, numbered among the floats alone,
// because the C ABI fills the float registers in their own sequence.
func emitCall(b *bytes.Buffer, f vkFn) {
	fr := "new(ccall.Frame)"
	retMethod, floatRet := floatMethod[f.Ret]
	if floatRet || f.floatParams() > 0 {
		fr = "fr"
		b.WriteString("\tfr := new(ccall.Frame)\n")
	}
	var args []string
	nf := 0
	for _, p := range f.Params {
		if m, ok := floatMethod[p.Type]; ok {
			fmt.Fprintf(b, "\tfr.%s(%d, %s)\n", m, nf, p.Name)
			nf++
			continue
		}
		args = append(args, argExpr(p))
	}
	call := fmt.Sprintf("%s.Call%d(%s", fr, len(args), f.VarName)
	if len(args) > 0 {
		call += ", " + strings.Join(args, ", ")
	}
	call += ")"
	// A pointer converted to uintptr is a number to the garbage collector, and
	// CallN is not one of the assembly functions the compiler keeps such an
	// argument alive across. Without this the struct, and everything it points
	// to, could be collected while C still reads it -- which is also why cgo's
	// own wrappers keep their arguments alive.
	var keep strings.Builder
	for _, p := range f.Params {
		if p.Type == "unsafe.Pointer" || strings.HasPrefix(p.Type, "*") {
			fmt.Fprintf(&keep, "\truntime.KeepAlive(%s)\n", p.Name)
		}
	}
	switch {
	case f.Ret == "":
		fmt.Fprintf(b, "\t%s\n%s", call, keep.String())
	case floatRet:
		fmt.Fprintf(b, "\t%s\n%s\treturn fr.%sResult()\n", call, keep.String(), retMethod)
	default:
		fmt.Fprintf(b, "\tr1 := %s\n%s\treturn %s\n", call, keep.String(), retExpr(f.Ret))
	}
}

// floatMethod is the ccall.Frame setter for each type the C ABI passes in a
// float register.
var floatMethod = map[string]string{"float32": "Float32", "float64": "Float64"}

func (f vkFn) floatParams() int {
	n := 0
	for _, p := range f.Params {
		if _, ok := floatMethod[p.Type]; ok {
			n++
		}
	}
	return n
}

func (f vkFn) signature() string {
	var b strings.Builder
	fmt.Fprintf(&b, "func %s(", f.GoName)
	for i, p := range f.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %s", p.Name, p.Type)
	}
	b.WriteByte(')')
	if f.Ret != "" {
		b.WriteByte(' ')
		b.WriteString(f.Ret)
	}
	return b.String()
}

func argExpr(p fieldDef) string {
	switch {
	case p.Type == "unsafe.Pointer":
		return "uintptr(" + p.Name + ")"
	case strings.HasPrefix(p.Type, "*"):
		return "uintptr(unsafe.Pointer(" + p.Name + "))"
	case p.Type == "uintptr":
		return p.Name
	default:
		return "uintptr(" + p.Name + ")"
	}
}

func retExpr(t string) string {
	switch {
	case t == "unsafe.Pointer":
		return "ptr(r1)"
	case strings.HasPrefix(t, "*"):
		return "(" + t + ")(ptr(r1))"
	case t == "uintptr":
		return "r1"
	default:
		return t + "(r1)"
	}
}

// emitProcs writes the entry-point table and the three-tier loader.
func (e *emitter) emitProcs(fns []vkFn) []byte {
	var b bytes.Buffer
	header(&b, e.pkg)

	b.WriteString("var (\n")
	for _, f := range fns {
		fmt.Fprintf(&b, "\t%s uintptr\n", f.VarName)
	}
	b.WriteString(")\n\n")

	b.WriteString(`// procs is every entry point, with the level that can resolve it. Vulkan
// dispatches in three tiers: a global function comes from the loader with no
// instance, an instance function once there is one, and a device function from
// vkGetDeviceProcAddr -- which is worth doing rather than taking the instance
// answer, because the instance form goes through the loader's trampoline on
// every call.
//
// ver is the core version that introduced the entry point, or 0 for one that
// only an extension provides. An extension entry point not resolving is the
// normal case, not an error, which is what MissingBelow is for.
var procs = [...]struct {
	name  string
	p     *uintptr
	level uint8
	ver   float64
}{
`)
	for _, f := range fns {
		lv := "levelInstance"
		switch f.Level {
		case "global":
			lv = "levelGlobal"
		case "device":
			lv = "levelDevice"
		}
		fmt.Fprintf(&b, "\t{%q, &%s, %s, %g},\n", f.CName, f.VarName, lv, f.CoreVersion)
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// emitMeta records the generation inputs.
func (e *emitter) emitMeta(version string, ncmd, nstruct int) []byte {
	var b bytes.Buffer
	header(&b, e.pkg)
	fmt.Fprintf(&b, "// What this package was generated from. Re-run internal/cmd/vkgen to change it.\n"+
		"// HeaderVersion is the registry's VK_HEADER_VERSION.\nconst (\n\tAPI     = %q\n\tVersion = %q\n\n"+
		"\tHeaderVersion = %d\n\tCommandCount  = %d\n\tStructCount   = %d\n)\n\n",
		e.api, version, e.reg.headerVersion(e.api), ncmd, nstruct)
	b.WriteString("// Extensions is every extension generated in, beyond the core version above.\n" +
		"// Whether a driver has one is a question for the instance or the device.\nvar Extensions = []string{\n")
	for _, x := range slices.Sorted(slices.Values(e.fs.extensions)) {
		fmt.Fprintf(&b, "\t%q,\n", x)
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// emitLayoutCgo writes the cgo half of the layout check: for every emitted
// struct and union, its C size, alignment and member offsets, read from the
// real headers; and for a struct with bitfields, a function that sets each one
// to a known pattern, so the accessors can be held to what C does.
//
// It is a normal file rather than a test file because Go does not allow cgo in
// one, and it is behind a tag because it needs the Vulkan SDK -- which the
// binding it checks does not.
//
// Each struct sits under the guard computeGuards gave it, so a header older
// than the vendored registry skips what it has never heard of, and the test
// reports how much that was. The platform headers are included directly, after
// stubs for their window-system types, so every platform's structs are checked
// on any machine.
func (e *emitter) emitLayoutCgo(names []string) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("//go:build vklayout && (darwin || linux)\n\n// Code generated by internal/cmd/vkgen. DO NOT EDIT.\n\npackage " + e.pkg + "\n\n" +
		"/*\n#cgo darwin CFLAGS: -I/usr/local/include -I/opt/homebrew/include\n#cgo linux CFLAGS: -I/usr/include\n" +
		"#include <stddef.h>\n#include <stdint.h>\n")
	if e.api == "vulkansc" {
		// Vulkan SC has its own headers, from the Vulkan SC SDK, and no
		// platform surfaces this generator maps.
		b.WriteString("#include <vulkan/vulkan_sc_core.h>\n")
	} else {
		b.WriteString("#include <vulkan/vulkan_core.h>\n\n" + layoutStubs)
		// An SDK older than a platform has no header for it; its structs then
		// count as undeclared rather than failing the build.
		for _, h := range layoutHeaders {
			fmt.Fprintf(&b, "#if __has_include(<vulkan/%s>)\n#include <vulkan/%s>\n#endif\n", h, h)
		}
	}
	b.WriteString("\n#define VKGEN_PAT(k, w) ((0x5A5A5A5A5A5A5A5Aull ^ ((unsigned long long)(k) * 0x9E3779B97F4A7C15ull)) & " +
		"((w) >= 64 ? ~0ull : (1ull << (w)) - 1))\n\n")
	for _, n := range names {
		sd := e.structs[n]
		v := sd.VkName
		g, ok := e.guard[v]
		if !ok {
			return nil, fmt.Errorf("%s: no header guard; nothing the registry requires reaches it", v)
		}
		if !sd.IsUnion && len(sd.Fields) == 0 {
			return nil, fmt.Errorf("%s: no fields", v)
		}
		fmt.Fprintf(&b, "#if %s\n", g)
		fmt.Fprintf(&b, "static size_t vkgen_size_%s(void) { return sizeof(%s); }\n", v, v)
		fmt.Fprintf(&b, "static size_t vkgen_align_%s(void) { return _Alignof(%s); }\n", v, v)
		if !sd.IsUnion {
			fmt.Fprintf(&b, "static void vkgen_off_%s(size_t *o) {\n", v)
			for i, f := range sd.Fields {
				if f.CName == "" {
					fmt.Fprintf(&b, "\to[%d] = (size_t)-1;\n", i)
				} else {
					fmt.Fprintf(&b, "\to[%d] = offsetof(%s, %s);\n", i, v, f.CName)
				}
			}
			b.WriteString("}\n")
		}
		if len(sd.Bitfields) > 0 {
			fmt.Fprintf(&b, "static void vkgen_bits_%s(void *p) {\n\t%s *s = p;\n", v, v)
			for k, f := range sd.Bitfields {
				fmt.Fprintf(&b, "\ts->%s = VKGEN_PAT(%d, %d);\n", f.CName, k, f.Width)
			}
			b.WriteString("}\n")
		}
		// The stubs keep the Go side uniform; a zero size means "this SDK does
		// not have it" and the test skips rather than fails.
		fmt.Fprintf(&b, "#else\nstatic size_t vkgen_size_%s(void) { return 0; }\nstatic size_t vkgen_align_%s(void) { return 0; }\n", v, v)
		if !sd.IsUnion {
			fmt.Fprintf(&b, "static void vkgen_off_%s(size_t *o) { (void)o; }\n", v)
		}
		if len(sd.Bitfields) > 0 {
			fmt.Fprintf(&b, "static void vkgen_bits_%s(void *p) { (void)p; }\n", v)
		}
		b.WriteString("#endif\n")
	}
	b.WriteString("*/\nimport \"C\"\n\nimport \"unsafe\"\n\n")
	b.WriteString(`// cStruct is what the headers say about one struct. A zero Size means the
// installed SDK does not declare it, which is not a failure. Offsets is nil for
// a union, and an offset of ^uintptr(0) marks a bitfield storage unit, which C
// cannot take the offset of.
type cStruct struct {
	Name    string
	Size    uintptr
	Align   uintptr
	Offsets []uintptr
}

func cLayouts() []cStruct {
	out := make([]cStruct, 0, ` + fmt.Sprint(len(names)) + `)
`)
	for _, n := range names {
		sd := e.structs[n]
		v := sd.VkName
		if sd.IsUnion {
			fmt.Fprintf(&b, "\tout = append(out, cStruct{Name: %q, Size: uintptr(C.vkgen_size_%s()), Align: uintptr(C.vkgen_align_%s())})\n", v, v, v)
			continue
		}
		fmt.Fprintf(&b, "\t{\n\t\tvar o [%d]C.size_t\n\t\tC.vkgen_off_%s(&o[0])\n"+
			"\t\toff := make([]uintptr, len(o))\n\t\tfor i, v := range o {\n\t\t\toff[i] = uintptr(v)\n\t\t}\n"+
			"\t\tout = append(out, cStruct{%q, uintptr(C.vkgen_size_%s()), uintptr(C.vkgen_align_%s()), off})\n\t}\n",
			len(sd.Fields), v, v, v, v)
	}
	b.WriteString("\treturn out\n}\n\n// cBits has C set every bitfield of a struct to its test pattern.\nfunc cBits(name string, p unsafe.Pointer) {\n\tswitch name {\n")
	for _, n := range names {
		if sd := e.structs[n]; len(sd.Bitfields) > 0 {
			fmt.Fprintf(&b, "\tcase %q:\n\t\tC.vkgen_bits_%s(p)\n", sd.VkName, sd.VkName)
		}
	}
	b.WriteString("\t}\n}\n")
	return b.Bytes(), nil
}

// emitLayoutTest writes the comparison. Reflection gives the Go side, so this
// stays short whatever the struct count is.
func (e *emitter) emitLayoutTest(names []string) []byte {
	var b bytes.Buffer
	b.WriteString(`//go:build vklayout && (darwin || linux)

// Code generated by internal/cmd/vkgen. DO NOT EDIT.

// Struct layout conformance:
//
//	go test -tags vklayout .
//
// This is the load-bearing test of the whole binding. A generated struct that
// does not match its C original is not a compile error, it is silent memory
// corruption, and there are over a thousand of them. Every struct and union is
// held to the real headers -- size, alignment and every member offset -- and
// every bitfield accessor to the bits C itself writes.
package ` + e.pkg + `

import (
	"bytes"
	"reflect"
	"testing"
	"unsafe"
)

var goTypes = map[string]reflect.Type{
`)
	for _, n := range names {
		sd := e.structs[n]
		fmt.Fprintf(&b, "\t%q: reflect.TypeFor[%s](),\n", sd.VkName, sd.GoName)
	}
	b.WriteString("}\n\n// bitfields is each struct's bitfield accessors, in the order C numbers their\n// test patterns.\nvar bitfields = map[string][]bitCheck{\n")
	for _, n := range names {
		sd := e.structs[n]
		if len(sd.Bitfields) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\t%q: {", sd.VkName)
		for i, f := range sd.Bitfields {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "{%q, %d}", f.Name, f.Width)
		}
		b.WriteString("},\n")
	}
	b.WriteString(`}

type bitCheck struct {
	Name  string
	Width int
}

// pattern is the value C stores in a struct's k'th bitfield: distinct per
// field, and with bits set across the whole width.
func pattern(k, width int) uint64 {
	v := 0x5A5A5A5A5A5A5A5A ^ uint64(k)*0x9E3779B97F4A7C15
	if width < 64 {
		v &= 1<<width - 1
	}
	return v
}

func TestStructLayout(t *testing.T) {
	cs := cLayouts()
	if len(cs) == 0 {
		t.Fatal("no C layouts; the headers produced nothing")
	}
	checked, skipped, fields := 0, 0, 0
	for _, c := range cs {
		if c.Size == 0 {
			// The installed SDK predates this struct. The registry is vendored
			// and routinely newer, so this is expected.
			skipped++
			continue
		}
		checked++
		gt, ok := goTypes[c.Name]
		if !ok {
			t.Errorf("%s: no Go type", c.Name)
			continue
		}
		if gt.Size() != c.Size {
			t.Errorf("%s: Go size %d, C size %d", c.Name, gt.Size(), c.Size)
		}
		if uintptr(gt.Align()) != c.Align {
			t.Errorf("%s: Go alignment %d, C alignment %d", c.Name, gt.Align(), c.Align)
		}
		if c.Offsets == nil {
			continue
		}
		if gt.NumField() != len(c.Offsets) {
			t.Errorf("%s: Go has %d fields, C has %d", c.Name, gt.NumField(), len(c.Offsets))
			continue
		}
		for i, off := range c.Offsets {
			if off == ^uintptr(0) {
				continue
			}
			fields++
			if got := gt.Field(i).Offset; got != off {
				t.Errorf("%s.%s: Go offset %d, C offset %d", c.Name, gt.Field(i).Name, got, off)
			}
		}
	}
	t.Logf("checked %d structs and unions (%d member offsets); skipped %d this SDK does not declare",
		checked, fields, skipped)
	if checked == 0 {
		t.Fatal("nothing was checked; the headers declared none of these")
	}
}

// TestBitfields has C write every bitfield and reads it back through the
// accessors, then writes the same values through the setters and compares the
// bytes. C decides how bitfields are packed; this is what holds the generated
// shifts to it.
func TestBitfields(t *testing.T) {
	declared := map[string]bool{}
	for _, c := range cLayouts() {
		declared[c.Name] = c.Size != 0
	}
	u64 := reflect.TypeFor[uint64]()
	for name, fields := range bitfields {
		if !declared[name] {
			continue
		}
		gt := goTypes[name]
		fromC, mine := reflect.New(gt), reflect.New(gt)
		cBits(name, fromC.UnsafePointer())
		for k, f := range fields {
			want := pattern(k, f.Width)
			if got := fromC.MethodByName(f.Name).Call(nil)[0].Convert(u64).Uint(); got != want {
				t.Errorf("%s.%s: read %#x, C wrote %#x", name, f.Name, got, want)
			}
			set := mine.MethodByName("Set" + f.Name)
			set.Call([]reflect.Value{reflect.ValueOf(want).Convert(set.Type().In(0))})
		}
		c := unsafe.Slice((*byte)(fromC.UnsafePointer()), gt.Size())
		g := unsafe.Slice((*byte)(mine.UnsafePointer()), gt.Size())
		if !bytes.Equal(c, g) {
			t.Errorf("%s: setters wrote % x, C wrote % x", name, g, c)
		}
	}
}
`)
	return b.Bytes()
}

// goLiteral rewrites a C constant expression as a Go one. The registry writes
// what a C header would: unsigned and long-long suffixes, a float f, and ~0 for
// "no such index".
func goLiteral(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	if strings.HasPrefix(v, "\"") {
		return v, true
	}
	if m := complementRE.FindStringSubmatch(v); m != nil {
		bits := 32
		if strings.Contains(strings.ToUpper(m[2]), "LL") {
			bits = 64
		}
		base, err := strconv.ParseUint(m[1], 0, 64)
		if err != nil {
			return "", false
		}
		var val uint64
		if bits == 64 {
			val = ^base
		} else {
			val = uint64(^uint32(base))
		}
		if m[3] != "" {
			d, err := strconv.ParseUint(strings.TrimSpace(m[3]), 0, 64)
			if err != nil {
				return "", false
			}
			val -= d
		}
		if bits == 64 {
			return fmt.Sprintf("0x%016X", val), true
		}
		return fmt.Sprintf("0x%08X", val), true
	}
	if strings.ContainsAny(v, ".eE") && !strings.HasPrefix(strings.ToLower(v), "0x") {
		return strings.TrimRight(v, "fF"), true
	}
	v = strings.TrimRight(v, "uUlL")
	if _, err := strconv.ParseInt(v, 0, 64); err == nil {
		return v, true
	}
	if _, err := strconv.ParseUint(v, 0, 64); err == nil {
		return v, true
	}
	return "", false
}

var complementRE = regexp.MustCompile(`^\(?~(0[xX][0-9a-fA-F]+|\d+)([uUlL]*)(?:\s*-\s*(\d+))?\)?$`)
