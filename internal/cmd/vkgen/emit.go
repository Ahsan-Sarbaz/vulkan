package main

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type emitter struct {
	conv   *converter
	reg    *registry
	fs     *featureSet
	pkg    string
	api    string
	consts map[string]int64 // API constants, for array bounds
	// bitwidth64 names the enum types whose <enums> block declares 64 bits.
	bitwidth64 map[string]bool
	// need is every registry type the selected API reaches, by Vk name: what
	// the features and extensions require, what the commands take, and what
	// those contain. Nothing outside it is emitted.
	need map[string]bool
	// flagAlias maps a FlagBits enum to the one bitmask built from it, which
	// it is emitted as an alias of, so a bit constant is a Flags value.
	flagAlias map[string]string
	structs   map[string]*structDef // by Go name
	sizes     map[string]layout
	named     map[string]layout // scalar named types, by Go name
	// guard is the C condition the layout check compiles each struct under.
	guard map[string]string
}

type layout struct{ size, align int }

type structDef struct {
	VkName    string
	GoName    string
	IsUnion   bool
	Fields    []fieldDef
	Bitfields []bitField
}

type fieldDef struct {
	Name    string
	Type    string // Go type, already including any array prefix
	Comment string
	CName   string // the C member, "" for a bitfield storage unit
}

// bitField is one C bitfield, stored in a storage unit field and reached
// through an accessor pair, since Go has no bitfields.
type bitField struct {
	Name    string // exported accessor name
	CName   string
	Type    string // the member's Go type
	Storage string // the unit field it lives in
	Unit    string // that field's Go type
	Shift   int
	Width   int
}

// computeNeed closes the selected API over the types it reaches. Emitting the
// whole registry instead put types from disabled and Vulkan SC-only extensions
// in the package, which no header declares and nothing can call.
func (e *emitter) computeNeed() {
	byName := map[string]*xtype{}
	fbOwner := map[string][]string{}
	for i := range e.reg.Types.Type {
		t := &e.reg.Types.Type[i]
		n := t.name()
		if n == "" || (t.API != "" && !apiMatches(t.API, e.api)) {
			continue
		}
		if _, ok := byName[n]; !ok {
			byName[n] = t
		}
		if t.Category == "bitmask" && t.Alias == "" {
			for _, fb := range []string{t.Requires, t.BitValues} {
				if fb != "" {
					fbOwner[fb] = append(fbOwner[fb], n)
				}
			}
		}
	}
	e.need = map[string]bool{}
	var add func(n string)
	add = func(n string) {
		if n == "" || e.need[n] {
			return
		}
		e.need[n] = true
		t := byName[n]
		if t == nil {
			return
		}
		if t.Alias != "" {
			add(t.Alias)
		}
		switch t.Category {
		case "struct", "union":
			for _, m := range t.Member {
				if m.API == "" || apiMatches(m.API, e.api) {
					b, _ := baseName(m.ctype)
					add(b)
				}
			}
		case "bitmask":
			add(t.Requires)
			add(t.BitValues)
		case "enum":
			for _, b := range fbOwner[n] {
				add(b)
			}
		}
	}
	for _, n := range e.fs.typeOrder {
		add(n)
	}
	for _, c := range e.fs.commands {
		b, _ := baseName(c.Proto.ctype)
		add(b)
		for _, p := range c.Param {
			if p.API == "" || apiMatches(p.API, e.api) {
				b, _ := baseName(p.ctype)
				add(b)
			}
		}
	}

	// A FlagBits enum is the same type as its Flags in every way that matters to
	// a caller: the bits are OR-ed into it. An alias says so and removes the
	// conversion at every use. One shared by two bitmasks keeps its own type.
	e.flagAlias = map[string]string{}
	for fb, owners := range fbOwner {
		if len(owners) == 1 && e.need[fb] && e.conv.kind[fb] == "enum" {
			e.flagAlias[fb] = owners[0]
		}
	}
}

