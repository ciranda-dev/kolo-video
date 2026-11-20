package native

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/ebitengine/purego"
)

// From sys/attr.h and sys/kauth.h.
const (
	attrCmnExtendedSecurity = 0x00400000
	fsoptNoFollow           = 0x1
	kauthFilesecMagic       = 0x012cc16d
	kauthNoACL              = 0xffffffff
	kauthACEKindMask        = 0xf
	kauthACEPermit          = 1
	kauthACEOnlyInherit     = 1 << 8
	// Write data or add a file, delete, append or add a directory, delete a
	// child, write attributes, extended attributes or the security, take
	// ownership, generic all and generic write.
	kauthWriteRights = 1<<2 | 1<<4 | 1<<5 | 1<<6 | 1<<8 | 1<<10 | 1<<12 | 1<<13 | 1<<21 | 1<<23
)

// attrList is struct attrlist; only the common attributes are asked for.
type attrList struct {
	bitmapCount, _ uint16
	commonAttr     uint32
	_              [4]uint32
}

var attrs struct {
	once        sync.Once
	err         error
	getattrlist func(path *byte, al *attrList, buf *byte, size uintptr, opts uint32) int32
}

// aclAllowsWrite reports whether an entry of the extended access list of path,
// which the mode bits do not show, lets anyone write, add or delete entries or
// change the permissions.
func aclAllowsWrite(path string) (bool, error) {
	attrs.once.Do(func() {
		lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			attrs.err = err
			return
		}
		sym, err := purego.Dlsym(lib, "getattrlist")
		if err != nil {
			attrs.err = err
			return
		}
		purego.RegisterFunc(&attrs.getattrlist, sym)
	})
	if attrs.err != nil {
		return false, attrs.err
	}
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return false, err
	}
	// An access list holds at most 128 entries of 24 bytes.
	buf := make([]byte, 8192)
	al := attrList{bitmapCount: 5, commonAttr: attrCmnExtendedSecurity}
	if attrs.getattrlist(p, &al, &buf[0], uintptr(len(buf)), fsoptNoFollow) != 0 {
		return false, fmt.Errorf("cannot read the access list of %s", path)
	}
	return aclWrites(buf)
}

var errACL = errors.New("malformed access list")

// aclWrites reads a getattrlist reply for ATTR_CMN_EXTENDED_SECURITY: its
// length, an attrreference, then a kauth_filesec in host byte order.
func aclWrites(b []byte) (bool, error) {
	le := binary.NativeEndian
	if len(b) < 12 {
		return false, errACL
	}
	n := int64(le.Uint32(b))
	if n < 12 || n > int64(len(b)) {
		return false, errACL
	}
	size := int64(le.Uint32(b[8:]))
	if size == 0 {
		return false, nil
	}
	start := 4 + int64(int32(le.Uint32(b[4:])))
	if start < 12 || size < 44 || start+size > n {
		return false, errACL
	}
	fs := b[start : start+size]
	if le.Uint32(fs) != kauthFilesecMagic {
		return false, errACL
	}
	count := le.Uint32(fs[36:])
	if count == kauthNoACL {
		return false, nil
	}
	if uint64(count)*24 > uint64(len(fs)-44) {
		return false, errACL
	}
	for i := range int(count) {
		ace := fs[44+24*i:]
		flags, rights := le.Uint32(ace[16:]), le.Uint32(ace[20:])
		if flags&kauthACEKindMask == kauthACEPermit && flags&kauthACEOnlyInherit == 0 && rights&kauthWriteRights != 0 {
			return true, nil
		}
	}
	return false, nil
}
