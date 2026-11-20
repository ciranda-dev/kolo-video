package h264

import (
	"fmt"
	"math"
	"reflect"
)

// SPS holds the sequence parameter set fields this package checks.
type SPS struct {
	ID                             uint32
	ProfileIDC, Constraints, Level uint8
	// Width and Height are the picture size after cropping.
	Width, Height int
	MaxRefFrames  uint32
	FullRange     bool
	// Matrix is matrix_coefficients, 2 (unspecified) without a colour
	// description.
	Matrix uint8
}

// ParseKeyframe returns the first SPS of an Annex B keyframe packet, checked
// as Decoder checks it with the default bounds. It never panics.
func ParseKeyframe(packet []byte) (SPS, error) {
	if len(packet) > DefaultMaxPacketBytes {
		return SPS{}, fmt.Errorf("%w: packet of %d bytes", ErrTooLarge, len(packet))
	}
	nals, err := splitAnnexB(packet, maxNALs(DefaultMaxPixels))
	if err != nil {
		return SPS{}, err
	}
	for _, nal := range nals {
		if nal[0]&0x80 != 0 {
			return SPS{}, fmt.Errorf("%w: forbidden_zero_bit set", ErrCorrupt)
		}
		if nalType(nal) == nalSPS {
			s, err := parseSPS(nal, DefaultMaxPixels)
			if err != nil {
				return SPS{}, err
			}
			return s.export(), nil
		}
	}
	return SPS{}, fmt.Errorf("%w: no SPS in the packet", ErrCorrupt)
}

// Codec returns the codec string, such as "avc3.42c01f" (RFC 6381 3.3).
func (s SPS) Codec() string {
	return fmt.Sprintf("avc3.%02x%02x%02x", s.ProfileIDC, s.Constraints, s.Level)
}

// Profile classifies the SPS by RFC 6184 Table 5; ok is false for other
// profiles.
func (s SPS) Profile() (p Profile, ok bool) {
	c := s.Constraints
	switch {
	case s.ProfileIDC == 66 && c&0x4f == 0x40, s.ProfileIDC == 77 && c&0x8f == 0x80:
		return ConstrainedBaseline, true
	case s.ProfileIDC == 77 && c&0xaf == 0:
		return Main, true
	case s.ProfileIDC == 100 && c == 0:
		return High, true
	}
	return 0, false
}

// level holds the ITU-T H.264 Table A-1 limits used here.
type level struct {
	maxMBPS, maxFS, maxDpbMbs, maxBR int64
}

// levelFor returns the limits of a level, from Table A-1 for levels 1 to 5.1.
// Level 1b is level_idc 9 in High and level_idc 11 with constraint_set3 in
// Baseline and Main (ITU-T H.264 7.4.2.1.1).
func levelFor(profileIDC, constraints, levelIDC uint8) (level, bool) {
	if levelIDC == 9 && profileIDC == 100 || levelIDC == 11 && constraints&0x10 != 0 && (profileIDC == 66 || profileIDC == 77) {
		return level{1485, 99, 396, 128}, true
	}
	switch levelIDC {
	case 10:
		return level{1485, 99, 396, 64}, true
	case 11:
		return level{3000, 396, 900, 192}, true
	case 12:
		return level{6000, 396, 2376, 384}, true
	case 13:
		return level{11880, 396, 2376, 768}, true
	case 20:
		return level{11880, 396, 2376, 2000}, true
	case 21:
		return level{19800, 792, 4752, 4000}, true
	case 22:
		return level{20250, 1620, 8100, 4000}, true
	case 30:
		return level{40500, 1620, 8100, 10000}, true
	case 31:
		return level{108000, 3600, 18000, 14000}, true
	case 32:
		return level{216000, 5120, 20480, 20000}, true
	case 40:
		return level{245760, 8192, 32768, 20000}, true
	case 41:
		return level{245760, 8192, 32768, 50000}, true
	case 42:
		return level{522240, 8704, 34816, 50000}, true
	case 50:
		return level{589824, 22080, 110400, 135000}, true
	case 51:
		return level{983040, 36864, 184320, 240000}, true
	}
	return level{}, false
}

