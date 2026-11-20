package kolovideo

import (
	"fmt"
	"image"
	"math"
	"time"
)

// maxDimension is the largest frame width or height, the VP9 limit (16-bit
// size fields).
const maxDimension = 1 << 16

// Frame is a raw I420 picture: planar YUV 4:2:0, 8 bits per sample, BT.601
// limited range. The U and V planes are (Width+1)/2 by (Height+1)/2.
type Frame struct {
	Width, Height int
	Y, U, V       []byte
	// YStride and UVStride are the bytes between the starts of two rows.
	YStride, UVStride int
	// Timestamp is the capture time on the system monotonic clock.
	Timestamp time.Duration
	// Duration is the nominal display time of the frame.
	Duration time.Duration
}

// NewFrame allocates a black frame with tightly packed planes.
func NewFrame(width, height int) (*Frame, error) {
	if err := checkSize(width, height); err != nil {
		return nil, err
	}
	cw, ch := (width+1)/2, (height+1)/2
	ys, cs := width*height, cw*ch
	buf := make([]byte, ys+2*cs)
	fill(buf[:ys], 16)
	fill(buf[ys:], 128)
	return &Frame{
		Width: width, Height: height,
		Y: buf[:ys:ys], U: buf[ys : ys+cs : ys+cs], V: buf[ys+cs:],
		YStride: width, UVStride: cw,
	}, nil
}

// Validate reports whether the sizes, strides and plane lengths agree.
func (f *Frame) Validate() error {
	if f == nil {
		return fmt.Errorf("%w: nil frame", ErrInvalidFrame)
	}
	if err := checkSize(f.Width, f.Height); err != nil {
		return err
	}
	cw, ch := (f.Width+1)/2, (f.Height+1)/2
	if !planeFits(len(f.Y), f.YStride, f.Width, f.Height) {
		return fmt.Errorf("%w: Y plane too short for stride %d", ErrInvalidFrame, f.YStride)
	}
	if !planeFits(len(f.U), f.UVStride, cw, ch) || !planeFits(len(f.V), f.UVStride, cw, ch) {
		return fmt.Errorf("%w: U or V plane too short for stride %d", ErrInvalidFrame, f.UVStride)
	}
	return nil
}

// checkSize rejects sizes out of range or whose planes would not fit in an
// int (on 32-bit systems).
func checkSize(w, h int) error {
	if w <= 0 || h <= 0 || w > maxDimension || h > maxDimension ||
		uint64(w)*uint64(h)+2*uint64((w+1)/2)*uint64((h+1)/2) > math.MaxInt {
		return fmt.Errorf("%w: size %dx%d", ErrInvalidFrame, w, h)
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

// YCbCr returns an image that shares the frame's planes, or nil if the frame
// is invalid. The image/color package treats the samples as full range, so
// use RGBA or Renderer for exact colors.
func (f *Frame) YCbCr() *image.YCbCr {
	if f.Validate() != nil {
		return nil
	}
	return &image.YCbCr{
		Y: f.Y, Cb: f.U, Cr: f.V,
		YStride: f.YStride, CStride: f.UVStride,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, f.Width, f.Height),
	}
}

// RGBA converts the frame to a new image of the same size.
func (f *Frame) RGBA() (*image.RGBA, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	dst := image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))
	toRGBA(dst, dst.Rect, f)
	return dst, nil
}

// rec709to601 rewrites samples encoded with the BT.709 matrix to BT.601.
// Coefficients are the 16.16 fixed-point product of the BT.601 forward and
// BT.709 inverse matrices, both limited range.
func (f *Frame) rec709to601() {
	cw, ch := (f.Width+1)/2, (f.Height+1)/2
	for cy := range ch {
		ur := f.U[cy*f.UVStride : cy*f.UVStride+cw]
		vr := f.V[cy*f.UVStride : cy*f.UVStride+cw]
		for cx := range cw {
			du, dv := int32(ur[cx])-128, int32(vr[cx])-128
			dy := (6508*du + 12563*dv + 1<<15) >> 16
			for y := 2 * cy; y < 2*cy+2 && y < f.Height; y++ {
				row := f.Y[y*f.YStride:]
				for x := 2 * cx; x < 2*cx+2 && x < f.Width; x++ {
					row[x] = clamp8(int32(row[x]) + dy)
				}
			}
			ur[cx] = clamp8(128 + (64871*du-7252*dv+1<<15)>>16)
			vr[cx] = clamp8(128 + (-4748*du+64448*dv+1<<15)>>16)
		}
	}
}

// fullToLimited maps full range samples (0-255) to limited range.
func (f *Frame) fullToLimited() {
	cw, ch := (f.Width+1)/2, (f.Height+1)/2
	for y := range f.Height {
		row := f.Y[y*f.YStride : y*f.YStride+f.Width]
		for i, v := range row {
			row[i] = lumaFullToLimited[v]
		}
	}
	for y := range ch {
		for _, p := range [][]byte{f.U, f.V} {
			row := p[y*f.UVStride : y*f.UVStride+cw]
			for i, v := range row {
				row[i] = chromaFullToLimited[v]
			}
		}
	}
}

var lumaFullToLimited, chromaFullToLimited = rangeTables()

func rangeTables() (luma, chroma [256]byte) {
	for i := range 256 {
		luma[i] = byte(16 + (i*219+127)/255)
		d := (i - 128) * 224
		if d >= 0 {
			d = (d + 127) / 255
		} else {
			d = (d - 127) / 255
		}
		chroma[i] = byte(128 + d)
	}
	return luma, chroma
}

func fill(b []byte, v byte) {
	for i := range b {
		b[i] = v
	}
}

func clamp8(v int32) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// copyFrame returns a tightly packed copy of a valid frame.
func copyFrame(src *Frame) *Frame {
	f, _ := NewFrame(src.Width, src.Height)
	cw, ch := (src.Width+1)/2, (src.Height+1)/2
	for y := range src.Height {
		copy(f.Y[y*f.YStride:y*f.YStride+src.Width], src.Y[y*src.YStride:])
	}
	for y := range ch {
		copy(f.U[y*cw:(y+1)*cw], src.U[y*src.UVStride:])
		copy(f.V[y*cw:(y+1)*cw], src.V[y*src.UVStride:])
	}
	f.Timestamp, f.Duration = src.Timestamp, src.Duration
	return f
}
