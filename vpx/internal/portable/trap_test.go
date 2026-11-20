package portable

import (
	"errors"
	"math/rand/v2"
	"strconv"
	"testing"
)

func TestDecoderTrap(t *testing.T) {
	for _, vp9 := range []bool{false, true} {
		t.Run("vp9="+strconv.FormatBool(vp9), func(t *testing.T) {
			pkts := encodeAll(t, params(vp9, 64, 48, 300), 6)
			dec, err := NewDecoder(vp9, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Close()
			for _, p := range pkts[:3] {
				if _, _, err := dec.Decode(p.data); err != nil {
					t.Fatal(err)
				}
			}
			dec.Break()
			_, ok, err := dec.Decode(pkts[3].data)
			var trap *TrapError
			if !errors.Is(err, ErrCorrupt) || !errors.As(err, &trap) || errors.Is(err, ErrCodec) || ok {
				t.Fatalf("trapping call: ok=%v err=%v, want ErrCorrupt", ok, err)
			}
			if dec.Alive() {
				t.Fatal("trapped module not dropped")
			}
			if dec.Corrupted() {
				t.Fatal("Corrupted after a trap")
			}
			// The next call builds a new module with the same configuration.
			img, ok, err := dec.Decode(pkts[0].data)
			if err != nil || !ok || img.Width != 64 || img.Height != 48 {
				t.Fatalf("after replacement: ok=%v err=%v", ok, err)
			}
			if !dec.Alive() {
				t.Fatal("no module after replacement")
			}
		})
	}
}

func TestEncoderTrapIsSticky(t *testing.T) {
	for _, vp9 := range []bool{false, true} {
		t.Run("vp9="+strconv.FormatBool(vp9), func(t *testing.T) {
			p := params(vp9, 64, 48, 300)
			enc, err := NewEncoder(p)
			if err != nil {
				t.Fatal(err)
			}
			defer enc.Close()
			other, err := NewEncoder(p)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			f := harness(64, 48, 0)
			emit := func([]byte, bool) {}
			restore := enc.Break()
			for i := range 3 {
				if i == 1 {
					// The module is whole again: the failure must still stick.
					restore()
				}
				err = enc.Encode(f.y, f.u, f.v, 64, 32, 0, 3000, true, emit)
				var trap *TrapError
				if !errors.Is(err, ErrCodec) || errors.Is(err, ErrCorrupt) || !errors.As(err, &trap) {
					t.Fatalf("Encode after a trap: %v, want a sticky ErrCodec", err)
				}
			}
			if err := enc.SetBitrate(500); !errors.Is(err, ErrCodec) {
				t.Fatalf("SetBitrate after a trap: %v", err)
			}
			if err := other.Encode(f.y, f.u, f.v, 64, 32, 0, 3000, true, emit); err != nil {
				t.Fatalf("other encoder: %v", err)
			}
		})
	}
}

// TestRealTrap feeds mutated packets until libvpx raises an error, which the
// build turns into a trap, and checks the decoder recovers every time.
func TestRealTrap(t *testing.T) {
	pkts := encodeAll(t, params(true, 64, 48, 300), 10)
	dec, err := NewDecoder(true, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	r := rand.New(rand.NewPCG(3, 4))
	traps := 0
	for range 300 {
		bad := append([]byte(nil), pkts[1+r.IntN(len(pkts)-1)].data...)
		for range 1 + r.IntN(4) {
			bad[r.IntN(len(bad))] ^= byte(1 + r.IntN(255))
		}
		if _, _, err := dec.Decode(bad); err != nil {
			var trap *TrapError
			if errors.As(err, &trap) {
				traps++
				if !errors.Is(err, ErrCorrupt) {
					t.Fatalf("trap is not ErrCorrupt: %v", err)
				}
			}
		}
		if _, ok, err := dec.Decode(pkts[0].data); err != nil || !ok {
			t.Fatalf("decoder unusable after a bad packet: ok=%v err=%v", ok, err)
		}
	}
	if traps == 0 {
		t.Fatal("no mutated packet trapped")
	}
	t.Logf("%d traps recovered", traps)
}