// maxMbsPerSide is Sqrt(MaxFS x 8) at level 5.1 (ITU-T H.264 A.3.1).
const maxMbsPerSide = 543

// Sizes every engine supports, the documented range of the Windows decoder.
const (
	minSide = 48
	maxSide = 4096
)

// spsFields are the syntax elements of seq_parameter_set_data (ITU-T H.264
// 7.3.2.1.1) for the profiles accepted here.
type spsFields struct {
	profileIDC, constraints, levelIDC uint8
	id                                uint32
	chromaFormatIDC                   uint32
	scalingMatrix                     bool
	scalingLists                      [8][]int32 // delta_scale values, nil when not present
	log2MaxFrameNum4                  uint32
	pocType                           uint32
	log2MaxPocLsb4                    uint32
	deltaPicOrderAlwaysZero           bool
	offsetNonRef, offsetTopBottom     int32
	offsetRefFrame                    []int32
	maxRefFrames                      uint32
	gaps                              bool
	widthMbs1, heightMapUnits1        uint32
	direct8x8                         bool
	cropping                          bool
	cropL, cropR, cropT, cropB        uint32
	vuiPresent                        bool
	vui                               vuiInfo
}

// vuiInfo are the syntax elements of vui_parameters (ITU-T H.264 E.1.1).
type vuiInfo struct {
	aspectRatioPresent                     bool
	aspectRatioIDC                         uint8
	sarWidth, sarHeight                    uint16
	overscanPresent, overscanAppropriate   bool
	videoSignalPresent                     bool
	videoFormat                            uint8
	fullRange, colourPresent               bool
	primaries, transfer, matrix            uint8
	chromaLocPresent                       bool
	chromaLocTop, chromaLocBottom          uint32
	timingPresent                          bool
	unitsInTick, timeScale                 uint32
	fixedFrameRate                         bool
	nalHRD, vclHRD                         *hrdInfo
	lowDelayHRD                            bool
	picStructPresent                       bool
	restriction                            bool
	mvOverPicBoundaries                    bool
	maxBytesPerPicDenom, maxBitsPerMbDenom uint32
	log2MaxMvLengthH, log2MaxMvLengthV     uint32
	maxNumReorderFrames, maxDecFrameBuffer uint32
}

// hrdInfo are the syntax elements of hrd_parameters (ITU-T H.264 E.1.2).
type hrdInfo struct {
	bitRateScale, cpbSizeScale uint8
	cpbs                       []hrdCPB
	initialDelayLen1           uint8
	removalDelayLen1           uint8
	outputDelayLen1            uint8
	timeOffsetLen              uint8
}

type hrdCPB struct {
	bitRate1, cpbSize1 uint32
	cbr                bool
}

// spsInfo is a parsed and checked SPS.
type spsInfo struct {
	f                   spsFields
	widthMbs, heightMbs int
	width, height       int
	maxDpbFrames        int
	vuiBit              int // bit offset of vui_parameters_present_flag in the RBSP
	restrictionBit      int // bit offset of bitstream_restriction_flag, 0 without VUI
	picSizeMb           int
}

func (s *spsInfo) export() SPS {
	v := &s.f.vui
	matrix := uint8(2)
	if v.videoSignalPresent && v.colourPresent {
		matrix = v.matrix
	}
	return SPS{
		ID: s.f.id, ProfileIDC: s.f.profileIDC, Constraints: s.f.constraints, Level: s.f.levelIDC,
		Width: s.width, Height: s.height, MaxRefFrames: s.f.maxRefFrames,
		FullRange: v.videoSignalPresent && v.fullRange, Matrix: matrix,
	}
}

func corrupt(what string, err error) error {
	return fmt.Errorf("%w: %s: %v", ErrCorrupt, what, err)
}

