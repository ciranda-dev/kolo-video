//go:build (darwin || linux) && (amd64 || arm64)

package portable

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"github.com/ebitengine/purego"
)

// native binds the kv_* functions of a libkolovpx shared library. The library
// is named by KOLOVIDEO_NATIVE_LIB, or is the artifact of vpx/internal/native
// for this platform; without either the tests that need it are skipped.
type native struct {
	encNew     func(vp9, w, h int32, kbps uint32, threads, speed int32, maxIntra uint32, err *int32) uintptr
	encFree    func(e uintptr)
	encode     func(e uintptr, y unsafe.Pointer, ys int32, u, v unsafe.Pointer, uvs int32, pts int64, dur uint32, key int32) int32
	nextFrame  func(e uintptr, out *[3]uintptr) int32
	decNew     func(vp9, threads int32, err *int32) uintptr
	decFree    func(d uintptr)
	decode     func(d uintptr, data unsafe.Pointer, n uint32, out *[8]uintptr) int32
	setBitrate func(e uintptr, kbps uint32) int32
}

func loadNative(t testing.TB) *native {
	t.Helper()
	path := os.Getenv("KOLOVIDEO_NATIVE_LIB")
	if path == "" {
		ext := map[string]string{"darwin": "dylib", "linux": "so"}[runtime.GOOS]
		path = filepath.Join("..", "native", fmt.Sprintf("libkolovpx-%s-%s.%s", runtime.GOOS, runtime.GOARCH, ext))
	}
	if _, err := os.Stat(path); err != nil {
		t.Skip("no native libkolovpx: set KOLOVIDEO_NATIVE_LIB")
	}
	h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		t.Skip("native library does not load: ", err)
	}
	n := &native{}
	purego.RegisterLibFunc(&n.encNew, h, "kv_enc_new")
	purego.RegisterLibFunc(&n.encFree, h, "kv_enc_free")
	purego.RegisterLibFunc(&n.encode, h, "kv_encode")
	purego.RegisterLibFunc(&n.nextFrame, h, "kv_next_frame")
	purego.RegisterLibFunc(&n.decNew, h, "kv_dec_new")
	purego.RegisterLibFunc(&n.decFree, h, "kv_dec_free")
	purego.RegisterLibFunc(&n.decode, h, "kv_decode")
	purego.RegisterLibFunc(&n.setBitrate, h, "kv_set_bitrate")
	return n
}

// addr converts an address returned by the library to a pointer.
func addr(a uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&a)) }

