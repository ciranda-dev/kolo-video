package h264

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"sync"
	"time"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

// Defaults and bounds. The level limits are those of level 5.1 (ITU-T H.264
// Table A-1), the highest level every engine documents.
const (
	// DefaultFrameRate is the rate assumed for frames without a Duration:
	// kolovideo.DefaultFrameRate.
	DefaultFrameRate = kolovideo.DefaultFrameRate
	// DefaultBitsPerPixel sets the default bitrate: about 4.4 Mbit/s for
	// 1080p at 30 fps.
	DefaultBitsPerPixel = 0.07
	// DefaultKeyframeInterval starts a new group of pictures every 2 s.
	DefaultKeyframeInterval = 2 * time.Second
	// DefaultMaxPixels bounds the coded size of decoded pictures: 36864
	// macroblocks, the level 5.1 MaxFS.
	DefaultMaxPixels = 9437184
	// DefaultMaxPacketBytes bounds a packet, as kolo-media's playout MaxBytes.
	DefaultMaxPacketBytes = 16 << 20
	// MaxBitrate is the level 5.1 MaxBR in bits per second, in the Baseline
	// and Main unit.
	MaxBitrate = 240000000
)

// Level 5.1 MaxFS and MaxMBPS.
const (
	maxFrameMbs = 36864
	maxMbRate   = 983040
)

// Config sets up an Encoder. Zero fields take defaults.
type Config struct {
	// Width and Height are even, 48 to 4096, at most 36864 macroblocks, and
	// macroblocks times FrameRate at most 983040 (level 5.1).
	Width, Height int
	// FrameRate is kolovideo.MinFrameRate to MaxFrameRate, DefaultFrameRate
	// if zero.
	FrameRate float64
	// Bitrate is the target in bits per second, 1 to MaxBitrate;
	// DefaultBitsPerPixel times pixels times FrameRate if zero.
	Bitrate int
	// KeyframeInterval is the time between keyframes on frame timestamps,
	// DefaultKeyframeInterval if zero, negative for keyframes on request only.
	KeyframeInterval time.Duration
	// Profile is ConstrainedBaseline if zero.
	Profile Profile
}

func (c Config) withDefaults() (Config, error) {
	bad := func(what string, v any) (Config, error) {
		return c, fmt.Errorf("kolovideo/h264: %s %v: %w", what, v, kolovideo.ErrInvalidConfig)
	}
	if c.Width < minSide || c.Height < minSide || c.Width > maxSide || c.Height > maxSide || c.Width%2 != 0 || c.Height%2 != 0 {
		return bad("size", fmt.Sprintf("%dx%d", c.Width, c.Height))
	}
	mbs := ((c.Width + 15) / 16) * ((c.Height + 15) / 16)
	if mbs > maxFrameMbs {
		return bad("macroblocks per frame", mbs)
	}
	if c.FrameRate != 0 && !(c.FrameRate >= kolovideo.MinFrameRate && c.FrameRate <= kolovideo.MaxFrameRate) {
		return bad("frame rate", c.FrameRate)
	}
	if c.FrameRate == 0 {
		c.FrameRate = DefaultFrameRate
	}
	if float64(mbs)*c.FrameRate > maxMbRate {
		return bad("macroblocks per second", float64(mbs)*c.FrameRate)
	}
	if c.Bitrate < 0 || c.Bitrate > MaxBitrate {
		return bad("bitrate", c.Bitrate)
	}
	if c.Bitrate == 0 {
		c.Bitrate = int(max(1, min(DefaultBitsPerPixel*float64(c.Width)*float64(c.Height)*c.FrameRate, MaxBitrate)))
	}
	if c.KeyframeInterval == 0 {
		c.KeyframeInterval = DefaultKeyframeInterval
	}
	if c.Profile == 0 {
		c.Profile = ConstrainedBaseline
	}
	if c.Profile < ConstrainedBaseline || c.Profile > High {
		return bad("profile", c.Profile)
	}
	return c, nil
}