// initLayouts records the size of every named scalar type, so a union is sized
// from what its members really are. A type with no known layout is an error:
// guessing eight bytes is how a union of VkBool32s came out too big.
func (e *emitter) initLayouts() error {
	e.sizes = map[string]layout{}
	e.named = map[string]layout{}
	for _, t := range e.reg.Types.Type {
		n := t.name()
		if n == "" || t.Alias != "" {
			continue
		}
		var l layout
		switch t.Category {
		case "handle", "funcpointer":
			l = layout{8, 8}
		case "enum":
			l = layout{4, 4}
			if e.bitwidth64[n] {
				l = layout{8, 8}
			}
		case "bitmask":
			l = layout{4, 4}
			if strings.Contains(t.InnerXML, "VkFlags64") {
				l = layout{8, 8}
			}
		case "basetype":
			g := e.conv.basetype[n]
			if g == "" {
				continue // opaque: only ever behind a pointer
			}
			var err error
			if l, err = e.layoutOf(g); err != nil {
				return err
			}
		default:
			continue
		}
		e.named[trimVk(n)] = l
	}
	return nil
}

// layoutOf is the size and alignment of a Go type, computed the way the
// compiler will. Needed for unions, whose storage this has to size, and for
// bitfield storage units.
func (e *emitter) layoutOf(t string) (layout, error) {
	if l, ok := e.sizes[t]; ok {
		return l, nil
	}
	if strings.HasPrefix(t, "[") {
		i := strings.Index(t, "]")
		n, err := strconv.Atoi(t[1:i])
		if err != nil {
			return layout{}, fmt.Errorf("array type %q: %w", t, err)
		}
		el, err := e.layoutOf(t[i+1:])
		if err != nil {
			return layout{}, err
		}
		l := layout{size: el.size * n, align: el.align}
		e.sizes[t] = l
		return l, nil
	}
	if strings.HasPrefix(t, "*") || t == "unsafe.Pointer" || t == "uintptr" {
		return layout{8, 8}, nil
	}
	switch t {
	case "int8", "uint8", "byte", "bool":
		return layout{1, 1}, nil
	case "int16", "uint16":
		return layout{2, 2}, nil
	case "int32", "uint32", "float32":
		return layout{4, 4}, nil
	case "int64", "uint64", "float64", "int", "uint":
		return layout{8, 8}, nil
	}
	if l, ok := e.named[t]; ok {
		return l, nil
	}
	if stdEnum.MatchString(t) {
		return layout{4, 4}, nil
	}
	if sd, ok := e.structs[t]; ok {
		size, align := 0, 1
		for _, f := range sd.Fields {
			fl, err := e.layoutOf(f.Type)
			if err != nil {
				return layout{}, fmt.Errorf("%s.%s: %w", sd.VkName, f.Name, err)
			}
			if sd.IsUnion {
				// A union is its largest member, not the sum.
				size = max(size, fl.size)
			} else {
				size = (size+fl.align-1)/fl.align*fl.align + fl.size
			}
			align = max(align, fl.align)
		}
		l := layout{(size + align - 1) / align * align, align}
		e.sizes[t] = l
		return l, nil
	}
	return layout{}, fmt.Errorf("no layout known for %q", t)
}

