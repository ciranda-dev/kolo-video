package h264

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand"
	"testing"
	"time"
)

// vtPattern returns frame t of a w by h test picture with tight strides: a
// gradient, a moving block, 48 pixel tiles that blink, and noise seeded by t,
// in limited range. It is the generator of the interop files and the
// round-trip tests; the same (w, h, t) always gives the same planes.
func vtPattern(w, h, t int) (y, u, v []byte) {
	y = make([]byte, w*h)
	cw, ch := (w+1)/2, (h+1)/2
	u, v = make([]byte, cw*ch), make([]byte, cw*ch)
	// The noise is the rand.New(rand.NewSource(t)).Intn(5) stream, which the
	// committed interop files depend on. Past its first 607 outputs the source
	// is x[n] = x[n-607] + x[n-273], computed in a stack ring and written in
	// stack row chunks, so the race detector does not check every pixel.
	var ring [607]uint64
	src := rand.NewSource(int64(t)).(rand.Source64)
	for k := range ring {
		ring[k] = src.Uint64()
	}
	n := 0
	bx, by := (t*7)%max(1, w-64), (t*3)%max(1, h-64)
	var row [256]byte
	for j := 0; j < h; j++ {
		for i0 := 0; i0 < w; i0 += len(row) {
			m := min(len(row), w-i0)
			for k := 0; k < m; k++ {
				i := i0 + k
				val := 16 + ((i+3*t)*219/w+(j+t)*40/h)%220
				if (i/48+j/48+t/8)%5 == 0 {
					val = 200
				}
				if i >= bx && i < bx+64 && j >= by && j < by+64 {
					val = 40
				}
				for {
					x := ring[n%607]
					if n >= 607 {
						x += ring[(n+334)%607]
						ring[n%607] = x
					}
					n++
					// Int31n(5): bits 32 to 62, redrawn above the largest multiple of 5.
					if r := x << 1 >> 33; r <= 1<<31-1-(1<<31)%5 {
						val += int(r%5) - 2
						break
					}
				}
				row[k] = byte(max(16, min(235, val)))
			}
			copy(y[j*w+i0:], row[:m])
		}
	}
	for i := 0; i < cw; i++ {
		u[i] = byte(128 + (i*60/cw - 30))
	}
	for j := 0; j < ch; j++ {
		copy(u[j*cw:], u[:cw])
		r := v[j*cw : (j+1)*cw]
		for k := range r {
			r[k] = byte(128 + (j*60/ch - 30))
		}
	}
	return y, u, v
}

// vtPatternMathRand is the plain form of vtPattern, the reference it must match.
func vtPatternMathRand(w, h, t int) (y, u, v []byte) {
	y = make([]byte, w*h)
	cw, ch := (w+1)/2, (h+1)/2
	u, v = make([]byte, cw*ch), make([]byte, cw*ch)
	rnd := rand.New(rand.NewSource(int64(t)))
	bx, by := (t*7)%max(1, w-64), (t*3)%max(1, h-64)
	for j := 0; j < h; j++ {
		for i := 0; i < w; i++ {
			val := 16 + ((i+3*t)*219/w+(j+t)*40/h)%220
			if (i/48+j/48+t/8)%5 == 0 {
				val = 200
			}
			if i >= bx && i < bx+64 && j >= by && j < by+64 {
				val = 40
			}
			val += rnd.Intn(5) - 2
			y[j*w+i] = byte(max(16, min(235, val)))
		}
	}
	for j := 0; j < ch; j++ {
		for i := 0; i < cw; i++ {
			u[j*cw+i] = byte(128 + (i*60/cw - 30))
			v[j*cw+i] = byte(128 + (j*60/ch - 30))
		}
	}
	return y, u, v
}

// vtLumaPSNR is the peak signal to noise ratio of two equal luma planes,
// 99 when they are identical.
func vtLumaPSNR(a, b []byte) float64 {
	var se float64
	total := float64(len(a))
	// Stack chunks, so the race detector checks one range per chunk.
	var ca, cb [512]byte
	for len(a) > 0 {
		n := copy(ca[:], a)
		copy(cb[:n], b[:n])
		for i := 0; i < n; i++ {
			d := float64(ca[i]) - float64(cb[i])
			se += d * d
		}
		a, b = a[n:], b[n:]
	}
	if se == 0 {
		return 99
	}
	return 10 * math.Log10(255*255/(se/total))
}

