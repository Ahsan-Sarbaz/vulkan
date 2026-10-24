package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// cBase maps a C or Vulkan scalar to the Go type with the same layout.
//
// size_t is uint because this binding is 64-bit only: purego's tier-1
// architectures are amd64 and arm64, and a non-dispatchable Vulkan handle is
// pointer-sized only there. The layout test is what would catch it otherwise.
//
// char is byte rather than int8: Vulkan's chars are UTF-8 strings, and a
// []byte is what a Go caller has for one.
var cBase = map[string]string{
	"void":     "",
	"char":     "byte",
	"int":      "int32",
	"int8_t":   "int8",
	"uint8_t":  "uint8",
	"int16_t":  "int16",
	"uint16_t": "uint16",
	"int32_t":  "int32",
	"uint32_t": "uint32",
	"int64_t":  "int64",
	"uint64_t": "uint64",
	"size_t":   "uint",
	"float":    "float32",
	"double":   "float64",
}

// platformValue are the window-system types Vulkan embeds by value, with their
// ABI on the 64-bit platforms that define them. layoutStubs declares the same
// types to C, so the layout test checks the structs around them; a size here
// that disagreed with the real platform header would be a bug in both.
//
// Every other platform type is an opaque struct reached only through a
// pointer, and needs no entry: a pointer to it is one unsafe.Pointer.
var platformValue = map[string]string{
	// X11: XID and VisualID are unsigned long, eight bytes on LP64.
	"Window":   "uint64",
	"VisualID": "uint64",
	"RROutput": "uint64",
	// XCB: fixed-width ids.
	"xcb_window_t":   "uint32",
	"xcb_visualid_t": "uint32",
	// Win32: DWORD is unsigned long, four bytes under LLP64; the handles are
	// pointers, and LPCWSTR is a UTF-16 string.
	"DWORD":     "uint32",
	"HANDLE":    "unsafe.Pointer",
	"HINSTANCE": "unsafe.Pointer",
	"HMONITOR":  "unsafe.Pointer",
	"HWND":      "unsafe.Pointer",
	"LPCWSTR":   "*uint16",
	// Fuchsia.
	"zx_handle_t": "uint32",
}

// layoutStubs declares the platform types whose real headers are not on a
// build machine, matching platformValue, so the layout test can include every
// platform's Vulkan header and check its structs.
const layoutStubs = `typedef unsigned long XID;
typedef struct _XDisplay Display;
typedef XID Window;
typedef unsigned long VisualID;
typedef XID RROutput;
typedef struct xcb_connection_t xcb_connection_t;
typedef uint32_t xcb_window_t;
typedef uint32_t xcb_visualid_t;
typedef void* HANDLE;
typedef struct HINSTANCE__* HINSTANCE;
typedef struct HMONITOR__* HMONITOR;
typedef struct HWND__* HWND;
typedef uint32_t DWORD;
typedef const uint16_t* LPCWSTR;
typedef struct _SECURITY_ATTRIBUTES SECURITY_ATTRIBUTES;
typedef uint32_t zx_handle_t;
typedef struct IDirectFB IDirectFB;
typedef struct IDirectFBSurface IDirectFBSurface;
`

// layoutHeaders are the platform headers the layout test includes after the
// stubs. Each declares its extension's structs unconditionally; vulkan.h is
// what gates them behind VK_USE_PLATFORM_*, which is why it is not used.
var layoutHeaders = []string{
	"vulkan_android.h", "vulkan_beta.h", "vulkan_directfb.h", "vulkan_fuchsia.h",
	"vulkan_ios.h", "vulkan_macos.h", "vulkan_metal.h", "vulkan_ohos.h",
	"vulkan_screen.h", "vulkan_ubm.h", "vulkan_vi.h", "vulkan_wayland.h",
	"vulkan_win32.h", "vulkan_xcb.h", "vulkan_xlib.h", "vulkan_xlib_xrandr.h",
}

// stdEnum matches the video-codec enums the registry embeds by value. They are
// declared in the video headers, not vk.xml, and are C enums: four bytes. A
// video-codec struct is only ever reached through a pointer, so any other
// by-value Std type is a registry change this generator has to be taught about.
var stdEnum = regexp.MustCompile(`^StdVideo\w*(Profile|Level|ProfileIdc|LevelIdc)$`)

// converter turns registry type names into Go types. It needs the whole type
// table because a member's type may be a handle, an enum, a bitmask or another
// struct, and each maps differently.
type converter struct {
	kind map[string]string // vk type name -> category
	// alias resolves a type declared only as an alias of another.
	alias map[string]string
	// external is every type the registry names but does not define, because it
	// belongs to a platform header: Display, HWND, ANativeWindow, ubm_device.
	// Deriving the set from the registry beats keeping a list that a new window
	// system would silently fall off.
	external map[string]bool
	// basetype is the Go type of each basetype, "" for one that is an opaque
	// struct (ANativeWindow, CAMetalLayer) reached only through a pointer.
	basetype map[string]string
	// stdEnums are the video-codec enums some struct embeds by value, which
	// types.go declares as int32 types of their own.
	stdEnums map[string]bool
}

