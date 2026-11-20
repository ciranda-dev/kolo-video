package kolovideo

import (
	"fmt"
	"image"
)

// Conversions use the BT.601 limited range integer formulas (8-bit fixed
// point) published for 8-bit YUV video rendering.

// FromNV12 converts an NV12 picture (a Y plane and an interleaved UV plane)
// to a new I420 frame.
func FromNV12(width, height int, y []byte, yStride int, uv []byte, uvStride int) (*Frame, error) {
	if err := checkSize(width, height); err != nil {
		return nil, err
	}
	cw, ch := (width+1)/2, (height+1)/2
	if !planeFits(len(y), yStride, width, height) || !planeFits(len(uv), uvStride, 2*cw, ch) {
		return nil, fmt.Errorf("%w: NV12 planes too short for %dx%d", ErrInvalidFrame, width, height)
	}
	f, _ := NewFrame(width, height)
	for r := range height {
		copy(f.Y[r*width:(r+1)*width], y[r*yStride:])
	}
	for r := range ch {
		src := uv[r*uvStride : r*uvStride+2*cw]
		u, v := f.U[r*cw:(r+1)*cw], f.V[r*cw:(r+1)*cw]
		for x := range cw {
			u[x] = src[2*x]
			v[x] = src[2*x+1]
		}
	}
	return f, nil
}

// FromYUY2 converts a packed 4:2:2 picture in Y0 U Y1 V order (also called
// YUYV) to a new I420 frame. The width must be even.
func FromYUY2(width, height int, src []byte, stride int) (*Frame, error) {
	return fromPacked422(width, height, src, stride, 0, 1, 3)
}

// FromUYVY converts a packed 4:2:2 picture in U Y0 V Y1 order to a new I420
// frame. The width must be even.
func FromUYVY(width, height int, src []byte, stride int) (*Frame, error) {
	return fromPacked422(width, height, src, stride, 1, 0, 2)
}

// fromPacked422 takes the offsets of the first Y, the U and the V byte in
// each 4-byte group; the second Y is 2 bytes after the first.
func fromPacked422(width, height int, src []byte, stride, yo, uo, vo int) (*Frame, error) {
	if width%2 != 0 {
		return nil, fmt.Errorf("%w: packed 4:2:2 width %d is odd", ErrInvalidFrame, width)
	}
	if err := checkSize(width, height); err != nil {
		return nil, err
	}
	if !planeFits(len(src), stride, 2*width, height) {
		return nil, fmt.Errorf("%w: 4:2:2 buffer too short for %dx%d", ErrInvalidFrame, width, height)
	}
	f, _ := NewFrame(width, height)
	cw := width / 2
	for r := 0; r < height; r += 2 {
		s0 := src[r*stride : r*stride+2*width]
		s1 := s0
		if r+1 < height {
			s1 = src[(r+1)*stride : (r+1)*stride+2*width]
		}
		y0 := f.Y[r*width : (r+1)*width]
		u, v := f.U[r/2*cw:(r/2+1)*cw], f.V[r/2*cw:(r/2+1)*cw]
		for x := range cw {
			p0, p1 := s0[4*x:4*x+4], s1[4*x:4*x+4]
			y0[2*x] = p0[yo]
			y0[2*x+1] = p0[yo+2]
			u[x] = byte((int(p0[uo]) + int(p1[uo]) + 1) / 2)
			v[x] = byte((int(p0[vo]) + int(p1[vo]) + 1) / 2)
		}
		if r+1 < height {
			y1 := f.Y[(r+1)*width : (r+2)*width]
			for x := range cw {
				y1[2*x] = s1[4*x+yo]
				y1[2*x+1] = s1[4*x+yo+2]
			}
		}
	}
	return f, nil
}

// FromBGRA converts 32-bit B, G, R, A pixels to a new I420 frame. Alpha is
// ignored.
func FromBGRA(width, height int, src []byte, stride int) (*Frame, error) {
	return fromRGB32(width, height, src, stride, 2, 1, 0)
}

// FromRGBA converts 32-bit R, G, B, A pixels, such as image.RGBA.Pix, to a
// new I420 frame. Alpha is ignored.
func FromRGBA(width, height int, src []byte, stride int) (*Frame, error) {
	return fromRGB32(width, height, src, stride, 0, 1, 2)
}

func fromRGB32(width, height int, src []byte, stride, ro, gi, bo int) (*Frame, error) {
	if err := checkSize(width, height); err != nil {
		return nil, err
	}
	if !planeFits(len(src), stride, 4*width, height) {
		return nil, fmt.Errorf("%w: RGB buffer too short for %dx%d", ErrInvalidFrame, width, height)
	}
	f, _ := NewFrame(width, height)
	cw := f.UVStride
	for r := 0; r < height; r += 2 {
		rows := min(2, height-r)
		for cx := range cw {
			var rs, gs, bs, n int32
			for dy := range rows {
				line := src[(r+dy)*stride:]
				yrow := f.Y[(r+dy)*width:]
				for x := 2 * cx; x < 2*cx+2 && x < width; x++ {
					p := line[4*x : 4*x+4]
					cr, cg, cb := int32(p[ro]), int32(p[gi]), int32(p[bo])
					yrow[x] = byte((66*cr+129*cg+25*cb+128)>>8 + 16)
					rs += cr
					gs += cg
					bs += cb
					n++
				}
			}
			// Scale edge blocks of 1 or 2 pixels to a 4-pixel sum.
			rs, gs, bs = rs*4/n, gs*4/n, bs*4/n
			f.U[r/2*cw+cx] = byte((-38*rs-74*gs+112*bs+512)>>10 + 128)
			f.V[r/2*cw+cx] = byte((112*rs-94*gs-18*bs+512)>>10 + 128)
		}
	}
	return f, nil
}

// toRGBA writes f into dst at r.Min; r must be the frame size and lie
// inside dst.Rect.
func toRGBA(dst *image.RGBA, r image.Rectangle, f *Frame) {
	for y := range f.Height {
		yrow := f.Y[y*f.YStride : y*f.YStride+f.Width]
		urow := f.U[y/2*f.UVStride:]
		vrow := f.V[y/2*f.UVStride:]
		off := dst.PixOffset(r.Min.X, r.Min.Y+y)
		out := dst.Pix[off : off+4*f.Width]
		for x, yv := range yrow {
			c := 298 * (int32(yv) - 16)
			d := int32(urow[x/2]) - 128
			e := int32(vrow[x/2]) - 128
			p := out[4*x : 4*x+4]
			p[0] = clamp8((c + 409*e + 128) >> 8)
			p[1] = clamp8((c - 100*d - 208*e + 128) >> 8)
			p[2] = clamp8((c + 516*d + 128) >> 8)
			p[3] = 255
		}
	}
}
