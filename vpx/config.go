package vpx

import (
	"errors"
	"fmt"
	"math"
	"time"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

var (
	// ErrCorrupt means a packet is truncated or not a valid frame.
	ErrCorrupt = errors.New("kolovideo/vpx: corrupt packet")
	// ErrTooLarge means a packet is larger than MaxPacketBytes or declares a
	// frame larger than MaxPixels.
	ErrTooLarge = errors.New("kolovideo/vpx: packet or frame too large")
	// ErrNeedKeyframe means the decoder drops packets until a keyframe,
	// at the start and after a decoding error.
	ErrNeedKeyframe = errors.New("kolovideo/vpx: waiting for a keyframe")
	// ErrCodec means libvpx failed for a reason other than bad input.
	ErrCodec = errors.New("kolovideo/vpx: codec error")
)

// Codec is a video codec.
type Codec int

// Supported codecs.
const (
	VP8 Codec = iota + 1
	VP9
)

// String returns the codec name.
func (c Codec) String() string {
	switch c {
	case VP8:
		return "VP8"
	case VP9:
		return "VP9"
	}
	return fmt.Sprintf("Codec(%d)", int(c))
}

// Encoder defaults.
const (
	// DefaultFrameRate is the rate assumed for frames without a Duration:
	// kolovideo.DefaultFrameRate.
	DefaultFrameRate = kolovideo.DefaultFrameRate
	// DefaultBitsPerPixel sets the default bitrate: 0.07 bits per pixel per
	// frame, about 4.4 Mbit/s for 1080p at 30 fps.
	DefaultBitsPerPixel = 0.07
	// DefaultKeyframeInterval starts a new group of pictures every 2 s.
	DefaultKeyframeInterval = 2 * time.Second
)

// DefaultMaxPixels bounds decoded frames: 8,912,896 pixels, the VP9 level
// 5 picture size (4096x2176), as kolovideo.DefaultMaxPixels for capture.
const DefaultMaxPixels = kolovideo.DefaultMaxPixels

// DefaultMaxPacketBytes bounds a packet: 16 MiB, the default MaxBytes of the
// kolo-media playout. A VP8 or VP9 frame at DefaultMaxPixels is far smaller.
const DefaultMaxPacketBytes = 16 << 20

// maxPacketBytes is the largest MaxPacketBytes: 256 MiB, as in package h264.
const maxPacketBytes = 1 << 28

// Config sets up an Encoder. Zero fields take defaults.
type Config struct {
	Codec Codec
	// Width and Height of every frame: 1 to 16383 for VP8, 1 to 65536 for
	// VP9.
	Width, Height int
	// FrameRate is the nominal rate, DefaultFrameRate if zero; otherwise
	// kolovideo.MinFrameRate to kolovideo.MaxFrameRate.
	FrameRate float64
	// Bitrate is the target in bits per second, DefaultBitsPerPixel times
	// pixels times FrameRate if zero.
	Bitrate int
	// KeyframeInterval is the time between keyframes, measured on frame
	// timestamps; DefaultKeyframeInterval if zero, negative for keyframes
	// only on request.
	KeyframeInterval time.Duration
}

func (c Config) withDefaults() (Config, error) {
	limit := 16383
	switch c.Codec {
	case VP8:
	case VP9:
		limit = 65536
	default:
		return c, fmt.Errorf("kolovideo/vpx: codec %v: %w", c.Codec, kolovideo.ErrInvalidConfig)
	}
	if c.Width < 1 || c.Height < 1 || c.Width > limit || c.Height > limit {
		return c, fmt.Errorf("kolovideo/vpx: %v size %dx%d: %w", c.Codec, c.Width, c.Height, kolovideo.ErrInvalidConfig)
	}
	if c.FrameRate != 0 && !(c.FrameRate >= kolovideo.MinFrameRate && c.FrameRate <= kolovideo.MaxFrameRate) {
		return c, fmt.Errorf("kolovideo/vpx: frame rate %v: %w", c.FrameRate, kolovideo.ErrInvalidConfig)
	}
	if c.FrameRate == 0 {
		c.FrameRate = DefaultFrameRate
	}
	if c.Bitrate < 0 || uint64(c.Bitrate)/1000 > math.MaxUint32 {
		return c, fmt.Errorf("kolovideo/vpx: bitrate %d: %w", c.Bitrate, kolovideo.ErrInvalidConfig)
	}
	if c.Bitrate == 0 {
		// Capped so the conversion to int cannot overflow on 32-bit systems.
		c.Bitrate = int(min(DefaultBitsPerPixel*float64(c.Width)*float64(c.Height)*c.FrameRate, math.MaxInt32))
	}
	if c.KeyframeInterval == 0 {
		c.KeyframeInterval = DefaultKeyframeInterval
	}
	return c, nil
}

// kbps converts bits per second to libvpx's kilobits, at least 1 and at most
// what a uint32 holds.
func kbps(bps int) uint32 {
	return uint32(min(max(1, (uint64(max(bps, 0))+500)/1000), math.MaxUint32))
}

// DecoderConfig sets up a Decoder.
type DecoderConfig struct {
	Codec Codec
	// MaxPixels is the largest frame accepted, checked in each frame
	// header before decoding; DefaultMaxPixels if zero.
	MaxPixels int
	// MaxPacketBytes bounds a packet, checked before it is copied;
	// DefaultMaxPacketBytes if zero, at most 256 MiB.
	MaxPacketBytes int
}

func (c DecoderConfig) withDefaults() (DecoderConfig, error) {
	if c.Codec != VP8 && c.Codec != VP9 {
		return c, fmt.Errorf("kolovideo/vpx: codec %v: %w", c.Codec, kolovideo.ErrInvalidConfig)
	}
	if c.MaxPixels < 0 {
		return c, fmt.Errorf("kolovideo/vpx: max pixels %d: %w", c.MaxPixels, kolovideo.ErrInvalidConfig)
	}
	if c.MaxPixels == 0 {
		c.MaxPixels = DefaultMaxPixels
	}
	if c.MaxPacketBytes < 0 || c.MaxPacketBytes > maxPacketBytes {
		return c, fmt.Errorf("kolovideo/vpx: max packet bytes %d: %w", c.MaxPacketBytes, kolovideo.ErrInvalidConfig)
	}
	if c.MaxPacketBytes == 0 {
		c.MaxPacketBytes = DefaultMaxPacketBytes
	}
	return c, nil
}

// Packet is one encoded frame.
type Packet struct {
	Data     []byte
	Keyframe bool
	// Timestamp and Duration come from the frame and pass through the
	// decoder unchanged.
	Timestamp time.Duration
	Duration  time.Duration
}