// DecoderConfig sets up a Decoder. Zero fields take defaults.
type DecoderConfig struct {
	// MaxPixels bounds the coded picture size, macroblocks times 256 before
	// cropping, which is what the system decoder allocates: 1920x1080 is
	// coded as 1920x1088. DefaultMaxPixels if zero, at most DefaultMaxPixels.
	MaxPixels int
	// MaxPacketBytes bounds a packet, DefaultMaxPacketBytes if zero, at most
	// 256 MiB so that bit offsets fit in 32 bits.
	MaxPacketBytes int
}

// maxPacketBytes keeps bit offsets of a packet within an int32.
const maxPacketBytes = 1 << 28

func (c DecoderConfig) withDefaults() (DecoderConfig, error) {
	if c.MaxPixels < 0 || c.MaxPixels > DefaultMaxPixels || c.MaxPacketBytes < 0 || c.MaxPacketBytes > maxPacketBytes {
		return c, fmt.Errorf("kolovideo/h264: max pixels %d, max packet bytes %d: %w", c.MaxPixels, c.MaxPacketBytes, kolovideo.ErrInvalidConfig)
	}
	if c.MaxPixels == 0 {
		c.MaxPixels = DefaultMaxPixels
	}
	if c.MaxPacketBytes == 0 {
		c.MaxPacketBytes = DefaultMaxPacketBytes
	}
	return c, nil
}

// Capabilities probes the system codec once per process and returns the
// result.
func Capabilities() Limits {
	e := current()
	engines.Lock()
	defer engines.Unlock()
	if engines.caps == nil {
		c := e.caps()
		engines.caps = &c
	}
	c := *engines.caps
	c.Encode.Profiles = slices.Clone(c.Encode.Profiles)
	c.Decode.Profiles = slices.Clone(c.Decode.Profiles)
	return c
}

// paramSets are the parameter sets of one keyframe, by id.
type paramSets struct {
	spsRaw [32][]byte // as received
	sps    [32]*spsInfo
	ppsRaw [256][]byte
	pps    [256]*ppsInfo
	active *spsInfo
	norm   [][]byte // every SPS as passed on, in packet order
	ppsOut [][]byte // every PPS, in packet order
}

// add parses and stores the SPS and PPS units of a keyframe. normalize
// returns the SPS bytes to pass on.
func (ps *paramSets) add(spsNALs, ppsNALs [][]byte, maxPixels int, normalize func(nal []byte, s *spsInfo) ([]byte, *spsInfo, error)) error {
	for _, nal := range spsNALs {
		s, err := parseSPS(nal, maxPixels)
		if err != nil {
			return err
		}
		if ps.sps[s.f.id] != nil {
			return fmt.Errorf("%w: SPS id %d twice in one packet", ErrUnsupportedStream, s.f.id)
		}
		out, s, err := normalize(nal, s)
		if err != nil {
			return err
		}
		ps.spsRaw[s.f.id], ps.sps[s.f.id] = bytes.Clone(nal), s
		ps.norm = append(ps.norm, out)
	}
	for _, nal := range ppsNALs {
		p, err := parsePPS(nal, &ps.sps)
		if err != nil {
			return err
		}
		if ps.pps[p.id] != nil {
			return fmt.Errorf("%w: PPS id %d twice in one packet", ErrUnsupportedStream, p.id)
		}
		ps.ppsRaw[p.id], ps.pps[p.id] = bytes.Clone(nal), p
		ps.ppsOut = append(ps.ppsOut, ps.ppsRaw[p.id])
	}
	return nil
}

