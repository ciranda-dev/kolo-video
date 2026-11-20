package vpx

import (
	"embed"
	"errors"
	"fmt"
	"strings"
	"sync"

	kolovideo "github.com/ciranda-dev/kolo-video"
	"github.com/ciranda-dev/kolo-video/vpx/internal/native"
)

// ErrUnavailable means the requested libvpx engine cannot run.
var ErrUnavailable = errors.New("kolovideo/vpx: libvpx unavailable")

// EngineKind names a libvpx engine.
type EngineKind int

// Engines. The native engine is the prebuilt library, the portable engine is
// the same libvpx translated to Go.
const (
	EngineAuto EngineKind = iota
	EngineNative
	EnginePortable
)

// String returns the engine name.
func (k EngineKind) String() string {
	switch k {
	case EngineAuto:
		return "auto"
	case EngineNative:
		return "native"
	case EnginePortable:
		return "portable"
	}
	return fmt.Sprintf("EngineKind(%d)", int(k))
}

// The reasons the native engine can fail to load. LoadError, Runtime.NativeErr
// and the error of Load with EngineNative match them with errors.Is; the error
// text names the platform, each way that was tried and the operating system's
// message.
var (
	// ErrNativeUnsupported means this platform or build has no native library:
	// Linux without the kolovideo_native tag, or another platform.
	ErrNativeUnsupported = native.ErrUnsupported
	// ErrNativeIntegrity means the embedded library does not match its checksum.
	ErrNativeIntegrity = native.ErrIntegrity
	// ErrNativeABI means the library does not offer the interface this package
	// expects.
	ErrNativeABI = native.ErrABI
	// ErrNativeSignature means the code signature of a library file on macOS
	// is not valid.
	ErrNativeSignature = native.ErrSignature
	// ErrNativeCache means the cache directory or file cannot be used: another
	// user owns it or a directory above it can be changed by others. That
	// includes a group-writable directory such as a home with mode 0775 and a
	// private group: use mode 0755 or set LoadConfig.CacheDir.
	ErrNativeCache = native.ErrCache
)

// LoadConfig selects and sets up the engine. Library and CacheDir apply to the
// native engine only.
type LoadConfig struct {
	// Engine is EngineAuto if zero: native, with the portable engine as the
	// fallback when the native library cannot load.
	Engine EngineKind
	// CacheDir is the root of the native library cache, os.UserCacheDir()/kolo-video
	// if empty.
	CacheDir string
	// Library is the path of a libkolovpx the application ships and signs
	// itself, loaded instead of the embedded one. It is not checked against a
	// hash. An altered library that still carries a valid signature can be
	// killed by the operating system when it is loaded, which cannot be caught.
	Library string
}

// Runtime describes the engine in use.
type Runtime struct {
	Engine  EngineKind
	Version string // libvpx version, such as "v1.16.0"
	// NativeErr is why the native engine is not in use, nil if it is. Check it
	// after Load with EngineAuto: ErrNativeIntegrity and ErrNativeSignature are
	// not reported any other way.
	NativeErr error
}

var (
	loadMu sync.Mutex
	loaded *loadState
)

type loadState struct {
	cfg LoadConfig
	rt  Runtime
	eng engine
}

// Load selects the libvpx engine. It is optional: NewEncoder, NewDecoder,
// Engine, LoadError and Capabilities load the default configuration when
// nothing is loaded (on macOS and Windows that extracts and loads the native
// library), so call Load first to choose another one. Once an engine is
// active, Load with another configuration returns kolovideo.ErrInvalidConfig
// and the first engine stays. With Engine set to EngineNative, a native load
// failure returns ErrUnavailable wrapping the cause and nothing is loaded.
//
// With EngineAuto a failed native load is not an error: the portable engine
// runs and Load returns nil. Read Runtime.NativeErr (or LoadError) to learn why.
// ErrNativeIntegrity and ErrNativeSignature there mean that the embedded bytes
// or a bundle library did not verify, which is corruption or tampering, not the
// plain absence of a native library (ErrNativeUnsupported); log them.
func Load(cfg LoadConfig) (Runtime, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	if loaded != nil {
		if cfg != loaded.cfg {
			return Runtime{}, fmt.Errorf("kolovideo/vpx: libvpx already loaded as %v with another configuration: %w", loaded.rt.Engine, kolovideo.ErrInvalidConfig)
		}
		return loaded.rt, nil
	}
	return load(cfg)
}

