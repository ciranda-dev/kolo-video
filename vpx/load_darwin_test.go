package vpx

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ciranda-dev/kolo-video/vpx/internal/native"
)

const childEnv = "KOLOVIDEO_VPX_CHILD"

const entitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>com.apple.security.cs.disable-library-validation</key><true/></dict></plist>
`

type childResult struct {
	Engine, Err, NativeErr, Source string
	TypedNativeErr                 bool
}

// TestHardenedChild is the program TestHardenedBundle runs inside the fake
// application; it does nothing otherwise.
func TestHardenedChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip("helper of TestHardenedBundle")
	}
	setLoaded(t, nil)
	cfg := LoadConfig{CacheDir: os.Getenv(childEnv + "_CACHE")}
	if os.Getenv(childEnv+"_FORCE") != "" {
		cfg.Engine = EngineNative
	}
	var res childResult
	rt, err := Load(cfg)
	if err != nil {
		res.Err = err.Error()
		res.Engine = "none"
		res.TypedNativeErr = errors.Is(err, ErrUnavailable)
	} else {
		res.Engine = rt.Engine.String()
		if rt.NativeErr != nil {
			res.NativeErr = rt.NativeErr.Error()
			var ne *native.Error
			res.TypedNativeErr = errors.As(rt.NativeErr, &ne)
		}
		roundTrip(t, VP8)
		roundTrip(t, VP9)
		if rt.Engine == EngineNative {
			l, err := native.Load(native.Config{CacheDir: cfg.CacheDir})
			if err != nil {
				t.Fatal(err)
			}
			res.Source = string(l.Source)
		}
	}
	b, _ := json.Marshal(res)
	os.Stdout.WriteString("KVRESULT " + string(b) + "\n")
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

// TestHardenedBundle runs a hardened-runtime, ad-hoc signed copy of this test
// binary inside a fake application bundle and loads the engine through Load.
func TestHardenedBundle(t *testing.T) {
	if os.Getenv(childEnv) != "" {
		t.Skip("running as the helper")
	}
	if !hasNative() {
		t.Skip("no native engine in this build")
	}
	if _, err := exec.LookPath("codesign"); err != nil {
		t.Skip("codesign: ", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	libPath, err := WriteLibrary(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lib, err := os.ReadFile(libPath)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		entitled    bool
		bundleLib   string // "other" signed with another identity, "unsigned", "" absent
		force       bool
		wantEngine  string
		wantSource  string
		wantInError []string
	}{
		{"refused without the entitlement", false, "other", false, "portable", "",
			[]string{"disable-library-validation", "Contents/Frameworks", "kolovideo/vpx: "}},
		{"forced native is refused", false, "other", true, "none", "",
			[]string{"libvpx unavailable", "disable-library-validation"}},
		{"entitlement loads the bundle library", true, "other", false, "native", "bundle", nil},
		{"entitlement without a bundle library uses the cache", true, "", false, "native", "cache", nil},
		{"unsigned bundle library is refused before dlopen", true, "unsigned", false, "native", "cache", nil},
		{"no entitlement and no bundle library", false, "", false, "portable", "",
			[]string{"disable-library-validation", "kolovideo/vpx: "}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			contents := filepath.Join(dir, "Kolo Test.app", "Contents")
			for _, d := range []string{"MacOS", "Frameworks"} {
				if err := os.MkdirAll(filepath.Join(contents, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			bin := filepath.Join(contents, "MacOS", "vpx.test")
			if err := os.WriteFile(bin, self, 0o755); err != nil {
				t.Fatal(err)
			}
			sign := []string{"-s", "-", "-f", "-o", "runtime", "-i", "dev.example.kolotest"}
			if c.entitled {
				plist := filepath.Join(dir, "entitlements.plist")
				if err := os.WriteFile(plist, []byte(entitlements), 0o644); err != nil {
					t.Fatal(err)
				}
				sign = append(sign, "--entitlements", plist)
			}
			run(t, "codesign", append(sign, bin)...)
			if c.bundleLib != "" {
				p := filepath.Join(contents, "Frameworks", "libkolovpx.dylib")
				if err := os.WriteFile(p, lib, 0o755); err != nil {
					t.Fatal(err)
				}
				if c.bundleLib == "unsigned" {
					run(t, "codesign", "--remove-signature", p)
				} else {
					run(t, "codesign", "-s", "-", "-f", "-i", "dev.example.otherlibrary", p)
				}
			}
			cmd := exec.Command(bin, "-test.run=^TestHardenedChild$", "-test.count=1")
			cmd.Env = append(os.Environ(), childEnv+"=1", childEnv+"_CACHE="+filepath.Join(dir, "cache"),
				"TMPDIR="+dir, "HOME="+dir, "XDG_CACHE_HOME="+dir)
			if c.force {
				cmd.Env = append(cmd.Env, childEnv+"_FORCE=1")
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child: %v\n%s", err, out)
			}
			var res childResult
			found := false
			for _, line := range strings.Split(string(out), "\n") {
				if s, ok := strings.CutPrefix(line, "KVRESULT "); ok {
					found = true
					if err := json.Unmarshal([]byte(s), &res); err != nil {
						t.Fatal(err)
					}
				}
			}
			if !found {
				t.Fatalf("no result in the child output:\n%s", out)
			}
			t.Logf("%+v", res)
			if res.Engine != c.wantEngine || res.Source != c.wantSource {
				t.Fatalf("engine %q source %q, want %q %q", res.Engine, res.Source, c.wantEngine, c.wantSource)
			}
			if c.wantEngine == "native" && (res.Err != "" || res.NativeErr != "") {
				t.Fatalf("native load reported %+v", res)
			}
			if c.wantEngine != "native" && !res.TypedNativeErr {
				t.Fatalf("the native error is not typed: %+v", res)
			}
			for _, w := range c.wantInError {
				if !strings.Contains(res.Err+res.NativeErr, w) {
					t.Errorf("error lacks %q: %q", w, res.Err+res.NativeErr)
				}
			}
			if c.wantEngine == "portable" && strings.Count(res.NativeErr, "\n") > 0 {
				t.Errorf("error spans several lines: %q", res.NativeErr)
			}
		})
	}
}
