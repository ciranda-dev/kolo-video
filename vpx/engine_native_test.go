package vpx

import (
	"errors"
	"testing"
	"unsafe"

	"github.com/ciranda-dev/kolo-video/vpx/internal/native"
)

// layoutDecoder is a native decoder whose library reports the given picture layout.
func layoutDecoder(report func(o *[8]uintptr)) *nativeDecoder {
	lib := &native.Lib{Decode: func(_ uintptr, _ unsafe.Pointer, _ uint32, o *[8]uintptr) int32 {
		report(o)
		return 0
	}}
	return &nativeDecoder{lib: lib, d: 1}
}

// TestNativeDecodeValidatesLayout checks the picture layout that the library
// reports before any slice is made from it: a bad stride or a null plane is an
// error, never a panic or a frame.
func TestNativeDecodeValidatesLayout(t *testing.T) {
	const w, h = 6, 4
	y, u, v := make([]byte, 64), make([]byte, 32), make([]byte, 32)
	good := func(o *[8]uintptr) {
		o[0], o[1] = w, h
		o[2], o[3], o[4] = uintptr(unsafe.Pointer(&y[0])), uintptr(unsafe.Pointer(&u[0])), uintptr(unsafe.Pointer(&v[0]))
		o[5], o[6] = 8, 4
	}
	img, ok, err := layoutDecoder(good).decode([]byte{1})
	if err != nil || !ok || img.width != w || img.height != h || len(img.y) != (h-1)*8+w || len(img.u) != (h/2-1)*4+w/2 {
		t.Fatalf("valid layout: %+v %v %v", img, ok, err)
	}
	neg := ^uintptr(0) // a stride of -1 as the library's signed int
	for name, mutate := range map[string]func(o *[8]uintptr){
		"luma stride below width":   func(o *[8]uintptr) { o[5] = w - 1 },
		"chroma stride below width": func(o *[8]uintptr) { o[6] = w/2 - 1 },
		"negative luma stride":      func(o *[8]uintptr) { o[5] = neg },
		"negative chroma stride":    func(o *[8]uintptr) { o[6] = neg },
		"zero luma stride":          func(o *[8]uintptr) { o[5] = 0 },
		"null luma plane":           func(o *[8]uintptr) { o[2] = 0 },
		"null u plane":              func(o *[8]uintptr) { o[3] = 0 },
		"null v plane":              func(o *[8]uintptr) { o[4] = 0 },
		"zero height":               func(o *[8]uintptr) { o[1] = 0 },
		"negative height":           func(o *[8]uintptr) { o[1] = neg },
		"negative width":            func(o *[8]uintptr) { o[0] = neg },
	} {
		_, ok, err := layoutDecoder(func(o *[8]uintptr) { good(o); mutate(o) }).decode([]byte{1})
		var ce *codecError
		if ok || !errors.As(err, &ce) {
			t.Errorf("%s: ok=%v err=%v, want a codec error", name, ok, err)
		}
	}
	// No picture at all is not an error.
	if _, ok, err := layoutDecoder(func(o *[8]uintptr) {}).decode([]byte{1}); ok || err != nil {
		t.Errorf("empty output: ok=%v err=%v", ok, err)
	}
}
