package vpx

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// BenchmarkEncodeThreads reports the per-picture encode latency of the native
// engine at each thread count, for choosing encoderThreads. Run it on an idle
// host: go test -run xxx -bench EncodeThreads -benchtime 300x ./vpx
func BenchmarkEncodeThreads(b *testing.B) {
	eng, err := testEngine(EngineNative)
	if err != nil {
		b.Skip("no native engine: ", err)
	}
	sizes := []struct{ w, h int }{{640, 480}, {1280, 720}, {1920, 1080}}
	for _, vp9 := range []bool{false, true} {
		for _, s := range sizes {
			for _, threads := range []int{1, 2, 4, 8} {
				name := fmt.Sprintf("%s/%dx%d/threads=%d", map[bool]string{false: "VP8", true: "VP9"}[vp9], s.w, s.h, threads)
				b.Run(name, func(b *testing.B) {
					p := harnessParams(vp9, s.w, s.h, threads)
					p.kbps = kbps(int(DefaultBitsPerPixel * float64(s.w*s.h) * DefaultFrameRate))
					enc, err := eng.newEncoder(p)
					if err != nil {
						b.Fatal(err)
					}
					defer enc.close()
					const warm = 15
					var lat []time.Duration
					for i := range max(b.N, warm+1) {
						f := testFrame(b, s.w, s.h, i)
						start := time.Now()
						err := enc.encode(f.Y, f.U, f.V, f.YStride, f.UVStride, int64(i)*3000, 3000, i == 0, func([]byte, bool) {})
						if err != nil {
							b.Fatal(err)
						}
						if i >= warm {
							lat = append(lat, time.Since(start))
						}
					}
					slices.Sort(lat)
					var sum time.Duration
					for _, d := range lat {
						sum += d
					}
					ms := func(d time.Duration) float64 { return float64(d) / 1e6 }
					b.ReportMetric(ms(sum/time.Duration(len(lat))), "mean-ms")
					b.ReportMetric(ms(lat[len(lat)/2]), "p50-ms")
					b.ReportMetric(ms(lat[len(lat)*99/100]), "p99-ms")
					b.ReportMetric(ms(lat[len(lat)-1]), "max-ms")
				})
			}
		}
	}
}