func b2i(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

func (n *native) encodeAll(t testing.TB, p EncParams, frames int) []packet {
	t.Helper()
	var r int32
	e := n.encNew(b2i(p.VP9), int32(p.Width), int32(p.Height), p.Kbps, int32(p.Threads), int32(p.Speed), p.MaxIntraPct, &r)
	if e == 0 {
		t.Fatalf("native encoder: %d", r)
	}
	defer n.encFree(e)
	var out []packet
	for i := range frames {
		f := harness(p.Width, p.Height, i)
		if r := n.encode(e, unsafe.Pointer(&f.y[0]), int32(f.w), unsafe.Pointer(&f.u[0]), unsafe.Pointer(&f.v[0]), int32((f.w+1)/2), int64(i)*3000, 3000, b2i(i == 0)); r != 0 {
			t.Fatalf("native encode: %d", r)
		}
		var o [3]uintptr
		for n.nextFrame(e, &o) != 0 {
			out = append(out, packet{append([]byte(nil), unsafe.Slice((*byte)(addr(o[0])), o[1])...), o[2] != 0})
		}
	}
	return out
}

type nativeDecoder struct {
	n *native
	d uintptr
}

func (n *native) newDecoder(t testing.TB, vp9 bool) *nativeDecoder {
	var r int32
	d := n.decNew(b2i(vp9), 1, &r)
	if d == 0 {
		t.Fatalf("native decoder: %d", r)
	}
	return &nativeDecoder{n, d}
}

func (d *nativeDecoder) close() { d.n.decFree(d.d) }

// decode returns the frame as tight planes, or nil when none is shown.
func (d *nativeDecoder) decode(p []byte) (*picture, error) {
	var o [8]uintptr
	if r := d.n.decode(d.d, unsafe.Pointer(&p[0]), uint32(len(p)), &o); r != 0 {
		return nil, fmt.Errorf("native decode: %d", r)
	}
	if o[0] == 0 {
		return nil, nil
	}
	w, h := int(o[0]), int(o[1])
	cw, ch := (w+1)/2, (h+1)/2
	pic := &picture{w: w, h: h, y: make([]byte, w*h), u: make([]byte, cw*ch), v: make([]byte, cw*ch)}
	for _, pl := range []struct {
		dst        []byte
		src        uintptr
		stride, pw int
		ph         int
	}{{pic.y, o[2], int(o[5]), w, h}, {pic.u, o[3], int(o[6]), cw, ch}, {pic.v, o[4], int(o[6]), cw, ch}} {
		for y := range pl.ph {
			copy(pl.dst[y*pl.pw:], unsafe.Slice((*byte)(addr(pl.src+uintptr(y*pl.stride))), pl.pw))
		}
	}
	return pic, nil
}

func tight(img Image) *picture {
	w, h := img.Width, img.Height
	cw, ch := (w+1)/2, (h+1)/2
	pic := &picture{w: w, h: h, y: make([]byte, w*h), u: make([]byte, cw*ch), v: make([]byte, cw*ch)}
	for y := range h {
		copy(pic.y[y*w:], img.Y[y*img.YStride:][:w])
	}
	for y := range ch {
		copy(pic.u[y*cw:], img.U[y*img.UVStride:][:cw])
		copy(pic.v[y*cw:], img.V[y*img.UVStride:][:cw])
	}
	return pic
}

func samePicture(a, b *picture) bool {
	return a.w == b.w && a.h == b.h && bytes.Equal(a.y, b.y) && bytes.Equal(a.u, b.u) && bytes.Equal(a.v, b.v)
}

// TestNativeStreams checks that one-thread streams are byte-identical to the
// native engine's.
func TestNativeStreams(t *testing.T) {
	n := loadNative(t)
	for _, c := range []struct {
		vp9  bool
		w, h int
	}{{false, 320, 240}, {false, 161, 99}, {true, 320, 240}, {true, 161, 99}, {false, 640, 480}, {true, 640, 480}} {
		t.Run(fmt.Sprintf("vp9=%v/%dx%d", c.vp9, c.w, c.h), func(t *testing.T) {
			p := params(c.vp9, c.w, c.h, 2000)
			want := n.encodeAll(t, p, 60)
			got := encodeAll(t, p, 60)
			if len(got) != len(want) {
				t.Fatalf("%d packets, native %d", len(got), len(want))
			}
			for i := range got {
				if got[i].key != want[i].key || !bytes.Equal(got[i].data, want[i].data) {
					t.Fatalf("packet %d differs from native", i)
				}
			}
		})
	}
}

// TestCorruptAgainstNative mutates valid packets after a valid prefix and
// requires both engines to accept or reject alike on at least 2995 of 3000
// runs per codec, and every portable trap to be recovered. The pictures of
// packets both accept are only logged: on a corrupt packet the SIMD code of
// the native library and the plain C code of the portable build can compute
// different values from out-of-range coefficients (a native build without SIMD
// matches).
func TestCorruptAgainstNative(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	n := loadNative(t)
	for _, vp9 := range []bool{false, true} {
		t.Run(fmt.Sprintf("vp9=%v", vp9), func(t *testing.T) {
			p := params(vp9, 160, 120, 300)
			p.Speed = map[bool]int{false: -6, true: 7}[vp9]
			var pkts [][]byte
			enc, err := NewEncoder(p)
			if err != nil {
				t.Fatal(err)
			}
			for i := range 30 {
				f := camera(160, 120, i)
				if err := enc.Encode(f.y, f.u, f.v, f.w, (f.w+1)/2, int64(i)*3000, 3000, i == 0, func(d []byte, k bool) {
					pkts = append(pkts, append([]byte(nil), d...))
				}); err != nil {
					t.Fatal(err)
				}
			}
			enc.Close()
			r := rand.New(rand.NewPCG(1, 2))
			var same, traps, pictureDiff, accepted int
			for k := range 3000 {
				nd := n.newDecoder(t, vp9)
				wd, err := NewDecoder(vp9, 1)
				if err != nil {
					t.Fatal(err)
				}
				cut := 1 + r.IntN(len(pkts)-1)
				for _, p := range pkts[:cut] {
					if _, err := nd.decode(p); err != nil {
						t.Fatal(err)
					}
					if _, _, err := wd.Decode(p); err != nil {
						t.Fatal(err)
					}
				}
				bad := append([]byte(nil), pkts[cut]...)
				for range 1 + r.IntN(8) {
					bad[r.IntN(len(bad))] ^= byte(1 + r.IntN(255))
				}
				if k%3 == 0 {
					bad = bad[:r.IntN(len(bad))+1]
				}
				np, nerr := nd.decode(bad)
				img, ok, werr := wd.Decode(bad)
				if werr != nil {
					var trap *TrapError
					if errors.As(werr, &trap) {
						traps++
						if _, _, err := wd.Decode(pkts[0]); err != nil {
							t.Fatalf("decoder not replaced after a trap: %v", err)
						}
					}
				}
				if (nerr == nil) == (werr == nil) {
					same++
					if nerr == nil {
						accepted++
						if ok != (np != nil) || ok && !samePicture(np, tight(img)) {
							pictureDiff++
						}
					}
				}
				nd.close()
				wd.Close()
			}
			t.Logf("same accept/reject %d of 3000, portable traps %d, accepted by both %d, differing pictures %d", same, traps, accepted, pictureDiff)
			if same < 2995 {
				t.Errorf("engines agree on %d of 3000, want at least 2995", same)
			}
		})
	}
}
