// Package native loads the prebuilt libkolovpx shared library without cgo.
package native

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"
)

// LibvpxVersion is the libvpx release the embedded libraries are built from.
const LibvpxVersion = "v1.16.0"

// abiVersion is the value kv_abi_version must return.
var abiVersion int32 = 1

var (
	// ErrUnsupported means this build or platform has no native library.
	ErrUnsupported = errors.New("no native libvpx")
	// ErrIntegrity means the embedded bytes do not match their checksum.
	ErrIntegrity = errors.New("embedded native libvpx does not match its checksum")
	// ErrABI means the library does not export the expected kv_* interface.
	ErrABI = errors.New("native libvpx ABI mismatch")
	// ErrSignature means the code signature of a library file is not valid.
	ErrSignature = errors.New("native libvpx code signature is not valid")
	// ErrCache means the cache directory or file cannot be used.
	ErrCache = errors.New("native libvpx cache is unusable")
)

// Source tells where a loaded library came from.
type Source string

// Library sources.
const (
	SourceMemfd   Source = "memfd"
	SourceCache   Source = "cache"
	SourceBundle  Source = "bundle"
	SourceLibrary Source = "library"
)

// Config selects where the library is loaded from.
type Config struct {
	// CacheDir replaces os.UserCacheDir()/kolo-video as the cache root.
	CacheDir string
	// Library is the path of a libkolovpx the application ships itself. It is
	// not hash checked, and on macOS its signature is validated before it is
	// loaded. An altered library that still carries a valid signature can be
	// killed by the operating system when its pages are mapped, which cannot be
	// caught.
	Library string
}

// Lib is a loaded library with its kv_* functions bound.
type Lib struct {
	Source  Source
	Path    string // empty for SourceMemfd
	Version string // libvpx version string reported by the library
	ABI     int32

	handle uintptr

	EncNew     func(vp9, w, h int32, kbps uint32, threads, speed int32, maxIntra uint32, err *int32) uintptr
	EncFree    func(e uintptr)
	Encode     func(e uintptr, y unsafe.Pointer, yStride int32, u, v unsafe.Pointer, uvStride int32, pts int64, dur uint32, key int32) int32
	NextFrame  func(e uintptr, out *[3]uintptr) int32
	SetBitrate func(e uintptr, kbps uint32) int32
	DecNew     func(vp9, threads int32, err *int32) uintptr
	DecFree    func(d uintptr)
	Decode     func(d uintptr, data unsafe.Pointer, n uint32, out *[8]uintptr) int32
}

// Attempt is one failed way of loading the library.
type Attempt struct {
	Source Source
	Path   string
	Err    error
}

// Error describes why the native library was not loaded.
type Error struct {
	GOOS, GOARCH string
	Cause        error // set when no attempt was made
	Reason       string
	Attempts     []Attempt
	Hint         string
}

