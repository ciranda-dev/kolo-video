package h264

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"
)

// testSPS writes any SPS, including values the parser refuses.
type testSPS struct {
	f          spsFields
	header     byte // 0x67 if zero
	interlaced bool
	depth8     [2]uint32
	bypass     bool
	badTrail   bool
	zeroTail   int // zero bytes after rbsp_trailing_bits
}

func (t testSPS) nal() []byte {
	w := &bitWriter{}
	f := &t.f
	w.u(uint32(f.profileIDC), 8)
	w.u(uint32(f.constraints), 8)
	w.u(uint32(f.levelIDC), 8)
	w.ue(f.id)
	if f.profileIDC == 100 {
		w.ue(f.chromaFormatIDC)
		w.ue(t.depth8[0])
		w.ue(t.depth8[1])
		w.flag(t.bypass)
		w.flag(f.scalingMatrix)
		if f.scalingMatrix {
			for _, l := range f.scalingLists {
				w.flag(l != nil)
				for _, d := range l {
					w.se(d)
				}
			}
		}
	}
	w.ue(f.log2MaxFrameNum4)
	w.ue(f.pocType)
	switch f.pocType {
	case 0:
		w.ue(f.log2MaxPocLsb4)
	case 1:
		w.flag(f.deltaPicOrderAlwaysZero)
		w.se(f.offsetNonRef)
		w.se(f.offsetTopBottom)
		w.ue(uint32(len(f.offsetRefFrame)))
		for _, o := range f.offsetRefFrame {
			w.se(o)
		}
	}
	w.ue(f.maxRefFrames)
	w.flag(f.gaps)
	w.ue(f.widthMbs1)
	w.ue(f.heightMapUnits1)
	w.flag(!t.interlaced)
	if t.interlaced {
		w.flag(false)
	}
	w.flag(f.direct8x8)
	w.flag(f.cropping)
	if f.cropping {
		w.ue(f.cropL)
		w.ue(f.cropR)
		w.ue(f.cropT)
		w.ue(f.cropB)
	}
	w.flag(f.vuiPresent)
	if f.vuiPresent {
		writeVUI(w, &f.vui)
	}
	if t.badTrail {
		w.u(0, 8)
	} else {
		w.trailing()
	}
	w.b = append(w.b, make([]byte, t.zeroTail)...)
	h := t.header
	if h == 0 {
		h = 0x67
	}
	return escapeRBSP([]byte{h}, w.b)
}

// baseSPS is the 1280x720 Constrained Baseline SPS of the worked example.
func baseSPS() testSPS {
	return testSPS{header: 0x27, f: spsFields{
		profileIDC: 66, constraints: 0xc0, levelIDC: 31, chromaFormatIDC: 1,
		log2MaxFrameNum4: 1, log2MaxPocLsb4: 2, maxRefFrames: 1,
		widthMbs1: 79, heightMapUnits1: 44, direct8x8: true,
	}}
}

// testSPSFor writes an SPS for a w x h picture with pic_order_cnt_type 2 and
// 4-bit frame_num, the fields the fake encoder's slices use.
func testSPSFor(w, h int, profile, constraints, level uint8) []byte {
	wm, hm := (w+15)/16, (h+15)/16
	t := testSPS{f: spsFields{
		profileIDC: profile, constraints: constraints, levelIDC: level, chromaFormatIDC: 1,
		pocType: 2, maxRefFrames: 1, widthMbs1: uint32(wm - 1), heightMapUnits1: uint32(hm - 1), direct8x8: true,
	}}
	if cr, cb := (16*wm-w)/2, (16*hm-h)/2; cr != 0 || cb != 0 {
		t.f.cropping, t.f.cropR, t.f.cropB = true, uint32(cr), uint32(cb)
	}
	return t.nal()
}

type testPPSFields struct {
	id, spsID     uint32
	entropy       bool
	bottomPOC     bool
	groups        uint32
	qp            int32
	redundant     bool
	ext           bool
	transform8x8  bool
	scalingMatrix bool
	badTrail      bool
	bipred        uint32
	zeroTail      int
}

func (p testPPSFields) nal() []byte {
	w := &bitWriter{}
	w.ue(p.id)
	w.ue(p.spsID)
	w.flag(p.entropy)
	w.flag(p.bottomPOC)
	w.ue(p.groups)
	if p.groups != 0 {
		w.ue(0) // slice_group_map_type 0
		for range p.groups + 1 {
			w.ue(0)
		}
	}
	w.ue(0)
	w.ue(0)
	w.flag(false)
	w.u(p.bipred, 2)
	w.se(p.qp)
	w.se(0)
	w.se(0)
	w.flag(true)
	w.flag(false)
	w.flag(p.redundant)
	if p.ext {
		w.flag(p.transform8x8)
		w.flag(p.scalingMatrix)
		if p.scalingMatrix {
			n := 6
			if p.transform8x8 {
				n = 8
			}
			for range n {
				w.flag(true)
				w.se(-8) // use the default list
			}
		}
		w.se(0)
	}
	if p.badTrail {
		w.u(0, 8)
	} else {
		w.trailing()
	}
	w.b = append(w.b, make([]byte, p.zeroTail)...)
	return escapeRBSP([]byte{0x68}, w.b)
}

