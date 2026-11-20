package h264

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"testing"
)

// The fuzz seeds live here, so no testdata/fuzz directory is needed.

func seedSPS(t testing.TB) [][]byte {
	var out [][]byte
	for _, p := range probeSPS {
		out = append(out, unhex(t, p.sps))
	}
	ts := baseSPS()
	ts.f.vuiPresent, ts.f.vui = true, fullVUI()
	return append(out, ts.nal(), unhex(t, "27640034ac568010001219"), fdSPS(t))
}

func FuzzSplitAnnexB(f *testing.F) {
	f.Add(keyPkt().Data)
	f.Add([]byte{0, 0, 1, 0x65, 0, 0, 0, 0, 1, 0x41, 0, 0, 3, 1})
	f.Add([]byte{0, 0, 0, 1, 0, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		nals, err := splitAnnexB(data, 64)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrTooLarge) {
				t.Fatal(err)
			}
			return
		}
		if len(nals) == 0 || len(nals) > 64 {
			t.Fatal(len(nals))
		}
		for _, n := range nals {
			if len(n) == 0 || n[len(n)-1] == 0 {
				t.Fatalf("unit %x", n)
			}
		}
		again, err := splitAnnexB(appendAnnexB(nil, nals), 64)
		if err != nil || !reflect.DeepEqual(again, nals) {
			t.Fatalf("rejoined units split differently: %v", err)
		}
	})
}

func FuzzAVCC(f *testing.F) {
	f.Add([]byte{0, 0, 0, 2, 0x65, 1, 0, 0, 0, 1, 6}, byte(4))
	f.Add([]byte{0, 2, 0x65, 1}, byte(2))
	f.Add([]byte{2, 0x65, 1}, byte(1))
	f.Fuzz(func(t *testing.T, data []byte, size byte) {
		lenSize := int(size % 5)
		nals, err := splitAVCC(data, lenSize, 64)
		if err != nil {
			if !errors.Is(err, ErrCodec) {
				t.Fatal(err)
			}
			return
		}
		n := 0
		for _, u := range nals {
			if len(u) == 0 {
				t.Fatal("empty unit")
			}
			n += lenSize + len(u)
		}
		if n != len(data) {
			t.Fatalf("%d of %d bytes", n, len(data))
		}
		if lenSize == 4 && !bytes.Equal(appendAVCC(nil, nals), data) {
			t.Fatal("appendAVCC differs")
		}
	})
}

func checkSPSInvariants(t *testing.T, s *spsInfo, maxPixels int) {
	t.Helper()
	if s.width < minSide || s.height < minSide || s.width > maxSide || s.height > maxSide || s.width*s.height > maxPixels ||
		s.width%2 != 0 || s.height%2 != 0 || int64(s.f.maxRefFrames) > int64(s.maxDpbFrames) || s.maxDpbFrames > 16 {
		t.Fatalf("accepted %dx%d refs %d dpb %d", s.width, s.height, s.f.maxRefFrames, s.maxDpbFrames)
	}
	if s.f.cropL != 0 || s.f.cropT != 0 || s.f.profileIDC != 66 && s.f.profileIDC != 77 && s.f.profileIDC != 100 {
		t.Fatalf("accepted %+v", s.f)
	}
	lv, _ := levelFor(s.f.profileIDC, s.f.constraints, s.f.levelIDC)
	side := int(math.Sqrt(float64(8 * lv.maxFS)))
	if 256*s.picSizeMb > maxPixels || s.widthMbs > side || s.heightMbs > side || s.f.levelIDC == 9 && s.f.profileIDC != 100 {
		t.Fatalf("accepted %dx%d macroblocks at level_idc %d, profile_idc %d", s.widthMbs, s.heightMbs, s.f.levelIDC, s.f.profileIDC)
	}
	if c := s.export().Codec(); len(c) != 11 {
		t.Fatal(c)
	}
}

func FuzzParseSPS(f *testing.F) {
	for _, s := range seedSPS(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, nal []byte) {
		if len(nal) == 0 {
			return
		}
		s, err := parseSPS(nal, DefaultMaxPixels)
		if err != nil {
			if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrUnsupportedStream) {
				t.Fatal(err)
			}
			return
		}
		checkSPSInvariants(t, s, DefaultMaxPixels)
	})
}