func load(cfg LoadConfig) (Runtime, error) {
	var eng engine
	var nativeErr error
	switch cfg.Engine {
	case EngineAuto, EngineNative:
		eng, nativeErr = loadNative(cfg)
		if nativeErr != nil && cfg.Engine == EngineNative {
			return Runtime{}, &unavailable{nativeErr}
		}
	case EnginePortable:
		nativeErr = errors.New("kolovideo/vpx: native engine not requested")
	default:
		return Runtime{}, fmt.Errorf("kolovideo/vpx: engine %v: %w", cfg.Engine, kolovideo.ErrInvalidConfig)
	}
	if eng == nil {
		var err error
		if eng, err = loadPortable(); err != nil {
			return Runtime{}, &unavailable{err}
		}
	}
	rt := Runtime{Engine: eng.kind(), Version: eng.version(), NativeErr: nativeErr}
	loaded = &loadState{cfg, rt, eng}
	return rt, nil
}

// current returns the active state, loading the default engine first if needed.
func current() (*loadState, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	if loaded == nil {
		if _, err := load(LoadConfig{}); err != nil {
			return nil, err
		}
	}
	return loaded, nil
}

// Engine returns the engine in use, loading the default one if needed. It
// returns EngineAuto if no engine can run.
func Engine() EngineKind {
	st, err := current()
	if err != nil {
		return EngineAuto
	}
	return st.rt.Engine
}

// LoadError returns why the native engine is not in use, nil if it is. On
// Linux builds without the kolovideo_native tag it says so.
func LoadError() error {
	st, err := current()
	if err != nil {
		return err
	}
	return st.rt.NativeErr
}

// RealTime is a picture size and frame rate.
type RealTime struct {
	Width, Height int
	FrameRate     float64
}

// Limits are the largest modes the engine in use handled in real time.
type Limits struct {
	Engine EngineKind
	Encode RealTime
	Decode RealTime
}

// Capabilities returns reference limits, not a run-time probe: the largest
// encode and decode modes measured as real time for the engine in use, so a
// caller can cap resolution. Real time means a mean time per frame below the
// 33.3 ms period for a camera-like stream, on an Apple M5: the native engine
// reaches 1920x1080 at 30 fps for encode and decode, VP8 and VP9; the portable
// engine encodes up to 640x480 at 30 fps and decodes up to 1920x1080. A slower
// CPU reaches less. It returns the zero Limits if no engine can run.
func Capabilities() Limits {
	st, err := current()
	if err != nil {
		return Limits{}
	}
	if st.rt.Engine == EngineNative {
		return Limits{EngineNative, RealTime{1920, 1080, 30}, RealTime{1920, 1080, 30}}
	}
	return Limits{EnginePortable, RealTime{640, 480, 30}, RealTime{1920, 1080, 30}}
}

// WriteLibrary writes the embedded native library into dir and returns its
// path, for an application that signs the library and ships it, then names it
// in LoadConfig.Library (on macOS also as Contents/Frameworks/libkolovpx.dylib,
// which is loaded without configuration). It errors when no library is embedded.
func WriteLibrary(dir string) (string, error) { return native.WriteLibrary(dir) }

//go:embed notices/*
var noticeFS embed.FS

// Notices returns the licence and patent grant texts of the code in this
// package: libvpx, the C runtime parts of the portable engine and the native
// libraries, and the translator's helpers. An application that ships binaries
// must include them.
func Notices() string {
	files, _ := noticeFS.ReadDir("notices")
	var b strings.Builder
	for _, f := range files {
		text, _ := noticeFS.ReadFile("notices/" + f.Name())
		fmt.Fprintf(&b, "==== %s ====\n\n%s\n", f.Name(), text)
	}
	return b.String()
}
