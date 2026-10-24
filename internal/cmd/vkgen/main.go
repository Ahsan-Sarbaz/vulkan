// Command vkgen generates this module's Vulkan binding from the Khronos registry.
//
// The registry is the authority on what the API is, so generating from it is what
// makes the binding complete rather than whatever a hand-written one needed.
//
// One property of Vulkan decides the design. No command takes
// a struct by value or more than fifteen parameters, and the few that take a
// float set it on the ccall.Frame in its own register sequence, so all of them
// reach the zero-allocation call path and none needs purego's reflected one.
// What is hard instead is the structs: over a thousand of them, each of which
// has to match C's layout exactly, where a mistake is silent corruption rather
// than a compile error. That is what the generated layout test is for.
//
// It generates the whole API by default -- core 1.4 and every supported
// extension -- because what a consumer may want to call is not the generator's
// business, and a binding that stops at what one renderer happens to use is a
// binding a consumer has to fork. What a program requires is a separate
// question, and it asks MissingBelow. Only the types that API reaches are
// emitted: a disabled or Vulkan SC-only extension's structs are in the registry
// but in no header, so nothing could call them and nothing could check them.
//
//	go run ./internal/cmd/vkgen
//	go run ./internal/cmd/vkgen -fetch
//	go test -tags vklayout .
//
// A narrower binding, such as -version 1.1 -all-extensions=false or -api
// vulkansc, is for generating into another package with -out: this module's own
// hand-written tests call extension entry points the full binding has.
package main

