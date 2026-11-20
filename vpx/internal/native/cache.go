package native

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func cacheLoad(s *source, dir string) (*Lib, error) {
	p, err := ensureFile(dir, libName(), s.data, s.sum)
	if err != nil {
		return nil, err
	}
	l, err := openCached(p, s)
	if err == nil {
		prune(dir)
	}
	return l, err
}

// Leftovers older than these are removed after a successful load: a write takes
// milliseconds, so a temporary file a minute old is from a crashed process; the
// library of an older release that no start has touched for 30 days is unused.
const (
	tempMaxAge  = time.Minute
	staleDirAge = 30 * 24 * time.Hour
)

// prune removes, best effort, the temporary files that crashed writes left in
// dir and the cache directories of other releases (vpx/<version>-<sha256>) that
// have not changed for staleDirAge. It deletes only the library file, its
// temporary files and an empty directory, never anything else.
func prune(dir string) {
	now := time.Now()
	sweep(dir, now, tempMaxAge, false)
	parent := filepath.Dir(dir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		other := filepath.Join(parent, e.Name())
		if other == dir || !e.IsDir() || !cacheDirName.MatchString(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > staleDirAge {
			sweep(other, now, 0, true)
			os.Remove(other)
		}
	}
}

// cacheDirName matches the directory name cacheDir makes.
var cacheDirName = regexp.MustCompile(`^v[0-9.]+-[0-9a-f]{64}$`)

// sweep removes the temporary files of the library in dir older than minAge and,
// with lib set, the library file too.
func sweep(dir string, now time.Time, minAge time.Duration, lib bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		temp := strings.HasPrefix(e.Name(), libName()+".tmp")
		if !e.Type().IsRegular() || !(temp || (lib && e.Name() == libName())) {
			continue
		}
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) >= minAge {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// cacheDir returns <base>/vpx/<libvpx version>-<sha256>, with the default
// base os.UserCacheDir()/kolo-video.
func cacheDir(base string, sum [32]byte) (string, error) {
	if base == "" {
		u, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrCache, err)
		}
		base = filepath.Join(u, "kolo-video")
	}
	return filepath.Join(base, "vpx", LibvpxVersion+"-"+hex.EncodeToString(sum[:])), nil
}

// trustedDir resolves the symbolic links of dir's existing part, creates the
// missing directories and returns the resolved path. Every directory from the
// filesystem root to dir must be fit to hold a library that is about to be
// loaded (checkAncestor, checkDir), or a writer of a parent could swap the
// verified file between the hash check and the load.
func trustedDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if fi, err := os.Lstat(abs); err == nil && !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	top, rest := abs, []string(nil)
	for {
		_, err := os.Lstat(top)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(top)
		if next == top {
			break
		}
		rest = append([]string{filepath.Base(top)}, rest...)
		top = next
	}
	real, err := filepath.EvalSymlinks(top)
	if err != nil {
		return "", err
	}
	if err := checkChain(real, len(rest) == 0); err != nil {
		return "", err
	}
	if len(rest) == 0 {
		return real, nil
	}
	full := real
	for _, name := range rest {
		full = filepath.Join(full, name)
		if err := makeDir(full); err != nil && !os.IsExist(err) {
			return "", err
		}
	}
	// Another user may have created a missing directory first.
	if err := checkChain(full, true); err != nil {
		return "", err
	}
	return full, nil
}

// checkChain checks every directory of the resolved path p, from its root. The
// last one is the cache directory itself when leaf is set.
func checkChain(p string, leaf bool) error {
	vol := filepath.VolumeName(p)
	var parts []string
	for q := strings.TrimPrefix(p, vol); q != "" && q != string(filepath.Separator); q = filepath.Dir(q) {
		parts = append([]string{filepath.Base(q)}, parts...)
	}
	cur := vol + string(filepath.Separator)
	for i := -1; i < len(parts); i++ {
		if i >= 0 {
			cur = filepath.Join(cur, parts[i])
		}
		fi, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("%s is not a plain directory", cur)
		}
		var cerr error
		if leaf && i == len(parts)-1 {
			cerr = checkDir(cur, fi)
		} else {
			cerr = checkAncestor(cur, fi)
		}
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

// ensureFile returns dir/name holding data, writing it when the file is missing
// or its hash is wrong. Concurrent callers and processes are safe: the file is
// written to a temporary name and renamed into place.
func ensureFile(dir, name string, data []byte, sum [32]byte) (string, error) {
	dir, err := trustedDir(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCache, err)
	}
	p := filepath.Join(dir, name)
	if fileHasSum(p, sum, len(data)) {
		return p, nil
	}
	werr := writeAtomic(dir, name, data, 0o500)
	// A concurrent writer may be replacing the file now: the path can then show
	// the old file with no link left, and Windows refuses to replace a file that
	// is open or just made read-only. Once the writers' renames are done the
	// right file is in place, so retry.
	retries := 50
	if werr != nil && runtime.GOOS != "windows" {
		retries = 0
	}
	for i := 0; ; i++ {
		if fileHasSum(p, sum, len(data)) {
			return p, nil
		}
		if i >= retries {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if werr != nil {
		return "", fmt.Errorf("%w: %v", ErrCache, werr)
	}
	return "", fmt.Errorf("%w: the file does not match its checksum after writing", ErrCache)
}

// fileHasSum reports whether p is a regular file of the given size with the
// given SHA-256 that only this user can change: see checkFile.
func fileHasSum(p string, sum [32]byte, size int) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != int64(size) || !checkFile(p, fi) {
		return false
	}
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, int64(size)+1)); err != nil {
		return false
	}
	var got [32]byte
	h.Sum(got[:0])
	return got == sum
}

// writeAtomic writes data to dir/name through a synced temporary file and a
// rename, replacing any existing file.
func writeAtomic(dir, name string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(dir, name+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	dst := filepath.Join(dir, name)
	if fi, err := os.Lstat(dst); err == nil && fi.Mode().IsRegular() {
		os.Chmod(dst, 0o600) // a read-only file cannot be replaced on Windows
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// WriteLibrary writes the embedded library to dir for an application bundle and
// returns its path. The embedded bytes are verified against their checksum first.
func WriteLibrary(dir string) (string, error) {
	s, err := embedded()
	e := &Error{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	switch {
	case err != nil:
		e.Cause, e.Reason = ErrIntegrity, err.Error()
	case s == nil:
		e.Cause = ErrUnsupported
	default:
		if err := s.verify(); err != nil {
			e.Cause = err
		}
	}
	if e.Cause != nil {
		return "", e
	}
	return writeLibrary(s, dir)
}

func writeLibrary(s *source, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("kolovideo/vpx: write native libvpx: %w", err)
	}
	if err := writeAtomic(dir, libName(), s.data, 0o644); err != nil {
		return "", fmt.Errorf("kolovideo/vpx: write native libvpx: %w", err)
	}
	return filepath.Join(dir, libName()), nil
}
