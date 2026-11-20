//go:build !windows

package native

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// currentUID and ownerOf are variables so that tests can make a directory
// look as if another user owned it.
var (
	currentUID = os.Getuid
	ownerOf    = func(fi os.FileInfo) (uid, nlink int, ok bool) {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, 0, false
		}
		return int(st.Uid), int(st.Nlink), true
	}
)

// checkDir accepts only a cache directory owned by the current user that no one
// else can write to, repairing the mode when the owner is right.
func checkDir(dir string, fi os.FileInfo) error {
	uid, _, ok := ownerOf(fi)
	if !ok {
		return errors.New("cannot read directory owner")
	}
	if uid != currentUID() {
		return fmt.Errorf("%s is owned by uid %d", dir, uid)
	}
	if err := noACLWrite(dir); err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// checkAncestor accepts a directory above the cache directory when it is owned
// by the current user or by root and no one else can add or remove entries in
// it, except in a directory with the sticky bit (such as /tmp), where users can
// only remove their own entries.
func checkAncestor(dir string, fi os.FileInfo) error {
	uid, _, ok := ownerOf(fi)
	if !ok {
		return errors.New("cannot read directory owner")
	}
	if uid != currentUID() && uid != 0 {
		return fmt.Errorf("%s is owned by uid %d", dir, uid)
	}
	if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("%s can be written by its group or by others (a home directory with mode 0775 and a private group counts: use mode 0755 or set LoadConfig.CacheDir)", dir)
	}
	return noACLWrite(dir)
}

// checkFile accepts a cached library that is owned by the current user, that
// no one else can write to and that has no other hard link.
func checkFile(p string, fi os.FileInfo) bool {
	uid, nlink, ok := ownerOf(fi)
	return ok && uid == currentUID() && fi.Mode().Perm()&0o022 == 0 && nlink == 1 && noACLWrite(p) == nil
}

// noACLWrite refuses an access list entry that allows writing: on macOS such an
// entry grants what the mode bits deny.
func noACLWrite(p string) error {
	w, err := aclAllowsWrite(p)
	if err != nil {
		return err
	}
	if w {
		return fmt.Errorf("%s has an access list entry that allows writing", p)
	}
	return nil
}

func makeDir(dir string) error { return os.Mkdir(dir, 0o700) }
