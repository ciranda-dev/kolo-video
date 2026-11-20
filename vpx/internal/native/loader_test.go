package native

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"
)

func mix(h, v uintptr) uintptr { return h*31 + v }

func neg(n int32) uintptr { return uintptr(int64(n)) }

func TestStubCalls(t *testing.T) {
	s := stubSource(t)
	l, err := load(s, Config{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.unload)
	if l.Version != "v0.0.0-stub" || l.ABI != abiVersion {
		t.Fatalf("version %q abi %d", l.Version, l.ABI)
	}

	var e int32
	got := l.EncNew(1, 640, -480, 2000, 4, -3, 70, &e)
	want := mix(mix(mix(mix(mix(mix(1, 640), neg(-480)), 2000), 4), neg(-3)), 70)
	if got != want || e != 7 {
		t.Fatalf("EncNew = %d err %d, want %d err 7", got, e, want)
	}

	y, u, v := []byte{10, 1}, []byte{20, 2}, []byte{30, 3}
	pts := int64(1)<<33 + 5
	r := l.Encode(got, unsafe.Pointer(&y[0]), 640, unsafe.Pointer(&u[0]), unsafe.Pointer(&v[0]), 320, pts, 3000, 1)
	if want := int32(10 + 3*20 + 5*30 + 640 + 7*320 + int32(pts) + 3000*11 + 13); r != want {
		t.Fatalf("Encode = %d, want %d", r, want)
	}

	var out [3]uintptr
	if l.NextFrame(got, &out) != 1 || out != [3]uintptr{got, 42, 1} {
		t.Fatalf("NextFrame out %v", out)
	}
	if r := l.SetBitrate(100, 50); r != 150 {
		t.Fatalf("SetBitrate = %d", r)
	}
	l.EncFree(got)

	d := l.DecNew(1, 4, &e)
	if d != mix(mix(1, 4), 5) || e != 9 {
		t.Fatalf("DecNew = %d err %d", d, e)
	}
	data := []byte{1, 2, 3}
	var dout [8]uintptr
	if n := l.Decode(d, unsafe.Pointer(&data[0]), 3, &dout); n != 3 {
		t.Fatalf("Decode = %d", n)
	}
	for i := range dout {
		if want := d + uintptr(data[i%3]) + uintptr(i); dout[i] != want {
			t.Fatalf("Decode out[%d] = %d, want %d", i, dout[i], want)
		}
	}
	l.DecFree(d)
}

func TestLoadSource(t *testing.T) {
	s := stubSource(t)
	dir := t.TempDir()
	l, err := load(s, Config{CacheDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.unload)
	want := SourceCache
	if runtime.GOOS == "linux" {
		want = SourceMemfd
	}
	if l.Source != want {
		t.Fatalf("source %q, want %q", l.Source, want)
	}
	if want == SourceMemfd && l.Path != "" {
		t.Fatalf("memfd path %q", l.Path)
	}
	if want == SourceCache && !strings.HasPrefix(l.Path, realPath(t, dir)) {
		t.Fatalf("cache path %q outside %q", l.Path, dir)
	}
}

func TestLibraryOverride(t *testing.T) {
	s := stubSource(t)
	p := filepath.Join(t.TempDir(), libName())
	if err := os.WriteFile(p, s.data, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "never")
	l, err := load(nil, Config{Library: p, CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.unload)
	if l.Source != SourceLibrary || l.Path != p {
		t.Fatalf("source %q path %q", l.Source, l.Path)
	}
	if _, err := os.Stat(cache); err == nil {
		t.Fatal("cache directory created for a Library override")
	}
}

func TestLibraryMissing(t *testing.T) {
	stubSource(t)
	p := filepath.Join(t.TempDir(), "missing-"+libName())
	_, err := load(nil, Config{Library: p})
	var e *Error
	if !errors.As(err, &e) || len(e.Attempts) != 1 || e.Attempts[0].Source != SourceLibrary {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasPrefix(err.Error(), "kolovideo/vpx: ") || !strings.Contains(err.Error(), Target()) {
		t.Fatalf("message %q", err)
	}
}

func TestABIMismatch(t *testing.T) {
	s := stubSource(t)
	old := abiVersion
	abiVersion = old + 1
	defer func() { abiVersion = old }()
	_, err := load(s, Config{CacheDir: t.TempDir()})
	if !errors.Is(err, ErrABI) || !strings.Contains(err.Error(), "library reports 1, want 2") {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingSymbol(t *testing.T) {
	stubSource(t)
	lib := "libm.so.6"
	switch runtime.GOOS {
	case "darwin":
		lib = "/usr/lib/libSystem.B.dylib"
	case "windows":
		lib = filepath.Join(os.Getenv("SystemRoot"), "System32", "kernel32.dll")
	}
	h, err := dlOpen(lib)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bind(h, new(Lib)); !errors.Is(err, ErrABI) || !strings.Contains(err.Error(), "missing symbol kv_") {
		t.Fatalf("err = %v", err)
	}
}

func TestIntegrityMismatch(t *testing.T) {
	s := stubSource(t)
	s.sum[0] ^= 1
	dir := filepath.Join(t.TempDir(), "cache")
	_, err := load(s, Config{CacheDir: dir})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("cache written for bytes that fail the checksum")
	}
}

func TestNoEmbeddedLibrary(t *testing.T) {
	stubSource(t)
	_, err := load(nil, Config{CacheDir: t.TempDir()})
	var e *Error
	if !errors.Is(err, ErrUnsupported) || !errors.As(err, &e) || e.GOOS != runtime.GOOS || e.GOARCH != runtime.GOARCH {
		t.Fatalf("err = %v", err)
	}
	if want := "kolovideo/vpx: no native libvpx for " + Target(); err.Error() != want {
		t.Fatalf("message %q, want %q", err, want)
	}
}

func TestEmbeddedSums(t *testing.T) {
	s, err := embedded()
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Skip("no library embedded for " + Target())
	}
	if err := s.verify(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadEmbedded(t *testing.T) {
	s, err := embedded()
	if err != nil || s == nil || !dlAvailable {
		t.Skip("no embedded library or loader for " + Target())
	}
	l, err := Load(Config{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.unload)
	if l.ABI != abiVersion || !strings.HasPrefix(l.Version, "v") {
		t.Fatalf("abi %d version %q", l.ABI, l.Version)
	}
	t.Logf("%s: source %s version %s abi %d", Target(), l.Source, l.Version, l.ABI)
}

func TestWithoutLoader(t *testing.T) {
	if dlAvailable {
		t.Skip("loader built")
	}
	_, err := Load(Config{})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), Target()) {
		t.Fatalf("err = %v", err)
	}
	if runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") && !strings.Contains(err.Error(), "kolovideo_native") {
		t.Fatalf("message lacks the tag: %v", err)
	}
}

func TestHints(t *testing.T) {
	d := &dlError{errors.New("refused")}
	h := hintFor(d)
	switch runtime.GOOS {
	case "darwin":
		if !strings.Contains(h, "com.apple.security.cs.disable-library-validation") || !strings.Contains(h, "Contents/Frameworks") {
			t.Fatalf("hint %q", h)
		}
	default:
		if h != "" {
			t.Fatalf("hint %q", h)
		}
	}
	if h := hintFor(ErrSignature); !strings.Contains(h, "codesign") {
		t.Fatalf("hint %q", h)
	}
	if hintFor(ErrCache) != "" {
		t.Fatal("unexpected hint")
	}
}
