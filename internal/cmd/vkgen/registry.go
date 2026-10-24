package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The subset of vk.xml this generator reads.
type registry struct {
	Platforms struct {
		Platform []struct {
			Name    string `xml:"name,attr"`
			Protect string `xml:"protect,attr"`
		} `xml:"platform"`
	} `xml:"platforms"`
	Types struct {
		Type []xtype `xml:"type"`
	} `xml:"types"`
	Enums    []xenums `xml:"enums"`
	Commands struct {
		Command []xcommand `xml:"command"`
	} `xml:"commands"`
	Feature    []xfeature `xml:"feature"`
	Extensions struct {
		Extension []xextension `xml:"extension"`
	} `xml:"extensions"`
}

type xtype struct {
	Category string `xml:"category,attr"`
	NameAttr string `xml:"name,attr"`
	Alias    string `xml:"alias,attr"`
	Requires string `xml:"requires,attr"`
	// BitValues names the FlagBits type of a 64-bit bitmask, which the registry
	// links through this attribute rather than requires.
	BitValues string    `xml:"bitvalues,attr"`
	API       string    `xml:"api,attr"`
	NameElem  string    `xml:"name"`
	Member    []xmember `xml:"member"`
	InnerXML  string    `xml:",innerxml"`
}

// name is the type's own name, which the registry puts in an attribute for a
// struct and in a <name> child for a handle or a function pointer.
//
// It comes out of the raw XML rather than a struct tag because encoding/xml
// will not fill both a `name,attr` and a `name` element field on the same
// struct: one silently wins, and the funcpointers came out unnamed.
func (t xtype) name() string {
	if t.NameAttr != "" {
		return t.NameAttr
	}
	i := strings.Index(t.InnerXML, "<name>")
	if i < 0 {
		return ""
	}
	rest := t.InnerXML[i+len("<name>"):]
	before, _, ok := strings.Cut(rest, "</name>")
	if !ok {
		return ""
	}
	return strings.TrimSpace(before)
}

// xmember is one struct or union field. The C type is split across character
// data and a <type> element, with the identifier in <name> and any array bound
// after it, so the text has to be kept in order rather than collected.
type xmember struct {
	API      string `xml:"api,attr"`
	InnerXML string `xml:",innerxml"`

	ctype string // "const void*"
	mname string // "pNext"
	array string // "[VK_UUID_SIZE]" or "[4]" or ""
	bits  int    // bitfield width, 0 for none
}

func (m *xmember) parse() error {
	pre, name, post, err := splitDecl(m.InnerXML)
	if err != nil {
		return err
	}
	m.ctype, m.mname = pre, name
	post = strings.TrimSpace(post)
	if i := strings.Index(post, ":"); i >= 0 { // bitfield
		n, err := strconv.Atoi(strings.TrimSpace(post[i+1:]))
		if err != nil {
			return fmt.Errorf("bitfield width in %q: %w", post, err)
		}
		m.bits = n
		post = strings.TrimSpace(post[:i])
	}
	m.array = post
	return nil
}

type xenums struct {
	Name     string  `xml:"name,attr"`
	Type     string  `xml:"type,attr"`
	Bitwidth string  `xml:"bitwidth,attr"`
	Enum     []xenum `xml:"enum"`
}

type xenum struct {
	Name    string `xml:"name,attr"`
	Value   string `xml:"value,attr"`
	Bitpos  string `xml:"bitpos,attr"`
	Offset  string `xml:"offset,attr"`
	Extends string `xml:"extends,attr"`
	ExtNum  string `xml:"extnumber,attr"`
	Dir     string `xml:"dir,attr"`
	Alias   string `xml:"alias,attr"`
	API     string `xml:"api,attr"`
	Protect string `xml:"protect,attr"`
}

type xcommand struct {
	API   string   `xml:"api,attr"`
	Name  string   `xml:"name,attr"` // set only on an alias entry
	Alias string   `xml:"alias,attr"`
	Proto xproto   `xml:"proto"`
	Param []xparam `xml:"param"`
}

type xproto struct {
	InnerXML string `xml:",innerxml"`
	ctype    string
	pname    string
}

type xparam struct {
	API      string `xml:"api,attr"`
	InnerXML string `xml:",innerxml"`
	ctype    string
	pname    string
	array    string
}