// checkSlices parses the slice headers of one packet and checks that they
// form one picture in increasing macroblock order, all under one SPS.
func checkSlices(slices [][]byte, ps *paramSets) (*spsInfo, error) {
	var first, prev sliceInfo
	for i, nal := range slices {
		sl, err := parseSlice(nal, &ps.pps)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			if sl.firstMb != 0 {
				return nil, fmt.Errorf("%w: first slice starts at macroblock %d", ErrUnsupportedStream, sl.firstMb)
			}
			first = sl
		} else if !samePicture(&first, &sl) || sl.firstMb <= prev.firstMb {
			return nil, fmt.Errorf("%w: second picture or slices out of order in one packet", ErrUnsupportedStream)
		}
		prev = sl
	}
	return first.pps.sps, nil
}

// Encoder encodes I420 frames with the system codec. Its methods are safe
// for concurrent use.
type Encoder struct {
	mu        sync.Mutex
	cfg       Config
	impl      encImpl
	cleanup   runtime.Cleanup
	started   bool
	base      time.Duration
	lastPts   int64
	lastKey   time.Duration
	wantKey   bool
	frameTick int64
	ps        *paramSets
}

// NewEncoder creates an encoder. On Windows it also checks the profile; call
// Close to free it.
func NewEncoder(cfg Config) (*Encoder, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	impl, err := current().newEncoder(encParams{
		width: cfg.Width, height: cfg.Height, fps: cfg.FrameRate, bps: cfg.Bitrate,
		profile: cfg.Profile, keyInterval: cfg.KeyframeInterval,
	})
	if err != nil {
		return nil, err
	}
	e := &Encoder{cfg: cfg, impl: impl, wantKey: true, frameTick: int64(max(1, 90000/cfg.FrameRate+0.5))}
	if params := impl.params(); params != nil {
		ps, err := e.paramSets(params, nil)
		if err != nil {
			impl.close()
			return nil, err
		}
		e.ps = ps
	}
	e.cleanup = runtime.AddCleanup(e, encImpl.close, impl)
	return e, nil
}

// Config returns the configuration with defaults filled in.
func (e *Encoder) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// Encode returns the access unit for f, or none when the system encoder
// dropped the frame. A keyframe is made for the first frame, every
// KeyframeInterval and when forceKeyframe is set.
func (e *Encoder) Encode(f *kolovideo.Frame, forceKeyframe bool) ([]Packet, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.impl == nil {
		return nil, kolovideo.ErrClosed
	}
	if f.Width != e.cfg.Width || f.Height != e.cfg.Height {
		return nil, fmt.Errorf("kolovideo/h264: frame is %dx%d, encoder is %dx%d: %w", f.Width, f.Height, e.cfg.Width, e.cfg.Height, kolovideo.ErrInvalidFrame)
	}
	if !e.started {
		e.started, e.base, e.lastPts, e.lastKey = true, f.Timestamp, -1, f.Timestamp
	}
	if forceKeyframe || (e.cfg.KeyframeInterval > 0 && f.Timestamp-e.lastKey >= e.cfg.KeyframeInterval) {
		e.wantKey = true
	}
	dur := e.frameTick
	if f.Duration > 0 {
		dur = max(1, ticks90k(f.Duration))
	}
	// A timestamp that does not advance gets one frame time: the system
	// encoder needs increasing timestamps.
	pts := ticks90k(f.Timestamp - e.base)
	if pts <= e.lastPts {
		pts = e.lastPts + min(dur, math.MaxInt64-e.lastPts)
	}
	e.lastPts = pts
	var pkts []Packet
	var emitErr error
	err := e.impl.encode(f.Y, f.U, f.V, f.YStride, f.UVStride, pts, dur, e.wantKey, func(nals, params [][]byte, _ bool) {
		if emitErr != nil {
			return
		}
		if len(pkts) > 0 {
			emitErr = fmt.Errorf("%w: more than one access unit for one frame", ErrCodec)
			return
		}
		data, key, err := e.normalize(nals, params)
		if err != nil {
			emitErr = err
			return
		}
		pkts = append(pkts, Packet{Data: data, Keyframe: key, Timestamp: f.Timestamp, Duration: f.Duration})
	})
	if err == nil {
		err = emitErr
	}
	if err != nil {
		if errors.Is(err, ErrProfile) {
			e.closeLocked()
		}
		return nil, err
	}
	if len(pkts) > 0 && pkts[0].Keyframe {
		e.wantKey, e.lastKey = false, f.Timestamp
	}
	return pkts, nil
}

