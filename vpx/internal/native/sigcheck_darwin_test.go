package native

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeStub(t *testing.T, path string, s *source) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, s.data, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSignatureValid(t *testing.T) {
	s := stubSource(t)
	p := filepath.Join(t.TempDir(), libName())
	writeStub(t, p, s)
	if err := checkSignature(p); err != nil {
		t.Fatal(err)
	}
}

func TestSignatureUnsigned(t *testing.T) {
	s := stubSource(t)
	p := filepath.Join(t.TempDir(), libName())
	writeStub(t, p, s)
	if out, err := exec.Command("codesign", "--remove-signature", p).CombinedOutput(); err != nil {
		t.Skipf("codesign: %v %s", err, out)
	}
	if err := checkSignature(p); !errors.Is(err, ErrSignature) {
		t.Fatalf("err = %v", err)
	}
}

// codeLimit returns the offset where the code signature starts: every byte
// before it is covered by the page hashes.
func codeLimit(t *testing.T, b []byte) int {
	t.Helper()
	f, err := macho.NewFile(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range f.Loads {
		if raw := l.Raw(); binary.LittleEndian.Uint32(raw) == 0x1d {
			return int(binary.LittleEndian.Uint32(raw[8:]))
		}
	}
	t.Fatal("no LC_CODE_SIGNATURE")
	return 0
}

// A signed library with altered pages is killed by the kernel when the altered
// page is mapped, so the check must refuse it before dlopen.
func TestSignatureTampered(t *testing.T) {
	s := stubSource(t)
	for _, off := range []int{0x1000, codeLimit(t, s.data) / 2, codeLimit(t, s.data) - 1} {
		b := append([]byte(nil), s.data...)
		b[off] ^= 0xff
		p := filepath.Join(t.TempDir(), libName())
		writeStub(t, p, &source{data: b})
		if err := checkSignature(p); !errors.Is(err, ErrSignature) {
			t.Errorf("offset %#x: err = %v", off, err)
		}
	}
}

func fakeApp(t *testing.T, lib *source) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), "Fake.app", "Contents")
	exe := filepath.Join(app, "MacOS", "fake")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if lib != nil {
		writeStub(t, filepath.Join(app, "Frameworks", libName()), lib)
	}
	old := executablePath
	executablePath = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executablePath = old })
	return filepath.Join(app, "Frameworks", libName())
}

func TestBundleLibrary(t *testing.T) {
	s := stubSource(t)
	want, err := filepath.EvalSymlinks(fakeApp(t, s))
	if err != nil {
		t.Fatal(err)
	}
	l, err := load(s, Config{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.unload)
	if l.Source != SourceBundle || l.Path != want {
		t.Fatalf("source %q path %q, want bundle %q", l.Source, l.Path, want)
	}
}

func TestBundleTamperedFallsBackToCache(t *testing.T) {
	s := stubSource(t)
	bad := append([]byte(nil), s.data...)
	bad[codeLimit(t, bad)/2] ^= 0xff
	fakeApp(t, &source{data: bad})
	l, err := load(s, Config{CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.unload)
	if l.Source != SourceCache {
		t.Fatalf("source %q", l.Source)
	}
}

func TestBundleTamperedAndCacheFails(t *testing.T) {
	s := stubSource(t)
	bad := append([]byte(nil), s.data...)
	bad[codeLimit(t, bad)/2] ^= 0xff
	fakeApp(t, &source{data: bad})
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := load(s, Config{CacheDir: blocked})
	var e *Error
	if !errors.As(err, &e) || len(e.Attempts) != 2 || e.Attempts[0].Source != SourceBundle || !errors.Is(e.Attempts[0].Err, ErrSignature) || !errors.Is(e.Attempts[1].Err, ErrCache) {
		t.Fatalf("err = %v", err)
	}
}

// A bundle library that others can write, or that is a link, is refused and the
// cache takes over.
func TestBundleFileRules(t *testing.T) {
	s := stubSource(t)
	for name, arm := range map[string]func(t *testing.T, p string){
		"group writable": func(t *testing.T, p string) {
			if err := os.Chmod(p, 0o775); err != nil {
				t.Fatal(err)
			}
		},
		"symbolic link": func(t *testing.T, p string) {
			other := filepath.Join(t.TempDir(), "real.dylib")
			writeStub(t, other, s)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, p); err != nil {
				t.Fatal(err)
			}
		},
		"another owner": func(t *testing.T, p string) {
			old := ownerOf
			ownerOf = func(fi os.FileInfo) (int, int, bool) { return os.Getuid() + 1000, 1, true }
			t.Cleanup(func() { ownerOf = old })
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := fakeApp(t, s)
			arm(t, p)
			blocked := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(blocked, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := load(s, Config{CacheDir: blocked})
			var e *Error
			if !errors.As(err, &e) || len(e.Attempts) == 0 || e.Attempts[0].Source != SourceBundle || !errors.Is(e.Attempts[0].Err, ErrSignature) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// A bundle library swapped between the check and the load is refused.
func TestBundleSwappedBeforeLoad(t *testing.T) {
	s := stubSource(t)
	p := fakeApp(t, s)
	other := filepath.Join(t.TempDir(), "other.dylib")
	writeStub(t, other, s)
	beforeLoad = func() { os.Rename(other, p) }
	t.Cleanup(func() { beforeLoad = func() {} })
	_, err := openBundle(p)
	if !errors.Is(err, ErrSignature) || !strings.Contains(err.Error(), "changed after it was checked") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoBundle(t *testing.T) {
	old := executablePath
	executablePath = func() (string, error) { return filepath.Join(t.TempDir(), "tool"), nil }
	defer func() { executablePath = old }()
	if p := bundleLibrary(); p != "" {
		t.Fatalf("bundleLibrary = %q outside an app", p)
	}
}

func TestLibraryOverrideTampered(t *testing.T) {
	s := stubSource(t)
	bad := append([]byte(nil), s.data...)
	bad[codeLimit(t, bad)/2] ^= 0xff
	p := filepath.Join(t.TempDir(), libName())
	writeStub(t, p, &source{data: bad})
	_, err := load(nil, Config{Library: p})
	if !errors.Is(err, ErrSignature) || !strings.Contains(err.Error(), "library "+p) || !strings.Contains(err.Error(), "codesign") {
		t.Fatalf("err = %v", err)
	}
}