// classifyBasetype reads a basetype's declaration. The registry writes these as
// C, sometimes with an Objective-C branch, so the plain C branch is the one
// that decides the layout.
func classifyBasetype(t xtype) (goType string, err error) {
	raw := xmlText(t.InnerXML)
	if strings.Contains(raw, "#ifdef __OBJC__") {
		_, after, _ := strings.Cut(raw, "#else")
		raw, _, _ = strings.Cut(after, "#endif")
	}
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), ";"))
	switch {
	case strings.Contains(raw, "*"):
		return "unsafe.Pointer", nil
	case strings.HasPrefix(raw, "struct "), strings.HasPrefix(raw, "typedef struct "),
		strings.HasPrefix(raw, "typedef void "):
		return "", nil
	}
	f := strings.Fields(strings.TrimPrefix(raw, "typedef"))
	if len(f) == 2 {
		if g, ok := cBase[f[0]]; ok && g != "" {
			return g, nil
		}
	}
	return "", fmt.Errorf("basetype %s: cannot map %q", t.name(), raw)
}

// xmlText is an element's character data with the tags removed and the
// entities the registry uses decoded.
func xmlText(inner string) string {
	s := tagRE.ReplaceAllString(inner, "")
	s = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&", "&quot;", `"`).Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

// baseName strips the qualifiers and pointers from a C declaration's type,
// leaving the name the registry defines.
func baseName(ctype string) (name string, stars int) {
	t := strings.ReplaceAll(ctype, "const", " ")
	t = strings.ReplaceAll(t, "struct", " ")
	stars = strings.Count(t, "*")
	t = strings.ReplaceAll(t, "*", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(t), " ")), stars
}

// goType converts a C declaration to a Go type. stars is counted rather than
// pattern-matched because the registry writes pointers as text.
func (c *converter) goType(ctype string) (string, error) {
	t, stars := baseName(ctype)
	base, opaque, err := c.baseType(t)
	if err != nil {
		return "", err
	}
	if opaque {
		// A struct this binding never sees inside: what crosses is its address.
		if stars == 0 {
			return "", fmt.Errorf("%s is opaque and cannot be held by value", t)
		}
		return strings.Repeat("*", stars-1) + "unsafe.Pointer", nil
	}
	switch {
	case stars == 0:
		if base == "" {
			return "", fmt.Errorf("void is not a value type")
		}
		return base, nil
	case base == "": // void*
		return strings.Repeat("*", stars-1) + "unsafe.Pointer", nil
	default:
		return strings.Repeat("*", stars) + base, nil
	}
}

// baseType maps a bare type name. opaque reports a C struct that is only ever
// reached through a pointer, whose contents this binding does not model.
func (c *converter) baseType(t string) (goType string, opaque bool, err error) {
	if g, ok := cBase[t]; ok {
		return g, false, nil
	}
	if g, ok := platformValue[t]; ok {
		return g, false, nil
	}
	if a, ok := c.alias[t]; ok {
		t = a
	}
	switch c.kind[t] {
	case "handle", "enum", "bitmask", "struct", "union":
		return trimVk(t), false, nil
	case "funcpointer":
		return "uintptr", false, nil
	case "basetype":
		g := c.basetype[t]
		if g == "" {
			return "", true, nil
		}
		return trimVk(t), false, nil
	}
	if stdEnum.MatchString(t) {
		c.stdEnums[t] = true
		return t, false, nil
	}
	if c.external[t] {
		return "", true, nil
	}
	return "", false, fmt.Errorf("unmapped type %q", t)
}

// trimVk strips the Vk prefix from a type and the PFN_vk prefix from a function
// pointer.
func trimVk(n string) string {
	switch {
	case strings.HasPrefix(n, "PFN_vk"):
		return "PFN" + strings.TrimPrefix(n, "PFN_vk")
	case strings.HasPrefix(n, "Vk"):
		return strings.TrimPrefix(n, "Vk")
	}
	return n
}

// trimCmd strips the vk prefix from a command.
func trimCmd(n string) string { return strings.TrimPrefix(n, "vk") }

// trimConst strips VK_ from an enumerant. The result keeps the registry's
// SCREAMING_SNAKE, which is what the spec and every Vulkan document use. A name
// that would start with a digit keeps the prefix to stay legal.
func trimConst(n string) string {
	s := strings.TrimPrefix(n, "VK_")
	if s == "" || unicode.IsDigit(rune(s[0])) {
		return "VK_" + s
	}
	return s
}

// exportMember uppercases a struct member. Vulkan's Hungarian prefixes are kept
// because they carry the ownership rule: p is a pointer, pp a pointer to one.
func exportMember(n string) string {
	if n == "" {
		return n
	}
	return strings.ToUpper(n[:1]) + n[1:]
}

// reservedParam are names a parameter cannot take: Go's keywords and
// predeclared functions, plus what a generated body declares or calls itself.
var reservedParam = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
	"len": true, "cap": true, "copy": true, "new": true, "make": true, "string": true,
	"fr": true, "r1": true, "ptr": true, "unsafe": true, "ccall": true,
}

func paramName(n string) string {
	if reservedParam[n] {
		return "x" + n
	}
	return n
}
