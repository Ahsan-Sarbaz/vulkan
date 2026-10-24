package main

import "testing"

func testConverter() *converter {
	return &converter{
		kind:     map[string]string{"VkGeometryInstanceFlagsKHR": "bitmask", "VkFormat": "enum"},
		alias:    map[string]string{},
		external: map[string]bool{"xcb_connection_t": true, "StdVideoH264SequenceParameterSet": true},
		basetype: map[string]string{"CAMetalLayer": ""},
		stdEnums: map[string]bool{},
	}
}

// What crosses for each kind of type a platform or the video headers own: an
// integer keeps its width, a pointer typedef is a pointer, and an opaque struct
// is only ever its address.
func TestGoTypePlatform(t *testing.T) {
	c := testConverter()
	c.kind["CAMetalLayer"] = "basetype"
	for _, tc := range []struct{ ctype, want string }{
		{"xcb_window_t", "uint32"},
		{"Window", "uint64"},
		{"DWORD", "uint32"},
		{"HANDLE", "unsafe.Pointer"},
		{"HANDLE*", "*unsafe.Pointer"},
		{"LPCWSTR", "*uint16"},
		{"const xcb_connection_t*", "unsafe.Pointer"},
		{"const CAMetalLayer*", "unsafe.Pointer"},
		{"const StdVideoH264SequenceParameterSet*", "unsafe.Pointer"},
		{"StdVideoH264ProfileIdc", "StdVideoH264ProfileIdc"},
		{"const char* const*", "**byte"},
		{"void**", "*unsafe.Pointer"},
	} {
		got, err := c.goType(tc.ctype)
		if err != nil || got != tc.want {
			t.Errorf("goType(%q) = %q, %v; want %q", tc.ctype, got, err, tc.want)
		}
	}
	for _, bad := range []string{"xcb_connection_t", "StdVideoH264SequenceParameterSet", "NoSuchType"} {
		if got, err := c.goType(bad); err == nil {
			t.Errorf("goType(%q) = %q; want an error", bad, got)
		}
	}
}

func TestClassifyBasetype(t *testing.T) {
	for _, tc := range []struct{ inner, want string }{
		{"typedef <type>uint32_t</type> <name>VkBool32</name>;", "uint32"},
		{"typedef <type>uint64_t</type> <name>VkDeviceAddress</name>;", "uint64"},
		{"typedef <type>void</type>* <name>VkRemoteAddressNV</name>;", "unsafe.Pointer"},
		{"struct <name>ANativeWindow</name>;", ""},
		{"#ifdef __OBJC__\n@class CAMetalLayer;\n#else\ntypedef void <name>CAMetalLayer</name>;\n#endif", ""},
		{"#ifdef __OBJC__\n@protocol MTLDevice;\ntypedef __unsafe_unretained id&lt;MTLDevice&gt; MTLDevice_id;\n#else\ntypedef void* <name>MTLDevice_id</name>;\n#endif", "unsafe.Pointer"},
		{"typedef struct __IOSurface* <name>IOSurfaceRef</name>;", "unsafe.Pointer"},
	} {
		got, err := classifyBasetype(xtype{InnerXML: tc.inner})
		if err != nil || got != tc.want {
			t.Errorf("classifyBasetype(%q) = %q, %v; want %q", tc.inner, got, err, tc.want)
		}
	}
}

// VkAccelerationStructureInstanceKHR's bitfields: two 32-bit units, each a
// 24-bit field below an 8-bit one. A third field may not join the first unit
// even though its type matches, because it would not fit.
func TestPackBitfields(t *testing.T) {
	e := &emitter{conv: testConverter(), sizes: map[string]layout{},
		named: map[string]layout{"GeometryInstanceFlagsKHR": {4, 4}}}
	sd := &structDef{VkName: "VkAccelerationStructureInstanceKHR"}
	run := []xmember{
		{ctype: "uint32_t", mname: "instanceCustomIndex", bits: 24},
		{ctype: "uint32_t", mname: "mask", bits: 8},
		{ctype: "uint32_t", mname: "instanceShaderBindingTableRecordOffset", bits: 24},
		{ctype: "VkGeometryInstanceFlagsKHR", mname: "flags", bits: 8},
	}
	if err := e.packBitfields(sd, run); err != nil {
		t.Fatal(err)
	}
	if len(sd.Fields) != 2 {
		t.Fatalf("%d storage units, want 2", len(sd.Fields))
	}
	want := []struct {
		storage      string
		shift, width int
		typ          string
	}{
		{"bits0", 0, 24, "uint32"},
		{"bits0", 24, 8, "uint32"},
		{"bits1", 0, 24, "uint32"},
		{"bits1", 24, 8, "GeometryInstanceFlagsKHR"},
	}
	for i, w := range want {
		f := sd.Bitfields[i]
		if f.Storage != w.storage || f.Shift != w.shift || f.Width != w.width || f.Type != w.typ {
			t.Errorf("%s: %s<<%d width %d %s; want %s<<%d width %d %s",
				f.CName, f.Storage, f.Shift, f.Width, f.Type, w.storage, w.shift, w.width, w.typ)
		}
	}

	signed := []xmember{{ctype: "VkFormat", mname: "format", bits: 8}}
	if err := e.packBitfields(&structDef{}, signed); err == nil {
		t.Error("an enum bitfield was accepted; its sign extension is not modelled")
	}
}

func TestGoLiteral(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"(~0U)", "0xFFFFFFFF"},
		{"(~0ULL)", "0xFFFFFFFFFFFFFFFF"},
		{"(~1U)", "0xFFFFFFFE"},
		{"(~0U-2)", "0xFFFFFFFD"},
		{"1000.0F", "1000.0"},
		{"0XE", "0XE"},
		{"256", "256"},
		{`"VK_KHR_surface"`, `"VK_KHR_surface"`},
	} {
		if got, ok := goLiteral(tc.in); !ok || got != tc.want {
			t.Errorf("goLiteral(%q) = %q, %v; want %q", tc.in, got, ok, tc.want)
		}
	}
}