// ticks90k converts a duration to 90 kHz ticks without overflow.
func ticks90k(d time.Duration) int64 {
	ns := int64(d)
	return ns/100000*9 + ns%100000*9/100000
}

// normalize turns the system encoder's access unit into a Kolo packet: NAL
// types 1, 5, 7 and 8 only, SPS and PPS before every IDR, every SPS with the
// Kolo VUI, 4-byte start codes. It checks the level, the profile and the
// slice types of the system's output.
func (e *Encoder) normalize(nals, params [][]byte) ([]byte, bool, error) {
	var spsN, ppsN, slices [][]byte
	idr, nonIDR := false, false
	for _, nal := range nals {
		if len(nal) == 0 || nal[0]&0x80 != 0 {
			return nil, false, fmt.Errorf("%w: system encoder gave an empty or invalid NAL unit", ErrCodec)
		}
		switch nalType(nal) {
		case nalSlice:
			slices, nonIDR = append(slices, nal), true
		case nalIDR:
			slices, idr = append(slices, nal), true
		case nalSPS:
			spsN = append(spsN, nal)
		case nalPPS:
			ppsN = append(ppsN, nal)
		}
	}
	if len(slices) == 0 || idr && nonIDR {
		return nil, false, fmt.Errorf("%w: system encoder gave %d slices, IDR %v and non-IDR %v", ErrCodec, len(slices), idr, nonIDR)
	}
	ps := e.ps
	if idr {
		for _, nal := range params {
			switch {
			case len(nal) == 0:
			case nalType(nal) == nalSPS && len(spsN) == 0:
				spsN = append(spsN, nal)
			case nalType(nal) == nalPPS && len(ppsN) == 0:
				ppsN = append(ppsN, nal)
			}
		}
		var err error
		if ps, err = e.paramSets(spsN, ppsN); err != nil {
			return nil, false, err
		}
	}
	if ps == nil {
		return nil, false, fmt.Errorf("%w: system encoder gave a delta frame before any keyframe", ErrCodec)
	}
	if _, err := checkSlices(slices, ps); err != nil {
		return nil, false, fmt.Errorf("%w: system encoder output: %v", ErrCodec, err)
	}
	e.ps = ps
	if idr {
		return appendAnnexB(nil, ps.norm, ps.ppsOut, slices), true, nil
	}
	return appendAnnexB(nil, slices), false, nil
}

// paramSets parses the system encoder's SPS and PPS (in one list or two),
// checks level and profile and rewrites every SPS with the Kolo VUI.
func (e *Encoder) paramSets(spsN, ppsN [][]byte) (*paramSets, error) {
	if ppsN == nil {
		all := spsN
		spsN = nil
		for _, nal := range all {
			switch nalType(nal) {
			case nalSPS:
				spsN = append(spsN, nal)
			case nalPPS:
				ppsN = append(ppsN, nal)
			}
		}
	}
	if len(spsN) == 0 || len(ppsN) == 0 {
		return nil, fmt.Errorf("%w: system encoder gave no SPS or no PPS for a keyframe", ErrCodec)
	}
	ps := &paramSets{}
	err := ps.add(spsN, ppsN, DefaultMaxPixels, func(nal []byte, s *spsInfo) ([]byte, *spsInfo, error) {
		sps := s.export()
		if p, ok := sps.Profile(); !ok || p != e.cfg.Profile {
			return nil, nil, fmt.Errorf("%w: asked for %v, got %s", ErrProfile, e.cfg.Profile, sps.Codec())
		}
		return rewriteSPS(nal, s, koloVUI(s.f.maxRefFrames), false, DefaultMaxPixels)
	})
	if err != nil {
		if errors.Is(err, ErrProfile) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: system encoder parameter sets: %v", ErrCodec, err)
	}
	return ps, nil
}

