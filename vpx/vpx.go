package vpx

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

// Encoder encodes I420 frames. Its methods are safe for concurrent use.
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
	frameTick uint64
}

// NewEncoder creates an encoder. Call Close to free it.
func NewEncoder(cfg Config) (*Encoder, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	st, err := current()
	if err != nil {
		return nil, err
	}
	threads, speed := encoderThreads(cfg.Codec, cfg.Width, cfg.Height, runtime.NumCPU())
	// Largest keyframe as a percentage of the per-frame budget: half the
	// optimal buffer (600 ms) at the frame rate, at least 300%.
	maxIntra := uint32(max(300, min(600*0.5*cfg.FrameRate/10, math.MaxUint32)))
	impl, err := st.eng.newEncoder(encParams{
		vp9: cfg.Codec == VP9, width: cfg.Width, height: cfg.Height, threads: threads, speed: speed,
		kbps: kbps(cfg.Bitrate), maxIntraPct: maxIntra,
	})
	if err != nil {
		return nil, err
	}
	enc := &Encoder{cfg: cfg, impl: impl, wantKey: true, frameTick: uint64(max(1, 90000/cfg.FrameRate+0.5))}
	enc.cleanup = runtime.AddCleanup(enc, encImpl.close, impl)
	return enc, nil
}

// encoderThreads returns the thread count and speed setting used by a
// widely deployed real-time encoder for this size and CPU count, except that
// VP8 never uses more than 4 threads: at 1080p, 8 threads encoded no faster
// than 4 and had spikes of 10 to 36 ms against 5 ms (BenchmarkEncodeThreads).
func encoderThreads(codec Codec, w, h, cpus int) (threads, speed int) {
	px := w * h
	if codec == VP9 {
		switch {
		case px >= 1280*720 && cpus > 4:
			return 4, 7
		case px >= 640*360 && cpus > 2:
			return 2, 7
		}
		return 1, 7
	}
	speed = -6
	if px < 352*288 {
		speed = -4
	}
	switch {
	case px >= 1920*1080 && cpus > 8:
		return 4, speed
	case px > 1280*960 && cpus >= 6:
		return 3, speed
	case px > 640*480 && cpus >= 3:
		if cpus >= 6 {
			return 3, speed
		}
		return 2, speed
	}
	return 1, speed
}

// Config returns the configuration with defaults filled in.
func (e *Encoder) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// Encode compresses one frame of the configured size. It returns no
// packet when rate control drops the frame. A keyframe is made when
// forceKeyframe is set, for the first frame, and every KeyframeInterval.
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
		return nil, fmt.Errorf("kolovideo/vpx: frame is %dx%d, encoder is %dx%d: %w", f.Width, f.Height, e.cfg.Width, e.cfg.Height, kolovideo.ErrInvalidFrame)
	}
	if f.YStride > math.MaxInt32 || f.UVStride > math.MaxInt32 {
		return nil, fmt.Errorf("kolovideo/vpx: stride above 2^31-1: %w", kolovideo.ErrInvalidFrame)
	}
	if !e.started {
		e.started, e.base, e.lastPts, e.lastKey = true, f.Timestamp, -1, f.Timestamp
	}
	if forceKeyframe || (e.cfg.KeyframeInterval > 0 && f.Timestamp-e.lastKey >= e.cfg.KeyframeInterval) {
		e.wantKey = true
	}
	dur := e.frameTick
	if f.Duration > 0 {
		dur = uint64(max(1, ticks90k(f.Duration)))
	}
	// 90 kHz is the RTP clock rate for VP8 and VP9. A timestamp that does
	// not advance gets one frame time, so rate control sees the true rate.
	pts := ticks90k(f.Timestamp - e.base)
	if pts <= e.lastPts {
		pts = e.lastPts + int64(min(dur, uint64(math.MaxInt64-e.lastPts)))
	}
	e.lastPts = pts
	var pkts []Packet
	// The library takes a 32-bit duration.
	err := e.impl.encode(f.Y, f.U, f.V, f.YStride, f.UVStride, pts, uint32(min(dur, math.MaxUint32)), e.wantKey, func(data []byte, key bool) {
		pkts = append(pkts, Packet{Data: append([]byte(nil), data...), Keyframe: key, Timestamp: f.Timestamp, Duration: f.Duration})
	})
	if err != nil {
		return nil, err
	}
	for _, p := range pkts {
		if p.Keyframe {
			e.wantKey = false
			e.lastKey = f.Timestamp
		}
	}
	return pkts, nil
}

// ticks90k converts a duration to 90 kHz ticks without overflow.
func ticks90k(d time.Duration) int64 {
	ns := int64(d)
	return ns/100000*9 + ns%100000*9/100000
}