type xfeature struct {
	API     string     `xml:"api,attr"`
	Name    string     `xml:"name,attr"`
	Number  string     `xml:"number,attr"`
	Require []xrequire `xml:"require"`
}

type xextension struct {
	Name      string     `xml:"name,attr"`
	Number    string     `xml:"number,attr"`
	Supported string     `xml:"supported,attr"`
	Platform  string     `xml:"platform,attr"`
	Require   []xrequire `xml:"require"`
}

type xrequire struct {
	API     string  `xml:"api,attr"`
	Depends string  `xml:"depends,attr"`
	Enum    []xenum `xml:"enum"`
	Type    []struct {
		Name string `xml:"name,attr"`
	} `xml:"type"`
	Command []struct {
		Name string `xml:"name,attr"`
	} `xml:"command"`
}

// splitDecl walks mixed content and returns the text before <name>, the name,
// and the text after. The pointer stars and array bounds live in that text,
// which is why it cannot simply be collected.
func splitDecl(inner string) (pre, name, post string, err error) {
	d := xml.NewDecoder(strings.NewReader(inner))
	var before, after strings.Builder
	seen := false
	for {
		tok, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", "", "", e
		}
		switch t := tok.(type) {
		case xml.CharData:
			if seen {
				after.WriteString(string(t))
			} else {
				before.WriteString(string(t))
			}
		case xml.StartElement:
			var body string
			if e := d.DecodeElement(&body, &t); e != nil {
				return "", "", "", e
			}
			switch t.Name.Local {
			case "name":
				name, seen = strings.TrimSpace(body), true
			case "comment":
				// dropped
			default: // type, enum (an array bound)
				if seen {
					after.WriteString(body)
				} else {
					before.WriteString(body)
				}
			}
		}
	}
	return strings.Join(strings.Fields(before.String()), " "), name, after.String(), nil
}

func loadRegistry(path string) (*registry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r registry
	if err := xml.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	for i := range r.Types.Type {
		t := &r.Types.Type[i]
		for j := range t.Member {
			if err := t.Member[j].parse(); err != nil {
				return nil, fmt.Errorf("%s member %d: %w", t.name(), j, err)
			}
		}
	}
	for i := range r.Commands.Command {
		c := &r.Commands.Command[i]
		if c.Alias != "" {
			continue
		}
		pre, name, _, err := splitDecl(c.Proto.InnerXML)
		if err != nil {
			return nil, fmt.Errorf("command %d proto: %w", i, err)
		}
		c.Proto.ctype, c.Proto.pname = pre, name
		for j := range c.Param {
			pre, name, post, err := splitDecl(c.Param[j].InnerXML)
			if err != nil {
				return nil, fmt.Errorf("%s param %d: %w", c.Proto.pname, j, err)
			}
			c.Param[j].ctype, c.Param[j].pname = pre, name
			c.Param[j].array = strings.TrimSpace(post)
		}
	}
	return &r, nil
}

// enumValue computes what an <enum> entry is worth. The extension form is the
// one worth spelling out: an extension's enumerants live in a block reserved
// for its number, at 1000000000 + (extnumber-1)*1000 + offset, negated when
// dir is "-" because a VkResult error is negative.
func enumValue(e xenum, extNumber int) (string, bool) {
	switch {
	case e.Value != "":
		return e.Value, true
	case e.Bitpos != "":
		n, err := strconv.Atoi(e.Bitpos)
		if err != nil {
			return "", false
		}
		if n >= 32 {
			return fmt.Sprintf("0x%016X", uint64(1)<<uint(n)), true
		}
		return fmt.Sprintf("0x%08X", uint64(1)<<uint(n)), true
	case e.Offset != "":
		off, err := strconv.Atoi(e.Offset)
		if err != nil {
			return "", false
		}
		num := extNumber
		if e.ExtNum != "" {
			if n, err := strconv.Atoi(e.ExtNum); err == nil {
				num = n
			}
		}
		if num == 0 {
			return "", false
		}
		v := 1000000000 + (num-1)*1000 + off
		if e.Dir == "-" {
			v = -v
		}
		return strconv.Itoa(v), true
	}
	return "", false
}