// byteSource turns fuzz input into field values.
type byteSource struct{ b []byte }

func (s *byteSource) next() byte {
	if len(s.b) == 0 {
		return 0
	}
	c := s.b[0]
	s.b = s.b[1:]
	return c
}

func (s *byteSource) u32() uint32 {
	return uint32(s.next())<<24 | uint32(s.next())<<16 | uint32(s.next())<<8 | uint32(s.next())
}

func (s *byteSource) pick(n int) int { return int(s.next()) % n }
func (s *byteSource) flag() bool     { return s.next()&1 == 1 }

// genSPS builds a syntactically valid SPS whose values stay in their syntax
// ranges; only the size, level and reference checks may refuse it.
func genSPS(src *byteSource) testSPS {
	levels := []uint8{9, 10, 11, 12, 13, 20, 21, 22, 30, 31, 32, 40, 41, 42, 50, 51}
	f := spsFields{
		profileIDC: []uint8{66, 77, 100}[src.pick(3)], constraints: src.next() &^ 3, levelIDC: levels[src.pick(len(levels))],
		chromaFormatIDC: 1, id: uint32(src.pick(32)), log2MaxFrameNum4: uint32(src.pick(13)), pocType: uint32(src.pick(3)),
		maxRefFrames: uint32(src.pick(17)), gaps: src.flag(), widthMbs1: uint32(src.next()) + uint32(src.pick(2))*256,
		heightMapUnits1: uint32(src.next()) + uint32(src.pick(2))*256, direct8x8: src.flag(),
	}
	if f.profileIDC == 100 && src.flag() {
		f.scalingMatrix = true
		for i := range f.scalingLists {
			switch src.pick(3) {
			case 1:
				f.scalingLists[i] = []int32{-8}
			case 2:
				size := 16
				if i >= 6 {
					size = 64
				}
				last := int32(8)
				for range size {
					d := int32(int8(src.next()))
					if (last+d+256)%256 == 0 {
						if d < 127 {
							d++
						} else {
							d--
						}
					}
					f.scalingLists[i] = append(f.scalingLists[i], d)
					last = (last + d + 256) % 256
				}
			}
		}
	}
	switch f.pocType {
	case 0:
		f.log2MaxPocLsb4 = uint32(src.pick(13))
	case 1:
		f.deltaPicOrderAlwaysZero = src.flag()
		f.offsetNonRef, f.offsetTopBottom = int32(src.u32()>>1)-1<<30, int32(src.u32()>>2)
		for range src.pick(4) {
			f.offsetRefFrame = append(f.offsetRefFrame, int32(src.u32()>>1)-1<<30)
		}
	}
	if src.flag() {
		f.cropping = true
		f.cropR, f.cropB = uint32(src.next()), uint32(src.next())
	}
	if src.flag() {
		f.vuiPresent = true
		v := &f.vui
		v.aspectRatioPresent, v.aspectRatioIDC = src.flag(), src.next()
		if !v.aspectRatioPresent {
			v.aspectRatioIDC = 0
		} else if v.aspectRatioIDC == 255 {
			v.sarWidth, v.sarHeight = uint16(src.u32()), uint16(src.u32())
		}
		if v.overscanPresent = src.flag(); v.overscanPresent {
			v.overscanAppropriate = src.flag()
		}
		if v.videoSignalPresent = src.flag(); v.videoSignalPresent {
			v.videoFormat, v.fullRange = src.next()&7, src.flag()
			if v.colourPresent = src.flag(); v.colourPresent {
				v.primaries, v.transfer, v.matrix = src.next(), src.next(), src.next()
			}
		}
		if v.chromaLocPresent = src.flag(); v.chromaLocPresent {
			v.chromaLocTop, v.chromaLocBottom = uint32(src.pick(6)), uint32(src.pick(6))
		}
		if v.timingPresent = src.flag(); v.timingPresent {
			v.unitsInTick, v.timeScale, v.fixedFrameRate = src.u32(), src.u32(), src.flag()
		}
		for _, h := range []**hrdInfo{&v.nalHRD, &v.vclHRD} {
			if src.flag() {
				hr := &hrdInfo{bitRateScale: src.next() & 15, cpbSizeScale: src.next() & 15,
					initialDelayLen1: src.next() & 31, removalDelayLen1: src.next() & 31, outputDelayLen1: src.next() & 31, timeOffsetLen: src.next() & 31}
				for range src.pick(32) + 1 {
					hr.cpbs = append(hr.cpbs, hrdCPB{src.u32() % (1<<32 - 1), src.u32() % (1<<32 - 1), src.flag()})
				}
				*h = hr
			}
		}
		if v.nalHRD != nil || v.vclHRD != nil {
			v.lowDelayHRD = src.flag()
		}
		v.picStructPresent = src.flag()
		if v.restriction = src.flag(); v.restriction {
			v.mvOverPicBoundaries = src.flag()
			v.maxBytesPerPicDenom, v.maxBitsPerMbDenom = uint32(src.pick(17)), uint32(src.pick(17))
			v.log2MaxMvLengthH, v.log2MaxMvLengthV = uint32(src.pick(17)), uint32(src.pick(17))
			v.maxDecFrameBuffer = uint32(src.pick(17))
			v.maxNumReorderFrames = uint32(src.pick(int(v.maxDecFrameBuffer) + 1))
		}
	}
	return testSPS{f: f}
}

