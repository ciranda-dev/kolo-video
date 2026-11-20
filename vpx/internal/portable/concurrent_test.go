package portable

import (
	"errors"
	"sync"
	"testing"
)

// TestConcurrentInstances runs independent encoders and decoders in several
// goroutines, some of which trap and recover, and checks that every stream is
// the one a single instance produces.
func TestConcurrentInstances(t *testing.T) {
	for _, vp9 := range []bool{false, true} {
		p := params(vp9, 64, 48, 300)
		want := encodeAll(t, p, 8)
		var wg sync.WaitGroup
		for g := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				enc, err := NewEncoder(p)
				if err != nil {
					t.Error(err)
					return
				}
				defer enc.Close()
				dec, err := NewDecoder(vp9, 1)
				if err != nil {
					t.Error(err)
					return
				}
				defer dec.Close()
				var got []packet
				for i := range 8 {
					f := harness(64, 48, i)
					if g%2 == 1 && i == 4 {
						dec.Break()
						if _, _, err := dec.Decode(want[0].data); !errors.Is(err, ErrCorrupt) {
							t.Errorf("goroutine %d: Decode after Break: %v", g, err)
						}
					}
					err := enc.Encode(f.y, f.u, f.v, 64, 32, int64(i)*3000, 3000, i == 0, func(d []byte, k bool) {
						got = append(got, packet{append([]byte(nil), d...), k})
					})
					if err != nil {
						t.Error(err)
						return
					}
				}
				if streamHash(got) != streamHash(want) {
					t.Errorf("goroutine %d: stream differs from the single-instance stream", g)
				}
				for _, p := range got {
					if _, _, err := dec.Decode(p.data); err != nil {
						t.Errorf("goroutine %d: %v", g, err)
						return
					}
				}
			}()
		}
		wg.Wait()
	}
}

func TestConcurrentVersion(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v := Version(); v != "v1.16.0" {
				t.Errorf("Version() = %q", v)
			}
		}()
	}
	wg.Wait()
}