// parseSPS parses an SPS NAL unit, header byte included, and applies the
// checks of decoder validation step 5.
func parseSPS(nal []byte, maxPixels int) (*spsInfo, error) {
	r := &bitReader{b: unescapeRBSP(make([]byte, 0, len(nal)), nal[1:])}
	s := &spsInfo{}
	f := &s.f
	f.profileIDC, f.constraints, f.levelIDC = uint8(r.u(8)), uint8(r.u(8)), uint8(r.u(8))
	f.id = r.ueMax(31, "seq_parameter_set_id")
	if r.err != nil {
		return nil, corrupt("SPS", r.err)
	}
	if f.profileIDC != 66 && f.profileIDC != 77 && f.profileIDC != 100 {
		return nil, fmt.Errorf("%w: profile_idc %d", ErrUnsupportedStream, f.profileIDC)
	}
	lv, ok := levelFor(f.profileIDC, f.constraints, f.levelIDC)
	if !ok {
		return nil, fmt.Errorf("%w: level_idc %d", ErrUnsupportedStream, f.levelIDC)
	}
	f.chromaFormatIDC = 1
	if f.profileIDC == 100 {
		f.chromaFormatIDC = r.ue()
		lumaDepth8, chromaDepth8 := r.ue(), r.ue()
		bypass := r.flag()
		if r.err != nil {
			return nil, corrupt("SPS", r.err)
		}
		if f.chromaFormatIDC != 1 || lumaDepth8 != 0 || chromaDepth8 != 0 || bypass {
			return nil, fmt.Errorf("%w: chroma_format_idc %d, bit depth %d and %d, transform bypass %v",
				ErrUnsupportedStream, f.chromaFormatIDC, lumaDepth8+8, chromaDepth8+8, bypass)
		}
		f.scalingMatrix = r.flag()
		if f.scalingMatrix {
			for i := range f.scalingLists {
				if r.flag() {
					f.scalingLists[i] = readScalingList(r, i)
				}
			}
		}
	}
	f.log2MaxFrameNum4 = r.ueMax(12, "log2_max_frame_num_minus4")
	f.pocType = r.ueMax(2, "pic_order_cnt_type")
	switch f.pocType {
	case 0:
		f.log2MaxPocLsb4 = r.ueMax(12, "log2_max_pic_order_cnt_lsb_minus4")
	case 1:
		f.deltaPicOrderAlwaysZero = r.flag()
		f.offsetNonRef, f.offsetTopBottom = r.se(), r.se()
		n := r.ueMax(255, "num_ref_frames_in_pic_order_cnt_cycle")
		for range n {
			f.offsetRefFrame = append(f.offsetRefFrame, r.se())
		}
	}
	f.maxRefFrames = r.ue()
	f.gaps = r.flag()
	f.widthMbs1, f.heightMapUnits1 = r.ue(), r.ue()
	frameMbsOnly := r.flag()
	if r.err != nil {
		return nil, corrupt("SPS", r.err)
	}
	if !frameMbsOnly {
		return nil, fmt.Errorf("%w: interlaced (frame_mbs_only_flag 0)", ErrUnsupportedStream)
	}
	f.direct8x8 = r.flag()
	f.cropping = r.flag()
	if f.cropping {
		f.cropL, f.cropR, f.cropT, f.cropB = r.ue(), r.ue(), r.ue(), r.ue()
	}
	s.vuiBit = r.pos
	f.vuiPresent = r.flag()
	if f.vuiPresent {
		s.restrictionBit = readVUI(r, &f.vui)
	}
	r.trailing()
	if r.err != nil {
		return nil, corrupt("SPS", r.err)
	}
	if err := s.check(lv, maxPixels); err != nil {
		return nil, err
	}
	return s, nil
}