// The interop file framing: for each packet a big-endian uint32 length, a
// big-endian int64 timestamp in nanoseconds, then the packet data.

func vtWriteStream(w io.Writer, pkts []Packet) error {
	for _, p := range pkts {
		var hdr [12]byte
		binary.BigEndian.PutUint32(hdr[:4], uint32(len(p.Data)))
		binary.BigEndian.PutUint64(hdr[4:], uint64(p.Timestamp))
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := w.Write(p.Data); err != nil {
			return err
		}
	}
	return nil
}

func vtReadStream(b []byte) ([]Packet, error) {
	var pkts []Packet
	for len(b) > 0 {
		if len(b) < 12 {
			return nil, fmt.Errorf("stream: %d bytes left, want a 12 byte header", len(b))
		}
		n := int(binary.BigEndian.Uint32(b[:4]))
		ts := time.Duration(int64(binary.BigEndian.Uint64(b[4:12])))
		b = b[12:]
		if n > len(b) {
			return nil, fmt.Errorf("stream: packet of %d bytes, %d left", n, len(b))
		}
		pkts = append(pkts, Packet{Data: b[:n:n], Timestamp: ts})
		b = b[n:]
	}
	return pkts, nil
}

func TestVTStreamFraming(t *testing.T) {
	in := []Packet{{Data: []byte{0, 0, 0, 1, 0x65, 1, 2}, Timestamp: 0}, {Data: []byte{0, 0, 0, 1, 0x41}, Timestamp: 33333333}}
	var buf bytes.Buffer
	if err := vtWriteStream(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := vtReadStream(buf.Bytes())
	if err != nil || len(out) != 2 || !bytes.Equal(out[1].Data, in[1].Data) || out[1].Timestamp != in[1].Timestamp {
		t.Fatalf("round trip: %v %v", out, err)
	}
	for _, bad := range [][]byte{buf.Bytes()[:5], buf.Bytes()[:15]} {
		if _, err := vtReadStream(bad); err == nil {
			t.Errorf("a truncated stream of %d bytes was accepted", len(bad))
		}
	}
}

func TestVTPatternDeterministic(t *testing.T) {
	y1, u1, v1 := vtPattern(96, 80, 3)
	y2, u2, v2 := vtPattern(96, 80, 3)
	if !bytes.Equal(y1, y2) || !bytes.Equal(u1, u2) || !bytes.Equal(v1, v2) {
		t.Fatal("the same frame differs between calls")
	}
	if y3, _, _ := vtPattern(96, 80, 4); bytes.Equal(y1, y3) {
		t.Fatal("two frames are equal")
	}
	if vtLumaPSNR(y1, y1) != 99 {
		t.Fatal("PSNR of equal planes")
	}
}

func TestVTPatternMatchesMathRand(t *testing.T) {
	// Seed 299 redraws once in Intn(5), at draw 204948.
	for _, c := range [][3]int{{1, 1, 5}, {33, 17, 0}, {96, 80, 3}, {320, 240, 29}, {640, 480, 299}, {641, 481, 3000}, {1280, 720, -3}, {1920, 1080, 119}} {
		y1, u1, v1 := vtPattern(c[0], c[1], c[2])
		y2, u2, v2 := vtPatternMathRand(c[0], c[1], c[2])
		if !bytes.Equal(y1, y2) || !bytes.Equal(u1, u2) || !bytes.Equal(v1, v2) {
			t.Errorf("vtPattern%v differs from the math/rand form", c)
		}
	}
}

func TestVTLumaPSNR(t *testing.T) {
	a := make([]byte, 1000)
	b := make([]byte, 1000)
	b[999] = 10
	// One error of 10 over 1000 pixels: MSE 0.1.
	if got, want := vtLumaPSNR(a, b), 10*math.Log10(255*255/0.1); math.Abs(got-want) > 1e-9 {
		t.Errorf("PSNR %v, want %v", got, want)
	}
}

func TestVTProbeParameterSets(t *testing.T) {
	sps, err := ParseKeyframe(appendAnnexB(nil, [][]byte{vtProbeSPS}, [][]byte{vtProbePPS}))
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := sps.Profile(); !ok || p != ConstrainedBaseline || sps.Width != 48 || sps.Height != 48 || sps.Matrix != 6 || sps.FullRange {
		t.Errorf("probe SPS %+v", sps)
	}
	if nalType(vtProbeSPS) != 7 || nalType(vtProbePPS) != 8 {
		t.Error("NAL types")
	}
}
