package kolovideo

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"runtime"
	"testing"
)

func TestNewFrame(t *testing.T) {
	f, err := NewFrame(5, 3)
	if err != nil {
		t.Fatal(err)
	}
	if f.YStride != 5 || f.UVStride != 3 || len(f.Y) != 15 || len(f.U) != 6 || len(f.V) != 6 {
		t.Fatalf("layout %+v", f)
	}
	if f.Y[0] != 16 || f.U[0] != 128 || f.V[5] != 128 {
		t.Fatal("not black")
	}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	f.U = append(f.U, 1)
	if f.V[0] != 128 {
		t.Fatal("appending to U overwrote V")
	}
	for _, s := range [][2]int{{0, 1}, {1, 0}, {-2, 2}, {maxDimension + 1, 2}} {
		if _, err := NewFrame(s[0], s[1]); !errors.Is(err, ErrInvalidFrame) {
			t.Errorf("%v: %v", s, err)
		}
	}
}

func TestValidate(t *testing.T) {
	good := func() *Frame { f, _ := NewFrame(4, 4); return f }
	cases := map[string]func(*Frame) *Frame{
		"nil":          func(*Frame) *Frame { return nil },
		"zero":         func(*Frame) *Frame { return &Frame{} },
		"short Y":      func(f *Frame) *Frame { f.Y = f.Y[:15]; return f },
		"short V":      func(f *Frame) *Frame { f.V = f.V[:3]; return f },
		"small stride": func(f *Frame) *Frame { f.YStride = 3; return f },
		"neg stride":   func(f *Frame) *Frame { f.UVStride = -2; return f },
		"big stride":   func(f *Frame) *Frame { f.YStride = math.MaxInt; return f },
		"huge size":    func(f *Frame) *Frame { f.Width = math.MaxInt; return f },
	}
	for name, mod := range cases {
		f := mod(good())
		if err := f.Validate(); !errors.Is(err, ErrInvalidFrame) {
			t.Errorf("%s: %v", name, err)
		}
		if f.YCbCr() != nil {
			t.Errorf("%s: YCbCr of invalid frame", name)
		}
		if _, err := f.RGBA(); err == nil {
			t.Errorf("%s: RGBA of invalid frame", name)
		}
		if err := (&Renderer{}).Render(image.NewRGBA(image.Rect(0, 0, 4, 4)), f); err == nil {
			t.Errorf("%s: Render of invalid frame", name)
		}
	}
	// A padded stride with exactly enough bytes for the last row is valid.
	f := &Frame{Width: 3, Height: 2, Y: make([]byte, 8+3), YStride: 8, U: make([]byte, 2), V: make([]byte, 2), UVStride: 2}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestYCbCrSharesPlanes(t *testing.T) {
	f, _ := NewFrame(6, 4)
	img := f.YCbCr()
	img.Y[img.YOffset(5, 3)] = 200
	if f.Y[3*f.YStride+5] != 200 {
		t.Fatal("YCbCr copied the planes")
	}
	if img.Bounds() != image.Rect(0, 0, 6, 4) || img.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		t.Fatal("wrong geometry")
	}
}

// Reference values: BT.601 limited range from the 8-bit integer formulas.
var knownColors = []struct {
	r, g, b byte
	y, u, v byte
}{
	{0, 0, 0, 16, 128, 128},
	{255, 255, 255, 235, 128, 128},
	{255, 0, 0, 82, 90, 240},
	{0, 255, 0, 144, 54, 34},
	{0, 0, 255, 41, 240, 110},
	{128, 128, 128, 126, 128, 128},
}

func solidRGBA(w, h int, r, g, b byte) []byte {
	pix := make([]byte, 4*w*h)
	for i := 0; i < len(pix); i += 4 {
		pix[i], pix[i+1], pix[i+2], pix[i+3] = r, g, b, 255
	}
	return pix
}

func TestKnownColors(t *testing.T) {
	for _, c := range knownColors {
		for _, s := range [][2]int{{4, 4}, {3, 5}} {
			f, err := FromRGBA(s[0], s[1], solidRGBA(s[0], s[1], c.r, c.g, c.b), 4*s[0])
			if err != nil {
				t.Fatal(err)
			}
			if f.Y[len(f.Y)-1] != c.y || f.U[len(f.U)-1] != c.u || f.V[0] != c.v {
				t.Errorf("RGB %d,%d,%d: YUV %d,%d,%d want %d,%d,%d", c.r, c.g, c.b,
					f.Y[len(f.Y)-1], f.U[len(f.U)-1], f.V[0], c.y, c.u, c.v)
			}
			img, _ := f.RGBA()
			got := img.RGBAAt(s[0]-1, s[1]-1)
			if absDiff(got.R, c.r) > 2 || absDiff(got.G, c.g) > 2 || absDiff(got.B, c.b) > 2 || got.A != 255 {
				t.Errorf("RGB %d,%d,%d round trip: %v", c.r, c.g, c.b, got)
			}
		}
	}
}

