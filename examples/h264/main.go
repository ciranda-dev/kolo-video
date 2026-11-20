// Command h264 encodes generated frames with H.264 when the operating system
// has a codec, decodes them again and prints the engine, the stream size and
// the worst luma PSNR. Where H.264 is absent it prints why and does the same
// with VP8. The only file it writes is the VP8 native library cache, in a
// temporary directory unless -cache names one.
package main

import (
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"os"
	"time"

	kolovideo "github.com/ciranda-dev/kolo-video"
	"github.com/ciranda-dev/kolo-video/h264"
	"github.com/ciranda-dev/kolo-video/vpx"
)

const width, height, frames = 640, 360, 90

// codec is one encode and decode pair; encode returns the packets of a frame, decode their pictures.
type codec struct {
	encode  func(*kolovideo.Frame) ([]packet, error)
	decode  func(packet) (*kolovideo.Frame, error)
	bitrate int
	close   func()
}

type packet struct {
	data     []byte
	keyframe bool
}

func main() {
	cache := flag.String("cache", "", "directory for the VP8 native library cache; empty uses a temporary directory that is removed at exit")
	flag.Parse()
	if err := run(*cache); err != nil {
		log.Fatal(err)
	}
}

func run(cache string) error {
	c, err := newH264()
	if errors.Is(err, h264.ErrUnavailable) {
		fmt.Println("H.264 is not available, using VP8:", err)
		c, err = newVP8(cache)
	}
	if err != nil {
		return err
	}
	defer c.close()

	total, keys, worst := 0, 0, math.Inf(1)
	for i := range frames {
		in, err := pattern(i)
		if err != nil {
			return err
		}
		pkts, err := c.encode(in)
		if err != nil {
			return err
		}
		for _, p := range pkts {
			total += len(p.data)
			if p.keyframe {
				keys++
			}
			out, err := c.decode(p)
			if err != nil {
				return err
			}
			if out != nil {
				worst = min(worst, psnr(in, out))
			}
		}
	}
	fmt.Printf("%d frames %dx%d, target %d bit/s: %d bytes (%.0f bit/s), %d keyframes, worst PSNR %.1f dB\n",
		frames, width, height, c.bitrate, total, float64(total*8)/(frames/30.0), keys, worst)
	return nil
}

func newH264() (*codec, error) {
	caps := h264.Capabilities()
	for _, s := range []h264.Support{caps.Encode, caps.Decode} {
		if !s.Available {
			return nil, s.Err
		}
	}
	enc, err := h264.NewEncoder(h264.Config{Width: width, Height: height})
	if err != nil {
		return nil, err
	}
	dec, err := h264.NewDecoder(h264.DecoderConfig{})
	if err != nil {
		enc.Close()
		return nil, err
	}
	fmt.Printf("H.264 through %s, encode hardware %v, decode profiles %v\n", caps.Encode.Engine, caps.Encode.Hardware, caps.Decode.Profiles)
	first := true
	return &codec{
		bitrate: enc.Config().Bitrate,
		encode: func(f *kolovideo.Frame) ([]packet, error) {
			pkts, err := enc.Encode(f, false)
			out := make([]packet, len(pkts))
			for i, p := range pkts {
				out[i] = packet{p.Data, p.Keyframe}
				if first && p.Keyframe {
					first = false
					if s, err := h264.ParseKeyframe(p.Data); err == nil {
						fmt.Printf("codec string %s, encoder hardware %v\n", s.Codec(), enc.Hardware())
					}
				}
			}
			return out, err
		},
		decode: func(p packet) (*kolovideo.Frame, error) {
			return dec.Decode(h264.Packet{Data: p.data, Keyframe: p.keyframe})
		},
		close: func() { enc.Close(); dec.Close() },
	}, nil
}

func newVP8(cache string) (*codec, error) {
	cleanup := func() {}
	if cache == "" {
		dir, err := os.MkdirTemp("", "kolovideo-h264-")
		if err != nil {
			return nil, err
		}
		cache = dir
		// A loaded library cannot be deleted on Windows; the error is ignored.
		cleanup = func() { os.RemoveAll(dir) }
	}
	rt, err := vpx.Load(vpx.LoadConfig{CacheDir: cache})
	if err != nil {
		cleanup()
		return nil, err
	}
	fmt.Printf("VP8 through libvpx %s, %v engine\n", rt.Version, rt.Engine)
	enc, err := vpx.NewEncoder(vpx.Config{Codec: vpx.VP8, Width: width, Height: height})
	if err != nil {
		cleanup()
		return nil, err
	}
	dec, err := vpx.NewDecoder(vpx.DecoderConfig{Codec: vpx.VP8})
	if err != nil {
		enc.Close()
		cleanup()
		return nil, err
	}
	return &codec{
		bitrate: enc.Config().Bitrate,
		encode: func(f *kolovideo.Frame) ([]packet, error) {
			pkts, err := enc.Encode(f, false)
			out := make([]packet, len(pkts))
			for i, p := range pkts {
				out[i] = packet{p.Data, p.Keyframe}
			}
			return out, err
		},
		decode: func(p packet) (*kolovideo.Frame, error) {
			return dec.Decode(vpx.Packet{Data: p.data, Keyframe: p.keyframe})
		},
		close: func() { enc.Close(); dec.Close(); cleanup() },
	}, nil
}

// pattern draws a gradient with a moving square.
func pattern(n int) (*kolovideo.Frame, error) {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			c := color.RGBA{byte(x * 255 / width), byte(y * 255 / height), 128, 255}
			if x >= 4*n && x < 4*n+80 && y >= 140 && y < 220 {
				c = color.RGBA{255, 255, 255, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	f, err := kolovideo.FromRGBA(width, height, img.Pix, img.Stride)
	if err != nil {
		return nil, err
	}
	f.Timestamp = time.Duration(n) * time.Second / 30
	f.Duration = time.Second / 30
	return f, nil
}

func psnr(a, b *kolovideo.Frame) float64 {
	var sum float64
	for y := range a.Height {
		for x := range a.Width {
			d := float64(a.Y[y*a.YStride+x]) - float64(b.Y[y*b.YStride+x])
			sum += d * d
		}
	}
	return 10 * math.Log10(255*255*float64(a.Width*a.Height)/sum)
}
