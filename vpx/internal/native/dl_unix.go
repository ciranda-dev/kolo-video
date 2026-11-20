//go:build (darwin || (linux && kolovideo_native)) && (amd64 || arm64)

package native

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/ebitengine/purego"
)

const dlAvailable = true

func notBuilt() string { return "" }

const macHint = "if the application uses the hardened runtime, add the entitlement com.apple.security.cs.disable-library-validation, or sign the library with the application's identity and ship it as Contents/Frameworks/libkolovpx.dylib (vpx.WriteLibrary writes it)"

func platformHint(error) string {
	if runtime.GOOS == "darwin" {
		return macHint
	}
	return ""
}

func dlOpen(path string) (uintptr, error) {
	h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return 0, errors.New(trimDlError(err.Error(), path))
	}
	return h, nil
}

func dlClose(h uintptr) { purego.Dlclose(h) }

func bind(h uintptr, l *Lib) (func() int32, func() string, error) {
	var abi func() int32
	var version func() string
	syms := []struct {
		name string
		fn   any
	}{
		{"kv_abi_version", &abi},
		{"kv_version", &version},
		{"kv_enc_new", &l.EncNew},
		{"kv_enc_free", &l.EncFree},
		{"kv_encode", &l.Encode},
		{"kv_next_frame", &l.NextFrame},
		{"kv_set_bitrate", &l.SetBitrate},
		{"kv_dec_new", &l.DecNew},
		{"kv_dec_free", &l.DecFree},
		{"kv_decode", &l.Decode},
	}
	for _, s := range syms {
		a, err := purego.Dlsym(h, s.name)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: missing symbol %s", ErrABI, s.name)
		}
		purego.RegisterFunc(s.fn, a)
	}
	return abi, version, nil
}

var dyldUUID = regexp.MustCompile(`<[0-9A-Fa-f-]{36}> `)

// trimDlError shortens the dyld message, which repeats the library path for
// every place it looked, to the first reason that is not a missing file.
func trimDlError(msg, path string) string {
	if runtime.GOOS != "darwin" {
		return msg
	}
	// dyld prints the symlink-resolved path (/private/var for /var), and
	// replacing the shorter one first would leave a stray prefix.
	paths := []string{path}
	if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
		paths = []string{real, path}
	}
	for _, p := range paths {
		msg = strings.ReplaceAll(msg, "/System/Volumes/Preboot/Cryptexes/OS"+p, p)
	}
	if _, tried, ok := strings.Cut(msg, "tried: "); ok {
		msg = ""
		for _, e := range strings.Split(tried, "), '") {
			if _, r, ok := strings.Cut(e, " ("); ok && (msg == "" || strings.HasPrefix(msg, "no such file")) {
				msg = strings.TrimSuffix(r, ")")
			}
		}
		if msg == "" {
			msg = tried
		}
	} else if _, r, ok := strings.Cut(msg, "): "); ok {
		msg = r
	}
	for _, p := range paths {
		msg = strings.ReplaceAll(msg, p, "<lib>")
	}
	msg = dyldUUID.ReplaceAllString(msg, "")
	const max = 300
	if len(msg) > max {
		msg = msg[:max] + "..."
	}
	return msg
}