import (
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const registryURL = "https://raw.githubusercontent.com/KhronosGroup/Vulkan-Docs/main/xml/vk.xml"

// defaultExtensions is what -all-extensions=false generates beyond core: what a
// windowed renderer needs, a surface, a swapchain, the platform surface
// creations, and the debug messenger the validation layers report through.
var defaultExtensions = []string{
	"VK_KHR_surface",
	"VK_KHR_swapchain",
	"VK_KHR_get_physical_device_properties2",
	"VK_KHR_portability_subset",
	"VK_KHR_portability_enumeration",
	"VK_EXT_metal_surface",
	"VK_MVK_macos_surface",
	"VK_KHR_win32_surface",
	"VK_KHR_xlib_surface",
	"VK_KHR_xcb_surface",
	"VK_KHR_wayland_surface",
	"VK_EXT_debug_utils",
	"VK_EXT_debug_report",
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("vkgen: ")

	var (
		xmlPath = flag.String("xml", "", "registry to read (default: vk.xml beside this command)")
		out     = flag.String("out", "", "package directory to write (default: the module root)")
		pkg     = flag.String("pkg", "vulkan", "package name")
		api     = flag.String("api", "vulkan", "registry api: vulkan or vulkansc")
		version = flag.String("version", "1.4", "highest core version to include")
		extra   = flag.String("extensions", "", "comma-separated extensions to add to the default set")
		allExt  = flag.Bool("all-extensions", true, "include every supported extension")
		fetch   = flag.Bool("fetch", false, "download a fresh vk.xml before generating")
	)
	flag.Parse()

	root, err := moduleRoot()
	if err != nil {
		log.Fatal(err)
	}
	if *xmlPath == "" {
		*xmlPath = filepath.Join(root, "internal", "cmd", "vkgen", "vk.xml")
	}
	if *out == "" {
		*out = root
	}
	if *fetch {
		if err := download(registryURL, *xmlPath); err != nil {
			log.Fatalf("fetch registry: %v", err)
		}
		log.Printf("fetched %s", *xmlPath)
	}
	maxVer, err := strconv.ParseFloat(*version, 64)
	if err != nil {
		log.Fatalf("bad -version %q: %v", *version, err)
	}
	exts := append([]string(nil), defaultExtensions...)
	for e := range strings.SplitSeq(*extra, ",") {
		if e = strings.TrimSpace(e); e != "" {
			exts = append(exts, e)
		}
	}

	reg, err := loadRegistry(*xmlPath)
	if err != nil {
		log.Fatalf("read registry: %v", err)
	}

	conv := &converter{
		kind:     map[string]string{},
		alias:    map[string]string{},
		external: map[string]bool{},
		basetype: map[string]string{},
		stdEnums: map[string]bool{},
	}
	for _, t := range reg.Types.Type {
		n := t.name()
		if n == "" || (t.API != "" && !apiMatches(t.API, *api)) {
			continue
		}
		if t.Category == "" {
			// Named but undefined: it comes from a platform header.
			conv.external[n] = true
			continue
		}
		if t.Alias != "" {
			conv.alias[n] = t.Alias
			if conv.kind[n] == "" {
				conv.kind[n] = t.Category
			}
			continue
		}
		conv.kind[n] = t.Category
		if t.Category == "basetype" {
			g, err := classifyBasetype(t)
			if err != nil {
				log.Fatal(err)
			}
			conv.basetype[n] = g
		}
	}

	fs, err := reg.resolve(*api, maxVer, exts, *allExt)
	if err != nil {
		log.Fatalf("resolve: %v", err)
	}

	e := &emitter{conv: conv, reg: reg, fs: fs, pkg: *pkg, api: *api,
		consts: map[string]int64{}, bitwidth64: map[string]bool{}}
	for _, blk := range reg.Enums {
		if blk.Bitwidth == "64" {
			e.bitwidth64[blk.Name] = true
		}
	}
	for _, d := range fs.enumConsts[""] {
		v := strings.TrimSuffix(strings.TrimSuffix(d.Value, "ULL"), "U")
		if n, err := strconv.ParseInt(v, 0, 64); err == nil {
			e.consts[d.Name] = n
		}
	}
	e.computeNeed()
	if err := e.initLayouts(); err != nil {
		log.Fatalf("layouts: %v", err)
	}
	if err := e.buildStructs(); err != nil {
		log.Fatalf("structs: %v", err)
	}
	e.computeGuards()

	fns := make([]vkFn, 0, len(fs.commands))
	var byLevel [3]int
	for _, c := range fs.commands {
		f, err := e.resolveCmd(c)
		if err != nil {
			log.Fatal(err)
		}
		if len(f.Params) > 15 {
			log.Fatalf("%s takes %d arguments, past purego's limit of 15", f.CName, len(f.Params))
		}
		if n := f.floatParams(); n > 8 {
			log.Fatalf("%s takes %d floats, past the 8 float registers ccall fills", f.CName, n)
		}
		if !f.floatsLast() {
			log.Fatalf("%s takes a float before an integer, which ccall's Windows path cannot place", f.CName)
		}
		switch f.Level {
		case "global":
			byLevel[0]++
		case "instance":
			byLevel[1]++
		default:
			byLevel[2]++
		}
		fns = append(fns, f)
	}

	names := make([]string, 0, len(e.structs))
	for n := range e.structs {
		names = append(names, n)
	}
	sort.Strings(names)

	types, err := e.emitTypes()
	if err != nil {
		log.Fatalf("types: %v", err)
	}
	layoutC, err := e.emitLayoutCgo(names)
	if err != nil {
		log.Fatalf("layout check: %v", err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	write(filepath.Join(*out, "types.go"), types)
	write(filepath.Join(*out, "consts.go"), e.emitConsts())
	write(filepath.Join(*out, "commands.go"), e.emitCommands(fns))
	write(filepath.Join(*out, "procs.go"), e.emitProcs(fns))
	write(filepath.Join(*out, "generated.go"), e.emitMeta(*version, len(fns), len(e.structs)))
	write(filepath.Join(*out, "layout_cgo.go"), layoutC)
	write(filepath.Join(*out, "layout_test.go"), e.emitLayoutTest(names))

	log.Printf("%s %s: %d commands (%d global, %d instance, %d device), %d structs and unions, %d extensions",
		*api, *version, len(fns), byLevel[0], byLevel[1], byLevel[2], len(e.structs), len(fs.extensions))
}

func write(path string, src []byte) {
	formatted, err := format.Source(src)
	if err != nil {
		_ = os.WriteFile(path+".broken", src, 0o644)
		log.Fatalf("format %s: %v (unformatted source at %s.broken)", path, err, path)
	}
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		log.Fatal(err)
	}
}

func download(url, path string) error {
	c := &http.Client{Timeout: 2 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
