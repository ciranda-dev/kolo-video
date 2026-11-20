//go:build windows && (amd64 || arm64)

package h264

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

// The real Media Foundation MFTs, on the machine that runs the tests. With
// KOLOVIDEO_H264_REQUIRE=1 absence fails; otherwise it skips. Frames are
// generated; no camera is read.

func mftRequire(t testing.TB) Limits {
	t.Helper()
	l := Capabilities()
	if l.Encode.Available && l.Decode.Available {
		return l
	}
	msg := fmt.Sprintf("H.264 is absent: encode %v, decode %v", l.Encode.Err, l.Decode.Err)
	if os.Getenv("KOLOVIDEO_H264_REQUIRE") == "1" {
		t.Fatal(msg)
	}
	t.Skip(msg)
	return l
}

// mftPattern returns frame t of a w by h test picture with tight strides: a
// gradient, a moving block, 48 pixel tiles that blink, and noise seeded by t,
// in limited range. The same (w, h, t) always gives the same planes, and the
// interop files of every engine use this generator.
func mftPattern(w, h, t int) (y, u, v []byte) {
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

func mftFrame(w, h, i int, fps float64) *kolovideo.Frame {
	y, u, v := mftPattern(w, h, i)
	return &kolovideo.Frame{
		Width: w, Height: h, Y: y, U: u, V: v, YStride: w, UVStride: (w + 1) / 2,
		Timestamp: time.Duration(float64(i) * float64(time.Second) / fps), Duration: time.Duration(float64(time.Second) / fps),
	}
}

func mftPSNR(a, b []byte) float64 {
	var se float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		se += d * d
	}
	if se == 0 {
		return 99
	}
	return 10 * math.Log10(255*255/(se/float64(len(a))))
}

// mftPlane copies the w by h corner of a strided plane.
func mftPlane(b []byte, stride, w, h int) []byte {
	out := make([]byte, 0, w*h)
	for r := 0; r < h; r++ {
		out = append(out, b[r*stride:r*stride+w]...)
	}
	return out
}

// mftFramePSNR is the PSNR of the Y, U and V planes of f against frame i of
// the generator.
func mftFramePSNR(f *kolovideo.Frame, i int) (yp, up, vp float64) {
	y, u, v := mftPattern(f.Width, f.Height, i)
	cw, ch := (f.Width+1)/2, (f.Height+1)/2
	return mftPSNR(y, mftPlane(f.Y, f.YStride, f.Width, f.Height)),
		mftPSNR(u, mftPlane(f.U, f.UVStride, cw, ch)),
		mftPSNR(v, mftPlane(f.V, f.UVStride, cw, ch))
}

