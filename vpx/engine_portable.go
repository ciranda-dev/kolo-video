package vpx

import (
	"errors"
	"fmt"

	kolovideo "github.com/ciranda-dev/kolo-video"
	"github.com/ciranda-dev/kolo-video/vpx/internal/portable"
)

type portableEngine struct{ ver string }

func loadPortable() (engine, error) {
	v := portable.Version()
	if v == "" {
		return nil, errors.New("kolovideo/vpx: portable libvpx does not start")
	}
	return &portableEngine{v}, nil
}

func (*portableEngine) kind() EngineKind  { return EnginePortable }
func (p *portableEngine) version() string { return p.ver }

// fromPortable maps an error of the portable engine to the package's errors.
func fromPortable(op string, err error) error {
	var ce *portable.CodecError
	var te *portable.TrapError
	switch {
	case errors.As(err, &ce):
		return &codecError{ce.Op, ce.Code}
	case errors.As(err, &te):
		kind := ErrCodec
		if errors.Is(te, portable.ErrCorrupt) {
			kind = ErrCorrupt
		}
		return fmt.Errorf("%w: %s: portable engine trapped: %v", kind, op, te.Value)
	case errors.Is(err, portable.ErrClosed):
		return kolovideo.ErrClosed
	}
	return fmt.Errorf("%w: %s: %v", ErrCodec, op, err)
}

type portableEncoder struct{ e *portable.Encoder }

func (*portableEngine) newEncoder(p encParams) (encImpl, error) {
	e, err := portable.NewEncoder(portable.EncParams{
		VP9: p.vp9, Width: p.width, Height: p.height, Threads: p.threads, Speed: p.speed,
		Kbps: p.kbps, MaxIntraPct: p.maxIntraPct,
	})
	if err != nil {
		return nil, fromPortable("create encoder", err)
	}
	return &portableEncoder{e}, nil
}

func (e *portableEncoder) encode(y, u, v []byte, yStride, uvStride int, pts int64, dur uint32, key bool, emit func([]byte, bool)) error {
	if err := e.e.Encode(y, u, v, yStride, uvStride, pts, dur, key, emit); err != nil {
		return fromPortable("encode", err)
	}
	return nil
}

func (e *portableEncoder) setBitrate(kbps uint32) error {
	if err := e.e.SetBitrate(kbps); err != nil {
		return fromPortable("set bitrate", err)
	}
	return nil
}

func (e *portableEncoder) close() { e.e.Close() }

type portableDecoder struct{ d *portable.Decoder }

func (*portableEngine) newDecoder(vp9 bool, threads int) (decImpl, error) {
	d, err := portable.NewDecoder(vp9, threads)
	if err != nil {
		return nil, fromPortable("create decoder", err)
	}
	return &portableDecoder{d}, nil
}

func (d *portableDecoder) decode(data []byte) (plane420, bool, error) {
	img, ok, err := d.d.Decode(data)
	if err != nil {
		return plane420{}, false, fromPortable("decode", err)
	}
	return plane420{img.Width, img.Height, img.Y, img.U, img.V, img.YStride, img.UVStride}, ok, nil
}

func (d *portableDecoder) corrupted() bool { return d.d.Corrupted() }
func (d *portableDecoder) close()          { d.d.Close() }
