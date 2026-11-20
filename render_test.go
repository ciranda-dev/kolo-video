package kolovideo

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"testing"
)

func solidFrame(t testing.TB, w, h int, c color.RGBA) *Frame {
	t.Helper()
	f, err := FromRGBA(w, h, solidRGBA(w, h, c.R, c.G, c.B), 4*w)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func near(a, b color.RGBA) bool {
	return absDiff(a.R, b.R) <= 3 && absDiff(a.G, b.G) <= 3 && absDiff(a.B, b.B) <= 3 && a.A == b.A
}

func TestPlacement(t *testing.T) {
	cases := []struct {
		db     image.Rectangle
		w, h   int
		mode   ScaleMode
		dr, sr image.Rectangle
	}{
		{image.Rect(0, 0, 200, 100), 100, 100, Fit, image.Rect(50, 0, 150, 100), image.Rect(0, 0, 100, 100)},
		{image.Rect(0, 0, 100, 200), 100, 100, Fit, image.Rect(0, 50, 100, 150), image.Rect(0, 0, 100, 100)},
		{image.Rect(10, 10, 74, 46), 64, 36, Fit, image.Rect(10, 10, 74, 46), image.Rect(0, 0, 64, 36)},
		{image.Rect(0, 0, 200, 100), 100, 100, Fill, image.Rect(0, 0, 200, 100), image.Rect(0, 25, 100, 75)},
		{image.Rect(0, 0, 100, 200), 100, 100, Fill, image.Rect(0, 0, 100, 200), image.Rect(25, 0, 75, 100)},
		{image.Rect(0, 0, 1, 1000), 1000, 2, Fit, image.Rect(0, 499, 1, 500), image.Rect(0, 0, 1000, 2)},
	}
	for _, c := range cases {
		dr, sr := placement(c.db, c.w, c.h, c.mode)
		if dr != c.dr || sr != c.sr {
			t.Errorf("%v %dx%d mode %d: %v %v, want %v %v", c.db, c.w, c.h, c.mode, dr, sr, c.dr, c.sr)
		}
	}
}

func TestRenderFit(t *testing.T) {
	red := color.RGBA{255, 0, 0, 255}
	f := solidFrame(t, 32, 32, red)
	dst := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for i := range dst.Pix {
		dst.Pix[i] = 99
	}
	var r Renderer
	if err := r.Render(dst, f); err != nil {
		t.Fatal(err)
	}
	black := color.RGBA{0, 0, 0, 255}
	for _, p := range []struct {
		x, y int
		want color.RGBA
	}{{0, 0, black}, {15, 31, black}, {16, 0, red}, {47, 31, red}, {48, 16, black}, {63, 31, black}} {
		if got := dst.RGBAAt(p.x, p.y); !near(got, p.want) {
			t.Errorf("(%d,%d) = %v, want %v", p.x, p.y, got, p.want)
		}
	}
}

func TestRenderFillScales(t *testing.T) {
	blue := color.RGBA{0, 0, 255, 255}
	f := solidFrame(t, 40, 30, blue)
	dst := image.NewRGBA(image.Rect(5, 5, 105, 205))
	r := Renderer{Scale: Fill}
	for range 2 {
		if err := r.Render(dst, f); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []image.Point{{5, 5}, {104, 204}, {55, 100}} {
		if got := dst.RGBAAt(p.X, p.Y); !near(got, blue) {
			t.Errorf("%v = %v", p, got)
		}
	}
}

func TestRenderSameSizeMatchesRGBA(t *testing.T) {
	f := benchFrame(30, 20)
	want, _ := f.RGBA()
	dst := image.NewRGBA(image.Rect(0, 0, 30, 20))
	if err := (&Renderer{}).Render(dst, f); err != nil {
		t.Fatal(err)
	}
	if string(dst.Pix) != string(want.Pix) {
		t.Fatal("same-size render differs from RGBA")
	}
}

func TestRenderErrors(t *testing.T) {
	f := benchFrame(4, 4)
	if err := (&Renderer{Scale: 7}).Render(image.NewRGBA(image.Rect(0, 0, 4, 4)), f); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal(err)
	}
	if err := (&Renderer{}).Render(nil, f); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal(err)
	}
	if err := (&Renderer{}).Render(image.NewRGBA(image.Rectangle{}), f); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkRender(b *testing.B) {
	for _, c := range []struct{ fw, fh, dw, dh int }{
		{1280, 720, 1280, 720},
		{1280, 720, 640, 360},
		{1920, 1080, 1280, 720},
		{640, 480, 1280, 720},
	} {
		b.Run(fmt.Sprintf("%dx%d_to_%dx%d", c.fw, c.fh, c.dw, c.dh), func(b *testing.B) {
			f := benchFrame(c.fw, c.fh)
			dst := image.NewRGBA(image.Rect(0, 0, c.dw, c.dh))
			var r Renderer
			for b.Loop() {
				if err := r.Render(dst, f); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