// emitTypes writes every type declaration: the handles, the scalar typedefs,
// the enum and bitmask types, the function-pointer placeholders, the aliases,
// and the structs and unions whose layout has to match C exactly.
func (e *emitter) emitTypes() ([]byte, error) {
	var b bytes.Buffer
	header(&b, e.pkg)
	b.WriteString("import \"unsafe\"\n\nvar _ unsafe.Pointer\n\n")

	emitted := map[string]bool{}
	block := func(doc string, lines []string) {
		if len(lines) == 0 {
			return
		}
		sort.Strings(lines)
		b.WriteString(doc + "\ntype (\n")
		for _, l := range lines {
			b.WriteString(l)
		}
		b.WriteString(")\n\n")
	}
	var handles, scalars, enums, fps []string
	for _, t := range e.reg.Types.Type {
		n := t.name()
		if n == "" || t.Alias != "" || !e.need[n] || emitted[n] {
			continue
		}
		if t.API != "" && !apiMatches(t.API, e.api) {
			continue
		}
		switch t.Category {
		case "handle":
			handles = append(handles, fmt.Sprintf("\t%s uintptr\n", trimVk(n)))
		case "basetype":
			g := e.conv.basetype[n]
			if g == "" {
				continue
			}
			scalars = append(scalars, fmt.Sprintf("\t%s %s\n", trimVk(n), g))
		case "enum":
			switch {
			case e.flagAlias[n] != "":
				enums = append(enums, fmt.Sprintf("\t%s = %s\n", trimVk(n), trimVk(e.flagAlias[n])))
			case e.bitwidth64[n]:
				enums = append(enums, fmt.Sprintf("\t%s uint64\n", trimVk(n)))
			default:
				enums = append(enums, fmt.Sprintf("\t%s int32\n", trimVk(n)))
			}
		case "bitmask":
			g := "uint32"
			if strings.Contains(t.InnerXML, "VkFlags64") {
				g = "uint64"
			}
			enums = append(enums, fmt.Sprintf("\t%s %s\n", trimVk(n), g))
		case "funcpointer":
			fps = append(fps, fmt.Sprintf("\t%s uintptr\n", trimVk(n)))
		default:
			continue
		}
		emitted[n] = true
	}
	var std []string
	for n := range e.conv.stdEnums {
		std = append(std, fmt.Sprintf("\t%s int32\n", n))
	}

	block("// Handles. A dispatchable handle is a pointer and a non-dispatchable one is\n"+
		"// a 64-bit value; on the architectures this binding supports both are eight\n"+
		"// bytes, which the layout test checks rather than assumes.", handles)
	block("// Scalar typedefs the registry defines.", scalars)
	block("// Enumeration and bitmask types. A FlagBits type is an alias of the Flags type\n"+
		"// built from it, so its constants combine into a Flags value directly.", enums)
	block("// Video-codec enums that Vulkan structs embed by value. They are declared in\n"+
		"// the video headers rather than the registry, and are four-byte C enums; their\n"+
		"// values are in the Vulkan Video specification.", std)
	block("// Callback types. A Go function reaches one through purego.NewCallback.", fps)

	// Structs and unions.
	names := make([]string, 0, len(e.structs))
	for n := range e.structs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sd := e.structs[n]
		emitted[sd.VkName] = true
		if sd.IsUnion {
			if err := e.emitUnion(&b, sd); err != nil {
				return nil, err
			}
			continue
		}
		fmt.Fprintf(&b, "// %s is %s.\ntype %s struct {\n", sd.GoName, sd.VkName, sd.GoName)
		for _, f := range sd.Fields {
			if f.Comment != "" {
				fmt.Fprintf(&b, "\t// %s\n", f.Comment)
			}
			fmt.Fprintf(&b, "\t%s %s\n", f.Name, f.Type)
		}
		b.WriteString("}\n\n")
		e.emitBitfields(&b, sd)
	}

	// Aliases last: the registry keeps the old extension names of everything
	// promoted to core, and code written against them should still compile.
	var aliases []string
	for _, t := range e.reg.Types.Type {
		n := t.name()
		if n == "" || t.Alias == "" || !e.need[n] || emitted[n] {
			continue
		}
		target := t.Alias
		for e.conv.alias[target] != "" {
			target = e.conv.alias[target]
		}
		if !emitted[target] || trimVk(n) == trimVk(target) {
			continue
		}
		switch t.Category {
		case "handle", "enum", "bitmask", "struct", "union", "funcpointer":
			emitted[n] = true
			aliases = append(aliases, fmt.Sprintf("\t%s = %s\n", trimVk(n), trimVk(target)))
		}
	}
	block("// Aliases: the names a type had before it was promoted or renamed.", aliases)
	return b.Bytes(), nil
}

