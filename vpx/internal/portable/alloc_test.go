package portable

import (
	"strconv"
	"testing"
)

// TestZeroAllocations checks that a picture in steady state costs no Go
// allocation, on either side. (The first pictures grow the module memory.)
func TestZeroAllocations(t *testing.T) {
	for _, vp9 := range []bool{false, true} {
		t.Run("vp9="+strconv.FormatBool(vp9), func(t *testing.T) {
			p := params(vp9, 160, 120, 500)
			enc, err := NewEncoder(p)
			if err != nil {
				t.Fatal(err)
			}
			defer enc.Close()
			pics := make([]*picture, 10)
			for i := range pics {
				pics[i] = camera(160, 120, i)
			}
			var pkts [][]byte
			n := 0
			emit := func(d []byte, _ bool) {
				pkts = append(pkts, append([]byte(nil), d...))
			}
			encode := func() {
				f := pics[n%len(pics)]
				if err := enc.Encode(f.y, f.u, f.v, 160, 80, int64(n)*3000, 3000, n == 0, emit); err != nil {
					t.Fatal(err)
				}
				n++
			}
			for range 20 {
				encode()
			}
			pkts = nil
			emit = func([]byte, bool) {}
			if a := testing.AllocsPerRun(100, encode); a != 0 {
				t.Errorf("encode: %v allocations per picture, want 0", a)
			}

			dec, err := NewDecoder(vp9, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Close()
			key := encodeAll(t, p, 10)
			i := 0
			decode := func() {
				if _, _, err := dec.Decode(key[i%len(key)].data); err != nil {
					t.Fatal(err)
				}
				i++
			}
			for range 2 * len(key) {
				decode()
			}
			if a := testing.AllocsPerRun(100, decode); a != 0 {
				t.Errorf("decode: %v allocations per packet, want 0", a)
			}
		})
	}
}