func (e *Error) Error() string {
	var b strings.Builder
	t := e.GOOS + "/" + e.GOARCH
	if e.Cause != nil && len(e.Attempts) == 0 {
		fmt.Fprintf(&b, "kolovideo/vpx: %v for %s", e.Cause, t)
		if e.Reason != "" {
			fmt.Fprintf(&b, " (%s)", e.Reason)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "kolovideo/vpx: load native libvpx (%s)", t)
	for i, a := range e.Attempts {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		b.WriteString(string(a.Source))
		msg := a.Err.Error()
		if a.Path != "" {
			b.WriteString(" " + a.Path)
			msg = strings.ReplaceAll(msg, a.Path, "<lib>")
		}
		b.WriteString(": " + msg)
	}
	if e.Hint != "" {
		fmt.Fprintf(&b, " (%s)", e.Hint)
	}
	return b.String()
}

// Unwrap returns the sentinel cause and every attempt error.
func (e *Error) Unwrap() []error {
	var l []error
	if e.Cause != nil {
		l = append(l, e.Cause)
	}
	for _, a := range e.Attempts {
		l = append(l, a.Err)
	}
	return l
}

// Target returns GOOS/GOARCH of this build.
func Target() string { return runtime.GOOS + "/" + runtime.GOARCH }

type source struct {
	data []byte
	sum  [32]byte
}

// embedded returns the library embedded for this target, nil when there is none.
func embedded() (*source, error) {
	s, ok := sums[Target()]
	if !ok || len(libData) == 0 {
		return nil, nil
	}
	b, err := hex.DecodeString(s.SHA256)
	if err != nil || len(b) != sha256.Size {
		return nil, fmt.Errorf("%w: bad checksum in sums", ErrIntegrity)
	}
	src := &source{data: libData}
	copy(src.sum[:], b)
	if len(libData) != s.Size {
		return nil, fmt.Errorf("%w: size %d, want %d", ErrIntegrity, len(libData), s.Size)
	}
	return src, nil
}

func (s *source) verify() error {
	if sha256.Sum256(s.data) != s.sum {
		return ErrIntegrity
	}
	return nil
}

func libName() string {
	switch runtime.GOOS {
	case "darwin":
		return "libkolovpx.dylib"
	case "windows":
		return "libkolovpx.dll"
	}
	return "libkolovpx.so"
}

// Load verifies the embedded library and loads it: from an app bundle or the
// cache on macOS, a memfd then the cache on Linux, the cache on Windows. With
// cfg.Library set only that file is tried. The error is an *Error.
func Load(cfg Config) (*Lib, error) {
	s, err := embedded()
	if err != nil {
		return nil, &Error{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Cause: ErrIntegrity, Reason: err.Error()}
	}
	return load(s, cfg)
}

func load(s *source, cfg Config) (*Lib, error) {
	e := &Error{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	if !dlAvailable {
		e.Cause, e.Reason = ErrUnsupported, notBuilt()
		return nil, e
	}
	if cfg.Library != "" {
		l, err := openFile(SourceLibrary, cfg.Library, true)
		if err == nil {
			return l, nil
		}
		e.Attempts = []Attempt{{SourceLibrary, cfg.Library, err}}
		e.Hint = hintFor(err)
		return nil, e
	}
	if s == nil {
		e.Cause = ErrUnsupported
		return nil, e
	}
	if err := s.verify(); err != nil {
		e.Cause = err
		return nil, e
	}
	try := func(src Source, path string, f func() (*Lib, error)) *Lib {
		l, err := f()
		if err != nil {
			e.Attempts = append(e.Attempts, Attempt{src, path, err})
			if h := hintFor(err); h != "" {
				e.Hint = h
			}
		}
		return l
	}
	switch runtime.GOOS {
	case "darwin":
		if p := bundleLibrary(); p != "" && fileExists(p) {
			if l := try(SourceBundle, p, func() (*Lib, error) { return openBundle(p) }); l != nil {
				return l, nil
			}
		}
	case "linux":
		if l := try(SourceMemfd, "", func() (*Lib, error) { return memfdLoad(s) }); l != nil {
			return l, nil
		}
	}
	dir, derr := cacheDir(cfg.CacheDir, s.sum)
	cachePath := ""
	if derr == nil {
		cachePath = filepath.Join(dir, libName())
	}
	if l := try(SourceCache, cachePath, func() (*Lib, error) {
		if derr != nil {
			return nil, derr
		}
		return cacheLoad(s, dir)
	}); l != nil {
		return l, nil
	}
	return nil, e
}

// openFile loads the library at path and checks its ABI; checkSig asks for the
// platform's code signature check first (macOS only).
func openFile(src Source, path string, checkSig bool) (*Lib, error) {
	if checkSig {
		if err := checkSignature(path); err != nil {
			return nil, err
		}
	}
	h, err := dlOpen(path)
	if err != nil {
		return nil, &dlError{err}
	}
	l := &Lib{Source: src, Path: path, handle: h}
	if src == SourceMemfd {
		l.Path = ""
	}
	abi, version, err := bind(h, l)
	if err == nil {
		err = l.finish(abi, version)
	}
	if err != nil {
		dlClose(h)
		return nil, err
	}
	return l, nil
}

// unload releases the library; tests use it so a temporary directory can be
// removed on Windows.
func (l *Lib) unload() { dlClose(l.handle) }

// dlError marks a refusal by the operating system's dynamic loader.
type dlError struct{ err error }

func (e *dlError) Error() string { return "load: " + e.err.Error() }
func (e *dlError) Unwrap() error { return e.err }

func hintFor(err error) string {
	var d *dlError
	if errors.As(err, &d) {
		return platformHint(d.err)
	}
	if errors.Is(err, ErrSignature) {
		return "sign the library with the application's identity: codesign -f -s <identity> " + libName()
	}
	return ""
}

// finish checks the ABI of a bound library and reads its version.
func (l *Lib) finish(abi func() int32, version func() string) error {
	l.ABI = abi()
	if l.ABI != abiVersion {
		return fmt.Errorf("%w: library reports %d, want %d", ErrABI, l.ABI, abiVersion)
	}
	l.Version = version()
	return nil
}
