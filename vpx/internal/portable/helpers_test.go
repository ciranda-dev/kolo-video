package portable

import (
	"math"
	"testing"
)

type picture struct {
	w, h    int
	y, u, v []byte
}

// harness is the moving pattern of vpx_test.go testFrame with tight strides.
func harness(w, h, n int) *picture {
	cw, ch := (w+1)/2, (h+1)/2
	p := &picture{w: w, h: h, y: make([]byte, w*h), u: make([]byte, cw*ch), v: make([]byte, cw*ch)}
	for y := range h {
		for x := range w {
			v := 16 + (x+y+2*n)%200
			if x >= n%w && x < n%w+w/8 && y >= h/4 && y < h/4+h/8 {
				v = 230
			}
			p.y[y*w+x] = byte(v)
		}
	}
	for y := range ch {
		for x := range cw {
			p.u[y*cw+x] = byte(64 + (x+n)%128)
			p.v[y*cw+x] = byte(64 + (y+n)%128)
		}
	}
	return p
}

func lumaPSNR(p *picture, img Image) float64 {
	var sum float64
	for y := range p.h {
		for x := range p.w {
			d := float64(p.y[y*p.w+x]) - float64(img.Y[y*img.YStride+x])
			sum += d * d
		}
	}
	mse := sum / float64(p.w*p.h)
	if mse == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/mse)
}

// params returns kolo-video's single-thread settings for a harness size.
func params(vp9 bool, w, h int, kbps uint32) EncParams {
	p := EncParams{VP9: vp9, Width: w, Height: h, Threads: 1, Kbps: kbps, MaxIntraPct: 900, Speed: -6}
	switch {
	case vp9:
		p.Speed = 7
	case w*h < 352*288:
		p.Speed = -4
	}
	return p
}

type packet struct {
	data []byte
	key  bool
}

// encodeAll encodes n harness pictures; the first is a keyframe. Packets are
// copied.
func encodeAll(t testing.TB, p EncParams, n int) []packet {
	t.Helper()
	enc, err := NewEncoder(p)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var out []packet
	for i := range n {
		f := harness(p.Width, p.Height, i)
		err := enc.Encode(f.y, f.u, f.v, f.w, (f.w+1)/2, int64(i)*3000, 3000, i == 0, func(d []byte, k bool) {
			out = append(out, packet{append([]byte(nil), d...), k})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// camera is harder content than harness: a panning texture, a moving disc and
// per-picture noise.
func camera(w, h, n int) *picture {
	cw, ch := (w+1)/2, (h+1)/2
	p := &picture{w: w, h: h, y: make([]byte, w*h), u: make([]byte, cw*ch), v: make([]byte, cw*ch)}
	seed := uint32(n)*2654435761 + 1
	rnd := func() int {
		seed = seed*1664525 + 1013904223
		return int(seed>>24) & 7
	}
	for y := range h {
		for x := range w {
			xx, yy := x+n*2, y+n
			v := 60 + int(50*math.Sin(float64(xx)/23)*math.Cos(float64(yy)/31)) + (xx*yy/97)%40
			if (x-(n*5)%w)*(x-(n*5)%w)+(y-h/2)*(y-h/2) < (h/6)*(h/6) {
				v = 200 - (x+y)%30
			}
			p.y[y*w+x] = byte(min(235, max(16, v+rnd()-4)))
		}
	}
	for y := range ch {
		for x := range cw {
			p.u[y*cw+x] = byte(110 + (x+n)%40 + rnd()/2)
			p.v[y*cw+x] = byte(120 + (y*2+n)%30 + rnd()/2)
		}
	}
	return p
}