func testPPS(id, spsID uint32) []byte { return testPPSFields{id: id, spsID: spsID}.nal() }

// testSliceHeader writes a slice: the header up to the picture order count,
// a few payload bytes and the trailing bits.
type testSliceHeader struct {
	idr       bool
	refIDC    int
	firstMb   uint32
	sliceType uint32
	ppsID     uint32
	frameNum  uint32
	frameBits int
	idrPicID  uint32
	pocLsb    uint32
	pocBits   int
}

func (s testSliceHeader) nal() []byte {
	w := &bitWriter{}
	w.ue(s.firstMb)
	w.ue(s.sliceType)
	w.ue(s.ppsID)
	w.u(s.frameNum, s.frameBits)
	if s.idr {
		w.ue(s.idrPicID)
	}
	if s.pocBits > 0 {
		w.u(s.pocLsb, s.pocBits)
	}
	w.u(0x5a5a5a, 24)
	w.trailing()
	h := byte(s.refIDC<<5 | nalSlice)
	if s.idr {
		h = byte(s.refIDC<<5 | nalIDR)
	}
	return escapeRBSP([]byte{h}, w.b)
}

// testSlice writes a slice for testSPSFor and testPPS(0, 0).
func testSlice(idr bool, firstMb, sliceType, ppsID, frameNum, idrPicID uint32) []byte {
	return testSliceHeader{idr: idr, refIDC: 2, firstMb: firstMb, sliceType: sliceType, ppsID: ppsID,
		frameNum: frameNum, frameBits: 4, idrPicID: idrPicID}.nal()
}

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// probeSPS are the parameter sets of the streams measured on the system
// encoders: the macOS encoder (vt) and the Windows encoder (mf).
var probeSPS = []struct {
	name, sps, pps, codec string
	width, height         int
	matrix                uint8
}{
	{"vt 640x480 CB", "2742c01eab40501ec8", "28ce3c80", "avc3.42c01e", 640, 480, 2},
	{"vt 1280x720 CB", "2742c01fab402802dc80", "28ce3c80", "avc3.42c01f", 1280, 720, 2},
	{"vt 1920x1080 CB", "2742c028ab403c0113f2a0", "28ce3c80", "avc3.42c028", 1920, 1080, 2},
	{"vt 1280x720 Main", "274d001fab402802dc80", "28ee3c80", "avc3.4d001f", 1280, 720, 2},
	{"vt 1280x720 High", "2764001fac56805005b9", "28ee3cb0", "avc3.64001f", 1280, 720, 2},
	{"vt 1280x720 Baseline software", "2742e01fa9180a00b7602d4080808c2b5ef7c040", "28de09c8", "avc3.42e01f", 1280, 720, 2},
	{"vt 3840x2160 High", "27640033ac56803c0043e4", "28ee3cb0", "avc3.640033", 3840, 2160, 2},
	{"vt 160x98 CB", "2742c00bab4187f08c42", "28ce3c80", "avc3.42c00b", 160, 98, 2},
	{"vt 48x48 CB", "2742c00bab4186f024c328", "28ce3c80", "avc3.42c00b", 48, 48, 2},
	{"vt 1280x720 CB BT.601", "2742c01fab402802dd3506010602", "28ce3c80", "avc3.42c01f", 1280, 720, 6},
	{"mf 320x240 CB", "6742c00d95a0507e84000003000400000300f03682211a80", "68ce3880", "avc3.42c00d", 320, 240, 2},
	{"mf 320x240 Main", "674d400d95a0507e84000003000400000300f03682211a80", "68ce3880", "avc3.4d400d", 320, 240, 2},
	{"mf 320x240 High", "6764000dac2b40a0fd08000003000800000301e06d044235", "68ce3830", "avc3.64000d", 320, 240, 2},
}

// mfDerived returns a Windows encoder SPS with another level and size, for
// the measured codec strings whose bytes were not kept.
func mfDerived(t testing.TB, profileHex string, level uint8, w, h int) []byte {
	t.Helper()
	for _, p := range probeSPS {
		if p.name == "mf 320x240 "+profileHex {
			s, err := parseSPS(unhex(t, p.sps), DefaultMaxPixels)
			if err != nil {
				t.Fatal(err)
			}
			ts := testSPS{f: s.f, header: 0x67}
			ts.f.levelIDC = level
			ts.f.widthMbs1, ts.f.heightMapUnits1 = uint32((w+15)/16-1), uint32((h+15)/16-1)
			return ts.nal()
		}
	}
	t.Fatal("no stream", profileHex)
	return nil
}