// readScalingList reads scaling_list (ITU-T H.264 7.3.2.1.1.1): 16
// coefficients for lists 0 to 5, 64 for 6 and 7.
func readScalingList(r *bitReader, i int) []int32 {
	size := 16
	if i >= 6 {
		size = 64
	}
	var deltas []int32
	last, next := int32(8), int32(8)
	for range size {
		d := r.seRange(-128, 127, "delta_scale")
		deltas = append(deltas, d)
		next = (last + d + 256) % 256
		if next == 0 || r.err != nil {
			break
		}
		last = next
	}
	return deltas
}

// readVUI reads vui_parameters and returns the bit offset of
// bitstream_restriction_flag.
func readVUI(r *bitReader, v *vuiInfo) int {
	if v.aspectRatioPresent = r.flag(); v.aspectRatioPresent {
		v.aspectRatioIDC = uint8(r.u(8))
		if v.aspectRatioIDC == 255 {
			v.sarWidth, v.sarHeight = uint16(r.u(16)), uint16(r.u(16))
		}
	}
	if v.overscanPresent = r.flag(); v.overscanPresent {
		v.overscanAppropriate = r.flag()
	}
	if v.videoSignalPresent = r.flag(); v.videoSignalPresent {
		v.videoFormat = uint8(r.u(3))
		v.fullRange = r.flag()
		if v.colourPresent = r.flag(); v.colourPresent {
			v.primaries, v.transfer, v.matrix = uint8(r.u(8)), uint8(r.u(8)), uint8(r.u(8))
		}
	}
	if v.chromaLocPresent = r.flag(); v.chromaLocPresent {
		v.chromaLocTop = r.ueMax(5, "chroma_sample_loc_type_top_field")
		v.chromaLocBottom = r.ueMax(5, "chroma_sample_loc_type_bottom_field")
	}
	if v.timingPresent = r.flag(); v.timingPresent {
		v.unitsInTick, v.timeScale = r.u(32), r.u(32)
		v.fixedFrameRate = r.flag()
	}
	if r.flag() {
		v.nalHRD = readHRD(r)
	}
	if r.flag() {
		v.vclHRD = readHRD(r)
	}
	if v.nalHRD != nil || v.vclHRD != nil {
		v.lowDelayHRD = r.flag()
	}
	v.picStructPresent = r.flag()
	at := r.pos
	if v.restriction = r.flag(); v.restriction {
		v.mvOverPicBoundaries = r.flag()
		v.maxBytesPerPicDenom = r.ueMax(16, "max_bytes_per_pic_denom")
		v.maxBitsPerMbDenom = r.ueMax(16, "max_bits_per_mb_denom")
		v.log2MaxMvLengthH = r.ueMax(16, "log2_max_mv_length_horizontal")
		v.log2MaxMvLengthV = r.ueMax(16, "log2_max_mv_length_vertical")
		v.maxNumReorderFrames, v.maxDecFrameBuffer = r.ue(), r.ue()
	}
	return at
}

func readHRD(r *bitReader) *hrdInfo {
	h := &hrdInfo{}
	n := r.ueMax(31, "cpb_cnt_minus1")
	h.bitRateScale, h.cpbSizeScale = uint8(r.u(4)), uint8(r.u(4))
	for range n + 1 {
		h.cpbs = append(h.cpbs, hrdCPB{bitRate1: r.ue(), cpbSize1: r.ue(), cbr: r.flag()})
	}
	h.initialDelayLen1, h.removalDelayLen1 = uint8(r.u(5)), uint8(r.u(5))
	h.outputDelayLen1, h.timeOffsetLen = uint8(r.u(5)), uint8(r.u(5))
	return h
}

