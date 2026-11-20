package kolovideo_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// privateTokens are sha256 hashes of lowercase words that identify a person or a machine and must
// never appear in the repository (a user name, the names and IDs of private devices, a scratch path).
// Only hashes are stored, so this file does not contain what it forbids.
var privateTokens = map[string]bool{
	"bd3dae5fb91f88a4f0978222dfd58f59a124257cb081486387cbae9df11fb879": true,
	"28d8f4b9010a888e3a689948099ce4e02408281ab599c2f1f86f7351b58350bd": true,
	"13915d646449a73e0be382ead440e435b81649995a806c4be7631a0d690cfed9": true,
	"2b2897ec47c0a6f0807e4b3c1e153a27bf9cf031a5911d0e152dad094cbfb6ad": true,
	"1be415eb0387bcfeef3a43e5ef6696308b75bda8c4a26e2201df141037a054de": true,
	"1b662dee0f61ae97fd01ea3db97588022558b571a59871dd7dadf4766874e227": true,
	"93a20e754b965e438f9a4eda1d1afcaa24c6179468a2aaf993ed27d945aec180": true,
	"378a7bae469ac750a40c8ab180c42393133643fcdcc58c1063eff42fbb96772a": true,
	"60ebb561ea8aa136f65e6f820a038462ecd282eb4070ce5de3094d4dc0ab6786": true,
	"26b47209413e5776b8a32b9ef89bb47312a75875bf475d39a78e5ae4dcc095f3": true,
	"d39ed8b3cea68e3fb9c0e34d3bfdb435c0e09892981141f90a0784b4e694455e": true,
	"a9b14571f6a57f96738daec2ca20e063e8b8113a98c303d31732aef012627d2c": true,
	"4a650e60f07521950ad9dc05c84d7a03a4ce475a26120c694cfe801387c4c592": true,
	"adb0be44c443a12a1d2dffe2281151e00e1673e5087153ac1afcfa952de57bac": true,
}

func isTokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func hashHit(b []byte, hashes map[string]bool) bool {
	sum := sha256.Sum256(b)
	return hashes[hex.EncodeToString(sum[:])]
}

// privateOffsets returns the offsets in data (already lower case) where a word, or a run of up to
// eight parts of a word joined by '-' or '_', hashes to one of hashes. Words are runs of letters,
// digits, '-' and '_'.
func privateOffsets(data []byte, hashes map[string]bool) []int {
	var hits []int
	for i := 0; i < len(data); {
		if !isTokenByte(data[i]) {
			i++
			continue
		}
		j := i
		for j < len(data) && isTokenByte(data[j]) {
			j++
		}
		var starts, ends []int
		for k := i; k < j; {
			if data[k] == '-' || data[k] == '_' {
				k++
				continue
			}
			e := k
			for e < j && data[e] != '-' && data[e] != '_' {
				e++
			}
			starts, ends = append(starts, k), append(ends, e)
			k = e
		}
		found := hashHit(data[i:j], hashes)
		for a := 0; a < len(starts) && !found; a++ {
			for b := a; b < len(starts) && b-a < 8 && !found; b++ {
				found = hashHit(data[starts[a]:ends[b]], hashes)
			}
		}
		if found {
			hits = append(hits, i)
		}
		i = j
	}
	return hits
}

// TestPrivateOffsets checks the detector on a made-up word, in the forms a device ID or a path takes.
func TestPrivateOffsets(t *testing.T) {
	sum := sha256.Sum256([]byte("zebra"))
	hashes := map[string]bool{hex.EncodeToString(sum[:]): true}
	sum = sha256.Sum256([]byte("quux-42"))
	hashes[hex.EncodeToString(sum[:])] = true
	for form, want := range map[string]bool{
		"zebra": true, "Zebra's": true, "zebra-uid": true, "ZEBRA_UID": true, "a-zebra-b": true, "x_zebra": true,
		"/tmp/quux-42/x": true, "quux-42-extra": true, "a_quux-42": true, "zebr": false, "zebras": false,
		"quux-43": false, "quux": false, "42": false,
	} {
		if got := len(privateOffsets(bytes.ToLower([]byte(form)), hashes)) > 0; got != want {
			t.Errorf("%q: found %v, want %v", form, got, want)
		}
	}
}

func TestNoPersonalData(t *testing.T) {
	// A test binary that runs elsewhere (a container, a VM) has no repository to read.
	if _, err := os.Stat("go.mod"); err != nil {
		t.Skip("not run from the repository root")
	}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || path == filepath.Join(".claude", "local") {
				return filepath.SkipDir
			}
			return nil
		}
		// Third-party licence texts name their authors; local files are not committed.
		if strings.HasPrefix(name, "LICENSE") || strings.HasSuffix(name, ".local.md") {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return err
		}
		for _, off := range privateOffsets(bytes.ToLower(data), privateTokens) {
			t.Errorf("%s: personal data at byte %d", path, off)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