// SetBitrate changes the target bitrate in bits per second, effective from
// the next frame.
func (e *Encoder) SetBitrate(bps int) error {
	if bps <= 0 || uint64(bps)/1000 > math.MaxUint32 {
		return fmt.Errorf("kolovideo/vpx: bitrate %d: %w", bps, kolovideo.ErrInvalidConfig)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.impl == nil {
		return kolovideo.ErrClosed
	}
	if err := e.impl.setBitrate(kbps(bps)); err != nil {
		return err
	}
	e.cfg.Bitrate = bps
	return nil
}

// Close frees the encoder. It is idempotent.
func (e *Encoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.impl != nil {
		e.cleanup.Stop()
		e.impl.close()
		e.impl = nil
	}
	return nil
}

// Decoder decodes packets into I420 frames. Its methods are safe for
// concurrent use.
type Decoder struct {
	mu      sync.Mutex
	cfg     DecoderConfig
	impl    decImpl
	cleanup runtime.Cleanup
	needKey bool
	buf     []byte
}

// NewDecoder creates a decoder. Call Close to free it.
func NewDecoder(cfg DecoderConfig) (*Decoder, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	st, err := current()
	if err != nil {
		return nil, err
	}
	impl, err := st.eng.newDecoder(cfg.Codec == VP9, 0)
	if err != nil {
		return nil, err
	}
	dec := &Decoder{cfg: cfg, impl: impl, needKey: true}
	dec.cleanup = runtime.AddCleanup(dec, decImpl.close, impl)
	return dec, nil
}

// Decode decodes one packet. It returns a nil frame and nil error when the
// packet holds no frame to show. Frames may change size at keyframes.
// After any error, packets are refused with ErrNeedKeyframe until the next
// keyframe.
func (d *Decoder) Decode(p Packet) (*kolovideo.Frame, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.impl == nil {
		return nil, kolovideo.ErrClosed
	}
	if len(p.Data) > d.cfg.MaxPacketBytes {
		d.needKey = true
		return nil, fmt.Errorf("%w: packet of %d bytes", ErrTooLarge, len(p.Data))
	}
	defer d.trimBuf()
	// Decode from a private copy, so the bytes libvpx reads are the bytes
	// that were checked even if the caller reuses p.Data meanwhile.
	d.buf = append(d.buf[:0], p.Data...)
	data, key, err := d.check(d.buf)
	if err != nil {
		d.needKey = true
		return nil, err
	}
	if d.needKey && !key {
		return nil, ErrNeedKeyframe
	}
	img, ok, err := d.impl.decode(data)
	if err != nil {
		d.needKey = true
		return nil, err
	}
	if d.impl.corrupted() {
		d.needKey = true
		return nil, fmt.Errorf("%w: decoder reports a corrupted frame", ErrCorrupt)
	}
	d.needKey = false
	if !ok {
		return nil, nil
	}
	f, err := d.copyImage(img)
	if err != nil {
		d.needKey = true
		return nil, err
	}
	f.Timestamp, f.Duration = p.Timestamp, p.Duration
	return f, nil
}

// retainBytes is the largest packet buffer a Decoder keeps between calls: 1
// MiB, well above a 1080p frame at the default bitrate. A larger packet
// uses a buffer that is dropped after the call.
const retainBytes = 1 << 20

func (d *Decoder) trimBuf() {
	if cap(d.buf) > retainBytes {
		d.buf = nil
	}
}

// check validates the frame headers in a packet against MaxPixels and
// returns the bytes to decode and whether the packet starts with a
// keyframe.
func (d *Decoder) check(b []byte) ([]byte, bool, error) {
	if len(b) == 0 {
		return nil, false, fmt.Errorf("%w: empty packet", ErrCorrupt)
	}
	if d.cfg.Codec == VP8 {
		info, err := vp8Info(b)
		if err != nil {
			return nil, false, err
		}
		return b, info.key, d.checkSize(info)
	}
	frames, indexed, err := vp9Frames(b)
	if err != nil {
		return nil, false, err
	}
	key := false
	for i, fr := range frames {
		info, err := vp9Info(fr)
		if err != nil {
			return nil, false, err
		}
		if err := d.checkSize(info); err != nil {
			return nil, false, err
		}
		if i == 0 {
			key = info.key
		}
	}
	if !indexed {
		d.buf = appendSuperframeIndex(b)
		b = d.buf
	}
	return b, key, nil
}

func (d *Decoder) checkSize(info frameInfo) error {
	if info.width > 0 && info.height > d.cfg.MaxPixels/info.width {
		return fmt.Errorf("%w: %dx%d", ErrTooLarge, info.width, info.height)
	}
	return nil
}

// copyImage copies a decoded image out of the engine's memory.
func (d *Decoder) copyImage(img plane420) (*kolovideo.Frame, error) {
	w, h := img.width, img.height
	if w <= 0 || h <= 0 || h > d.cfg.MaxPixels/w {
		return nil, fmt.Errorf("%w: decoded %dx%d", ErrTooLarge, w, h)
	}
	f, err := kolovideo.NewFrame(w, h)
	if err != nil {
		return nil, err
	}
	cw, ch := (w+1)/2, (h+1)/2
	for _, p := range []struct {
		dst, src []byte
		stride   int
		w, h     int
	}{{f.Y, img.y, img.yStride, w, h}, {f.U, img.u, img.uvStride, cw, ch}, {f.V, img.v, img.uvStride, cw, ch}} {
		if err := copyPlane(p.dst, p.w, p.src, p.stride, p.w, p.h); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func copyPlane(dst []byte, dstStride int, src []byte, stride, w, h int) error {
	if stride < w || len(src) < (h-1)*stride+w {
		return fmt.Errorf("%w: decoded plane stride %d for width %d", ErrCodec, stride, w)
	}
	for y := range h {
		copy(dst[y*dstStride:y*dstStride+w], src[y*stride:y*stride+w])
	}
	return nil
}

// Close frees the decoder. It is idempotent.
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
