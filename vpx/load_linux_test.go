package vpx

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ciranda-dev/kolo-video/vpx/internal/native"
)

//go:embed testdata/kvglibc-*
var glibcFixtures embed.FS

// TestHostileLinux runs Load in a container set up by the caller to make the
// native engine hard to load: a seccomp profile without memfd_create, a noexec
// TMPDIR, no /proc, an old glibc. It is skipped unless KOLOVIDEO_HOSTILE says
// what must happen: "native" (Load picks the native engine) or "portable" (Load
// falls back with a typed native error and EngineNative returns
// ErrUnavailable). KOLOVIDEO_HOSTILE_LIB=glibc loads a library that needs glibc
// 2.34 through LoadConfig.Library; KOLOVIDEO_HOSTILE_ERR is a text the native
// error must contain.
func TestHostileLinux(t *testing.T) {
	want := os.Getenv("KOLOVIDEO_HOSTILE")
	if want == "" {
		t.Skip("set KOLOVIDEO_HOSTILE=native or portable in a container")
	}
	cfg := LoadConfig{CacheDir: filepath.Join(t.TempDir(), "cache")}
	if os.Getenv("KOLOVIDEO_HOSTILE_LIB") == "glibc" {
		b, err := glibcFixtures.ReadFile(fmt.Sprintf("testdata/kvglibc-linux-%s.so", runtime.GOARCH))
		if err != nil {
			t.Skip(err)
		}
		cfg.Library = filepath.Join(t.TempDir(), "libkolovpx.so")
		if err := os.WriteFile(cfg.Library, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	setLoaded(t, nil)
	rt, err := Load(cfg)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Logf("engine=%v version=%s native error: %v", rt.Engine, rt.Version, rt.NativeErr)
	switch want {
	case "native":
		if rt.Engine != EngineNative || rt.NativeErr != nil {
			t.Fatalf("%+v", rt)
		}
	case "portable":
		var ne *native.Error
		if rt.Engine != EnginePortable || !errors.As(rt.NativeErr, &ne) || !strings.HasPrefix(rt.NativeErr.Error(), "kolovideo/vpx: ") {
			t.Fatalf("%+v", rt)
		}
		if s := os.Getenv("KOLOVIDEO_HOSTILE_ERR"); !strings.Contains(rt.NativeErr.Error(), s) {
			t.Fatalf("native error %q lacks %q", rt.NativeErr, s)
		}
		setLoaded(t, nil)
		cfg.Engine = EngineNative
		if _, err := Load(cfg); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("forced native: %v", err)
		}
	default:
		t.Fatalf("KOLOVIDEO_HOSTILE=%q", want)
	}
	setLoaded(t, nil)
	cfg.Engine = EngineAuto
	if _, err := Load(cfg); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, VP8)
	roundTrip(t, VP9)
}