// check applies the size, level and reference bounds. Every value is at most
// 2^32-1 here, so the 64-bit arithmetic cannot overflow.
func (s *spsInfo) check(lv level, maxPixels int) error {
	f := &s.f
	if f.widthMbs1 >= maxMbsPerSide || f.heightMapUnits1 >= maxMbsPerSide {
		return fmt.Errorf("%w: %d by %d macroblocks", ErrTooLarge, uint64(f.widthMbs1)+1, uint64(f.heightMapUnits1)+1)
	}
	wMbs, hMbs := int64(f.widthMbs1)+1, int64(f.heightMapUnits1)+1
	if f.cropL != 0 || f.cropT != 0 {
		return fmt.Errorf("%w: left or top cropping", ErrUnsupportedStream)
	}
	// Crop units are 2 by 2 for 4:2:0 frames (ITU-T H.264 7.4.2.1.1).
	if 2*int64(f.cropR) >= 16*wMbs || 2*int64(f.cropB) >= 16*hMbs {
		return fmt.Errorf("%w: cropping %d and %d past the coded size", ErrCorrupt, f.cropR, f.cropB)
	}
	w, h := 16*wMbs-2*int64(f.cropR), 16*hMbs-2*int64(f.cropB)
	if w < minSide || h < minSide || w > maxSide || h > maxSide {
		return fmt.Errorf("%w: picture %dx%d", ErrTooLarge, w, h)
	}
	// The system decoder allocates the coded size, whatever the cropping.
	if 256*wMbs*hMbs > int64(maxPixels) {
		return fmt.Errorf("%w: picture coded as %dx%d, above %d pixels", ErrTooLarge, 16*wMbs, 16*hMbs, maxPixels)
	}
	if wMbs*hMbs > lv.maxFS {
		return fmt.Errorf("%w: %d macroblocks above the level %d MaxFS %d", ErrUnsupportedStream, wMbs*hMbs, f.levelIDC, lv.maxFS)
	}
	if side := int64(math.Sqrt(float64(8 * lv.maxFS))); wMbs > side || hMbs > side {
		return fmt.Errorf("%w: %d by %d macroblocks, above %d per side at level %d", ErrUnsupportedStream, wMbs, hMbs, side, f.levelIDC)
	}
	dpb := min(lv.maxDpbMbs/(wMbs*hMbs), 16)
	if int64(f.maxRefFrames) > dpb {
		return fmt.Errorf("%w: max_num_ref_frames %d above MaxDpbFrames %d", ErrUnsupportedStream, f.maxRefFrames, dpb)
	}
	if v := &f.vui; v.restriction && (v.maxNumReorderFrames > v.maxDecFrameBuffer || int64(v.maxDecFrameBuffer) > dpb) {
		return fmt.Errorf("%w: max_num_reorder_frames %d, max_dec_frame_buffering %d, MaxDpbFrames %d",
			ErrCorrupt, v.maxNumReorderFrames, v.maxDecFrameBuffer, dpb)
	}
	s.widthMbs, s.heightMbs, s.width, s.height = int(wMbs), int(hMbs), int(w), int(h)
	s.maxDpbFrames, s.picSizeMb = int(dpb), int(wMbs*hMbs)
	return nil
}

func writeVUI(w *bitWriter, v *vuiInfo) {
	w.flag(v.aspectRatioPresent)
	if v.aspectRatioPresent {
		w.u(uint32(v.aspectRatioIDC), 8)
		if v.aspectRatioIDC == 255 {
			w.u(uint32(v.sarWidth), 16)
			w.u(uint32(v.sarHeight), 16)
		}
	}
	w.flag(v.overscanPresent)
	if v.overscanPresent {
		w.flag(v.overscanAppropriate)
	}
	w.flag(v.videoSignalPresent)
	if v.videoSignalPresent {
		w.u(uint32(v.videoFormat), 3)
		w.flag(v.fullRange)
		w.flag(v.colourPresent)
		if v.colourPresent {
			w.u(uint32(v.primaries), 8)
			w.u(uint32(v.transfer), 8)
			w.u(uint32(v.matrix), 8)
		}
	}
	w.flag(v.chromaLocPresent)
	if v.chromaLocPresent {
		w.ue(v.chromaLocTop)
		w.ue(v.chromaLocBottom)
	}
	w.flag(v.timingPresent)
	if v.timingPresent {
		w.u(v.unitsInTick, 32)
		w.u(v.timeScale, 32)
		w.flag(v.fixedFrameRate)
	}
	for _, h := range []*hrdInfo{v.nalHRD, v.vclHRD} {
		w.flag(h != nil)
		if h != nil {
			writeHRD(w, h)
		}
	}
	if v.nalHRD != nil || v.vclHRD != nil {
		w.flag(v.lowDelayHRD)
	}
	w.flag(v.picStructPresent)
	writeRestriction(w, v)
}

