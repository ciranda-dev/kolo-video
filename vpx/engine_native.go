package vpx

import (
	"unsafe"

	"github.com/ciranda-dev/kolo-video/vpx/internal/native"
)

type nativeEngine struct{ lib *native.Lib }

func loadNative(cfg LoadConfig) (engine, error) {
	lib, err := native.Load(native.Config{CacheDir: cfg.CacheDir, Library: cfg.Library})
	if err != nil {
		return nil, err
	}
	return &nativeEngine{lib}, nil
}

func (*nativeEngine) kind() EngineKind  { return EngineNative }
func (n *nativeEngine) version() string { return n.lib.Version }

func b2i(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

// addr converts an address returned by the library to a pointer.
func addr(a uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&a)) }

type nativeEncoder struct {
	lib *native.Lib
	e   uintptr
}

func (n *nativeEngine) newEncoder(p encParams) (encImpl, error) {
	var code int32
	e := n.lib.EncNew(b2i(p.vp9), int32(p.width), int32(p.height), p.kbps, int32(p.threads), int32(p.speed), p.maxIntraPct, &code)
	if e == 0 {
		return nil, &codecError{"create encoder", code}
	}
	return &nativeEncoder{n.lib, e}, nil
}

func (e *nativeEncoder) encode(y, u, v []byte, yStride, uvStride int, pts int64, dur uint32, key bool, emit func([]byte, bool)) error {
	if r := e.lib.Encode(e.e, unsafe.Pointer(&y[0]), int32(yStride), unsafe.Pointer(&u[0]), unsafe.Pointer(&v[0]), int32(uvStride), pts, dur, b2i(key)); r != 0 {
		return &codecError{"encode", r}
	}
	var out [3]uintptr
	for e.lib.NextFrame(e.e, &out) != 0 {
		emit(unsafe.Slice((*byte)(addr(out[0])), out[1]), out[2] != 0)
	}
	return nil
}

func (e *nativeEncoder) setBitrate(kbps uint32) error {
	if r := e.lib.SetBitrate(e.e, kbps); r != 0 {
		return &codecError{"set bitrate", r}
	}
	return nil
}

func (e *nativeEncoder) close() { e.lib.EncFree(e.e) }

type nativeDecoder struct {
	lib     *native.Lib
	d       uintptr
	corrupt bool
}

func (n *nativeEngine) newDecoder(vp9 bool, threads int) (decImpl, error) {
	var code int32
	d := n.lib.DecNew(b2i(vp9), int32(threads), &code)
	if d == 0 {
		return nil, &codecError{"create decoder", code}
	}
	return &nativeDecoder{lib: n.lib, d: d}, nil
}

func (d *nativeDecoder) decode(data []byte) (plane420, bool, error) {
	d.corrupt = false
	var o [8]uintptr
	if r := d.lib.Decode(d.d, unsafe.Pointer(&data[0]), uint32(len(data)), &o); r != 0 {
		return plane420{}, false, &codecError{"decode", r}
	}
	d.corrupt = o[7] != 0
	if o[0] == 0 {
		return plane420{}, false, nil
	}
	w, h := int(o[0]), int(o[1])
	cw, ch := (w+1)/2, (h+1)/2
	ys, cs := int(o[5]), int(o[6])
	if w < 1 || h < 1 || ys < w || cs < cw || o[2] == 0 || o[3] == 0 || o[4] == 0 {
		return plane420{}, false, &codecError{"decode", 1}
	}
	yl, cl := (h-1)*ys+w, (ch-1)*cs+cw
	return plane420{
		width: w, height: h, yStride: ys, uvStride: cs,
		y: unsafe.Slice((*byte)(addr(o[2])), yl),
		u: unsafe.Slice((*byte)(addr(o[3])), cl),
		v: unsafe.Slice((*byte)(addr(o[4])), cl),
	}, true, nil
}

func (d *nativeDecoder) corrupted() bool { return d.corrupt }
func (d *nativeDecoder) close()          { d.lib.DecFree(d.d) }