// SetBitrate changes the target bitrate from the next frame.
func (e *Encoder) SetBitrate(bps int) error {
	if bps < 1 || bps > MaxBitrate {
		return fmt.Errorf("kolovideo/h264: bitrate %d: %w", bps, kolovideo.ErrInvalidConfig)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.impl == nil {
		return kolovideo.ErrClosed
	}
	if err := e.impl.setBitrate(bps); err != nil {
		return err
	}
	e.cfg.Bitrate = bps
	return nil
}

// Hardware reports whether the system encodes this stream in hardware.
func (e *Encoder) Hardware() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.impl != nil && e.impl.hardware()
}

// Close frees the encoder. It is idempotent.
func (e *Encoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closeLocked()
	return nil
}

func (e *Encoder) closeLocked() {
	if e.impl != nil {
		e.cleanup.Stop()
		e.impl.close()
		e.impl = nil
	}
}

// Decoder decodes H.264 packets with the system codec. Its methods are safe
// for concurrent use. Every packet is checked in Go before the system sees
// it, and pictures come out in decoding order.
type Decoder struct {
	mu      sync.Mutex
	cfg     DecoderConfig
	impl    decImpl
	cleanup runtime.Cleanup
	needKey bool
	ps      *paramSets
	buf     []byte
}

// NewDecoder creates a decoder. Call Close to free it.
func NewDecoder(cfg DecoderConfig) (*Decoder, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	impl, err := current().newDecoder(decParams{maxPixels: cfg.MaxPixels})
	if err != nil {
		return nil, err
	}
	d := &Decoder{cfg: cfg, impl: impl, needKey: true}
	d.cleanup = runtime.AddCleanup(d, decImpl.close, impl)
	return d, nil
}

// Decode returns the picture in p, or nil and nil when p holds no picture.
// After any error, packets are refused with ErrNeedKeyframe until an IDR
// with its SPS and PPS.
func (d *Decoder) Decode(p Packet) (*kolovideo.Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.impl == nil {
		return nil, kolovideo.ErrClosed
	}
	f, err := d.decode(p)
	if err != nil {
		d.needKey = true
		return nil, err
	}
	if f != nil {
		f.Timestamp, f.Duration = p.Timestamp, p.Duration
	}
	return f, nil
}

func (d *Decoder) decode(p Packet) (*kolovideo.Frame, error) {
	if len(p.Data) > d.cfg.MaxPacketBytes {
		return nil, fmt.Errorf("%w: packet of %d bytes", ErrTooLarge, len(p.Data))
	}
	// Check and decode a private copy, so the bytes the system reads are the
	// bytes that were checked even if the caller reuses p.Data meanwhile.
	d.buf = append(d.buf[:0], p.Data...)
	au, ps, err := d.validate(d.buf)
	if err != nil || au.slices == nil {
		return nil, err
	}
	if ps != nil {
		d.ps = ps
	}
	pic, ok, err := d.impl.decode(au)
	if err != nil {
		return nil, err
	}
	d.needKey = false
	if !ok {
		return nil, nil
	}
	return copyPicture(pic, au)
}