func writeRestriction(w *bitWriter, v *vuiInfo) {
	w.flag(v.restriction)
	if v.restriction {
		w.flag(v.mvOverPicBoundaries)
		w.ue(v.maxBytesPerPicDenom)
		w.ue(v.maxBitsPerMbDenom)
		w.ue(v.log2MaxMvLengthH)
		w.ue(v.log2MaxMvLengthV)
		w.ue(v.maxNumReorderFrames)
		w.ue(v.maxDecFrameBuffer)
	}
}

func writeHRD(w *bitWriter, h *hrdInfo) {
	w.ue(uint32(len(h.cpbs) - 1))
	w.u(uint32(h.bitRateScale), 4)
	w.u(uint32(h.cpbSizeScale), 4)
	for _, c := range h.cpbs {
		w.ue(c.bitRate1)
		w.ue(c.cpbSize1)
		w.flag(c.cbr)
	}
	w.u(uint32(h.initialDelayLen1), 5)
	w.u(uint32(h.removalDelayLen1), 5)
	w.u(uint32(h.outputDelayLen1), 5)
	w.u(uint32(h.timeOffsetLen), 5)
}

// setRestriction sets the bitstream_restriction group Kolo writes: the
// ITU-T H.264 E.2.1 defaults, except no reordering and a buffer of
// max_num_ref_frames.
func setRestriction(v *vuiInfo, maxRef uint32) {
	v.restriction, v.mvOverPicBoundaries = true, true
	v.maxBytesPerPicDenom, v.maxBitsPerMbDenom = 2, 1
	v.log2MaxMvLengthH, v.log2MaxMvLengthV = 16, 16
	v.maxNumReorderFrames, v.maxDecFrameBuffer = 0, maxRef
}

// koloVUI is the VUI of every SPS the encoder emits: BT.601 limited range,
// the colour space of kolovideo.Frame, and no reordering.
func koloVUI(maxRef uint32) vuiInfo {
	v := vuiInfo{videoSignalPresent: true, videoFormat: 5, colourPresent: true, primaries: 6, transfer: 1, matrix: 6}
	setRestriction(&v, maxRef)
	return v
}

// needsNormalize reports whether the decoder must rewrite the SPS so that no
// picture waits for reordering.
func (s *spsInfo) needsNormalize() bool {
	v := &s.f.vui
	return !v.restriction || v.maxNumReorderFrames != 0 || v.maxDecFrameBuffer != s.f.maxRefFrames
}

// rewriteSPS returns the SPS NAL unit with its VUI replaced by v (encoder),
// or with only the bitstream_restriction group replaced when keepVUI is set
// and the SPS has a VUI (decoder). The result is parsed again and must give
// the same fields with v as VUI.
func rewriteSPS(nal []byte, s *spsInfo, v vuiInfo, keepVUI bool, maxPixels int) ([]byte, *spsInfo, error) {
	rbsp := unescapeRBSP(make([]byte, 0, len(nal)), nal[1:])
	w := &bitWriter{b: make([]byte, 0, len(rbsp)+16)}
	if keepVUI && s.f.vuiPresent {
		w.copyBits(rbsp, s.restrictionBit)
		writeRestriction(w, &v)
	} else {
		w.copyBits(rbsp, s.vuiBit)
		w.flag(true)
		writeVUI(w, &v)
	}
	w.trailing()
	out := escapeRBSP([]byte{nal[0]}, w.b)
	s2, err := parseSPS(out, maxPixels)
	if err != nil {
		return nil, nil, fmt.Errorf("rewritten SPS: %w", err)
	}
	want := s.f
	want.vuiPresent, want.vui = true, v
	if !reflect.DeepEqual(s2.f, want) {
		return nil, nil, fmt.Errorf("%w: rewritten SPS reads back different fields", ErrCodec)
	}
	return out, s2, nil
}

