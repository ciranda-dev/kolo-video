package native

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

var executablePath = os.Executable

// bundleLibrary returns the library path inside the application bundle that
// holds the running executable, or "" when it is not in a bundle.
func bundleLibrary() string {
	exe, err := executablePath()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	macos := filepath.Dir(exe)
	contents := filepath.Dir(macos)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" {
		return ""
	}
	return filepath.Join(contents, "Frameworks", libName())
}

// openBundle loads the library of an app bundle. It must be a plain file that
// this user or root owns and no one else can write, with a valid signature, and
// the same file before and after the load. The signature is any valid one: the
// bundle is as trusted as the application that holds it.
func openBundle(p string) (*Lib, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSignature, err)
	}
	uid, _, ok := ownerOf(fi)
	if !fi.Mode().IsRegular() || !ok || (uid != currentUID() && uid != 0) || fi.Mode().Perm()&0o022 != 0 || noACLWrite(p) != nil {
		return nil, fmt.Errorf("%w: %s is not a plain file owned by this user or root that no one else can write", ErrSignature, p)
	}
	if err := checkSignature(p); err != nil {
		return nil, err
	}
	same := func() bool {
		cur, err := os.Lstat(p)
		return err == nil && os.SameFile(fi, cur) && cur.Size() == fi.Size() && cur.ModTime().Equal(fi.ModTime())
	}
	beforeLoad()
	if !same() {
		return nil, fmt.Errorf("%w: %s changed after it was checked", ErrSignature, p)
	}
	l, err := openFile(SourceBundle, p, false)
	if err != nil {
		return nil, err
	}
	if !same() {
		l.unload()
		return nil, fmt.Errorf("%w: %s changed while it was loaded", ErrSignature, p)
	}
	return l, nil
}

const (
	kSecCSCheckAllArchitectures = 1 << 0
	kCFStringEncodingUTF8       = 0x08000100
)

var sec struct {
	once             sync.Once
	err              error
	urlFromPath      func(alloc uintptr, buf *byte, n int, dir bool) uintptr
	release          func(o uintptr)
	codeFromPath     func(url uintptr, flags uint32, out *uintptr) int32
	checkValidity    func(code uintptr, flags uint32, req uintptr) int32
	errorMessage     func(status int32, reserved uintptr) uintptr
	stringGetCString func(s uintptr, buf *byte, size int, enc uint32) bool
}

func loadSecurity() error {
	sec.once.Do(func() {
		cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			sec.err = err
			return
		}
		sf, err := purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			sec.err = err
			return
		}
		for _, s := range []struct {
			h    uintptr
			name string
			fn   any
		}{
			{cf, "CFURLCreateFromFileSystemRepresentation", &sec.urlFromPath},
			{cf, "CFRelease", &sec.release},
			{cf, "CFStringGetCString", &sec.stringGetCString},
			{sf, "SecStaticCodeCreateWithPath", &sec.codeFromPath},
			{sf, "SecStaticCodeCheckValidity", &sec.checkValidity},
			{sf, "SecCopyErrorMessageString", &sec.errorMessage},
		} {
			a, err := purego.Dlsym(s.h, s.name)
			if err != nil {
				sec.err = err
				return
			}
			purego.RegisterFunc(s.fn, a)
		}
	})
	return sec.err
}

func secError(status int32) string {
	if s := sec.errorMessage(status, 0); s != 0 {
		defer sec.release(s)
		buf := make([]byte, 256)
		if sec.stringGetCString(s, &buf[0], len(buf), kCFStringEncodingUTF8) {
			for i, c := range buf {
				if c == 0 {
					return string(buf[:i])
				}
			}
		}
	}
	return fmt.Sprintf("OSStatus %d", status)
}

// checkSignature validates the code signature of the file with
// SecStaticCodeCheckValidity, which hashes every page. A library whose signed
// pages were altered is killed by the kernel when they are mapped, so it must
// be refused before dlopen.
func checkSignature(path string) error {
	if err := loadSecurity(); err != nil {
		return fmt.Errorf("%w: Security.framework: %v", ErrSignature, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSignature, err)
	}
	b := []byte(abs)
	url := sec.urlFromPath(0, unsafe.SliceData(b), len(b), false)
	if url == 0 {
		return fmt.Errorf("%w: cannot make a URL for %s", ErrSignature, abs)
	}
	defer sec.release(url)
	var code uintptr
	if st := sec.codeFromPath(url, 0, &code); st != 0 {
		return fmt.Errorf("%w: %s", ErrSignature, secError(st))
	}
	defer sec.release(code)
	if st := sec.checkValidity(code, kSecCSCheckAllArchitectures, 0); st != 0 {
		return fmt.Errorf("%w: %s", ErrSignature, secError(st))
	}
	return nil
}
