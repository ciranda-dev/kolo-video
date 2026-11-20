package portable

import (
	"errors"
	"math"
	"strconv"
	"testing"
)

// TestMemoryLimit checks that the module cannot grow past 1 GiB: the
// allocation fails inside the module and the failure is reported, not raised.
func TestMemoryLimit(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("needs a 64-bit address space")
	}
	m, err := newModule()
	if err != nil {
		t.Fatal(err)
	}
	if p := m.Xkv_malloc(1 << 30); p != 0 {
		t.Fatal("kv_malloc of 1 GiB succeeded")
	}
	if p := m.Xkv_malloc(1<<29 + 1<<28); p == 0 {
		t.Fatal("kv_malloc of 768 MiB failed")
	}
	if p := m.Xkv_malloc(1<<28 + 1<<27); p != 0 {
		t.Fatal("allocations past 1 GiB in total succeeded")
	}
	if n := len(*m.Slice()); n > 1<<30 {
		t.Fatalf("memory is %d bytes, above 1 GiB", n)
	}
}

func TestDecoderPacketBufferLimit(t *testing.T) {
	pkts := encodeAll(t, params(false, 64, 48, 300), 2)
	dec, err := NewDecoder(false, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	err = dec.Reserve(1 << 30)
	var ce *CodecError
	if !errors.Is(err, ErrCodec) || !errors.As(err, &ce) || ce.Code != codecMem {
		t.Fatalf("Reserve(1 GiB) = %v, want an out of memory ErrCodec", err)
	}
	if err := dec.Reserve(math.MaxInt32); !errors.Is(err, ErrCodec) {
		t.Fatalf("Reserve(MaxInt32) = %v", err)
	}
	if _, ok, err := dec.Decode(pkts[0].data); err != nil || !ok {
		t.Fatalf("decoder unusable after a refused buffer: ok=%v err=%v", ok, err)
	}
}

func TestOversizedEncoder(t *testing.T) {
	for _, vp9 := range []bool{false, true} {
		p := params(vp9, 40000, 30000, 1000)
		if _, err := NewEncoder(p); !errors.Is(err, ErrCodec) && !errors.Is(err, ErrInvalidParam) {
			t.Fatalf("vp9=%v: NewEncoder(40000x30000) = %v", vp9, err)
		}
	}
	if _, err := NewEncoder(EncParams{Width: 0, Height: 10}); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("empty size: %v", err)
	}
}

func TestEncodeChecksPlanes(t *testing.T) {
	enc, err := NewEncoder(params(false, 32, 32, 300))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	f := harness(32, 32, 0)
	emit := func([]byte, bool) {}
	for name, c := range map[string]struct {
		y, u, v   []byte
		ys, uvs   int
		wantValid bool
	}{
		"valid":         {f.y, f.u, f.v, 32, 16, true},
		"short y":       {f.y[:len(f.y)-1], f.u, f.v, 32, 16, false},
		"short u":       {f.y, f.u[:10], f.v, 32, 16, false},
		"narrow y":      {f.y, f.u, f.v, 31, 16, false},
		"narrow chroma": {f.y, f.u, f.v, 32, 15, false},
		"nil":           {nil, nil, nil, 32, 16, false},
		"huge stride":   {f.y, f.u, f.v, math.MaxInt, 16, false},
	} {
		err := enc.Encode(c.y, c.u, c.v, c.ys, c.uvs, 0, 3000, true, emit)
		if c.wantValid != (err == nil) || err != nil && !errors.Is(err, ErrInvalidParam) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := enc.Encode(f.y, f.u, f.v, 32, 16, 3000, 3000, false, emit); err != nil {
		t.Fatalf("encoder unusable after refused pictures: %v", err)
	}
}

func TestClosed(t *testing.T) {
	enc, err := NewEncoder(params(false, 32, 32, 300))
	if err != nil {
		t.Fatal(err)
	}
	enc.Close()
	enc.Close()
	f := harness(32, 32, 0)
	if err := enc.Encode(f.y, f.u, f.v, 32, 16, 0, 3000, true, func([]byte, bool) {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Encode after Close: %v", err)
	}
	if err := enc.SetBitrate(100); !errors.Is(err, ErrClosed) {
		t.Fatalf("SetBitrate after Close: %v", err)
	}
	dec, err := NewDecoder(false, 1)
	if err != nil {
		t.Fatal(err)
	}
	dec.Close()
	dec.Close()
	if _, _, err := dec.Decode([]byte{1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Decode after Close: %v", err)
	}
}

func TestSetBitrate(t *testing.T) {
	enc, err := NewEncoder(params(false, 160, 120, 1000))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	var total int
	run := func(from, to int) int {
		total = 0
		for i := from; i < to; i++ {
			f := camera(160, 120, i)
			if err := enc.Encode(f.y, f.u, f.v, 160, 80, int64(i)*3000, 3000, i == 0, func(d []byte, _ bool) { total += len(d) }); err != nil {
				t.Fatal(err)
			}
		}
		return total
	}
	high := run(0, 40)
	if err := enc.SetBitrate(60); err != nil {
		t.Fatal(err)
	}
	run(40, 80)
	if low := run(80, 120); low*3 > high {
		t.Fatalf("40 frames took %d bytes at 1000 kbit/s and %d at 60 kbit/s", high, low)
	}
}

func TestEmptyPacket(t *testing.T) {
	dec, err := NewDecoder(true, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if _, _, err := dec.Decode(nil); !errors.Is(err, ErrInvalidParam) {
		t.Fatalf("Decode(nil) = %v", err)
	}
}
