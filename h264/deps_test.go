package h264

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func depsTargets(t *testing.T) []string {
	t.Helper()
	if _, err := os.Stat("engine.go"); err != nil {
		t.Skip("run from the package directory: ", err)
	}
	out, err := exec.Command("go", "tool", "dist", "list").Output()
	if err != nil {
		t.Skipf("go tool dist list: %v", err)
	}
	targets := strings.Fields(string(out))
	if len(targets) < 40 {
		t.Fatalf("only %d targets listed", len(targets))
	}
	return targets
}

// TestOneEngineFilePerTarget checks that for every GOOS/GOARCH the go tool
// knows, with cgo on and off, exactly one file sets platform in an init
// function and the package has no cgo file.
func TestOneEngineFilePerTarget(t *testing.T) {
	for _, target := range depsTargets(t) {
		goos, goarch, _ := strings.Cut(target, "/")
		for _, cgo := range []bool{false, true} {
			ctx := build.Default
			ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled = goos, goarch, cgo
			pkg, err := ctx.ImportDir(".", 0)
			if err != nil {
				t.Errorf("%s cgo=%v: %v", target, cgo, err)
				continue
			}
			var setters []string
			fset := token.NewFileSet()
			for _, name := range pkg.GoFiles {
				f, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.SkipObjectResolution)
				if err != nil {
					t.Fatal(err)
				}
				if setsPlatform(f) {
					setters = append(setters, name)
				}
			}
			if len(setters) != 1 {
				t.Errorf("%s cgo=%v: platform set by %v, want one file", target, cgo, setters)
			}
			if len(pkg.CgoFiles) > 0 {
				t.Errorf("%s cgo=%v: cgo files %v", target, cgo, pkg.CgoFiles)
			}
		}
	}
}

// setsPlatform reports whether an init function of f assigns platform.
func setsPlatform(f *ast.File) bool {
	found := false
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "init" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			if a, ok := n.(*ast.AssignStmt); ok {
				for _, l := range a.Lhs {
					if id, ok := l.(*ast.Ident); ok && id.Name == "platform" {
						found = true
					}
				}
			}
			return true
		})
	}
	return found
}

// TestLinuxHasNoPurego keeps the package static on Linux: purego would make
// a CGO_ENABLED=0 binary dynamically linked to glibc, and the package has no
// Linux engine that needs it.
func TestLinuxHasNoPurego(t *testing.T) {
	for _, target := range depsTargets(t) {
		goos, goarch, _ := strings.Cut(target, "/")
		if goos != "linux" {
			continue
		}
		for _, tags := range []string{"", "kolovideo_native"} {
			cmd := exec.Command("go", "list", "-deps", "-tags", tags, "-f", "{{.ImportPath}}", ".")
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0", "GOFLAGS=")
			deps, err := cmd.Output()
			if err != nil {
				var stderr []byte
				if ee, ok := err.(*exec.ExitError); ok {
					stderr = ee.Stderr
				}
				t.Fatalf("%s tags=%q: go list (it needs the module cache, GOMODCACHE): %v\n%s", target, tags, err, stderr)
			}
			for _, d := range strings.Fields(string(deps)) {
				if strings.Contains(d, "purego") || d == "runtime/cgo" {
					t.Errorf("%s tags=%q: h264 depends on %s", target, tags, d)
				}
			}
		}
	}
}
