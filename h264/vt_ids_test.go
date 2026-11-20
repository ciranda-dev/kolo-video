package h264

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

func TestVTStructSizes(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the system calls these structs through are 64-bit only")
	}
	var tm cmTime
	var ti cmTiming
	var rec vtDecodeRecord
	for _, c := range []struct {
		name      string
		got, want uintptr
		cite      string
	}{
		{"CMTime", unsafe.Sizeof(tm), 24, "CMTime.h"},
		{"CMTime.timescale offset", unsafe.Offsetof(tm.Timescale), 8, "CMTime.h"},
		{"CMTime.flags offset", unsafe.Offsetof(tm.Flags), 12, "CMTime.h"},
		{"CMTime.epoch offset", unsafe.Offsetof(tm.Epoch), 16, "CMTime.h"},
		{"CMSampleTimingInfo", unsafe.Sizeof(ti), 72, "CMSampleBuffer.h"},
		{"CMSampleTimingInfo.presentationTimeStamp offset", unsafe.Offsetof(ti.PTS), 24, "CMSampleBuffer.h"},
		{"CMSampleTimingInfo.decodeTimeStamp offset", unsafe.Offsetof(ti.DTS), 48, "CMSampleBuffer.h"},
		{"VTDecompressionOutputCallbackRecord", unsafe.Sizeof(rec), 16, "VTDecompressionSession.h"},
		{"VTDecompressionOutputCallbackRecord.decompressionOutputCallback offset", unsafe.Offsetof(rec.callback), 0, "VTDecompressionSession.h"},
		{"VTDecompressionOutputCallbackRecord.decompressionOutputRefCon offset", unsafe.Offsetof(rec.refcon), 8, "VTDecompressionSession.h"},
	} {
		if c.got != c.want {
			t.Errorf("%s (%s) is %d, want %d", c.name, c.cite, c.got, c.want)
		}
	}
	if got := vtTime(7); got != (cmTime{Value: 7, Timescale: 90000, Flags: 1}) {
		t.Errorf("vtTime %+v", got)
	}
}

func TestVTLevelAndPropTables(t *testing.T) {
	if vtPropCount != 7 || vtLevelCount != 4 {
		t.Errorf("table sizes %d %d", vtPropCount, vtLevelCount)
	}
}

// sdkHeaders returns the directories of the headers of the four frameworks,
// or skips when the macOS SDK is not installed.
func sdkHeaders(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("xcrun", "--show-sdk-path").Output()
	if err != nil {
		t.Skip("the macOS SDK is not installed")
	}
	sdk := strings.TrimSpace(string(out))
	var dirs []string
	for _, fw := range []string{"CoreFoundation", "CoreMedia", "CoreVideo", "VideoToolbox"} {
		d := filepath.Join(sdk, "System/Library/Frameworks", fw+".framework/Headers")
		if _, err := os.Stat(d); err != nil {
			t.Skip("the SDK has no " + fw + " headers")
		}
		dirs = append(dirs, d)
	}
	return dirs
}

func readHeader(t *testing.T, dirs []string, name string) string {
	t.Helper()
	for _, d := range dirs {
		if b, err := os.ReadFile(filepath.Join(d, name)); err == nil {
			return string(b)
		}
	}
	t.Fatalf("header %s not found in the SDK", name)
	return ""
}

var shiftRe = regexp.MustCompile(`^\(?\s*(\d+)[UL]*\s*<<\s*(\d+)\s*\)?$`)

// headerValue evaluates the right side of an enumerator: a decimal or hex
// number, a four character code, or 1UL << n.
func headerValue(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if len(s) == 6 && s[0] == '\'' && s[5] == '\'' {
		return int64(s[1])<<24 | int64(s[2])<<16 | int64(s[3])<<8 | int64(s[4]), true
	}
	if m := shiftRe.FindStringSubmatch(s); m != nil {
		a, _ := strconv.ParseInt(m[1], 10, 64)
		n, _ := strconv.ParseInt(m[2], 10, 64)
		return a << n, true
	}
	v, err := strconv.ParseInt(s, 0, 64)
	return v, err == nil
}

func TestVTConstantsMatchHeaders(t *testing.T) {
	dirs := sdkHeaders(t)
	for _, c := range vtConstants {
		src := readHeader(t, dirs, c.header)
		re := regexp.MustCompile(`\b` + c.name + `\s*=\s*([^,/;\n]+)`)
		m := re.FindStringSubmatch(src)
		if m == nil {
			t.Errorf("%s: no definition in %s", c.name, c.header)
			continue
		}
		got, ok := headerValue(m[1])
		if !ok || got != c.value {
			t.Errorf("%s in %s is %q, the table says %d", c.name, c.header, strings.TrimSpace(m[1]), c.value)
		}
	}
}

func TestVTSymbolsAreDeclared(t *testing.T) {
	dirs := sdkHeaders(t)
	for _, list := range [][]vtSymbol{vtSymbols, vtFunctions} {
		for _, s := range list {
			src := readHeader(t, dirs, s.header)
			if !regexp.MustCompile(`\b` + s.name + `\b`).MatchString(src) {
				t.Errorf("%s is not declared in %s", s.name, s.header)
			}
		}
	}
}

func TestVTConstantTableValuesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range vtConstants {
		if seen[c.name] {
			t.Errorf("%s listed twice", c.name)
		}
		seen[c.name] = true
	}
	for _, list := range [][]vtSymbol{vtSymbols, vtFunctions} {
		for _, s := range list {
			if seen[s.name] {
				t.Errorf("%s listed twice", s.name)
			}
			seen[s.name] = true
		}
	}
}