func absDiff(a, b byte) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func TestRoundTripGradient(t *testing.T) {
	const w, h = 64, 48
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			// Smooth in each 2x2 block, so chroma subsampling loses little.
			src.SetRGBA(x, y, color.RGBA{byte(x / 2 * 8), byte(y / 2 * 10), byte(255 - x/2*8), 255})
		}
	}
	f, err := FromRGBA(w, h, src.Pix, src.Stride)
	if err != nil {
		t.Fatal(err)
	}
	bgra := make([]byte, len(src.Pix))
	for i := 0; i < len(bgra); i += 4 {
		bgra[i], bgra[i+1], bgra[i+2], bgra[i+3] = src.Pix[i+2], src.Pix[i+1], src.Pix[i], 0
	}
	g, err := FromBGRA(w, h, bgra, 4*w)
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Y) != string(g.Y) || string(f.U) != string(g.U) || string(f.V) != string(g.V) {
		t.Fatal("BGRA and RGBA disagree")
	}
	out, _ := f.RGBA()
	worst := 0
	for i := range src.Pix {
		worst = max(worst, absDiff(src.Pix[i], out.Pix[i]))
	}
	if worst > 3 {
		t.Fatalf("round trip error %d", worst)
	}
}

func TestFromNV12(t *testing.T) {
	const w, h = 5, 3
	yp := make([]byte, 8*h)
	uv := make([]byte, 8*2)
	for i := range yp {
		yp[i] = byte(i)
	}
	for i := range uv {
		uv[i] = byte(100 + i)
	}
	f, err := FromNV12(w, h, yp, 8, uv, 8)
	if err != nil {
		t.Fatal(err)
	}
	if f.Y[w+1] != yp[8+1] || f.U[1] != uv[2] || f.V[1] != uv[3] || f.U[3] != uv[8] || f.V[5] != uv[13] {
		t.Fatalf("NV12 copy wrong: %v %v %v", f.Y, f.U, f.V)
	}
	if _, err := FromNV12(w, h, yp[:8*2+4], 8, uv, 8); !errors.Is(err, ErrInvalidFrame) {
		t.Fatal("short Y accepted")
	}
	if _, err := FromNV12(w, h, yp, 8, uv, 5); !errors.Is(err, ErrInvalidFrame) {
		t.Fatal("UV stride below width accepted")
	}
}

func TestFromPacked422(t *testing.T) {
	const w, h = 4, 3
	row := func(y0, u, y1, v byte) []byte { return []byte{y0, u, y1, v, y0 + 1, u + 1, y1 + 1, v + 1} }
	var yuy2, uyvy []byte
	for r := range byte(h) {
		p := row(10*r+16, 50+10*r, 10*r+17, 150+10*r)
		yuy2 = append(yuy2, p...)
		uyvy = append(uyvy, p[1], p[0], p[3], p[2], p[5], p[4], p[7], p[6])
	}
	a, err := FromYUY2(w, h, yuy2, 2*w)
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromUYVY(w, h, uyvy, 2*w)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*Frame{a, b} {
		if f.Y[0] != 16 || f.Y[1] != 17 || f.Y[2] != 17 || f.Y[w] != 26 || f.Y[2*w+3] != 38 {
			t.Fatalf("Y %v", f.Y)
		}
		// Rows 0 and 1 average; the odd last row stands alone.
		if f.U[0] != 55 || f.V[1] != 156 || f.U[2] != 70 || f.V[3] != 171 {
			t.Fatalf("U %v V %v", f.U, f.V)
		}
	}
	if _, err := FromYUY2(3, 2, make([]byte, 12), 6); !errors.Is(err, ErrInvalidFrame) {
		t.Fatal("odd width accepted")
	}
	if _, err := FromUYVY(4, 2, make([]byte, 15), 8); !errors.Is(err, ErrInvalidFrame) {
		t.Fatal("short buffer accepted")
	}
}

func TestRec709To601(t *testing.T) {
	// Encode RGB with BT.709, convert, decode with BT.601: colors survive.
	enc709 := func(r, g, b float64) (y, u, v byte) {
		yy := 0.2126*r + 0.7152*g + 0.0722*b
		q := func(x float64) byte { return byte(math.Round(x)) }
		return q(16 + 219*yy/255), q(128 + 224*(b-yy)/1.8556/255), q(128 + 224*(r-yy)/1.5748/255)
	}
	for _, c := range knownColors {
		f, _ := NewFrame(2, 2)
		y, u, v := enc709(float64(c.r), float64(c.g), float64(c.b))
		fill(f.Y, y)
		f.U[0], f.V[0] = u, v
		f.rec709to601()
		img, _ := f.RGBA()
		got := img.RGBAAt(1, 1)
		if absDiff(got.R, c.r) > 3 || absDiff(got.G, c.g) > 3 || absDiff(got.B, c.b) > 3 {
			t.Errorf("RGB %d,%d,%d after 709 to 601: %v", c.r, c.g, c.b, got)
		}
	}
}