// ppsInfo are the syntax elements of pic_parameter_set_rbsp (ITU-T H.264
// 7.3.2.2) for the tools accepted here.
type ppsInfo struct {
	id, spsID                uint32
	entropy                  bool
	bottomFieldPicOrder      bool
	numRefIdxL0, numRefIdxL1 uint32
	weightedPred             bool
	weightedBipred           uint32
	picInitQP, picInitQS     int32
	chromaQPOffset           int32
	deblockingControl        bool
	constrainedIntra         bool
	transform8x8             bool
	scalingMatrix            bool
	scalingLists             [8][]int32
	secondChromaQPOffset     int32
	hasExtension             bool
	sps                      *spsInfo
}

// parsePPS parses a PPS NAL unit against the SPS of the same packet and
// applies the checks of decoder validation step 6.
func parsePPS(nal []byte, sps *[32]*spsInfo) (*ppsInfo, error) {
	r := &bitReader{b: unescapeRBSP(make([]byte, 0, len(nal)), nal[1:])}
	p := &ppsInfo{}
	p.id = r.ueMax(255, "pic_parameter_set_id")
	p.spsID = r.ueMax(31, "seq_parameter_set_id")
	p.entropy = r.flag()
	p.bottomFieldPicOrder = r.flag()
	groups := r.ue()
	if r.err != nil {
		return nil, corrupt("PPS", r.err)
	}
	s := sps[p.spsID]
	if s == nil {
		return nil, fmt.Errorf("%w: PPS %d names SPS %d, which the packet does not hold", ErrCorrupt, p.id, p.spsID)
	}
	p.sps = s
	if groups != 0 {
		return nil, fmt.Errorf("%w: slice groups (FMO)", ErrUnsupportedStream)
	}
	if p.entropy && s.f.profileIDC == 66 {
		return nil, fmt.Errorf("%w: CABAC with profile_idc 66", ErrUnsupportedStream)
	}
	p.numRefIdxL0 = r.ueMax(31, "num_ref_idx_l0_default_active_minus1")
	p.numRefIdxL1 = r.ueMax(31, "num_ref_idx_l1_default_active_minus1")
	p.weightedPred = r.flag()
	p.weightedBipred = r.u(2)
	if p.weightedBipred > 2 {
		r.fail(fmt.Errorf("weighted_bipred_idc %d", p.weightedBipred))
	}
	p.picInitQP = r.seRange(-26, 25, "pic_init_qp_minus26")
	p.picInitQS = r.seRange(-26, 25, "pic_init_qs_minus26")
	p.chromaQPOffset = r.seRange(-12, 12, "chroma_qp_index_offset")
	p.deblockingControl = r.flag()
	p.constrainedIntra = r.flag()
	redundant := r.flag()
	if r.err != nil {
		return nil, corrupt("PPS", r.err)
	}
	if redundant {
		return nil, fmt.Errorf("%w: redundant pictures", ErrUnsupportedStream)
	}
	if r.moreData() {
		p.hasExtension = true
		p.transform8x8 = r.flag()
		if p.scalingMatrix = r.flag(); p.scalingMatrix {
			n := 6
			if p.transform8x8 {
				n = 8
			}
			for i := range n {
				if r.flag() {
					p.scalingLists[i] = readScalingList(r, i)
				}
			}
		}
		p.secondChromaQPOffset = r.seRange(-12, 12, "second_chroma_qp_index_offset")
		if r.err == nil && s.f.profileIDC != 100 {
			return nil, fmt.Errorf("%w: High profile PPS fields with profile_idc %d", ErrUnsupportedStream, s.f.profileIDC)
		}
	}
	r.trailing()
	if r.err != nil {
		return nil, corrupt("PPS", r.err)
	}
	return p, nil
}

