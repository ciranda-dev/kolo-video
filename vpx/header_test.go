package vpx

import (
	"bytes"
	"errors"
	"testing"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

// vp9Header writes the leading bits of a VP9 uncompressed header.
func vp9Header(bits ...[2]int) []byte {
	var out []byte
	var cur byte
	n := 0
	for _, b := range bits {
		for i := b[1] - 1; i >= 0; i-- {
			cur = cur<<1 | byte(b[0]>>i&1)
			if n++; n == 8 {
				out = append(out, cur)
				cur, n = 0, 0
			}
		}
	}
	if n > 0 {
		out = append(out, cur<<(8-n))
	}
	return append(out, 0, 0, 0, 0)
}

func TestVP9Info(t *testing.T) {
	sync := [][2]int{{0x49, 8}, {0x83, 8}, {0x42, 8}}
	key := append([][2]int{{2, 2}, {0, 1}, {0, 1}, {0, 1}, {0, 1}, {1, 1}, {0, 1}}, sync...)
	key = append(key, [2]int{1, 3}, [2]int{0, 1}, [2]int{639, 16}, [2]int{479, 16})
	info, err := vp9Info(vp9Header(key...))
	if err != nil || !info.key || info.width != 640 || info.height != 480 {
		t.Fatalf("key frame: %+v %v", info, err)
	}

	// Inter frame with explicit size: show_frame=1, error_resilient=1.
	inter := [][2]int{{2, 2}, {0, 1}, {0, 1}, {0, 1}, {1, 1}, {1, 1}, {1, 1}, {0xff, 8}, {0, 12}, {0, 3}, {9999, 16}, {9999, 16}}
	info, err = vp9Info(vp9Header(inter...))
	if err != nil || info.key || info.width != 10000 || info.height != 10000 {
		t.Fatalf("inter frame: %+v %v", info, err)
	}

	// Inter frame taking its size from reference 1.
	ref := [][2]int{{2, 2}, {0, 1}, {0, 1}, {0, 1}, {1, 1}, {1, 1}, {1, 1}, {0xff, 8}, {0, 12}, {0, 1}, {1, 1}}
	info, err = vp9Info(vp9Header(ref...))
	if err != nil || info.width != 0 {
		t.Fatalf("size from reference: %+v %v", info, err)
	}

	// Hidden intra-only frame: show_frame=0, intra_only=1.
	intra := append([][2]int{{2, 2}, {0, 1}, {0, 1}, {0, 1}, {1, 1}, {0, 1}, {1, 1}, {1, 1}}, sync...)
	intra = append(intra, [2]int{0xff, 8}, [2]int{99, 16}, [2]int{49, 16})
	info, err = vp9Info(vp9Header(intra...))
	if err != nil || info.key || info.width != 100 || info.height != 50 {
		t.Fatalf("intra-only: %+v %v", info, err)
	}

	if _, err := vp9Info(vp9Header([2]int{2, 2}, [2]int{1, 1}, [2]int{0, 1})); !errors.Is(err, kolovideo.ErrUnsupportedFormat) {
		t.Fatalf("profile 1: %v", err)
	}
	if _, err := vp9Info([]byte{0x00}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad marker: %v", err)
	}
	if _, err := vp9Info(vp9Header(key[:7]...)[:2]); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated: %v", err)
	}
	rgb := append(append([][2]int{}, key[:7+3]...), [2]int{7, 3})
	if _, err := vp9Info(vp9Header(rgb...)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("RGB in profile 0: %v", err)
	}
}

