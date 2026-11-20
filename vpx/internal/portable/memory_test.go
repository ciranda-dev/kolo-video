package portable

import (
	"fmt"
	"testing"
)

// TestMemoryAtMaxPixels records the module memory of an encoder and a decoder
// at 1920x1080 and at 4096x2176 (8912896 pixels, the default MaxPixels of the
// vpx package) and requires both to stay under the 1 GiB limit.
func TestMemoryAtMaxPixels(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	for _, c := range []struct {
		w, h int
	}{{1920, 1080}, {4096, 2176}} {
		for _, vp9 := range []bool{false, true} {
			t.Run(fmt.Sprintf("vp9=%v/%dx%d", vp9, c.w, c.h), func(t *testing.T) {
				enc, err := NewEncoder(params(vp9, c.w, c.h, 8000))
				if err != nil {
					t.Fatal(err)
				}
				defer enc.Close()
				dec, err := NewDecoder(vp9, 1)
				if err != nil {
					t.Fatal(err)
				}
				defer dec.Close()
				for i := range 4 {
					f := camera(c.w, c.h, i)
					err := enc.Encode(f.y, f.u, f.v, c.w, (c.w+1)/2, int64(i)*3000, 3000, i == 0, func(d []byte, _ bool) {
						if _, _, err := dec.Decode(d); err != nil {
							t.Fatal(err)
						}
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				t.Logf("encoder %d MiB, decoder %d MiB", enc.MemorySize()>>20, dec.MemorySize()>>20)
				if enc.MemorySize() > 1<<30 || dec.MemorySize() > 1<<30 {
					t.Fatal("above 1 GiB")
				}
			})
		}
	}
}