func TestProbeSPS(t *testing.T) {
	for _, p := range probeSPS {
		t.Run(p.name, func(t *testing.T) {
			pkt := appendAnnexB(nil, [][]byte{unhex(t, p.sps), unhex(t, p.pps)})
			s, err := ParseKeyframe(pkt)
			if err != nil {
				t.Fatal(err)
			}
			if s.Codec() != p.codec || s.Width != p.width || s.Height != p.height || s.Matrix != p.matrix || s.FullRange {
				t.Fatalf("got %s %dx%d matrix %d full %v", s.Codec(), s.Width, s.Height, s.Matrix, s.FullRange)
			}
			if _, ok := s.Profile(); !ok {
				t.Fatalf("%s not classified", s.Codec())
			}
			sp := &paramSets{}
			if err := sp.add([][]byte{unhex(t, p.sps)}, [][]byte{unhex(t, p.pps)}, DefaultMaxPixels, keepSPS); err != nil {
				t.Fatalf("PPS: %v", err)
			}
		})
	}
	for _, c := range []struct {
		profile, codec string
		level          uint8
	}{{"CB", "avc3.42c01e", 30}, {"Main", "avc3.4d401e", 30}, {"High", "avc3.64001e", 30}} {
		s, err := ParseKeyframe(appendAnnexB(nil, [][]byte{mfDerived(t, c.profile, c.level, 640, 480)}))
		if err != nil || s.Codec() != c.codec || s.Width != 640 || s.Height != 480 {
			t.Fatalf("mf 640x480 %s: %v %s %dx%d", c.profile, err, s.Codec(), s.Width, s.Height)
		}
	}
	refused := map[string][]byte{
		"vt 4096x2304 High avc3.640034": unhex(t, "27640034ac568010001219"),
		"mf 4096x4096 High avc3.64003c": mfDerived(t, "High", 60, 4096, 4096),
	}
	for name, sps := range refused {
		_, err := ParseKeyframe(appendAnnexB(nil, [][]byte{sps}))
		if !errors.Is(err, ErrUnsupportedStream) || !bytes.Contains([]byte(err.Error()), []byte("level_idc")) {
			t.Errorf("%s: %v, want the level check", name, err)
		}
	}
}

func keepSPS(nal []byte, s *spsInfo) ([]byte, *spsInfo, error) { return nal, s, nil }

func TestProfileClassification(t *testing.T) {
	for _, c := range []struct {
		idc, cons, level uint8
		want             Profile
		ok               bool
	}{
		{66, 0xc0, 31, ConstrainedBaseline, true},
		{66, 0xe0, 31, ConstrainedBaseline, true},
		{66, 0x40, 31, ConstrainedBaseline, true},
		{66, 0x00, 31, 0, false},
		{66, 0x50, 11, ConstrainedBaseline, true}, // level 1b
		{66, 0x50, 31, ConstrainedBaseline, true},
		{66, 0x48, 31, 0, false},
		{77, 0x80, 31, ConstrainedBaseline, true},
		{77, 0x00, 31, Main, true},
		{77, 0x40, 30, Main, true},
		{77, 0x20, 30, 0, false},
		{100, 0, 51, High, true},
		{100, 0x08, 51, 0, false},
		{88, 0, 30, 0, false},
	} {
		p, ok := SPS{ProfileIDC: c.idc, Constraints: c.cons, Level: c.level}.Profile()
		if p != c.want || ok != c.ok {
			t.Errorf("%d %02x %d: %v %v, want %v %v", c.idc, c.cons, c.level, p, ok, c.want, c.ok)
		}
	}
	if ConstrainedBaseline.String() != "Constrained Baseline" || High.String() != "High" || Main.String() != "Main" || Profile(9).String() != "Profile(9)" {
		t.Error("Profile.String")
	}
}

func fullVUI() vuiInfo {
	return vuiInfo{
		aspectRatioPresent: true, aspectRatioIDC: 255, sarWidth: 4, sarHeight: 3,
		overscanPresent: true, overscanAppropriate: true,
		videoSignalPresent: true, videoFormat: 2, fullRange: true, colourPresent: true, primaries: 1, transfer: 1, matrix: 1,
		chromaLocPresent: true, chromaLocTop: 5, chromaLocBottom: 2,
		timingPresent: true, unitsInTick: 1001, timeScale: 60000, fixedFrameRate: true,
		nalHRD:      &hrdInfo{bitRateScale: 3, cpbSizeScale: 4, cpbs: []hrdCPB{{100, 200, true}, {4294967294, 5, false}}, initialDelayLen1: 23, removalDelayLen1: 23, outputDelayLen1: 23, timeOffsetLen: 24},
		vclHRD:      &hrdInfo{cpbs: []hrdCPB{{1, 1, false}}},
		lowDelayHRD: true, picStructPresent: true,
		restriction: true, mvOverPicBoundaries: true, maxBytesPerPicDenom: 16, maxBitsPerMbDenom: 16,
		log2MaxMvLengthH: 16, log2MaxMvLengthV: 15, maxNumReorderFrames: 1, maxDecFrameBuffer: 2,
	}
}