// featureSet is the resolved API surface for one api/version/extension choice.
type featureSet struct {
	commands []xcommand
	// enumConsts is every enumerant, grouped by the type it extends ("" for a
	// free-standing constant).
	enumConsts map[string][]constDef
	// typesUsed is every type the selected features name, transitively closed
	// over struct members and command parameters.
	typesUsed map[string]bool
	// typeOrder is every required type in the order the features and
	// extensions name it, core first, so a guard inherited through a struct
	// member comes from the earliest declaration.
	typeOrder []string
	// typeGuard is the C condition under which the installed headers declare a
	// type: the feature macro for a core type, and for an extension type its
	// macro plus the spec version the registry describes. The layout check
	// compiles against whatever SDK is installed, and a vendored registry is
	// routinely newer than any released one, so the check has to skip what the
	// headers have never heard of rather than fail.
	typeGuard map[string]string
	versionOf map[string]string
	// extensions is every extension generated in, in registry order.
	extensions []string
}

// excludedPlatforms are window systems whose types this binding cannot map
// faithfully. GGP's GgpStreamDescriptor and GgpFrameToken are defined only in
// Stadia's unpublished SDK, so their sizes cannot be checked; SCI belongs to
// Vulkan SC. Generating a guess would be a struct whose layout nobody can
// verify, which is worse than not having it.
var excludedPlatforms = map[string]bool{"ggp": true, "sci": true}

type constDef struct {
	Name   string
	Value  string
	Origin string
	Alias  string
}

