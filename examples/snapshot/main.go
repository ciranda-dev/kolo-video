// Command snapshot saves camera frames as PNG files in a directory.
//
//	snapshot -list
//	snapshot -dir /tmp/shots -n 3
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"os/signal"
	"path/filepath"

	kolovideo "github.com/ciranda-dev/kolo-video"
)

func main() {
	dir := flag.String("dir", "", "existing directory for the PNG files")
	n := flag.Int("n", 1, "number of frames to save")
	device := flag.String("device", "", "camera ID from -list; empty for the default camera")
	list := flag.Bool("list", false, "list cameras and their modes, then exit")
	flag.Parse()

	if *list {
		devs, err := kolovideo.Devices()
		if err != nil {
			log.Fatal(err)
		}
		for _, d := range devs {
			fmt.Printf("%s\t%s\tdefault=%v\n", d.ID, d.Name, d.Default)
			for _, m := range d.Modes {
				fmt.Printf("\t%dx%d up to %.2f fps %v\n", m.Width, m.Height, m.FrameRate, m.Format)
			}
		}
		return
	}
	if *dir == "" || *n < 1 {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *dir, *device, *n); err != nil {
		stop()
		log.Fatal(err)
	}
}

func run(ctx context.Context, dir, device string, n int) error {
	if err := kolovideo.RequestAccess(ctx); err != nil {
		return err
	}
	c, err := kolovideo.OpenCapture(kolovideo.CaptureConfig{DeviceID: device})
	if err != nil {
		return err
	}
	defer c.Close()
	fmt.Printf("%s: %+v\n", c.Device().Name, c.Mode())

	for i := range n {
		f, err := c.Read(ctx)
		if err != nil {
			return err
		}
		img, err := f.RGBA()
		if err != nil {
			return err
		}
		path := filepath.Join(dir, fmt.Sprintf("frame-%03d.png", i))
		if err := savePNG(path, img); err != nil {
			return err
		}
		fmt.Println(path)
	}
	fmt.Printf("%+v\n", c.Stats())
	return nil
}

// savePNG writes img to a new file only its owner can read: a camera image is
// private, and O_EXCL never replaces an existing file.
func savePNG(path string, img image.Image) error {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := png.Encode(out, img); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