func FuzzSPSRoundTrip(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0x40, 9, 0, 1, 0, 1, 0, 1, 79, 0, 44, 0, 1})
	f.Add(bytes.Repeat([]byte{0xff}, 300))
	f.Add(bytes.Repeat([]byte{1, 2, 3, 250, 7}, 60))
	f.Add([]byte("200000000000012ww000~~~")) // a scaling list that reached 0 on its last allowed delta
	f.Fuzz(func(t *testing.T, data []byte) {
		ts := genSPS(&byteSource{b: data})
		s, err := parseSPS(ts.nal(), DefaultMaxPixels)
		if err != nil {
			if !errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrUnsupportedStream) && !errors.Is(err, ErrCorrupt) {
				t.Fatal(err)
			}
			// Only the size, crop, level and reference checks refuse a
			// syntactically valid SPS.
			if errors.Is(err, ErrCorrupt) && !bytes.Contains([]byte(err.Error()), []byte("cropping")) &&
				!bytes.Contains([]byte(err.Error()), []byte("max_dec_frame_buffering")) {
				t.Fatalf("%v for %+v", err, ts.f)
			}
			return
		}
		if !reflect.DeepEqual(s.f, ts.f) {
			t.Fatalf("read back\n%+v\nwrote\n%+v", s.f, ts.f)
		}
		checkSPSInvariants(t, s, DefaultMaxPixels)
	})
}

func FuzzRewriteSPS(f *testing.F) {
	for _, s := range seedSPS(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, nal []byte) {
		if len(nal) == 0 {
			return
		}
		s, err := parseSPS(nal, DefaultMaxPixels)
		if err != nil {
			return
		}
		enc, s2, err := rewriteSPS(nal, s, koloVUI(s.f.maxRefFrames), false, DefaultMaxPixels)
		if err != nil || s2.needsNormalize() || s2.f.vui.maxNumReorderFrames != 0 {
			t.Fatalf("encoder rewrite: %v", err)
		}
		if again, _, err := rewriteSPS(enc, s2, koloVUI(s2.f.maxRefFrames), false, DefaultMaxPixels); err != nil || !bytes.Equal(again, enc) {
			t.Fatal("encoder rewrite not idempotent")
		}
		v := s.f.vui
		setRestriction(&v, s.f.maxRefFrames)
		_, s3, err := rewriteSPS(nal, s, v, true, DefaultMaxPixels)
		if err != nil || s3.needsNormalize() || s3.f.vui.maxNumReorderFrames != 0 || s3.width != s.width || s3.height != s.height {
			t.Fatalf("decoder rewrite: %v", err)
		}
	})
}

