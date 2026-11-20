//go:build !(linux && kolovideo_native && (amd64 || arm64))

package native

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
)

func memfdLoad(*source) (*Lib, error) { return nil, errors.New("memfd is not available in this build") }

// beforeLoad is called between the last check of a cache file and the load; tests
// use it to change the file at that moment.
var beforeLoad = func() {}

// openCached loads the cache file p through one handle that is hashed and
// checked (owner, mode, links). Its identity is compared with the path just
// before and just after the load: a swapped file is refused and a library that
// did load is closed again. trustedDir makes sure only the user can swap it,
// and on Windows the open handle also blocks replacing it. Where a swapped
// library has run its start-up code before it is refused (macOS), that is the
// residual risk.
func openCached(p string, s *source) (*Lib, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCache, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !checkFile(p, fi) {
		return nil, fmt.Errorf("%w: %s is not a plain file that only this user can change", ErrCache, p)
	}
	h := sha256.New()
	if n, err := io.Copy(h, io.LimitReader(f, int64(len(s.data))+1)); err != nil || n != int64(len(s.data)) {
		return nil, fmt.Errorf("%w: %s has the wrong size", ErrCache, p)
	}
	var got [32]byte
	h.Sum(got[:0])
	if got != s.sum {
		return nil, fmt.Errorf("%w: %s does not match its checksum", ErrCache, p)
	}
	same := func() bool {
		cur, err := os.Lstat(p)
		return err == nil && os.SameFile(fi, cur) && cur.Size() == fi.Size() && cur.ModTime().Equal(fi.ModTime())
	}
	beforeLoad()
	if !same() {
		return nil, fmt.Errorf("%w: %s changed after it was checked", ErrCache, p)
	}
	l, err := openFile(SourceCache, p, false)
	if err != nil {
		return nil, err
	}
	if !same() {
		l.unload()
		return nil, fmt.Errorf("%w: %s changed while it was loaded", ErrCache, p)
	}
	return l, nil
}
