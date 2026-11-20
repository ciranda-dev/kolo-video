package portable

import (
	"errors"
	"testing"
)

// headerBytes of each packet are left alone by FuzzDecode: they hold the frame
// size, and the vpx package refuses sizes above MaxPixels before a packet
// reaches the engine.
const headerBytes = 32

func typed(err error) bool {
	return errors.Is(err, ErrCorrupt) || errors.Is(err, ErrCodec) || errors.Is(err, ErrInvalidParam)
}

// FuzzDecode feeds a mutated packet to a decoder that has seen a valid
// prefix. It must return a typed error or a picture, and stay usable.
func FuzzDecode(f *testing.F) {
	for _, vp9 := range []bool{false, true} {
		for i := range encodeAll(f, params(vp9, 48, 32, 200), 6) {
			f.Add(vp9, uint8(i), uint16(headerBytes), uint8(1), uint16(0))
		}
	}
	streams := map[bool][]packet{
		false: encodeAll(f, params(false, 48, 32, 200), 6),
		true:  encodeAll(f, params(true, 48, 32, 200), 6),
	}
	f.Fuzz(func(t *testing.T, vp9 bool, index uint8, at uint16, xor uint8, trunc uint16) {
		pkts := streams[vp9]
		i := int(index) % len(pkts)
		dec, err := NewDecoder(vp9, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Close()
		for _, p := range pkts[:i] {
			if _, _, err := dec.Decode(p.data); err != nil {
				t.Fatalf("valid prefix: %v", err)
			}
		}
		bad := append([]byte(nil), pkts[i].data...)
		if n := len(bad); n > headerBytes {
			bad[headerBytes+int(at)%(n-headerBytes)] ^= xor
		}
		if trunc != 0 {
			bad = bad[:1+int(trunc)%len(bad)]
		}
		img, ok, err := dec.Decode(bad)
		if err != nil && !typed(err) {
			t.Fatalf("untyped error: %v", err)
		}
		if ok && (img.Width != 48 || img.Height != 32 || len(img.Y) < 48*31+48) {
			t.Fatalf("bad picture: %dx%d", img.Width, img.Height)
		}
		if _, ok, err := dec.Decode(pkts[0].data); err != nil || !ok {
			t.Fatalf("decoder unusable after a mutated packet: ok=%v err=%v", ok, err)
		}
	})
}

// FuzzEncode encodes arbitrary pictures with arbitrary strides and checks that
// every packet decodes.
func FuzzEncode(f *testing.F) {
	f.Add(false, uint8(16), uint8(16), uint8(0), uint16(300), []byte("seed"), true)
	f.Add(true, uint8(33), uint8(17), uint8(5), uint16(100), []byte{1, 2, 3, 4, 5, 6, 7, 8}, false)
	f.Fuzz(func(t *testing.T, vp9 bool, w, h, pad uint8, kbps uint16, pix []byte, key bool) {
		if w == 0 || h == 0 {
			return
		}
		p := params(vp9, int(w), int(h), uint32(kbps)+1)
		enc, err := NewEncoder(p)
		if err != nil {
			if !typed(err) {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		defer enc.Close()
		dec, err := NewDecoder(vp9, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Close()
		cw, ch := (int(w)+1)/2, (int(h)+1)/2
		ys, cs := int(w)+int(pad), cw+int(pad)
		fill := func(n int) []byte {
			b := make([]byte, n)
			for i := range b {
				if len(pix) > 0 {
					b[i] = pix[i%len(pix)] + byte(i/len(pix))
				}
			}
			return b
		}
		y, u, v := fill(ys*int(h)), fill(cs*ch), fill(cs*ch)
		for i := range 3 {
			err := enc.Encode(y, u, v, ys, cs, int64(i)*3000, 3000, key && i == 0, func(d []byte, k bool) {
				if k && i != 0 && !key {
					t.Errorf("keyframe %d not requested", i)
				}
				if _, _, err := dec.Decode(d); err != nil {
					t.Errorf("packet %d does not decode: %v", i, err)
				}
			})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
		}
	})
}