// emitUnion writes a union as storage plus typed accessors. Go has no unions,
// and the alternative -- a struct of the largest member -- would silently be
// the wrong size for any other one.
func (e *emitter) emitUnion(b *bytes.Buffer, sd *structDef) error {
	l, err := e.layoutOf(sd.GoName)
	if err != nil {
		return err
	}
	pad := map[int]string{1: "uint8", 2: "uint16", 4: "uint32", 8: "uint64"}[l.align]
	fmt.Fprintf(b, "// %s is the union %s: %d bytes, %d-byte aligned. The zero array\n"+
		"// forces the alignment without taking space; the accessors reinterpret.\n"+
		"type %s struct {\n\t_    [0]%s\n\tdata [%d]byte\n}\n\n",
		sd.GoName, sd.VkName, l.size, l.align, sd.GoName, pad, l.size)
	for _, f := range sd.Fields {
		fmt.Fprintf(b, "// %s reads the union as its %s member.\n"+
			"func (u *%s) %s() *%s { return (*%s)(unsafe.Pointer(u)) }\n\n",
			f.Name, f.CName, sd.GoName, f.Name, f.Type, f.Type)
	}
	return nil
}

// emitBitfields writes an accessor pair per bitfield. The bits are allocated
// from the low end of each unit, which is what every C compiler on the
// supported platforms does.
func (e *emitter) emitBitfields(b *bytes.Buffer, sd *structDef) {
	for _, f := range sd.Bitfields {
		mask := fmt.Sprintf("0x%X", uint64(1)<<f.Width-1)
		fmt.Fprintf(b, "// %s is the %s bitfield: %d bits from bit %d of %s.\n", f.Name, f.CName, f.Width, f.Shift, f.Storage)
		fmt.Fprintf(b, "func (s *%s) %s() %s { return %s((s.%s >> %d) & %s) }\n\n",
			sd.GoName, f.Name, f.Type, f.Type, f.Storage, f.Shift, mask)
		fmt.Fprintf(b, "// Set%s stores the low %d bits of v in the %s bitfield.\n", f.Name, f.Width, f.CName)
		fmt.Fprintf(b, "func (s *%s) Set%s(v %s) { s.%s = s.%s&^(%s<<%d) | (%s(v)&%s)<<%d }\n\n",
			sd.GoName, f.Name, f.Type, f.Storage, f.Storage, mask, f.Shift, f.Unit, mask, f.Shift)
	}
}

// buildStructs resolves every struct and union the selected API reaches.
func (e *emitter) buildStructs() error {
	e.structs = map[string]*structDef{}
	for _, t := range e.reg.Types.Type {
		if t.Category != "struct" && t.Category != "union" {
			continue
		}
		if t.Alias != "" || !e.need[t.name()] || (t.API != "" && !apiMatches(t.API, e.api)) {
			continue
		}
		sd := &structDef{VkName: t.name(), GoName: trimVk(t.name()), IsUnion: t.Category == "union"}
		var members []xmember
		for _, m := range t.Member {
			if m.API == "" || apiMatches(m.API, e.api) {
				members = append(members, m)
			}
		}
		for i := 0; i < len(members); i++ {
			m := members[i]
			if m.bits > 0 {
				j := i
				for j < len(members) && members[j].bits > 0 {
					j++
				}
				if err := e.packBitfields(sd, members[i:j]); err != nil {
					return fmt.Errorf("%s: %w", t.name(), err)
				}
				i = j - 1
				continue
			}
			gt, err := e.conv.goType(m.ctype)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", t.name(), m.mname, err)
			}
			if m.array != "" {
				dims, err := e.arrayDims(m.array)
				if err != nil {
					return fmt.Errorf("%s.%s: %w", t.name(), m.mname, err)
				}
				gt = dims + gt
			}
			sd.Fields = append(sd.Fields, fieldDef{Name: exportMember(m.mname), Type: gt, CName: m.mname})
		}
		taken := map[string]bool{}
		for _, f := range sd.Fields {
			taken[f.Name] = true
		}
		for _, f := range sd.Bitfields {
			if taken[f.Name] || taken["Set"+f.Name] {
				return fmt.Errorf("%s: bitfield accessor %s collides with a field", t.name(), f.Name)
			}
		}
		e.structs[sd.GoName] = sd
	}
	return nil
}