func (r *registry) resolve(api string, maxVersion float64, wantExt []string, allExt bool) (*featureSet, error) {
	fs := &featureSet{
		enumConsts: map[string][]constDef{},
		typesUsed:  map[string]bool{},
		typeGuard:  map[string]string{},
		versionOf:  map[string]string{},
	}
	useType := func(name, guard string) {
		if !fs.typesUsed[name] {
			fs.typesUsed[name] = true
			fs.typeOrder = append(fs.typeOrder, name)
		}
		if _, ok := fs.typeGuard[name]; !ok {
			fs.typeGuard[name] = guard
		}
	}
	cmdByName := map[string]xcommand{}
	aliasCmd := map[string]string{}
	for _, c := range r.Commands.Command {
		if c.Alias != "" {
			aliasCmd[c.Name] = c.Alias
			continue
		}
		if c.API != "" && !apiMatches(c.API, api) {
			continue
		}
		cmdByName[c.Proto.pname] = c
	}

	wantCmd := map[string]string{}
	seenConst := map[string]bool{}
	addEnums := func(rq xrequire, origin string, extNum int) {
		for _, e := range rq.Enum {
			if e.API != "" && !apiMatches(e.API, api) {
				continue
			}
			key := e.Extends + "/" + e.Name
			if seenConst[key] {
				continue
			}
			if e.Alias != "" {
				seenConst[key] = true
				fs.enumConsts[e.Extends] = append(fs.enumConsts[e.Extends],
					constDef{Name: e.Name, Alias: e.Alias, Origin: origin})
				continue
			}
			v, ok := enumValue(e, extNum)
			if !ok {
				// A bare <enum name="..."/> inside a require is a reference to
				// a constant declared in an <enums> block, not a definition.
				// Marking it seen here would hide the real one.
				continue
			}
			seenConst[key] = true
			fs.enumConsts[e.Extends] = append(fs.enumConsts[e.Extends],
				constDef{Name: e.Name, Value: v, Origin: origin})
		}
	}

	feats := make([]xfeature, 0, len(r.Feature))
	for _, f := range r.Feature {
		if !apiMatches(f.API, api) {
			continue
		}
		n, err := strconv.ParseFloat(f.Number, 64)
		if err != nil || n > maxVersion {
			continue
		}
		feats = append(feats, f)
	}
	sort.SliceStable(feats, func(i, j int) bool {
		a, _ := strconv.ParseFloat(feats[i].Number, 64)
		b, _ := strconv.ParseFloat(feats[j].Number, 64)
		return a < b
	})
	for _, f := range feats {
		for _, rq := range f.Require {
			if rq.API != "" && !apiMatches(rq.API, api) {
				continue
			}
			// Guarded by version rather than by feature name: the registry
			// splits a version into features (VK_BASE_VERSION_1_3,
			// VK_GRAPHICS_VERSION_1_3) that a header does not define.
			for _, t := range rq.Type {
				useType(t.Name, "defined(VK_VERSION_"+strings.ReplaceAll(f.Number, ".", "_")+")")
			}
			for _, c := range rq.Command {
				if _, ok := wantCmd[c.Name]; !ok {
					wantCmd[c.Name] = "Vulkan " + f.Number
				}
			}
			addEnums(rq, "Vulkan "+f.Number, 0)
		}
	}

	want := map[string]bool{}
	for _, e := range wantExt {
		want[e] = true
	}
	for _, ex := range r.Extensions.Extension {
		if !allExt && !want[ex.Name] {
			continue
		}
		if !supportedBy(ex.Supported, api) || excludedPlatforms[ex.Platform] {
			continue
		}
		fs.extensions = append(fs.extensions, ex.Name)
		num, _ := strconv.Atoi(ex.Number)
		guard := extGuard(ex)
		for _, rq := range ex.Require {
			if rq.API != "" && !apiMatches(rq.API, api) {
				continue
			}
			for _, t := range rq.Type {
				useType(t.Name, guard)
			}
			for _, c := range rq.Command {
				if _, ok := wantCmd[c.Name]; !ok {
					wantCmd[c.Name] = ex.Name
				}
			}
			addEnums(rq, ex.Name, num)
		}
	}

	// Free-standing enum blocks (VK_API_VERSION and friends) plus every value
	// declared inside a <enums> block rather than by an extension.
	for _, blk := range r.Enums {
		for _, e := range blk.Enum {
			if e.API != "" && !apiMatches(e.API, api) {
				continue
			}
			group := blk.Name
			if group == "API Constants" {
				group = ""
			}
			key := group + "/" + e.Name
			if seenConst[key] {
				continue
			}
			seenConst[key] = true
			if e.Alias != "" {
				fs.enumConsts[group] = append(fs.enumConsts[group], constDef{Name: e.Name, Alias: e.Alias})
				continue
			}
			if v, ok := enumValue(e, 0); ok {
				fs.enumConsts[group] = append(fs.enumConsts[group], constDef{Name: e.Name, Value: v})
			}
		}
	}

	names := make([]string, 0, len(wantCmd))
	for n := range wantCmd {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c, ok := cmdByName[n]
		if !ok {
			if target, isAlias := aliasCmd[n]; isAlias {
				if c, ok = cmdByName[target]; ok {
					c.Proto.pname = n // emit under the alias name
				}
			}
		}
		if !ok {
			continue // an extension command whose api filter excluded it
		}
		fs.versionOf[n] = wantCmd[n]
		fs.commands = append(fs.commands, c)
	}
	return fs, nil
}

// extGuard is the condition under which a header declares everything the
// registry says this extension has. A struct added in a later revision bumps
// the spec version, so a header with the extension at an older revision is
// skipped rather than failing to compile.
func extGuard(ex xextension) string {
	g := "defined(" + ex.Name + ")"
	for _, rq := range ex.Require {
		for _, e := range rq.Enum {
			if strings.HasSuffix(e.Name, "_SPEC_VERSION") && e.Value != "" {
				return fmt.Sprintf("%s && defined(%s) && %s >= %s", g, e.Name, e.Name, e.Value)
			}
		}
	}
	return g
}

func apiMatches(attr, api string) bool {
	return slices.Contains(strings.Split(attr, ","), api)
}

func supportedBy(attr, api string) bool {
	if attr == "disabled" {
		return false
	}
	return apiMatches(attr, api)
}

// headerVersion is the registry's VK_HEADER_VERSION for an api, or 0 when it
// has none. Vulkan and Vulkan SC each declare their own.
func (r *registry) headerVersion(api string) int {
	for _, t := range r.Types.Type {
		if t.Category != "define" || t.name() != "VK_HEADER_VERSION" {
			continue
		}
		if t.API != "" && !apiMatches(t.API, api) {
			continue
		}
		_, after, _ := strings.Cut(t.InnerXML, "</name>")
		n, _ := strconv.Atoi(strings.TrimSpace(after))
		return n
	}
	return 0
}
