//go:build windows && (amd64 || arm64)

package native

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const dlAvailable = true

func notBuilt() string { return "" }

const winHint = "a Windows code integrity policy such as Smart App Control or WDAC can block unsigned DLLs"

func platformHint(err error) string {
	var n syscall.Errno
	if errors.As(err, &n) {
		switch n {
		case windows.ERROR_ACCESS_DENIED, windows.ERROR_INVALID_IMAGE_HASH, windows.ERROR_ACCESS_DISABLED_BY_POLICY, windows.ERROR_ACCESS_DISABLED_NO_SAFER_UI_BY_POLICY:
			return winHint
		}
	}
	return ""
}

// dlOpen loads the DLL from its absolute path and lets its dependencies come
// only from the same directory and System32, never from the default search path.
func dlOpen(path string) (uintptr, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	h, err := windows.LoadLibraryEx(abs, 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	return uintptr(h), err
}

func dlClose(h uintptr) { windows.FreeLibrary(windows.Handle(h)) }

func bind(h uintptr, l *Lib) (func() int32, func() string, error) {
	addr := map[string]uintptr{}
	for _, n := range []string{"kv_abi_version", "kv_version", "kv_enc_new", "kv_enc_free", "kv_encode", "kv_next_frame", "kv_set_bitrate", "kv_dec_new", "kv_dec_free", "kv_decode"} {
		a, err := windows.GetProcAddress(windows.Handle(h), n)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: missing symbol %s", ErrABI, n)
		}
		addr[n] = a
	}
	encNew, encFree, encode, next := addr["kv_enc_new"], addr["kv_enc_free"], addr["kv_encode"], addr["kv_next_frame"]
	setBitrate, decNew, decFree, decode := addr["kv_set_bitrate"], addr["kv_dec_new"], addr["kv_dec_free"], addr["kv_decode"]
	l.EncNew = func(vp9, w, h int32, kbps uint32, threads, speed int32, maxIntra uint32, err *int32) uintptr {
		r, _, _ := syscall.SyscallN(encNew, uintptr(vp9), uintptr(w), uintptr(h), uintptr(kbps), uintptr(threads), uintptr(speed), uintptr(maxIntra), uintptr(unsafe.Pointer(err)))
		return r
	}
	l.EncFree = func(e uintptr) { syscall.SyscallN(encFree, e) }
	l.Encode = func(e uintptr, y unsafe.Pointer, yStride int32, u, v unsafe.Pointer, uvStride int32, pts int64, dur uint32, key int32) int32 {
		r, _, _ := syscall.SyscallN(encode, e, uintptr(y), uintptr(yStride), uintptr(u), uintptr(v), uintptr(uvStride), uintptr(pts), uintptr(dur), uintptr(key))
		return int32(r)
	}
	l.NextFrame = func(e uintptr, out *[3]uintptr) int32 {
		r, _, _ := syscall.SyscallN(next, e, uintptr(unsafe.Pointer(out)))
		return int32(r)
	}
	l.SetBitrate = func(e uintptr, kbps uint32) int32 {
		r, _, _ := syscall.SyscallN(setBitrate, e, uintptr(kbps))
		return int32(r)
	}
	l.DecNew = func(vp9, threads int32, err *int32) uintptr {
		r, _, _ := syscall.SyscallN(decNew, uintptr(vp9), uintptr(threads), uintptr(unsafe.Pointer(err)))
		return r
	}
	l.DecFree = func(d uintptr) { syscall.SyscallN(decFree, d) }
	l.Decode = func(d uintptr, data unsafe.Pointer, n uint32, out *[8]uintptr) int32 {
		r, _, _ := syscall.SyscallN(decode, d, uintptr(data), uintptr(n), uintptr(unsafe.Pointer(out)))
		return int32(r)
	}
	return func() int32 { r, _, _ := syscall.SyscallN(addr["kv_abi_version"]); return int32(r) },
		func() string {
			r, _, _ := syscall.SyscallN(addr["kv_version"])
			return windows.BytePtrToString(*(**byte)(unsafe.Pointer(&r)))
		}, nil
}