func TestFullToLimited(t *testing.T) {
	f, _ := NewFrame(2, 2)
	fill(f.Y, 255)
	f.U[0], f.V[0] = 0, 255
	f.fullToLimited()
	if f.Y[3] != 235 || f.U[0] != 16 || f.V[0] != 240 {
		t.Fatalf("Y %d U %d V %d", f.Y[3], f.U[0], f.V[0])
	}
	fill(f.Y, 0)
	f.U[0] = 128
	f.fullToLimited()
	if f.Y[0] != 16 || f.U[0] != 128 {
		t.Fatalf("Y %d U %d", f.Y[0], f.U[0])
	}
}

func FuzzConvert(f *testing.F) {
	f.Add(uint8(0), 4, 4, 16, []byte{1, 2, 3})
	f.Add(uint8(1), 3, 1, 3, make([]byte, 20))
	f.Add(uint8(2), 2, 2, 4, make([]byte, 8))
	f.Fuzz(func(t *testing.T, kind uint8, w, h, stride int, data []byte) {
		var fr *Frame
		var err error
		switch kind % 5 {
		case 0:
			fr, err = FromNV12(w, h, data, stride, data, stride)
		case 1:
			fr, err = FromYUY2(w, h, data, stride)
		case 2:
			fr, err = FromUYVY(w, h, data, stride)
		case 3:
			fr, err = FromBGRA(w, h, data, stride)
		case 4:
			fr, err = FromRGBA(w, h, data, stride)
		}
		if err != nil {
			return
		}
		if err := fr.Validate(); err != nil {
			t.Fatal(err)
		}
		if fr.Width != w || fr.Height != h {
			t.Fatalf("size %dx%d, want %dx%d", fr.Width, fr.Height, w, h)
		}
	})
}

func FuzzFrame(f *testing.F) {
	f.Add(4, 4, 4, 2, 16, 4, 4)
	f.Fuzz(func(t *testing.T, w, h, ys, uvs, ny, nu, nv int) {
		if ny < 0 || nu < 0 || nv < 0 || ny > 1<<16 || nu > 1<<16 || nv > 1<<16 {
			return
		}
		fr := &Frame{Width: w, Height: h, YStride: ys, UVStride: uvs, Y: make([]byte, ny), U: make([]byte, nu), V: make([]byte, nv)}
		if fr.Validate() != nil {
			return
		}
		img, err := fr.RGBA()
		if err != nil || img.Rect.Dx() != w {
			t.Fatal(err)
		}
		if err := (&Renderer{Scale: Fill}).Render(image.NewRGBA(image.Rect(0, 0, 7, 3)), fr); err != nil {
			t.Fatal(err)
		}
		if c := copyFrame(fr); c.Validate() != nil {
			t.Fatal("copy invalid")
		}
	})
}

func benchFrame(w, h int) *Frame {
	f, _ := NewFrame(w, h)
	for i := range f.Y {
		f.Y[i] = byte(16 + i%200)
	}
	return f
}

var benchSizes = [][2]int{{640, 480}, {1280, 720}, {1920, 1080}}

func BenchmarkToRGBA(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(fmt.Sprintf("%dx%d", s[0], s[1]), func(b *testing.B) {
			f := benchFrame(s[0], s[1])
			dst := image.NewRGBA(image.Rect(0, 0, s[0], s[1]))
			for b.Loop() {
				toRGBA(dst, dst.Rect, f)
			}
		})
	}
}

func BenchmarkFromConversions(b *testing.B) {
	const w, h = 1280, 720
	nv := make([]byte, w*h*3/2)
	packed := make([]byte, 2*w*h)
	rgba := make([]byte, 4*w*h)
	for _, c := range []struct {
		name string
		fn   func() (*Frame, error)
	}{
		{"NV12", func() (*Frame, error) { return FromNV12(w, h, nv, w, nv[w*h:], w) }},
		{"YUY2", func() (*Frame, error) { return FromYUY2(w, h, packed, 2*w) }},
		{"BGRA", func() (*Frame, error) { return FromBGRA(w, h, rgba, 4*w) }},
	} {
		b.Run(c.name+"/1280x720", func(b *testing.B) {
			for b.Loop() {
				if _, err := c.fn(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestHugeSizeShortBuffer(t *testing.T) {
	small := make([]byte, 64)
	for name, fn := range map[string]func() (*Frame, error){
		"NV12": func() (*Frame, error) { return FromNV12(65536, 65536, small, 65536, small, 65536) },
		"YUY2": func() (*Frame, error) { return FromYUY2(65536, 65536, small, 131072) },
		"RGBA": func() (*Frame, error) { return FromRGBA(65536, 65536, small, 262144) },
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if _, err := fn(); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("%s: %v", name, err)
		}
		runtime.ReadMemStats(&after)
		if after.TotalAlloc-before.TotalAlloc > 1<<20 {
			t.Fatalf("%s allocated %d bytes before rejecting", name, after.TotalAlloc-before.TotalAlloc)
		}
	}
}