func TestSuperframe(t *testing.T) {
	a := bytes.Repeat([]byte{1}, 300)
	b := bytes.Repeat([]byte{2}, 5)
	// Two frames, 2-byte sizes: marker 0b110_01_001.
	idx := []byte{0xc9, 0x2c, 0x01, 0x05, 0x00, 0xc9}
	pkt := append(append(append([]byte{}, a...), b...), idx...)
	frames, indexed, err := vp9Frames(pkt)
	if err != nil || !indexed || len(frames) != 2 || !bytes.Equal(frames[0], a) || !bytes.Equal(frames[1], b) {
		t.Fatalf("split: %d frames, %v %v", len(frames), indexed, err)
	}
	for _, bad := range [][]byte{
		{},
		append(append([]byte{}, a[:10]...), idx...),
		{0x01, 0xc0, 0x00, 0xc0},
		// A 4-byte size with the top bit set; int overflow on 32-bit.
		{0xd8, 0x01, 0x00, 0x00, 0x80, 0xd8},
	} {
		if _, _, err := vp9Frames(bad); !errors.Is(err, ErrCorrupt) {
			t.Errorf("% x: %v", bad, err)
		}
	}
	// A marker-like last byte without its twin is a plain frame.
	for _, one := range [][]byte{{0xc0}, append(append([]byte{}, a...), 0xc9, 0x2c, 0x01, 0x05, 0x00, 0xc8)} {
		frames, indexed, err := vp9Frames(one)
		if err != nil || indexed || len(frames) != 1 {
			t.Errorf("% x: %d frames, %v", one[len(one)-1:], len(frames), err)
		}
	}
	for _, n := range []int{1, 255, 256, 70000} {
		f := bytes.Repeat([]byte{7}, n)
		frames, indexed, err := vp9Frames(appendSuperframeIndex(f))
		if err != nil || !indexed || len(frames) != 1 || !bytes.Equal(frames[0], f) {
			t.Fatalf("wrap %d bytes: %v", n, err)
		}
	}
}

func TestVP8Info(t *testing.T) {
	key := []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x80, 0x02, 0xe0, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	info, err := vp8Info(key)
	if err != nil || !info.key || info.width != 640 || info.height != 480 {
		t.Fatalf("key: %+v %v", info, err)
	}
	for _, bad := range [][]byte{
		{0x00},
		{0x00, 0x00, 0x00, 0x9d, 0x01, 0x2b, 0, 0, 0, 0},
		{0x00, 0x00, 0x00, 0x9d, 0x01, 0x2a, 0, 0, 1, 0},
		{0xf1, 0xff, 0xff, 0x00},
	} {
		if _, err := vp8Info(bad); !errors.Is(err, ErrCorrupt) {
			t.Errorf("% x: %v", bad, err)
		}
	}
}

func FuzzVP9Headers(f *testing.F) {
	f.Add([]byte{0x82, 0x49, 0x83, 0x42, 0x00, 0x13, 0xf0, 0x0e, 0xf0})
	f.Add(appendSuperframeIndex([]byte{0x86, 0x00, 0x40}))
	f.Fuzz(func(t *testing.T, b []byte) {
		frames, _, err := vp9Frames(b)
		if err != nil {
			return
		}
		for _, fr := range frames {
			if len(fr) == 0 {
				t.Fatal("empty frame")
			}
			info, err := vp9Info(fr)
			if err == nil && (info.width < 0 || info.height < 0 || info.width > 65536 || info.height > 65536) {
				t.Fatalf("size %dx%d", info.width, info.height)
			}
		}
	})
}

func FuzzVP8Header(f *testing.F) {
	f.Add([]byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x80, 0x02, 0xe0, 0x01})
	f.Fuzz(func(t *testing.T, b []byte) {
		info, err := vp8Info(b)
		if err == nil && (info.width > 16383 || info.height > 16383) {
			t.Fatalf("size %dx%d", info.width, info.height)
		}
	})
}