// validate runs decoder validation steps 2 to 10 and returns the access unit
// to decode, with nil slices when the packet holds no picture, and the new
// parameter sets of a keyframe.
func (d *Decoder) validate(b []byte) (accessUnit, *paramSets, error) {
	nals, err := splitAnnexB(b, maxNALs(d.cfg.MaxPixels))
	if err != nil {
		return accessUnit{}, nil, err
	}
	var spsN, ppsN, slices [][]byte
	idr, nonIDR := false, false
	for _, nal := range nals {
		if nal[0]&0x80 != 0 {
			return accessUnit{}, nil, fmt.Errorf("%w: forbidden_zero_bit set", ErrCorrupt)
		}
		switch t := nalType(nal); t {
		case nalSlice:
			slices, nonIDR = append(slices, nal), true
		case nalIDR:
			slices, idr = append(slices, nal), true
		case nalSPS:
			spsN = append(spsN, nal)
		case nalPPS:
			ppsN = append(ppsN, nal)
		case 2, 3, 4:
			return accessUnit{}, nil, fmt.Errorf("%w: data partitioning (NAL type %d)", ErrUnsupportedStream, t)
		}
	}
	if idr && nonIDR {
		return accessUnit{}, nil, fmt.Errorf("%w: IDR and non-IDR slices in one packet", ErrUnsupportedStream)
	}
	if !idr {
		if d.needKey && len(slices) > 0 {
			return accessUnit{}, nil, ErrNeedKeyframe
		}
		if err := d.checkRepeated(spsN, ppsN); err != nil {
			return accessUnit{}, nil, err
		}
		if len(slices) == 0 {
			return accessUnit{}, nil, nil
		}
		s, err := checkSlices(slices, d.ps)
		if err != nil {
			return accessUnit{}, nil, err
		}
		if s != d.ps.active {
			return accessUnit{}, nil, fmt.Errorf("%w: SPS change without an IDR", ErrUnsupportedStream)
		}
		return newAccessUnit(false, s, nil, nil, slices), nil, nil
	}
	if len(spsN) == 0 || len(ppsN) == 0 {
		return accessUnit{}, nil, fmt.Errorf("%w: IDR without SPS and PPS in the packet", ErrUnsupportedStream)
	}
	ps := &paramSets{}
	err = ps.add(spsN, ppsN, d.cfg.MaxPixels, func(nal []byte, s *spsInfo) ([]byte, *spsInfo, error) {
		if !s.needsNormalize() {
			return nal, s, nil
		}
		v := s.f.vui
		setRestriction(&v, s.f.maxRefFrames)
		return rewriteSPS(nal, s, v, true, d.cfg.MaxPixels)
	})
	if err != nil {
		return accessUnit{}, nil, err
	}
	s, err := checkSlices(slices, ps)
	if err != nil {
		return accessUnit{}, nil, err
	}
	ps.active = s
	return newAccessUnit(true, s, ps.norm, ps.ppsOut, slices), ps, nil
}

// checkRepeated drops parameter sets in a delta packet that repeat the
// received bytes of one in force, and refuses any other.
func (d *Decoder) checkRepeated(spsN, ppsN [][]byte) error {
	for _, nal := range spsN {
		if d.ps == nil || !slices.ContainsFunc(d.ps.spsRaw[:], func(b []byte) bool { return bytes.Equal(b, nal) }) {
			return fmt.Errorf("%w: new SPS outside a keyframe", ErrUnsupportedStream)
		}
	}
	for _, nal := range ppsN {
		if d.ps == nil || !slices.ContainsFunc(d.ps.ppsRaw[:], func(b []byte) bool { return bytes.Equal(b, nal) }) {
			return fmt.Errorf("%w: new PPS outside a keyframe", ErrUnsupportedStream)
		}
	}
	return nil
}

func newAccessUnit(key bool, s *spsInfo, sps, pps, slices [][]byte) accessUnit {
	return accessUnit{
		key: key, sps: sps, pps: pps, slices: slices,
		width: s.width, height: s.height, codedWidth: 16 * s.widthMbs, codedHeight: 16 * s.heightMbs,
	}
}

