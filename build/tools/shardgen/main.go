// Command shardgen splits the single-package Go file that wasm2go writes into several packages,
// so that no compiler run has to hold the whole translated module.
//
// The wasm call graph has no cycles. Functions are ordered callees first and cut into shards of
// equal source size; shard i only calls shards below it, so each shard is a package that imports
// the lower ones. Module state moves to package core and every function becomes
// F<n>(m *core.State, ...). The root package keeps Module, New and the exported methods.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type function struct {
	name   string // fn12, fn12 plain, or an X export
	method bool
	decl   *ast.FuncDecl
	start  int
	end    int
	body   string
	calls  []string
	shard  int
}

var (
	callRe  = regexp.MustCompile(`\bfn(\d+)\b`)
	fieldRe = regexp.MustCompile(`\bm\.(t0|elements|memory|_wasi_snapshot_preview1|g\d+)\b`)
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("shardgen: ")
	in := flag.String("in", "", "single-file Go source written by wasm2go")
	out := flag.String("out", "", "directory of the root package")
	pkg := flag.String("pkg", "portable", "name of the root package")
	base := flag.String("base", "", "import path of the directory that holds core and the shards")
	sub := flag.String("sub", "wasm", "directory of core and the shards, relative to -out")
	shards := flag.Int("shards", 4, "number of shard packages")
	header := flag.String("header", "", "first line of every generated file")
	flag.Parse()
	if *in == "" || *out == "" || *base == "" || *header == "" || *shards < 1 {
		flag.Usage()
		os.Exit(2)
	}
	src, err := os.ReadFile(*in)
	if err != nil {
		log.Fatal(err)
	}
	g := parse(src)
	g.order(*shards)
	files := g.emit(*pkg, *base, *shards, *header)
	for name, data := range files {
		data, err := format.Source(data)
		if err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		p := filepath.Join(*out, name)
		if strings.Contains(name, "/") {
			p = filepath.Join(*out, *sub, strings.TrimPrefix(name, "wasm/"))
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

type gen struct {
	src     []byte
	fset    *token.FileSet
	imports map[string]string // local name -> import path
	funcs   map[string]*function
	names   []string // functions in call order, callees first
	helpers []string // decl texts copied into every package
	data    []string
	aliases []string // Memory and import interface declarations
	fields  []string // State fields, in order
	newText string
	exports []*function
}

func (g *gen) text(a, b token.Pos) string {
	return string(g.src[g.fset.Position(a).Offset:g.fset.Position(b).Offset])
}

func (g *gen) declText(doc *ast.CommentGroup, n ast.Node) string {
	pos := n.Pos()
	if doc != nil {
		pos = doc.Pos()
	}
	return g.text(pos, n.End())
}

func parse(src []byte) *gen {
	g := &gen{src: src, fset: token.NewFileSet(), imports: map[string]string{}, funcs: map[string]*function{}}
	f, err := parser.ParseFile(g.fset, "in.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		log.Fatal(err)
	}
	for _, im := range f.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		g.imports[filepath.Base(p)] = p
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			g.genDecl(d)
		case *ast.FuncDecl:
			g.funcDecl(d)
		}
	}
	if g.newText == "" || len(g.fields) == 0 {
		log.Fatal("no New or Module in the input")
	}
	for _, fn := range g.funcs {
		for _, m := range callRe.FindAllStringSubmatch(fn.body, -1) {
			if n := "fn" + m[1]; n != fn.name {
				fn.calls = append(fn.calls, n)
			}
		}
		sort.Strings(fn.calls)
		for _, c := range fn.calls {
			if g.funcs[c] == nil {
				log.Fatalf("%s calls unknown %s", fn.name, c)
			}
		}
	}
	return g
}

func (g *gen) genDecl(d *ast.GenDecl) {
	if d.Tok == token.IMPORT {
		return
	}
	text := g.declText(d.Doc, d)
	if d.Tok == token.TYPE {
		ts := d.Specs[0].(*ast.TypeSpec)
		switch ts.Name.Name {
		case "Module":
			for _, fl := range ts.Type.(*ast.StructType).Fields.List {
				ft := g.text(fl.Type.Pos(), fl.Type.End())
				if len(fl.Names) == 0 {
					if ft != "wasmMemory" {
						log.Fatalf("unexpected embedded field %s", ft)
					}
					g.fields = append(g.fields, "Memory []byte")
					continue
				}
				for _, n := range fl.Names {
					name := n.Name
					switch {
					case name == "t0":
						name = "T0"
					case name == "elements":
						name = "Elements"
					case name == "_wasi_snapshot_preview1":
						name = "Wasi"
					case regexp.MustCompile(`^g\d+$`).MatchString(name):
						name = "G" + name[1:]
					default:
						log.Fatalf("unexpected Module field %s", name)
					}
					g.fields = append(g.fields, name+" "+ft)
				}
			}
		case "wasmMemory":
		case "Memory", "Xwasi_snapshot_preview1":
			g.aliases = append(g.aliases, text)
		default:
			g.helpers = append(g.helpers, text)
		}
		return
	}
	for _, s := range d.Specs {
		if vs, ok := s.(*ast.ValueSpec); ok && len(vs.Names) == 1 && strings.HasPrefix(vs.Names[0].Name, "data") && d.Tok == token.CONST {
			g.data = append(g.data, text)
			return
		}
	}
	g.helpers = append(g.helpers, text)
}

func (g *gen) funcDecl(d *ast.FuncDecl) {
	name := d.Name.Name
	text := g.declText(d.Doc, d)
	recv := ""
	if d.Recv != nil {
		recv = g.text(d.Recv.List[0].Type.Pos(), d.Recv.List[0].Type.End())
	}
	switch {
	case name == "New" && recv == "":
		g.newText = text
	case recv == "*wasmMemory":
	case recv == "*Module" && name == "Xmemory":
	case recv == "*Module" && (strings.HasPrefix(name, "fn") || strings.HasPrefix(name, "X")):
		g.addFunc(name, true, d)
	case recv == "" && regexp.MustCompile(`^fn\d+$`).MatchString(name):
		g.addFunc(name, false, d)
	case recv == "":
		g.helpers = append(g.helpers, text)
	default:
		log.Fatalf("unexpected method %s on %s", name, recv)
	}
}

func (g *gen) addFunc(name string, method bool, d *ast.FuncDecl) {
	fn := &function{name: name, method: method, decl: d, start: g.fset.Position(d.Pos()).Offset, end: g.fset.Position(d.End()).Offset, shard: -1}
	fn.body = string(g.src[fn.start:fn.end])
	g.funcs[name] = fn
}

// order sorts the functions callees first and cuts them into shards of equal size.
func (g *gen) order(k int) {
	names := make([]string, 0, len(g.funcs))
	total := 0
	for n, fn := range g.funcs {
		names = append(names, n)
		total += len(fn.body)
	}
	sort.Strings(names)
	state := map[string]int{}
	var visit func(string)
	visit = func(n string) {
		switch state[n] {
		case 1:
			log.Fatalf("call cycle through %s", n)
		case 2:
			return
		}
		state[n] = 1
		for _, c := range g.funcs[n].calls {
			visit(c)
		}
		state[n] = 2
		g.names = append(g.names, n)
	}
	for _, n := range names {
		visit(n)
	}
	budget := total/k + 1
	cur, shard := 0, 0
	for _, n := range g.names {
		fn := g.funcs[n]
		if cur+len(fn.body) > budget && cur > 0 && shard < k-1 {
			shard++
			cur = 0
		}
		cur += len(fn.body)
		fn.shard = shard
	}
}

func fname(fn *function) string {
	if strings.HasPrefix(fn.name, "fn") {
		return "F" + fn.name[2:]
	}
	return fn.name
}

// rewrite converts a function body from the Module method form to the shard form.
func (g *gen) rewrite(text string, own int, used map[int]bool) string {
	re := regexp.MustCompile(`(\bm\.)?\bfn(\d+)\b(\()?(\))?`)
	text = re.ReplaceAllStringFunc(text, func(s string) string {
		m := re.FindStringSubmatch(s)
		fn := g.funcs["fn"+m[2]]
		pre := ""
		if fn.shard != own {
			pre = fmt.Sprintf("s%d.", fn.shard)
			used[fn.shard] = true
		}
		call := pre + "F" + m[2]
		if m[3] == "" {
			return call
		}
		args := "("
		if fn.method {
			if m[1] == "" {
				log.Fatalf("method fn%s called without m", m[2])
			}
			args += "m"
			if m[4] == "" {
				args += ", "
			}
		} else if m[1] != "" {
			log.Fatalf("plain fn%s called with m", m[2])
		}
		return call + args + m[4]
	})
	return g.fields1(text)
}

func (g *gen) fields1(text string) string {
	return fieldRe.ReplaceAllStringFunc(text, func(s string) string {
		n := s[2:]
		switch {
		case n == "t0":
			return "m.T0"
		case n == "elements":
			return "m.Elements"
		case n == "memory":
			return "m.Memory"
		case n == "_wasi_snapshot_preview1":
			return "m.Wasi"
		}
		return "m.G" + n[1:]
	})
}

func (g *gen) params(fn *function) (list string, names []string, results string) {
	t := fn.decl.Type
	if t.Params != nil && len(t.Params.List) > 0 {
		list = g.text(t.Params.List[0].Pos(), t.Params.List[len(t.Params.List)-1].End())
		for _, f := range t.Params.List {
			for _, n := range f.Names {
				names = append(names, n.Name)
			}
		}
	}
	if t.Results != nil {
		results = " " + g.text(t.Results.Pos(), t.Results.End())
	}
	return
}

func (g *gen) signature(fn *function) string {
	list, _, results := g.params(fn)
	if fn.method {
		if list != "" {
			list = ", " + list
		}
		list = "m *core.State" + list
	}
	return "func " + fname(fn) + "(" + list + ")" + results
}

func (g *gen) functionText(fn *function, own int, used map[int]bool) string {
	d := fn.decl
	i := g.fset.Position(d.Body.Lbrace).Offset - fn.start
	doc := ""
	if d.Doc != nil {
		doc = g.text(d.Doc.Pos(), d.Pos())
	}
	return doc + g.signature(fn) + " " + g.rewrite(fn.body[i:], own, used)
}

func (g *gen) importsFor(text string, extra ...string) string {
	var paths []string
	for name, p := range g.imports {
		if regexp.MustCompile(`\b` + name + `\.`).MatchString(text) {
			paths = append(paths, strconv.Quote(p))
		}
	}
	for _, e := range extra {
		paths = append(paths, e)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return ""
	}
	return "import (\n" + strings.Join(paths, "\n") + "\n)\n\n"
}

func (g *gen) emit(pkg, base string, k int, header string) map[string][]byte {
	files := map[string][]byte{}
	helpers := strings.Join(g.helpers, "\n\n") + "\n"
	hdr := func(name string) string { return header + "\n\npackage " + name + "\n\n" }
	coreImp := strconv.Quote(base + "/core")

	var buf bytes.Buffer
	for s := 0; s < k; s++ {
		used := map[int]bool{}
		var code strings.Builder
		var fns []*function
		for _, n := range g.names {
			if g.funcs[n].shard == s {
				fns = append(fns, g.funcs[n])
			}
		}
		sort.Slice(fns, func(i, j int) bool { return fns[i].start < fns[j].start })
		for _, fn := range fns {
			code.WriteString(g.functionText(fn, s, used))
			code.WriteString("\n\n")
		}
		extra := []string{coreImp}
		for t := range used {
			extra = append(extra, fmt.Sprintf("s%d %s", t, strconv.Quote(fmt.Sprintf("%s/s%d", base, t))))
		}
		if s > 0 && !used[s-1] {
			extra = append(extra, fmt.Sprintf("_ %s", strconv.Quote(fmt.Sprintf("%s/s%d", base, s-1))))
		}
		name := fmt.Sprintf("wasm/s%d", s)
		buf.Reset()
		buf.WriteString(hdr(fmt.Sprintf("s%d", s)))
		buf.WriteString(g.importsFor(code.String(), extra...))
		buf.WriteString(code.String())
		files[name+"/code.go"] = append([]byte(nil), buf.Bytes()...)
		buf.Reset()
		buf.WriteString(hdr(fmt.Sprintf("s%d", s)))
		buf.WriteString(g.importsFor(helpers))
		buf.WriteString(helpers)
		files[name+"/helpers.go"] = append([]byte(nil), buf.Bytes()...)
	}

	state := "type State struct {\n" + strings.Join(g.fields, "\n") + "\n}\n\n"
	state += "// Slice returns the module memory.\nfunc (m *State) Slice() *[]byte { return &m.Memory }\n\n"
	state += "// Grow grows the module memory by delta pages up to max pages.\nfunc (m *State) Grow(delta, max int64) int64 { return memory_grow(&m.Memory, delta, max) }\n\n"
	coreText := strings.Join(g.aliases, "\n\n") + "\n\n// State is the state of one module instance: memory, table, globals and imports.\n" + state
	buf.Reset()
	buf.WriteString(hdr("core"))
	buf.WriteString(g.importsFor(coreText))
	buf.WriteString(coreText)
	files["wasm/core/state.go"] = append([]byte(nil), buf.Bytes()...)
	buf.Reset()
	buf.WriteString(hdr("core"))
	buf.WriteString(g.importsFor(helpers))
	buf.WriteString(helpers)
	files["wasm/core/helpers.go"] = append([]byte(nil), buf.Bytes()...)

	// root package
	var root strings.Builder
	root.WriteString("type Module struct {\n\tcore.State\n}\n\ntype Xwasi_snapshot_preview1 = core.Xwasi_snapshot_preview1\n\ntype Memory = core.Memory\n\n")
	root.WriteString("func (m *Module) Xmemory() Memory { return &m.State }\n\n")
	var exps []*function
	for _, fn := range g.funcs {
		if strings.HasPrefix(fn.name, "X") {
			exps = append(exps, fn)
		}
	}
	sort.Slice(exps, func(i, j int) bool { return exps[i].start < exps[j].start })
	usedRoot := map[int]bool{}
	for _, fn := range exps {
		list, names, results := g.params(fn)
		usedRoot[fn.shard] = true
		call := fmt.Sprintf("s%d.%s(&m.State", fn.shard, fn.name)
		if len(names) > 0 {
			call += ", " + strings.Join(names, ", ")
		}
		call += ")"
		ret := "return "
		if results == "" {
			ret = ""
		}
		root.WriteString(fmt.Sprintf("func (m *Module) %s(%s)%s { %s%s }\n\n", fn.name, list, results, ret, call))
	}
	newText := g.newText
	closure := regexp.MustCompile(`\b(m\.)?fn(\d+)\b`)
	newText = closure.ReplaceAllStringFunc(newText, func(s string) string {
		m := closure.FindStringSubmatch(s)
		fn := g.funcs["fn"+m[2]]
		usedRoot[fn.shard] = true
		ref := fmt.Sprintf("s%d.F%s", fn.shard, m[2])
		if !fn.method {
			return ref
		}
		list, names, results := g.params(fn)
		args := "&m.State"
		if len(names) > 0 {
			args += ", " + strings.Join(names, ", ")
		}
		ret := "return "
		if results == "" {
			ret = ""
		}
		return fmt.Sprintf("func(%s)%s { %s%s(%s) }", list, results, ret, ref, args)
	})
	newText = g.fields1(newText)
	extra := []string{coreImp}
	for t := range usedRoot {
		extra = append(extra, fmt.Sprintf("s%d %s", t, strconv.Quote(fmt.Sprintf("%s/s%d", base, t))))
	}
	rootText := root.String() + newText + "\n\n" + strings.Join(g.data, "\n\n") + "\n\n" + helpers
	buf.Reset()
	buf.WriteString(hdr(pkg))
	buf.WriteString(g.importsFor(rootText, extra...))
	buf.WriteString(rootText)
	files["libvpx.go"] = append([]byte(nil), buf.Bytes()...)
	return files
}