// packBitfields lays out a run of consecutive bitfields the way C does: each
// shares the current storage unit of its type's size while it fits, and opens
// a new one when it would straddle the boundary.
func (e *emitter) packBitfields(sd *structDef, run []xmember) error {
	var unitBits, used int
	storage, unit := "", ""
	for _, m := range run {
		gt, err := e.conv.goType(m.ctype)
		if err != nil {
			return fmt.Errorf("%s: %w", m.mname, err)
		}
		base, _ := baseName(m.ctype)
		if !strings.HasPrefix(cBase[base], "uint") && e.conv.kind[base] != "bitmask" {
			return fmt.Errorf("%s: bitfield of signed or enum type %s is not supported", m.mname, m.ctype)
		}
		l, err := e.layoutOf(gt)
		if err != nil {
			return fmt.Errorf("%s: %w", m.mname, err)
		}
		bits := l.size * 8
		if m.bits > bits {
			return fmt.Errorf("%s: %d bits in a %d-bit type", m.mname, m.bits, bits)
		}
		if storage == "" || bits != unitBits || used+m.bits > unitBits {
			unitBits, used = bits, 0
			unit = fmt.Sprintf("uint%d", bits)
			storage = fmt.Sprintf("bits%d", len(sd.Fields))
			sd.Fields = append(sd.Fields, fieldDef{Name: storage, Type: unit})
		}
		f := &sd.Fields[len(sd.Fields)-1]
		if f.Comment == "" {
			f.Comment = "bitfields, from bit 0:"
		}
		f.Comment += fmt.Sprintf(" %s:%d", m.mname, m.bits)
		sd.Bitfields = append(sd.Bitfields, bitField{
			Name: exportMember(m.mname), CName: m.mname, Type: gt,
			Storage: storage, Unit: unit, Shift: used, Width: m.bits,
		})
		used += m.bits
	}
	return nil
}

// computeGuards gives every emitted struct the condition the installed headers
// declare it under. A struct reached only as another's member is named in no
// <require>, so it inherits the guard of the first struct that contains it --
// it is declared in that header block.
func (e *emitter) computeGuards() {
	e.guard = map[string]string{}
	var queue []string
	visit := func(vk, g string) {
		if _, ok := e.guard[vk]; ok {
			return
		}
		if _, ok := e.structs[trimVk(vk)]; !ok {
			return
		}
		e.guard[vk] = g
		queue = append(queue, vk)
	}
	for _, n := range e.fs.typeOrder {
		t := n
		for e.conv.alias[t] != "" {
			t = e.conv.alias[t]
		}
		visit(t, e.fs.typeGuard[n])
	}
	for len(queue) > 0 {
		vk := queue[0]
		queue = queue[1:]
		for _, f := range e.structs[trimVk(vk)].Fields {
			if f.CName == "" {
				continue
			}
			t := strings.TrimLeft(f.Type, "*")
			if i := strings.LastIndex(t, "]"); i >= 0 {
				t = t[i+1:]
			}
			if sd, ok := e.structs[t]; ok {
				visit(sd.VkName, e.guard[vk])
			}
		}
	}
}

// arrayDims turns "[VK_UUID_SIZE]" or "[2][4]" into a Go array prefix.
func (e *emitter) arrayDims(s string) (string, error) {
	var out strings.Builder
	for part := range strings.SplitSeq(s, "[") {
		part = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(part), "]"))
		if part == "" {
			continue
		}
		if n, err := strconv.Atoi(part); err == nil {
			fmt.Fprintf(&out, "[%d]", n)
			continue
		}
		v, ok := e.consts[part]
		if !ok {
			return "", fmt.Errorf("array bound %q is not a known constant", part)
		}
		fmt.Fprintf(&out, "[%d]", v)
	}
	return out.String(), nil
}

func header(b *bytes.Buffer, pkg string) {
	fmt.Fprintf(b, "// Code generated by internal/cmd/vkgen. DO NOT EDIT.\n\npackage %s\n\n", pkg)
}