func TestParseSPSTable(t *testing.T) {
	if got := hex.EncodeToString(baseSPS().nal()); got != "2742c01fab402802dc80" {
		t.Fatalf("writer gives %s for the worked example", got)
	}
	ok := map[string]func(*testSPS){
		"base": func(*testSPS) {},
		"level 1b by flag": func(s *testSPS) {
			s.f.levelIDC, s.f.constraints, s.f.widthMbs1, s.f.heightMapUnits1 = 11, 0xd0, 2, 2
		},
		"level 1b High": func(s *testSPS) {
			s.f.profileIDC, s.f.constraints, s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1 = 100, 0, 9, 2, 2
		},
		// Level 1.1 without constraint_set3 is not 1b: 300 macroblocks fit its MaxFS of 396.
		"level 1.1 Constrained Baseline 320x240": func(s *testSPS) {
			s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1 = 11, 19, 14
		},
		"level 1.1 Main 320x240": func(s *testSPS) {
			s.f.profileIDC, s.f.constraints, s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1 = 77, 0, 11, 19, 14
		},
		"169 macroblocks wide at level 3.1": func(s *testSPS) {
			s.f.widthMbs1, s.f.heightMapUnits1 = 168, 20
		},
		"High with scaling lists": func(s *testSPS) {
			s.f.profileIDC, s.f.constraints, s.f.levelIDC = 100, 0, 51
			s.f.scalingMatrix = true
			s.f.scalingLists[0] = []int32{-8}
			s.f.scalingLists[3] = []int32{127, -128, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
			s.f.scalingLists[7] = make([]int32, 64)
		},
		"poc type 1": func(s *testSPS) {
			s.f.pocType, s.f.log2MaxPocLsb4 = 1, 0
			s.f.deltaPicOrderAlwaysZero, s.f.offsetNonRef, s.f.offsetTopBottom = true, -2147483647, 2147483647
			s.f.offsetRefFrame = make([]int32, 255)
			s.f.offsetRefFrame[254] = -5
		},
		"poc type 2, bounds": func(s *testSPS) {
			s.f.pocType, s.f.log2MaxPocLsb4, s.f.log2MaxFrameNum4, s.f.id, s.f.gaps = 2, 0, 12, 31, true
		},
		"poc lsb 12": func(s *testSPS) { s.f.log2MaxPocLsb4 = 12 },
		"crop right and bottom": func(s *testSPS) {
			s.f.cropping, s.f.cropR, s.f.cropB = true, 8, 7
		},
		"full VUI": func(s *testSPS) { s.f.vuiPresent, s.f.vui = true, fullVUI() },
		"refs at MaxDpbFrames": func(s *testSPS) {
			s.f.maxRefFrames = 5 // 18000 / 3600 at level 3.1
		},
		"largest": func(s *testSPS) {
			s.f.profileIDC, s.f.constraints, s.f.levelIDC = 100, 0, 51
			s.f.widthMbs1, s.f.heightMapUnits1, s.f.maxRefFrames = 255, 143, 5
		},
		"tall": func(s *testSPS) {
			s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1, s.f.maxRefFrames = 51, 143, 255, 5
		},
	}
	for name, mod := range ok {
		t.Run(name, func(t *testing.T) {
			ts := baseSPS()
			mod(&ts)
			s, err := parseSPS(ts.nal(), DefaultMaxPixels)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(s.f, ts.f) {
				t.Fatalf("read back\n%+v\nwrote\n%+v", s.f, ts.f)
			}
		})
	}
	bad := []struct {
		name string
		mod  func(*testSPS)
		want error
	}{
		{"profile 88", func(s *testSPS) { s.f.profileIDC = 88 }, ErrUnsupportedStream},
		{"profile 110", func(s *testSPS) { s.f.profileIDC = 110 }, ErrUnsupportedStream},
		{"level 52", func(s *testSPS) { s.f.levelIDC = 52 }, ErrUnsupportedStream},
		{"level 14", func(s *testSPS) { s.f.levelIDC = 14 }, ErrUnsupportedStream},
		{"level 8", func(s *testSPS) { s.f.levelIDC = 8 }, ErrUnsupportedStream},
		{"level_idc 9 with Baseline", func(s *testSPS) { s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1 = 9, 2, 2 }, ErrUnsupportedStream},
		{"level_idc 9 with Main", func(s *testSPS) {
			s.f.profileIDC, s.f.constraints, s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1 = 77, 0, 9, 2, 2
		}, ErrUnsupportedStream},
		// Sqrt(8 x 3600) is 169 at level 3.1 (A.3.1): 256x14 fits MaxFS, not the side bound.
		{"256 macroblocks wide at level 3.1", func(s *testSPS) { s.f.widthMbs1, s.f.heightMapUnits1 = 255, 13 }, ErrUnsupportedStream},
		{"170 macroblocks high at level 3.1", func(s *testSPS) { s.f.widthMbs1, s.f.heightMapUnits1 = 20, 169 }, ErrUnsupportedStream},
		{"zero byte after the trailing bits", func(s *testSPS) { s.zeroTail = 1 }, ErrCorrupt},
		{"zero bytes after the trailing bits", func(s *testSPS) { s.zeroTail = 6 }, ErrCorrupt},
		{"sps id 32", func(s *testSPS) { s.f.id = 32 }, ErrCorrupt},
		{"chroma 2", func(s *testSPS) { s.f.profileIDC, s.f.constraints, s.f.chromaFormatIDC = 100, 0, 2 }, ErrUnsupportedStream},
		{"chroma 0", func(s *testSPS) { s.f.profileIDC, s.f.constraints, s.f.chromaFormatIDC = 100, 0, 0 }, ErrUnsupportedStream},
		{"luma 9 bits", func(s *testSPS) { s.f.profileIDC, s.f.constraints, s.depth8[0] = 100, 0, 1 }, ErrUnsupportedStream},
		{"chroma 9 bits", func(s *testSPS) { s.f.profileIDC, s.f.constraints, s.depth8[1] = 100, 0, 1 }, ErrUnsupportedStream},
		{"transform bypass", func(s *testSPS) { s.f.profileIDC, s.f.constraints, s.bypass = 100, 0, true }, ErrUnsupportedStream},
		{"delta_scale -129", func(s *testSPS) {
			s.f.profileIDC, s.f.constraints, s.f.scalingMatrix = 100, 0, true
			s.f.scalingLists[1] = []int32{-129}
		}, ErrCorrupt},
		{"delta_scale 128", func(s *testSPS) {
			s.f.profileIDC, s.f.constraints, s.f.scalingMatrix = 100, 0, true
			s.f.scalingLists[6] = []int32{128}
		}, ErrCorrupt},
		// Lists that end at 0 right after the value out of range, so only the range refuses them.
		{"delta_scale 128 in a list that ends", func(s *testSPS) {
			s.f.profileIDC, s.f.constraints, s.f.scalingMatrix = 100, 0, true
			s.f.scalingLists[6] = []int32{128, 120}
		}, ErrCorrupt},
		{"delta_scale -129 in a list that ends", func(s *testSPS) {
			s.f.profileIDC, s.f.constraints, s.f.scalingMatrix = 100, 0, true
			s.f.scalingLists[1] = []int32{-129, 121}
		}, ErrCorrupt},
		{"log2_max_frame_num 13", func(s *testSPS) { s.f.log2MaxFrameNum4 = 13 }, ErrCorrupt},
		{"log2_max_poc_lsb 13", func(s *testSPS) { s.f.log2MaxPocLsb4 = 13 }, ErrCorrupt},
		{"poc type 3", func(s *testSPS) { s.f.pocType = 3 }, ErrCorrupt},
		{"poc cycle 256", func(s *testSPS) { s.f.pocType, s.f.offsetRefFrame = 1, make([]int32, 256) }, ErrCorrupt},
		{"interlaced", func(s *testSPS) { s.interlaced = true }, ErrUnsupportedStream},
		{"width 543 macroblocks", func(s *testSPS) { s.f.widthMbs1, s.f.heightMapUnits1 = 542, 2 }, ErrTooLarge},
		{"height 543 macroblocks", func(s *testSPS) { s.f.widthMbs1, s.f.heightMapUnits1 = 2, 542 }, ErrTooLarge},
		{"width 2^32-2 macroblocks", func(s *testSPS) { s.f.widthMbs1 = 1<<32 - 2 }, ErrTooLarge},
		{"544 macroblocks wide cropped to 4096", func(s *testSPS) {
			s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1, s.f.cropping, s.f.cropR = 51, 543, 2, true, 2304
		}, ErrTooLarge},
		{"544 macroblocks high cropped to 4096", func(s *testSPS) {
			s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1, s.f.cropping, s.f.cropB = 51, 2, 543, true, 2304
		}, ErrTooLarge},
		{"left crop", func(s *testSPS) { s.f.cropping, s.f.cropL = true, 1 }, ErrUnsupportedStream},
		{"top crop", func(s *testSPS) { s.f.cropping, s.f.cropT = true, 1 }, ErrUnsupportedStream},
		{"right crop past width", func(s *testSPS) { s.f.cropping, s.f.cropR = true, 640 }, ErrCorrupt},
		{"bottom crop past height", func(s *testSPS) { s.f.cropping, s.f.cropB = true, 360 }, ErrCorrupt},
		{"crop 2^32-2", func(s *testSPS) { s.f.cropping, s.f.cropB = true, 1<<32-2 }, ErrCorrupt},
		{"width 46", func(s *testSPS) { s.f.widthMbs1, s.f.cropping, s.f.cropR = 2, true, 1 }, ErrTooLarge},
		{"height 46", func(s *testSPS) { s.f.heightMapUnits1, s.f.cropping, s.f.cropB = 2, true, 1 }, ErrTooLarge},
		{"width 4112", func(s *testSPS) { s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1 = 51, 256, 8 }, ErrTooLarge},
		{"pixels above MaxPixels", func(s *testSPS) {
			s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1 = 51, 255, 144
		}, ErrTooLarge},
		{"MaxFS of level 3.1", func(s *testSPS) { s.f.widthMbs1, s.f.heightMapUnits1 = 79, 45 }, ErrUnsupportedStream},
		{"MaxFS of level 1b", func(s *testSPS) {
			s.f.levelIDC, s.f.constraints, s.f.widthMbs1, s.f.heightMapUnits1 = 11, 0xd0, 9, 9
		}, ErrUnsupportedStream},
		{"refs above MaxDpbFrames", func(s *testSPS) { s.f.maxRefFrames = 6 }, ErrUnsupportedStream},
		{"refs above 16", func(s *testSPS) {
			s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1, s.f.maxRefFrames = 51, 2, 2, 17
		}, ErrUnsupportedStream},
		{"refs 2^32-2", func(s *testSPS) { s.f.maxRefFrames = 1<<32 - 2 }, ErrUnsupportedStream},
		{"refs at 4096x2304", func(s *testSPS) {
			s.f.levelIDC, s.f.widthMbs1, s.f.heightMapUnits1, s.f.maxRefFrames = 51, 255, 143, 6
		}, ErrUnsupportedStream},
		{"chroma loc 6", func(s *testSPS) {
			s.f.vuiPresent, s.f.vui = true, fullVUI()
			s.f.vui.chromaLocTop = 6
		}, ErrCorrupt},
		{"cpb_cnt 33", func(s *testSPS) {
			s.f.vuiPresent, s.f.vui = true, fullVUI()
			s.f.vui.vclHRD = &hrdInfo{cpbs: make([]hrdCPB, 33)}
		}, ErrCorrupt},
		{"max_bytes_per_pic_denom 17", func(s *testSPS) {
			s.f.vuiPresent, s.f.vui = true, fullVUI()
			s.f.vui.maxBytesPerPicDenom = 17
		}, ErrCorrupt},
		{"max_bits_per_mb_denom 17", func(s *testSPS) {
			s.f.vuiPresent, s.f.vui = true, fullVUI()
			s.f.vui.maxBitsPerMbDenom = 17
		}, ErrCorrupt},
		{"log2_max_mv_length_horizontal 17", func(s *testSPS) {
			s.f.vuiPresent, s.f.vui = true, fullVUI()
			s.f.vui.log2MaxMvLengthH = 17
		}, ErrCorrupt},
		{"log2_max_mv_length_vertical 17", func(s *testSPS) {
			s.f.vuiPresent, s.f.vui = true, fullVUI()
			s.f.vui.log2MaxMvLengthV = 17
		}, ErrCorrupt},
		{"reorder above buffering", func(s *testSPS) {
			s.f.vuiPresent, s.f.vui = true, fullVUI()
			s.f.vui.maxNumReorderFrames, s.f.vui.maxDecFrameBuffer = 3, 2
		}, ErrCorrupt},
		{"buffering above MaxDpbFrames", func(s *testSPS) {
			s.f.vuiPresent, s.f.vui = true, fullVUI()
			s.f.vui.maxDecFrameBuffer = 6
		}, ErrCorrupt},
		{"bad trailing bits", func(s *testSPS) { s.badTrail = true }, ErrCorrupt},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			ts := baseSPS()
			c.mod(&ts)
			_, err := parseSPS(ts.nal(), DefaultMaxPixels)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
	nal := baseSPS().nal()
	for i := 1; i < len(nal); i++ {
		if _, err := parseSPS(nal[:i], DefaultMaxPixels); !errors.Is(err, ErrCorrupt) {
			t.Errorf("truncated to %d bytes: %v", i, err)
		}
	}
	// seq_parameter_set_id with 32 leading zero bits.
	if _, err := parseSPS([]byte{0x67, 66, 0xc0, 31, 0, 0, 0, 0, 0x80, 0x80}, DefaultMaxPixels); !errors.Is(err, ErrCorrupt) {
		t.Errorf("33-bit Exp-Golomb code: %v", err)
	}
	if _, err := parseSPS(nal, 1280*720-1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("MaxPixels: %v", err)
	}
}

func TestLevelTable(t *testing.T) {
	// ITU-T H.264 Table A-1, levels 1 to 5.1: MaxMBPS, MaxFS, MaxDpbMbs, MaxBR.
	want := map[uint8]level{
		10: {1485, 99, 396, 64}, 9: {1485, 99, 396, 128}, 11: {3000, 396, 900, 192},
		12: {6000, 396, 2376, 384}, 13: {11880, 396, 2376, 768}, 20: {11880, 396, 2376, 2000},
		21: {19800, 792, 4752, 4000}, 22: {20250, 1620, 8100, 4000}, 30: {40500, 1620, 8100, 10000},
		31: {108000, 3600, 18000, 14000}, 32: {216000, 5120, 20480, 20000}, 40: {245760, 8192, 32768, 20000},
		41: {245760, 8192, 32768, 50000}, 42: {522240, 8704, 34816, 50000}, 50: {589824, 22080, 110400, 135000},
		51: {983040, 36864, 184320, 240000},
	}
	for idc := range 256 {
		l, ok := levelFor(100, 0, uint8(idc))
		w, wok := want[uint8(idc)]
		if ok != wok || l != w {
			t.Errorf("level_idc %d: %v %v, want %v %v", idc, l, ok, w, wok)
		}
	}
	if l, _ := levelFor(66, 0x10, 11); l != want[9] {
		t.Error("level 1b by constraint_set3")
	}
	if l, _ := levelFor(100, 0x10, 11); l != want[11] {
		t.Error("constraint_set3 with High is not level 1b")
	}
	for _, p := range []uint8{66, 77} {
		if _, ok := levelFor(p, 0, 9); ok {
			t.Errorf("level_idc 9 with profile_idc %d", p)
		}
		if l, _ := levelFor(p, 0, 11); l != want[11] {
			t.Errorf("level_idc 11 without constraint_set3 with profile_idc %d", p)
		}
	}
}

func TestRewriteWorkedExample(t *testing.T) {
	nal := unhex(t, "2742c01fab402802dc80")
	s, err := parseSPS(nal, DefaultMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	out, s2, err := rewriteSPS(nal, s, koloVUI(s.f.maxRefFrames), false, DefaultMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	// The VUI of "SPS normalization": video_format 5, colour 6, 1, 6,
	// bitstream_restriction 1, 1, 2, 1, 16, 16, 0, 1.
	if got := hex.EncodeToString(out); got != "2742c01fab402802dd3506010606d0442350" {
		t.Fatalf("got %s", got)
	}
	e := s2.export()
	if e.Codec() != "avc3.42c01f" || e.Matrix != 6 || e.FullRange || s2.f.vui.maxNumReorderFrames != 0 || s2.f.vui.maxDecFrameBuffer != 1 {
		t.Fatalf("rewritten SPS reads %+v", s2.f.vui)
	}
}

func TestRewriteSPS(t *testing.T) {
	inputs := map[string][]byte{}
	for _, p := range probeSPS {
		inputs[p.name] = unhex(t, p.sps)
	}
	ts := baseSPS()
	ts.f.vuiPresent, ts.f.vui = true, fullVUI()
	inputs["full VUI with timing and HRD"] = ts.nal()
	ts.f.vui.restriction = false
	inputs["VUI without restriction"] = ts.nal()
	for name, nal := range inputs {
		t.Run(name, func(t *testing.T) {
			s, err := parseSPS(nal, DefaultMaxPixels)
			if err != nil {
				t.Fatal(err)
			}
			enc, s2, err := rewriteSPS(nal, s, koloVUI(s.f.maxRefFrames), false, DefaultMaxPixels)
			if err != nil {
				t.Fatal(err)
			}
			if s2.needsNormalize() || s2.f.vui.matrix != 6 || s2.f.vui.aspectRatioPresent || s2.f.vui.timingPresent {
				t.Fatalf("encoder rewrite gives %+v", s2.f.vui)
			}
			again, _, err := rewriteSPS(enc, s2, koloVUI(s2.f.maxRefFrames), false, DefaultMaxPixels)
			if err != nil || !bytes.Equal(again, enc) {
				t.Fatalf("encoder rewrite not idempotent: %x %x %v", again, enc, err)
			}
			v := s.f.vui
			setRestriction(&v, s.f.maxRefFrames)
			dec, s3, err := rewriteSPS(nal, s, v, true, DefaultMaxPixels)
			if err != nil {
				t.Fatal(err)
			}
			if s3.needsNormalize() {
				t.Fatal("decoder rewrite still needs normalization")
			}
			want := s.f
			want.vuiPresent, want.vui = true, v
			if !reflect.DeepEqual(s3.f, want) {
				t.Fatalf("decoder rewrite changed fields")
			}
			if s.f.vuiPresent {
				// Bits up to bitstream_restriction_flag are kept.
				a := unescapeRBSP(nil, nal[1:])
				b := unescapeRBSP(nil, dec[1:])
				r1, r2 := &bitReader{b: a}, &bitReader{b: b}
				for i := 0; i < s.restrictionBit; i++ {
					if r1.u(1) != r2.u(1) {
						t.Fatalf("bit %d of %d differs", i, s.restrictionBit)
					}
				}
			}
			again, _, err = rewriteSPS(dec, s3, v, true, DefaultMaxPixels)
			if err != nil || !bytes.Equal(again, dec) {
				t.Fatalf("decoder rewrite not idempotent")
			}
		})
	}
	// A VUI that does not read back as written (overscan_appropriate without
	// overscan_info_present) must fail the read-back comparison.
	s0, err := parseSPS(baseSPS().nal(), DefaultMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	odd := koloVUI(1)
	odd.overscanAppropriate = true
	if _, _, err := rewriteSPS(baseSPS().nal(), s0, odd, false, DefaultMaxPixels); !errors.Is(err, ErrCodec) {
		t.Fatalf("rewrite that changes a field: %v", err)
	}
	// The Windows encoder's SPS already has no reordering: kept as is.
	s, err := parseSPS(unhex(t, probeSPS[10].sps), DefaultMaxPixels)
	if err != nil || s.needsNormalize() {
		t.Fatalf("mf SPS: %v %v", err, s.needsNormalize())
	}
}

func TestParsePPS(t *testing.T) {
	cb, _ := parseSPS(baseSPS().nal(), DefaultMaxPixels)
	hs := baseSPS()
	hs.f.profileIDC, hs.f.constraints, hs.f.id = 100, 0, 1
	high, err := parseSPS(hs.nal(), DefaultMaxPixels)
	if err != nil {
		t.Fatal(err)
	}
	sps := &[32]*spsInfo{0: cb, 1: high}
	for _, c := range []struct {
		name string
		p    testPPSFields
		want error
	}{
		{"plain", testPPSFields{}, nil},
		{"id 255", testPPSFields{id: 255}, nil},
		{"High extension", testPPSFields{spsID: 1, entropy: true, ext: true, transform8x8: true, scalingMatrix: true}, nil},
		{"Main CABAC", testPPSFields{spsID: 1, entropy: true}, nil},
		{"id 256", testPPSFields{id: 256}, ErrCorrupt},
		{"absent SPS", testPPSFields{spsID: 2}, ErrCorrupt},
		{"SPS id 32", testPPSFields{spsID: 32}, ErrCorrupt},
		{"FMO", testPPSFields{groups: 1}, ErrUnsupportedStream},
		{"CABAC with 66", testPPSFields{entropy: true}, ErrUnsupportedStream},
		{"redundant pictures", testPPSFields{redundant: true}, ErrUnsupportedStream},
		{"High fields with 66", testPPSFields{ext: true}, ErrUnsupportedStream},
		{"qp 26", testPPSFields{qp: 26}, ErrCorrupt},
		{"qp -27", testPPSFields{qp: -27}, ErrCorrupt},
		{"bad trailing bits", testPPSFields{badTrail: true}, ErrCorrupt},
		{"zero byte after the trailing bits", testPPSFields{zeroTail: 1}, ErrCorrupt},
		{"zero bytes after the trailing bits", testPPSFields{zeroTail: 4}, ErrCorrupt},
		{"weighted_bipred_idc 2", testPPSFields{bipred: 2}, nil},
		{"weighted_bipred_idc 3", testPPSFields{bipred: 3}, ErrCorrupt},
	} {
		_, err := parsePPS(c.p.nal(), sps)
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	nal := testPPSFields{spsID: 1, ext: true, transform8x8: true}.nal()
	for i := 1; i < len(nal); i++ {
		parsePPS(nal[:i], sps) // a prefix may be a shorter valid PPS; it must not panic
	}
}

func TestSliceHeader(t *testing.T) {
	ps := &paramSets{}
	if err := ps.add([][]byte{testSPSFor(640, 480, 66, 0xc0, 30)}, [][]byte{testPPS(0, 0), testPPS(3, 0)}, DefaultMaxPixels, keepSPS); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		nal  []byte
		want error
	}{
		{"IDR", testSlice(true, 0, 7, 0, 0, 1), nil},
		{"P", testSlice(false, 0, 0, 3, 5, 0), nil},
		{"last macroblock", testSlice(false, 1199, 5, 0, 5, 0), nil},
		{"I in P picture", testSlice(false, 0, 2, 0, 5, 0), nil},
		{"first_mb past the picture", testSlice(false, 1200, 5, 0, 5, 0), ErrCorrupt},
		{"B", testSlice(false, 0, 1, 0, 1, 0), ErrUnsupportedStream},
		{"B 6", testSlice(false, 0, 6, 0, 1, 0), ErrUnsupportedStream},
		{"SP", testSlice(false, 0, 3, 0, 1, 0), ErrUnsupportedStream},
		{"SI", testSlice(false, 0, 9, 0, 1, 0), ErrUnsupportedStream},
		{"slice_type 10", testSlice(false, 0, 10, 0, 1, 0), ErrCorrupt},
		{"IDR P", testSlice(true, 0, 5, 0, 0, 1), ErrCorrupt},
		{"IDR frame_num 1", testSlice(true, 0, 7, 0, 1, 1), ErrCorrupt},
		{"IDR non-reference", testSliceHeader{idr: true, sliceType: 7, frameBits: 4}.nal(), ErrCorrupt},
		{"idr_pic_id 65536", testSlice(true, 0, 7, 0, 0, 65536), ErrCorrupt},
		{"absent PPS", testSlice(false, 0, 5, 1, 1, 0), ErrCorrupt},
		{"PPS 256", testSlice(false, 0, 5, 256, 1, 0), ErrCorrupt},
		{"truncated", []byte{0x41, 0x00}, ErrCorrupt},
	} {
		_, err := parseSlice(c.nal, &ps.pps)
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}

func TestParseKeyframeErrors(t *testing.T) {
	for name, c := range map[string]struct {
		data []byte
		want error
	}{
		"too large":     {make([]byte, DefaultMaxPacketBytes+1), ErrTooLarge},
		"no start code": {[]byte{0x67}, ErrCorrupt},
		"forbidden bit": {appendAnnexB(nil, [][]byte{{0xe7, 1}}), ErrCorrupt},
		"no SPS":        {appendAnnexB(nil, [][]byte{dPPS, dIDR}), ErrCorrupt},
		"bad SPS":       {appendAnnexB(nil, [][]byte{{0x67, 66}}), ErrCorrupt},
	} {
		if _, err := ParseKeyframe(c.data); !errors.Is(err, c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	s, err := ParseKeyframe(appendAnnexB(nil, [][]byte{{nalAUD, 0x10}, dSPS, testSPSFor(320, 240, 100, 0, 30)}))
	if err != nil || s.Width != 640 || s.ProfileIDC != 66 {
		t.Fatalf("first SPS: %v %+v", err, s)
	}
}

func TestPPSAndSliceFields(t *testing.T) {
	cb, _ := parseSPS(baseSPS().nal(), DefaultMaxPixels)
	sps := &[32]*spsInfo{0: cb}
	w := &bitWriter{}
	w.ue(0)
	w.ue(0)
	w.flag(false)
	w.flag(false)
	w.ue(0)
	w.ue(0)
	w.ue(0)
	w.flag(false)
	w.u(3, 2) // weighted_bipred_idc 3
	w.trailing()
	if _, err := parsePPS(escapeRBSP([]byte{0x68}, w.b), sps); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("weighted_bipred_idc 3: %v", err)
	}
	ps := &paramSets{}
	ps.add([][]byte{baseSPS().nal()}, [][]byte{testPPSFields{bottomPOC: true}.nal()}, DefaultMaxPixels, keepSPS)
	w = &bitWriter{}
	w.ue(0)
	w.ue(5)
	w.ue(0)
	w.u(3, 5)
	w.u(9, 6)
	w.se(-4)
	w.trailing()
	sl, err := parseSlice(escapeRBSP([]byte{0x41}, w.b), &ps.pps)
	if err != nil || sl.frameNum != 3 || sl.pocLsb != 9 || sl.deltaPocBottom != -4 {
		t.Fatalf("%v %+v", err, sl)
	}
}
