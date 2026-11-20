package kolovideo

import (
	"fmt"
	"image"

	"golang.org/x/image/draw"
)

// ScaleMode says how Renderer maps a frame onto a destination of another
// shape.
type ScaleMode int

const (
	// Fit shows the whole frame and fills the bars left over with black.
	Fit ScaleMode = iota
	// Fill covers the whole destination and crops the frame's overflow.
	Fill
)

// Renderer draws frames into RGBA images of any size. It keeps a scratch
// image between calls, so reuse one Renderer per video stream. A Renderer
// is not safe for concurrent use.
type Renderer struct {
	Scale   ScaleMode
	scratch *image.RGBA
}

// Render draws f into dst.Bounds(), keeping the frame's aspect ratio.
// Scaling uses approximate bilinear interpolation.
func (r *Renderer) Render(dst *image.RGBA, f *Frame) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if r.Scale != Fit && r.Scale != Fill {
		return fmt.Errorf("%w: scale mode %d", ErrInvalidConfig, r.Scale)
	}
	if dst == nil {
		return fmt.Errorf("%w: nil destination", ErrInvalidConfig)
	}
	db := dst.Bounds()
	if db.Empty() {
		return nil
	}
	dr, sr := placement(db, f.Width, f.Height, r.Scale)
	if dr != db {
		fillBlack(dst, db, dr)
	}
	if dr.Dx() == f.Width && dr.Dy() == f.Height && sr == image.Rect(0, 0, f.Width, f.Height) {
		toRGBA(dst, dr, f)
		return nil
	}
	if r.scratch == nil || r.scratch.Rect.Dx() != f.Width || r.scratch.Rect.Dy() != f.Height {
		r.scratch = image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))
	}
	toRGBA(r.scratch, r.scratch.Rect, f)
	draw.ApproxBiLinear.Scale(dst, dr, r.scratch, sr, draw.Src, nil)
	return nil
}

// placement returns the destination and source rectangles for a w by h
// frame drawn into db.
func placement(db image.Rectangle, w, h int, mode ScaleMode) (dr, sr image.Rectangle) {
	dw, dh := db.Dx(), db.Dy()
	sr = image.Rect(0, 0, w, h)
	// Compare aspect ratios as dw/dh against w/h without division.
	wider := int64(dw)*int64(h) > int64(w)*int64(dh)
	if mode == Fit {
		if wider {
			nw := max(1, int(int64(w)*int64(dh)/int64(h)))
			x := db.Min.X + (dw-nw)/2
			return image.Rect(x, db.Min.Y, x+nw, db.Max.Y), sr
		}
		nh := max(1, int(int64(h)*int64(dw)/int64(w)))
		y := db.Min.Y + (dh-nh)/2
		return image.Rect(db.Min.X, y, db.Max.X, y+nh), sr
	}
	if wider {
		ch := max(1, int(int64(w)*int64(dh)/int64(dw)))
		y := (h - ch) / 2
		return db, image.Rect(0, y, w, y+ch)
	}
	cw := max(1, int(int64(h)*int64(dw)/int64(dh)))
	x := (w - cw) / 2
	return db, image.Rect(x, 0, x+cw, h)
}

// fillBlack paints the parts of db outside inner opaque black.
func fillBlack(dst *image.RGBA, db, inner image.Rectangle) {
	black := image.NewUniform(image.Black)
	for _, r := range []image.Rectangle{
		image.Rect(db.Min.X, db.Min.Y, db.Max.X, inner.Min.Y),
		image.Rect(db.Min.X, inner.Max.Y, db.Max.X, db.Max.Y),
		image.Rect(db.Min.X, inner.Min.Y, inner.Min.X, inner.Max.Y),
		image.Rect(inner.Max.X, inner.Min.Y, db.Max.X, inner.Max.Y),
	} {
		if !r.Empty() {
			draw.Draw(dst, r, black, image.Point{}, draw.Src)
		}
	}
}