// TestHostileHeaders checks that the size in a frame header is refused before
// libvpx sees the packet: the bytes after the header are junk, so a decoder
// that let libvpx start would answer ErrCorrupt (or allocate the frame).
func TestHostileHeaders(t *testing.T) {
	junk := bytes.Repeat([]byte{0xa5}, 64)
	sync := [][2]int{{0x49, 8}, {0x83, 8}, {0x42, 8}}
	vp9Key := func(w, h int) []byte {
		bits := append([][2]int{{2, 2}, {0, 1}, {0, 1}, {0, 1}, {0, 1}, {1, 1}, {0, 1}}, sync...)
		bits = append(bits, [2]int{1, 3}, [2]int{0, 1}, [2]int{w - 1, 16}, [2]int{h - 1, 16})
		return append(vp9Header(bits...), junk...)
	}
	vp8Key := func(w, h int) []byte {
		return append([]byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, byte(w), byte(w >> 8), byte(h), byte(h >> 8)}, junk...)
	}
	// superframe joins frames behind a real index with 2-byte sizes.
	superframe := func(frames ...[]byte) []byte {
		marker := byte(0xc0 | 1<<3 | (len(frames) - 1))
		var out, index []byte
		index = append(index, marker)
		for _, f := range frames {
			out = append(out, f...)
			index = append(index, byte(len(f)), byte(len(f)>>8))
		}
		return append(append(out, index...), marker)
	}
	cases := map[string]struct {
		codec Codec
		data  []byte
	}{
		"VP8 16383x16383":              {VP8, vp8Key(16383, 16383)},
		"VP8 4097x2177":                {VP8, vp8Key(4097, 2177)},
		"VP9 65536x65536":              {VP9, vp9Key(65536, 65536)},
		"VP9 4097x2177":                {VP9, vp9Key(4097, 2177)},
		"VP9 superframe, giant second": {VP9, superframe(vp9Key(64, 48), vp9Key(65536, 65536))},
		"VP9 wide and flat":            {VP9, vp9Key(65536, 200)},
		"VP8 wide and flat":            {VP8, vp8Key(16383, 600)},
	}
	eachEngine(t, func(t *testing.T) {
		for name, c := range cases {
			dec := newDecoder(t, DecoderConfig{Codec: c.codec})
			if _, err := dec.Decode(Packet{Data: c.data}); !errors.Is(err, ErrTooLarge) {
				t.Errorf("%s: %v, want ErrTooLarge", name, err)
			}
			// A refused packet leaves the decoder waiting for a keyframe, and a good one is accepted.
			pkts := encodeAll(t, Config{Codec: c.codec, Width: 64, Height: 48}, 2)
			if _, err := dec.Decode(pkts[1]); !errors.Is(err, ErrNeedKeyframe) {
				t.Errorf("%s: inter frame after a refused packet: %v", name, err)
			}
			if f, err := dec.Decode(pkts[0]); err != nil || f == nil || f.Width != 64 {
				t.Errorf("%s: keyframe after a refused packet: %v %v", name, f, err)
			}
		}
	})
}

// TestDecodeLeavesCallerBytes checks that the decoder works on its own copy of
// a packet: the caller's slice, and the spare capacity behind it where the
// one-frame index of a VP9 packet would be written, are not touched.
func TestDecodeLeavesCallerBytes(t *testing.T) {
	eachEngine(t, func(t *testing.T) {
		for _, codec := range []Codec{VP8, VP9} {
			for i, p := range encodeAll(t, Config{Codec: codec, Width: 64, Height: 48}, 3) {
				spare := bytes.Repeat([]byte{0x5a}, len(p.Data)+32)
				copy(spare, p.Data)
				want := bytes.Clone(spare)
				dec := newDecoder(t, DecoderConfig{Codec: codec})
				if i > 0 {
					dec.Decode(encodeAll(t, Config{Codec: codec, Width: 64, Height: 48}, 1)[0])
				}
				if _, err := dec.Decode(Packet{Data: spare[:len(p.Data)], Keyframe: p.Keyframe}); err != nil {
					t.Fatalf("%v packet %d: %v", codec, i, err)
				}
				if !bytes.Equal(spare, want) {
					t.Fatalf("%v packet %d: the decoder changed the caller's memory", codec, i)
				}
			}
		}
	})
}
