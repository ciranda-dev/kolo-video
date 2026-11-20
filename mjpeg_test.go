package kolovideo

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"testing"
)

func jpegBytes(t testing.TB, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// stripDHT removes every DHT segment, like cameras that rely on the
// default tables.
func stripDHT(t *testing.T, b []byte) []byte {
	out := append([]byte(nil), b[:2]...)
	i := 2
	for b[i+1] != 0xda {
		n := int(b[i+2])<<8 | int(b[i+3])
		if b[i+1] != 0xc4 {
			out = append(out, b[i:i+2+n]...)
		}
		i += 2 + n
	}
	if bytes.Contains(out, []byte{0xff, 0xc4}) {
		t.Fatal("DHT left")
	}
	return append(out, b[i:]...)
}

func TestDecodeMJPEG(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 32, 16))
	for i := 0; i < len(src.Pix); i += 4 {
		src.Pix[i], src.Pix[i+1], src.Pix[i+2], src.Pix[i+3] = 255, 0, 0, 255
	}
	data := jpegBytes(t, src)
	for name, d := range map[string][]byte{"with tables": data, "default tables": stripDHT(t, data)} {
		f, err := decodeMJPEG(d, 32, 16)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		img, _ := f.RGBA()
		if got := img.RGBAAt(10, 10); absDiff(got.R, 255) > 6 || got.G > 6 || got.B > 6 {
			t.Fatalf("%s: color %v", name, got)
		}
		if f.Y[0] < 76 || f.Y[0] > 88 {
			t.Fatalf("%s: Y %d not limited-range red", name, f.Y[0])
		}
	}
	gray := image.NewGray(image.Rect(0, 0, 8, 8))
	f, err := decodeMJPEG(jpegBytes(t, gray), 8, 8)
	if err != nil || f.Y[0] != 16 || f.U[0] != 128 {
		t.Fatalf("gray: %v", err)
	}
	if _, err := decodeMJPEG(data, 16, 16); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("size mismatch: %v", err)
	}
	for _, bad := range [][]byte{nil, {0xff, 0xd8}, {0xff, 0xd8, 0x00, 0x00}, {0xff, 0xd8, 0xff, 0xe0, 0x00, 0x01}, data[:len(data)/2]} {
		if _, err := decodeMJPEG(bad, 32, 16); err == nil {
			t.Errorf("% x accepted", bad)
		}
	}
}

func TestCopyYCbCrSubsampling(t *testing.T) {
	for _, r := range []image.YCbCrSubsampleRatio{image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio422, image.YCbCrSubsampleRatio420, image.YCbCrSubsampleRatio440} {
		m := image.NewYCbCr(image.Rect(0, 0, 5, 3), r)
		for i := range m.Cb {
			m.Cb[i], m.Cr[i] = 100, 200
		}
		f, _ := NewFrame(5, 3)
		copyYCbCr(f, m)
		for i := range f.U {
			if f.U[i] != 100 || f.V[i] != 200 {
				t.Fatalf("ratio %v: U %v V %v", r, f.U, f.V)
			}
		}
	}
}

func FuzzDecodeMJPEG(f *testing.F) {
	img := image.NewRGBA(image.Rect(0, 0, 16, 8))
	f.Add(jpegBytes(f, img))
	f.Add([]byte{0xff, 0xd8, 0xff, 0xda, 0x00, 0x02})
	f.Fuzz(func(t *testing.T, data []byte) {
		fr, err := decodeMJPEG(data, 16, 8)
		if err == nil && fr.Validate() != nil {
			t.Fatal("invalid frame")
		}
	})
}

func TestHuffmanTablesMarkers(t *testing.T) {
	src := image.NewGray(image.Rect(0, 0, 8, 8))
	plain := stripDHT(t, jpegBytes(t, src))
	// Fill bytes before a marker must be skipped.
	withFill := append([]byte{0xff, 0xd8, 0xff, 0xff}, plain[2:]...)
	for name, d := range map[string][]byte{"plain": plain, "fill": withFill} {
		out, err := withHuffmanTables(d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Contains(out, defaultDHT) {
			t.Fatalf("%s: tables not inserted", name)
		}
		if _, err := decodeMJPEG(d, 8, 8); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A standalone TEM marker has no length.
	tem := append([]byte{0xff, 0xd8, 0xff, 0x01}, plain[2:]...)
	if out, err := withHuffmanTables(tem); err != nil || !bytes.Contains(out, defaultDHT) {
		t.Fatalf("TEM: %v", err)
	}
	for _, bad := range [][]byte{{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x00, 0, 0}, {0xff, 0xd8, 0x12, 0x34}} {
		if _, err := withHuffmanTables(bad); !errors.Is(err, ErrInvalidFrame) {
			t.Errorf("% x: %v", bad, err)
		}
	}
}

// The frame is parsed from a private copy, never from the caller's buffer, which
// may be device memory that changes between two reads.
func TestWithHuffmanTablesCopies(t *testing.T) {
	data := jpegBytes(t, image.NewGray(image.Rect(0, 0, 8, 8)))
	for name, d := range map[string][]byte{"with tables": data, "default tables": stripDHT(t, data)} {
		out, err := withHuffmanTables(d)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if &out[0] == &d[0] {
			t.Errorf("%s: the result shares the caller's memory", name)
		}
	}
}