func mftMS(xs []float64, p float64) float64 {
	s := slices.Clone(xs)
	sort.Float64s(s)
	return s[int(math.Min(float64(len(s)-1), p*float64(len(s))))]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// The interop file framing: for each packet a big-endian uint32 length, a
// big-endian int64 timestamp in nanoseconds, then the packet data.

func mftWriteStream(w io.Writer, pkts []Packet) error {
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

func mftReadStream(b []byte) ([]Packet, error) {
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

type mftRun struct {
	pkts       []Packet
	keys       []int
	encMS      []float64
	decMS      []float64
	minY, sumY float64
	minU, minV float64
	codec      string
	bitrate    float64
}

type mftOpts struct {
	w, h, n int
	profile Profile
	forceAt int
	fps     float64
	bps     int
}

// mftRoundTrip encodes n generated frames and decodes every packet with a
// decoder of its own.
func mftRoundTrip(t *testing.T, o mftOpts) mftRun {
	t.Helper()
	if o.fps == 0 {
		o.fps = 30
	}
	enc, err := NewEncoder(Config{Width: o.w, Height: o.h, FrameRate: o.fps, Profile: o.profile, Bitrate: o.bps})
	if err != nil {
		t.Fatalf("NewEncoder %dx%d %v: %v", o.w, o.h, o.profile, err)
	}
	defer enc.Close()
	dec, err := NewDecoder(DecoderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if enc.Hardware() || dec.Hardware() {
		t.Error("the inbox MFTs report hardware")
	}
	r := mftRun{minY: 99, minU: 99, minV: 99}
	total := 0
	for i := 0; i < o.n; i++ {
		f := mftFrame(o.w, o.h, i, o.fps)
		st := time.Now()
		pkts, err := enc.Encode(f, i == o.forceAt)
		r.encMS = append(r.encMS, ms(time.Since(st)))
		if err != nil || len(pkts) != 1 {
			t.Fatalf("frame %d: %d packets, %v", i, len(pkts), err)
		}
		p := pkts[0]
		if !bytes.HasPrefix(p.Data, []byte{0, 0, 0, 1}) {
			t.Fatalf("frame %d: no 4-byte start code", i)
		}
		nals, err := splitAnnexB(p.Data, maxNALs(o.w*o.h))
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		for _, n := range nals {
			if ty := nalType(n); ty != 1 && ty != 5 && ty != 7 && ty != 8 {
				t.Fatalf("frame %d: NAL type %d", i, ty)
			}
		}
		if p.Keyframe {
			r.keys = append(r.keys, i)
			sps, err := ParseKeyframe(p.Data)
			if err != nil || nalType(nals[0]) != 7 || nalType(nals[1]) != 8 || sps.Matrix != 6 || sps.FullRange {
				t.Fatalf("frame %d: keyframe does not start with the normalized SPS and the PPS: %+v %v", i, sps, err)
			}
			if r.codec == "" {
				r.codec = sps.Codec()
				if got, ok := sps.Profile(); !ok || got != o.profile {
					t.Errorf("asked for %v, the stream is %s", o.profile, r.codec)
				}
			}
		}
		total += len(p.Data)
		r.pkts = append(r.pkts, p)
		st = time.Now()
		out, err := dec.Decode(p)
		r.decMS = append(r.decMS, ms(time.Since(st)))
		if err != nil || out == nil {
			t.Fatalf("frame %d: decode %v, %v", i, out != nil, err)
		}
		if out.Width != o.w || out.Height != o.h {
			t.Fatalf("frame %d: decoded %dx%d, want %dx%d", i, out.Width, out.Height, o.w, o.h)
		}
		yp, up, vp := mftFramePSNR(out, i)
		r.minY, r.sumY = min(r.minY, yp), r.sumY+yp
		r.minU, r.minV = min(r.minU, up), min(r.minV, vp)
	}
	r.bitrate = float64(total*8) * o.fps / float64(o.n)
	return r
}

func TestMFTCapabilitiesReal(t *testing.T) {
	l := mftRequire(t)
	t.Logf("encode %+v", l.Encode)
	t.Logf("decode %+v", l.Decode)
	if l.Encode.Hardware || l.Decode.Hardware || l.Encode.Engine != "Media Foundation" || l.Decode.Engine != "Media Foundation" {
		t.Errorf("hardware or engine: %+v", l)
	}
	if l.Decode.MaxWidth != 4096 || l.Decode.MaxHeight != 2304 {
		t.Errorf("decode limits %dx%d", l.Decode.MaxWidth, l.Decode.MaxHeight)
	}
}

func TestMFTProfiles(t *testing.T) {
	l := mftRequire(t)
	want := []Profile{ConstrainedBaseline, Main, High}
	if !slices.Equal(l.Encode.Profiles, want) || !slices.Equal(l.Decode.Profiles, want) {
		t.Fatalf("profiles: encode %v, decode %v, want %v", l.Encode.Profiles, l.Decode.Profiles, want)
	}
	prefix := map[Profile]string{ConstrainedBaseline: "avc3.42c0", Main: "avc3.4d40", High: "avc3.6400"}
	for _, p := range want {
		enc, err := NewEncoder(Config{Width: 320, Height: 240, Profile: p})
		if err != nil {
			t.Fatalf("%v: %v", p, err)
		}
		pkts, err := enc.Encode(mftFrame(320, 240, 0, 30), false)
		enc.Close()
		if err != nil || len(pkts) != 1 {
			t.Fatalf("%v: %v", p, err)
		}
		sps, err := ParseKeyframe(pkts[0].Data)
		if err != nil || len(sps.Codec()) != 11 || sps.Codec()[:9] != prefix[p] {
			t.Errorf("%v: codec %s, %v", p, sps.Codec(), err)
		}
		t.Logf("%v: %s", p, sps.Codec())
	}
}

func TestMFTRoundTrip(t *testing.T) {
	mftRequire(t)
	type c struct {
		w, h int
		p    Profile
	}
	var cases []c
	if testing.Short() {
		cases = []c{{640, 480, ConstrainedBaseline}, {320, 240, High}}
	} else {
		for _, sz := range [][2]int{{640, 480}, {1280, 720}, {1920, 1080}} {
			for _, p := range []Profile{ConstrainedBaseline, Main, High} {
				cases = append(cases, c{sz[0], sz[1], p})
			}
		}
	}
	for _, c := range cases {
		n, force := 90, 45
		if testing.Short() {
			n, force = 45, 20
		}
		r := mftRoundTrip(t, mftOpts{w: c.w, h: c.h, n: n, profile: c.p, forceAt: force})
		t.Logf("%dx%d %v: %s, keyframes at %v, %.0f bit/s, encode p50 %.2f p99 %.2f max %.2f ms, decode p50 %.2f p99 %.2f ms, luma PSNR min %.2f mean %.2f dB, chroma min %.2f %.2f dB",
			c.w, c.h, c.p, r.codec, r.keys, r.bitrate, mftMS(r.encMS, .5), mftMS(r.encMS, .99), mftMS(r.encMS, 1), mftMS(r.decMS, .5), mftMS(r.decMS, .99),
			r.minY, r.sumY/float64(n), r.minU, r.minV)
		if want := []int{0, force}; !slices.Equal(r.keys, want) {
			t.Errorf("%dx%d %v: keyframes at %v, want %v", c.w, c.h, c.p, r.keys, want)
		}
		if r.minY < 35 || r.minU < 35 || r.minV < 35 {
			t.Errorf("%dx%d %v: PSNR below the 35 dB floor: %.2f %.2f %.2f", c.w, c.h, c.p, r.minY, r.minU, r.minV)
		}
	}
}

// Odd crops and the smallest size: the decoder's frame is the padded coded
// size and the picture is its top-left corner.
func TestMFTPaddedLayout(t *testing.T) {
	mftRequire(t)
	sizes := [][2]int{{48, 48}, {162, 98}, {1920, 1080}}
	if testing.Short() {
		sizes = sizes[:2]
	}
	for _, sz := range sizes {
		// The default bitrate of a picture this small is too low for 35 dB.
		r := mftRoundTrip(t, mftOpts{w: sz[0], h: sz[1], n: 12, profile: ConstrainedBaseline, forceAt: -1, bps: max(0, 1000000-sz[0]*sz[1]*10)})
		t.Logf("%dx%d: %s, luma min %.2f dB, chroma min %.2f %.2f dB", sz[0], sz[1], r.codec, r.minY, r.minU, r.minV)
		if r.minY < 35 || r.minU < 35 || r.minV < 35 {
			t.Errorf("%dx%d: PSNR %.2f %.2f %.2f", sz[0], sz[1], r.minY, r.minU, r.minV)
		}
	}
}

func TestMFTOnePerInput(t *testing.T) {
	mftRequire(t)
	enc, err := NewEncoder(Config{Width: 640, Height: 480})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := NewDecoder(DecoderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	for i := 0; i < 60; i++ {
		pkts, err := enc.Encode(mftFrame(640, 480, i, 30), false)
		if err != nil || len(pkts) != 1 {
			t.Fatalf("input %d: %d access units, %v", i, len(pkts), err)
		}
		f, err := dec.Decode(pkts[0])
		if err != nil || f == nil {
			t.Fatalf("input %d: no picture from the decoder (%v)", i, err)
		}
	}
}

func TestMFTLevel6Refused(t *testing.T) {
	mftRequire(t)
	if _, err := NewEncoder(Config{Width: 4096, Height: 4096}); !errors.Is(err, kolovideo.ErrInvalidConfig) {
		t.Errorf("4096x4096 encoder: %v", err)
	}
	dec, err := NewDecoder(DecoderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	// Level 6.0 would be dropped silently by the MFT, so the Go check refuses.
	for _, level := range []uint8{60, 52} {
		sps := testSPSFor(1920, 1080, 100, 0, level)
		p := Packet{Data: appendAnnexB(nil, [][]byte{sps}, [][]byte{testPPS(0, 0)}, [][]byte{testSlice(true, 0, 7, 0, 0, 1)})}
		if f, err := dec.Decode(p); f != nil || !errors.Is(err, ErrUnsupportedStream) {
			t.Errorf("level_idc %d: %v %v", level, f != nil, err)
		}
	}
}

// TestMFTCorrupt feeds mutated packets: the process stays up, every packet
// gives a frame or an error, and a clean stream decodes afterwards.
func TestMFTCorrupt(t *testing.T) {
	mftRequire(t)
	n, mutations := 90, 3000
	if testing.Short() {
		n, mutations = 45, 600
	}
	r := mftRoundTrip(t, mftOpts{w: 640, h: 480, n: n, profile: ConstrainedBaseline, forceAt: -1})
	dec, err := NewDecoder(DecoderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	rnd := rand.New(rand.NewSource(7))
	var frames, errs int
	kinds := map[string]int{}
	for i := 0; i < mutations; i++ {
		data := bytes.Clone(r.pkts[rnd.Intn(len(r.pkts))].Data)
		switch rnd.Intn(4) {
		case 0:
			for k := 0; k <= rnd.Intn(8); k++ {
				data[rnd.Intn(len(data))] ^= 1 << rnd.Intn(8)
			}
		case 1:
			data = data[:1+rnd.Intn(len(data))]
		case 2:
			a := rnd.Intn(len(data))
			copy(data[a:], data[rnd.Intn(len(data)):])
		default:
			data = bytes.Clone(r.pkts[rnd.Intn(len(r.pkts))].Data)
		}
		f, err := dec.Decode(Packet{Data: data})
		switch {
		case err != nil:
			errs++
			for _, e := range []error{ErrCorrupt, ErrNeedKeyframe, ErrUnsupportedStream, ErrTooLarge, ErrCodec} {
				if errors.Is(err, e) {
					kinds[e.Error()]++
				}
			}
		case f != nil:
			frames++
		}
	}
	t.Logf("%d mutations: %d frames, %d errors %v", mutations, frames, errs, kinds)
	dec2, err := NewDecoder(DecoderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer dec2.Close()
	ok := 0
	for _, p := range r.pkts {
		if f, err := dec.Decode(p); err == nil && f != nil {
			ok++
		}
	}
	// The mutated decoder needs its keyframe again, then decodes the stream.
	t.Logf("after the mutations the same decoder decoded %d of %d clean packets", ok, len(r.pkts))
	if ok < len(r.pkts)-1 {
		t.Errorf("the decoder did not recover: %d of %d", ok, len(r.pkts))
	}
	for i, p := range r.pkts {
		if f, err := dec2.Decode(p); err != nil || f == nil {
			t.Fatalf("a fresh decoder failed at packet %d: %v", i, err)
		}
	}
}

func mftHandles() uint32 {
	var n uint32
	p := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")
	p.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&n)))
	return n
}

func TestMFTOpenClose(t *testing.T) {
	mftRequire(t)
	cycles := 300
	if testing.Short() {
		cycles = 60
	}
	runtime.GC()
	g0, h0 := runtime.NumGoroutine(), mftHandles()
	start := time.Now()
	for i := 0; i < cycles; i++ {
		enc, err := NewEncoder(Config{Width: 64, Height: 48})
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		pkts, err := enc.Encode(mftFrame(64, 48, 0, 30), false)
		if err != nil || len(pkts) != 1 {
			t.Fatalf("cycle %d: %v", i, err)
		}
		dec, err := NewDecoder(DecoderConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if f, err := dec.Decode(pkts[0]); err != nil || f == nil {
			t.Fatalf("cycle %d: decode %v", i, err)
		}
		enc.Close()
		dec.Close()
		enc.Close()
	}
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	g1, h1 := runtime.NumGoroutine(), mftHandles()
	t.Logf("%d cycles in %v; goroutines %d to %d; handles %d to %d", cycles, time.Since(start).Round(time.Millisecond), g0, g1, h0, h1)
	if g1 > g0+2 {
		t.Errorf("goroutines grew from %d to %d", g0, g1)
	}
	if h1 > h0+20 {
		t.Errorf("handles grew from %d to %d", h0, h1)
	}
}

func TestMFTBitrateChange(t *testing.T) {
	mftRequire(t)
	enc, err := NewEncoder(Config{Width: 1280, Height: 720, Bitrate: 2000000, KeyframeInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var before, after, nb, na int
	for i := 0; i < 240; i++ {
		if i == 120 {
			if err := enc.SetBitrate(500000); err != nil {
				t.Fatal(err)
			}
		}
		pkts, err := enc.Encode(mftFrame(1280, 720, i, 30), false)
		if err != nil || len(pkts) != 1 {
			t.Fatalf("frame %d: %v", i, err)
		}
		switch {
		case i >= 60 && i < 120:
			before, nb = before+len(pkts[0].Data), nb+1
		case i >= 180:
			after, na = after+len(pkts[0].Data), na+1
		}
	}
	b, a := float64(before*8)*30/float64(nb), float64(after*8)*30/float64(na)
	t.Logf("bitrate %.0f bit/s before (target 2000000), %.0f after SetBitrate(500000)", b, a)
	if a > 750000 {
		t.Errorf("after the change the stream runs at %.0f bit/s", a)
	}
}

func TestMFTInterop(t *testing.T) {
	mftRequire(t)
	files, _ := filepath.Glob(filepath.Join("testdata", "interop-*.bin"))
	if len(files) == 0 {
		t.Skip("no interop files")
	}
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		pkts, err := mftReadStream(b)
		if err != nil || len(pkts) != 30 {
			t.Fatalf("%s: %d packets, %v", name, len(pkts), err)
		}
		dec, err := NewDecoder(DecoderConfig{})
		if err != nil {
			t.Fatal(err)
		}
		minY, sum := 99.0, 0.0
		for i, p := range pkts {
			f, err := dec.Decode(p)
			if err != nil || f == nil {
				t.Fatalf("%s packet %d: %v", name, i, err)
			}
			yp, _, _ := mftFramePSNR(f, i)
			minY, sum = min(minY, yp), sum+yp
		}
		sps, _ := dec.SPS()
		dec.Close()
		t.Logf("%s: 30 of 30 decoded (%s), luma PSNR min %.2f mean %.2f dB", filepath.Base(name), sps.Codec(), minY, sum/30)
		if minY < 35 {
			t.Errorf("%s: PSNR %.2f", name, minY)
		}
	}
}

// TestMFTExportInterop writes the interop files into the directory named by
// KOLOVIDEO_H264_INTEROP_OUT, or prints them as base64 lines when it is "-".
func TestMFTExportInterop(t *testing.T) {
	dir := os.Getenv("KOLOVIDEO_H264_INTEROP_OUT")
	if dir == "" {
		t.Skip("KOLOVIDEO_H264_INTEROP_OUT is not set")
	}
	mftRequire(t)
	for p, n := range map[Profile]string{ConstrainedBaseline: "cb", Main: "main", High: "high"} {
		r := mftRoundTrip(t, mftOpts{w: 320, h: 240, n: 30, profile: p, forceAt: -1})
		var buf bytes.Buffer
		if err := mftWriteStream(&buf, r.pkts); err != nil {
			t.Fatal(err)
		}
		name := "interop-mft-320x240-" + n + ".bin"
		if dir == "-" {
			fmt.Printf("INTEROP %s %s\n", name, base64.StdEncoding.EncodeToString(buf.Bytes()))
		} else if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d bytes, codec %s", name, buf.Len(), r.codec)
	}
}