// copyPicture copies the cropped top-left corner of a decoded picture
// (decoder validation step 11).
func copyPicture(pic picture, au accessUnit) (*kolovideo.Frame, error) {
	w, h := au.width, au.height
	if pic.width < w || pic.height < h || pic.width > au.codedWidth || pic.height > au.codedHeight {
		return nil, fmt.Errorf("%w: decoded frame %dx%d for a %dx%d picture coded as %dx%d",
			ErrCodec, pic.width, pic.height, w, h, au.codedWidth, au.codedHeight)
	}
	f, err := kolovideo.NewFrame(w, h)
	if err != nil {
		return nil, err
	}
	cw, ch := w/2, h/2
	if err := copyPlane(f.Y, f.YStride, pic.y, pic.yStride, w, h); err != nil {
		return nil, err
	}
	if !pic.nv12 {
		if err := copyPlane(f.U, f.UVStride, pic.u, pic.uvStride, cw, ch); err != nil {
			return nil, err
		}
		return f, copyPlane(f.V, f.UVStride, pic.v, pic.uvStride, cw, ch)
	}
	if !planeFits(len(pic.u), pic.uvStride, 2*cw, ch) {
		return nil, fmt.Errorf("%w: decoded chroma plane of %d bytes, stride %d, for %dx%d", ErrCodec, len(pic.u), pic.uvStride, cw, ch)
	}
	for y := range ch {
		splitUV(f.U[y*f.UVStride:y*f.UVStride+cw], f.V[y*f.UVStride:y*f.UVStride+cw], pic.u[y*pic.uvStride:y*pic.uvStride+2*cw])
	}
	return f, nil
}

// The two loops below go through stack chunks, so a race-enabled build checks
// one range per chunk instead of every chroma byte.

// splitUV writes the alternate bytes of uv into u and v; uv holds 2*len(u).
func splitUV(u, v, uv []byte) {
	var cu, cv [256]byte
	var c [512]byte
	for len(u) > 0 {
		n := min(len(u), len(cu))
		copy(c[:2*n], uv)
		for i := range n {
			cu[i], cv[i] = c[2*i], c[2*i+1]
		}
		copy(u, cu[:n])
		copy(v, cv[:n])
		u, v, uv = u[n:], v[n:], uv[2*n:]
	}
}

// joinUV writes u and v alternately into uv; uv holds 2*len(u).
func joinUV(uv, u, v []byte) {
	var cu, cv [256]byte
	var c [512]byte
	for len(u) > 0 {
		n := copy(cu[:], u)
		copy(cv[:n], v)
		for i := range n {
			c[2*i], c[2*i+1] = cu[i], cv[i]
		}
		copy(uv, c[:2*n])
		u, v, uv = u[n:], v[n:], uv[2*n:]
	}
}

func copyPlane(dst []byte, dstStride int, src []byte, stride, w, h int) error {
	if !planeFits(len(src), stride, w, h) {
		return fmt.Errorf("%w: decoded plane of %d bytes, stride %d, for %dx%d", ErrCodec, len(src), stride, w, h)
	}
	for y := range h {
		copy(dst[y*dstStride:y*dstStride+w], src[y*stride:y*stride+w])
	}
	return nil
}

// planeFits reports whether n bytes hold rows of rowBytes at stride, without
// overflowing int.
func planeFits(n, stride, rowBytes, rows int) bool {
	if rowBytes <= 0 || rows <= 0 || stride < rowBytes || n < rowBytes {
		return false
	}
	return (n-rowBytes)/stride >= rows-1
}

// SPS returns the active sequence parameter set, ok false before the first
// keyframe.
func (d *Decoder) SPS() (s SPS, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ps == nil {
		return SPS{}, false
	}
	return d.ps.active.export(), true
}

// Hardware reports whether the system decodes this stream in hardware.
func (d *Decoder) Hardware() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.impl != nil && d.impl.hardware()
}

// Close frees the decoder. It is idempotent. It waits for a Decode in
// progress.
func (d *Decoder) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.impl != nil {
		d.cleanup.Stop()
		d.impl.close()
		d.impl = nil
	}
	return nil
}