// sliceInfo holds the slice header fields up to the picture order count
// (ITU-T H.264 7.3.3), which tell one picture from the next (7.4.1.2.4).
type sliceInfo struct {
	nalType, refIDC int
	firstMb         uint32
	sliceType       uint32 // slice_type modulo 5
	pps             *ppsInfo
	frameNum        uint32
	idrPicID        uint32
	pocLsb          uint32
	deltaPocBottom  int32
	deltaPoc        [2]int32
}

// sliceHeaderBytes bounds the bytes unescaped for a slice header: enough for
// nine Exp-Golomb codes of 63 bits after removing every possible emulation
// prevention byte.
const sliceHeaderBytes = 128

// parseSlice parses the start of a slice header against the PPS in force and
// applies the checks of decoder validation step 9 for one slice.
func parseSlice(nal []byte, pps *[256]*ppsInfo) (sliceInfo, error) {
	sl := sliceInfo{nalType: nalType(nal), refIDC: int(nal[0] >> 5 & 3)}
	r := &bitReader{b: unescapeRBSP(make([]byte, 0, sliceHeaderBytes), nal[1:min(len(nal), 1+sliceHeaderBytes)])}
	sl.firstMb = r.ue()
	t := r.ueMax(9, "slice_type")
	ppsID := r.ueMax(255, "pic_parameter_set_id")
	if r.err != nil {
		return sl, corrupt("slice header", r.err)
	}
	p := pps[ppsID]
	if p == nil {
		return sl, fmt.Errorf("%w: slice names PPS %d, which is not in force", ErrCorrupt, ppsID)
	}
	sl.pps, sl.sliceType = p, t%5
	s := p.sps
	if uint64(sl.firstMb) >= uint64(s.picSizeMb) {
		return sl, fmt.Errorf("%w: first_mb_in_slice %d of %d", ErrCorrupt, sl.firstMb, s.picSizeMb)
	}
	switch sl.sliceType {
	case 0, 2:
	default:
		return sl, fmt.Errorf("%w: slice_type %d (B, SP or SI)", ErrUnsupportedStream, t)
	}
	idr := sl.nalType == nalIDR
	if idr && (sl.sliceType != 2 || sl.refIDC == 0) {
		return sl, fmt.Errorf("%w: IDR slice of type %d with nal_ref_idc %d", ErrCorrupt, t, sl.refIDC)
	}
	sl.frameNum = r.u(int(s.f.log2MaxFrameNum4) + 4)
	if idr {
		sl.idrPicID = r.ueMax(65535, "idr_pic_id")
		if sl.frameNum != 0 {
			r.fail(fmt.Errorf("IDR frame_num %d", sl.frameNum))
		}
	}
	switch {
	case s.f.pocType == 0:
		sl.pocLsb = r.u(int(s.f.log2MaxPocLsb4) + 4)
		if p.bottomFieldPicOrder {
			sl.deltaPocBottom = r.se()
		}
	case s.f.pocType == 1 && !s.f.deltaPicOrderAlwaysZero:
		sl.deltaPoc[0] = r.se()
		if p.bottomFieldPicOrder {
			sl.deltaPoc[1] = r.se()
		}
	}
	if r.err != nil {
		return sl, corrupt("slice header", r.err)
	}
	return sl, nil
}

// samePicture reports whether b continues the picture of a (ITU-T H.264
// 7.4.1.2.4, for the fields parsed here). A PPS is stored once per id, so
// equal pointers mean an equal pic_parameter_set_id.
func samePicture(a, b *sliceInfo) bool {
	return a.nalType == b.nalType && (a.refIDC == 0) == (b.refIDC == 0) && a.frameNum == b.frameNum &&
		a.idrPicID == b.idrPicID && a.pocLsb == b.pocLsb && a.deltaPocBottom == b.deltaPocBottom &&
		a.deltaPoc == b.deltaPoc && a.pps == b.pps
}
