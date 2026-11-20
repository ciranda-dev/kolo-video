// Command vp8 encodes generated frames with VP8, decodes them again and
// prints the stream size and the worst luma PSNR. The only file it writes is
// the native library cache, in a temporary directory unless -cache names one.
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"os"
	"time"

	kolovideo "github.com/ciranda-dev/kolo-video"
	"github.com/ciranda-dev/kolo-video/vpx"
)

const width, height, frames = 640, 360, 90

func main() {
	cache := flag.String("cache", "", "directory for the native library cache; empty uses a temporary directory that is removed at exit")
	flag.Parse()
	if err := run(*cache); err != nil {
		log.Fatal(err)
	}
}

func run(cache string) error {
	if cache == "" {
		dir, err := os.MkdirTemp("", "kolovideo-vp8-")
		if err != nil {
			return err
		}
		// A loaded library cannot be deleted on Windows; the error is ignored.
		defer os.RemoveAll(dir)
		cache = dir
	}
	rt, err := vpx.Load(vpx.LoadConfig{CacheDir: cache})
	if err != nil {
		return err
	}
	fmt.Printf("libvpx %s, %v engine\n", rt.Version, rt.Engine)
	if rt.NativeErr != nil {
		fmt.Println("native engine not used:", rt.NativeErr)
	}
	enc, err := vpx.NewEncoder(vpx.Config{Codec: vpx.VP8, Width: width, Height: height})
	if err != nil {
		return err
	}
	defer enc.Close()
	dec, err := vpx.NewDecoder(vpx.DecoderConfig{Codec: vpx.VP8})
	if err != nil {
		return err
	}
	defer dec.Close()

	total, keys, worst := 0, 0, math.Inf(1)
	for i := range frames {
		in, err := pattern(i)
		if err != nil {
			return err
		}
		pkts, err := enc.Encode(in, false)
		if err != nil {
			return err
		}
		for _, p := range pkts {
			total += len(p.Data)
			if p.Keyframe {
				keys++
			}
			out, err := dec.Decode(p)
			if err != nil {
				return err
			}
			if out != nil {
				worst = min(worst, psnr(in, out))
			}
		}
	}
	cfg := enc.Config()
	fmt.Printf("%d frames %dx%d, target %d bit/s: %d bytes (%.0f bit/s), %d keyframes, worst PSNR %.1f dB\n",
		frames, width, height, cfg.Bitrate, total, float64(total*8)/(frames/cfg.FrameRate), keys, worst)
	return nil
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
