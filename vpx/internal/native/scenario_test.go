package native

import (
	"os"
	"testing"
)

// TestScenario is run by the validation scripts in hostile environments:
// KV_T2_EXPECT is memfd, cache, bundle or fail, KV_T2_CACHE the cache root and
// KV_T2_LIB an optional Library override, KV_T2_FIXTURE a library file or "embedded" for
// the real embedded library instead of the stub.
func TestScenario(t *testing.T) {
	want := os.Getenv("KV_T2_EXPECT")
	if want == "" {
		t.Skip("scenario runs only from the validation scripts")
	}
	s := stubSource(t)
	switch f := os.Getenv("KV_T2_FIXTURE"); f {
	case "":
	case "embedded":
		e, err := embedded()
		if err != nil || e == nil {
			t.Fatalf("no embedded library: %v", err)
		}
		s = e
	default:
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s = newSource(b)
	}
	l, err := load(s, Config{CacheDir: os.Getenv("KV_T2_CACHE"), Library: os.Getenv("KV_T2_LIB")})
	if err != nil {
		t.Logf("RESULT fail: %v", err)
		if want != "fail" {
			t.Fatalf("expected %s, got error", want)
		}
		return
	}
	t.Cleanup(l.unload)
	t.Logf("RESULT %s path=%q version=%s abi=%d", l.Source, l.Path, l.Version, l.ABI)
	if string(l.Source) != want {
		t.Fatalf("expected %s, got %s", want, l.Source)
	}
}
