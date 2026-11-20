package portable

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

func TestVersion(t *testing.T) {
	if v := Version(); v != "v1.16.0" {
		t.Fatalf("Version() = %q", v)
	}
}

// TestRoundTrip checks the bitrate and the minimum luma PSNR that every engine
// of kolo-video produces for the harness pattern at 2 Mbit/s and one thread.
func TestRoundTrip(t *testing.T) {
	cases := []struct {
		vp9  bool
		w, h int
		kbps int
		psnr string
	}{
		{false, 320, 240, 882, "50.12"},
		{false, 161, 99, 180, "50.98"},
		{true, 320, 240, 288, "51.50"},
		{true, 161, 99, 89, "48.26"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("vp9=%v/%dx%d", c.vp9, c.w, c.h), func(t *testing.T) {
			enc, err := NewEncoder(params(c.vp9, c.w, c.h, 2000))
			if err != nil {
				t.Fatal(err)
			}
			defer enc.Close()
			dec, err := NewDecoder(c.vp9, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer dec.Close()
			minPSNR, decoded, bytes := 1e9, 0, 0
			for i := range 60 {
				f := harness(c.w, c.h, i)
				var pkts [][]byte
				err := enc.Encode(f.y, f.u, f.v, f.w, (f.w+1)/2, int64(i)*3000, 3000, i == 0, func(d []byte, k bool) {
					if k != (i == 0) {
						t.Errorf("frame %d: key=%v", i, k)
					}
					pkts = append(pkts, append([]byte(nil), d...))
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range pkts {
					bytes += len(p)
					img, ok, err := dec.Decode(p)
					if err != nil {
						t.Fatalf("frame %d: %v", i, err)
					}
					if dec.Corrupted() {
						t.Fatalf("frame %d flagged corrupt", i)
					}
					if !ok {
						continue
					}
					if img.Width != c.w || img.Height != c.h {
						t.Fatalf("decoded %dx%d", img.Width, img.Height)
					}
					minPSNR = min(minPSNR, lumaPSNR(f, img))
					decoded++
				}
			}
			if decoded < 55 {
				t.Fatalf("decoded %d of 60 frames", decoded)
			}
			if got := fmt.Sprintf("%.2f", minPSNR); got != c.psnr {
				t.Errorf("min PSNR %s, want %s", got, c.psnr)
			}
			if got := int(float64(bytes)*8*30/60/1000 + 0.5); got != c.kbps {
				t.Errorf("bitrate %d kbit/s, want %d", got, c.kbps)
			}
			t.Logf("min PSNR %.2f dB, %d kbit/s", minPSNR, int(float64(bytes)*8*30/60/1000+0.5))
		})
	}
}

func streamHash(pkts []packet) string {
	h := sha256.New()
	for _, p := range pkts {
		fmt.Fprintf(h, "%d:", len(p.data))
		h.Write(p.data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// goldens are the SHA-256 of the packets of 60 harness pictures at 2 Mbit/s,
// one thread. The native engine produces the same bytes (see native_test.go).
var goldens = map[string]string{
	"vp9=false/320x240": "35059e7ea1686b3c06713b20a26c24c68cd51c9e197eab926053bf110537882b",
	"vp9=false/161x99":  "78aa076f4e2db9caf80c78ed608ead8434ee14a091c35549d60d011f0bb5b64b",
	"vp9=true/320x240":  "bb21301d34a71cdd9e481a5d072547b4b732aa794d71749e4467eb688e5d8032",
	"vp9=true/161x99":   "5844a3bcdc08893568b2fb7d75db8c364f9e2511edada59cb97cb15f1d76c5db",
	"vp9=false/640x480": "ade162ca30d9946c84559f8c7af1ebb1d40f6967b8509b3feccda5fbf80a3dc6",
	"vp9=true/640x480":  "a7da3b87d9eef0611d2f4210e4d72e0ab2f0472e6ade3751bf28572bde758e06",
}

func TestGolden(t *testing.T) {
	for _, c := range []struct {
		vp9  bool
		w, h int
	}{{false, 320, 240}, {false, 161, 99}, {true, 320, 240}, {true, 161, 99}, {false, 640, 480}, {true, 640, 480}} {
		name := fmt.Sprintf("vp9=%v/%dx%d", c.vp9, c.w, c.h)
		got := streamHash(encodeAll(t, params(c.vp9, c.w, c.h, 2000), 60))
		if got != goldens[name] {
			t.Errorf("%s: hash %s, want %s", name, got, goldens[name])
		}
	}
}