func fuzzParamSets(t testing.TB) *paramSets {
	ps := &paramSets{}
	high := baseSPS()
	high.f.profileIDC, high.f.constraints, high.f.id, high.f.pocType = 100, 0, 1, 1
	high.f.offsetRefFrame = []int32{1, -1}
	err := ps.add([][]byte{unhex(t, "2742c01fab402802dc80"), high.nal()},
		[][]byte{unhex(t, "28ce3c80"), testPPSFields{id: 1, spsID: 1, bottomPOC: true, entropy: true}.nal()}, DefaultMaxPixels, keepSPS)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func FuzzParsePPS(f *testing.F) {
	for _, p := range probeSPS {
		f.Add(unhex(f, p.pps))
	}
	f.Add(testPPSFields{spsID: 1, entropy: true, ext: true, transform8x8: true, scalingMatrix: true}.nal())
	f.Add(testPPSFields{groups: 3}.nal())
	ps := fuzzParamSets(f)
	f.Fuzz(func(t *testing.T, nal []byte) {
		if len(nal) == 0 {
			return
		}
		p, err := parsePPS(nal, &ps.sps)
		if err != nil {
			return
		}
		if p.sps == nil || p.sps != ps.sps[p.spsID] || p.entropy && p.sps.f.profileIDC == 66 {
			t.Fatalf("%+v", p)
		}
	})
}

func FuzzSliceHeader(f *testing.F) {
	f.Add(testSliceHeader{idr: true, refIDC: 3, sliceType: 7, frameBits: 5, pocBits: 6}.nal())
	f.Add(testSliceHeader{refIDC: 2, firstMb: 3599, sliceType: 0, ppsID: 1, frameNum: 7, frameBits: 5}.nal())
	f.Add(testSliceHeader{refIDC: 2, sliceType: 1, frameBits: 5}.nal())
	ps := fuzzParamSets(f)
	f.Fuzz(func(t *testing.T, nal []byte) {
		if len(nal) == 0 {
			return
		}
		sl, err := parseSlice(nal, &ps.pps)
		if err != nil {
			return
		}
		if int(sl.firstMb) >= sl.pps.sps.picSizeMb || sl.sliceType != 0 && sl.sliceType != 2 || sl.nalType == nalIDR && (sl.sliceType != 2 || sl.frameNum != 0) {
			t.Fatalf("%+v", sl)
		}
	})
}

func FuzzDecoderCheck(f *testing.F) {
	f.Add(keyPkt().Data)
	f.Add(deltaPkt().Data)
	f.Add(pkt(dSPS, dPPS, dIDR, testSlice(true, 300, 7, 0, 0, 1)).Data)
	f.Add(pkt(dDelta, testSlice(false, 600, 5, 0, 1, 0)).Data)
	f.Add(pkt(unhex(f, "2742c01fab402802dc80"), unhex(f, "28ce3c80"), testSliceHeader{idr: true, refIDC: 3, sliceType: 7, frameBits: 5, pocBits: 6}.nal()).Data)
	for _, s := range seedSPS(f) {
		f.Add(pkt(s, dPPS, dIDR).Data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		fd := &fakeDec{}
		d := &Decoder{cfg: DecoderConfig{MaxPixels: 1280 * 720, MaxPacketBytes: 1 << 16}, impl: fd, needKey: true}
		for _, p := range [][]byte{data, keyPkt().Data, data, deltaPkt().Data, data} {
			f, err := d.Decode(Packet{Data: p})
			if err == nil && f != nil && (f.Width < minSide || f.Height < minSide) {
				t.Fatal(f.Width, f.Height)
			}
		}
		for _, au := range fd.aus {
			if len(au.slices) == 0 || au.key != (len(au.sps) > 0) || au.key != (len(au.pps) > 0) {
				t.Fatalf("%+v", au)
			}
			for _, s := range au.slices {
				if typ := nalType(s); typ != nalSlice && typ != nalIDR || (typ == nalIDR) != au.key {
					t.Fatalf("slice type %d", typ)
				}
			}
			for _, nal := range au.sps {
				s, err := parseSPS(nal, DefaultMaxPixels)
				if err != nil || s.needsNormalize() {
					t.Fatalf("SPS passed on: %v", err)
				}
			}
		}
	})
}

func FuzzParseKeyframe(f *testing.F) {
	f.Add(keyPkt().Data)
	for _, s := range seedSPS(f) {
		f.Add(appendAnnexB(nil, [][]byte{s}))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := ParseKeyframe(data)
		if err != nil {
			return
		}
		if len(s.Codec()) != 11 || s.Width < minSide || s.Height < minSide || s.Width*s.Height > DefaultMaxPixels {
			t.Fatalf("%+v", s)
		}
	})
}
