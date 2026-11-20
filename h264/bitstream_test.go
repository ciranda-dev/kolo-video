package h264

import (
	"bytes"
	"errors"
	"testing"
)

func TestSplitAnnexB(t *testing.T) {
	for _, c := range []struct {
		name string
		in   []byte
		want [][]byte
		err  error
	}{
		{"4-byte", []byte{0, 0, 0, 1, 0x67, 1, 0, 0, 0, 1, 0x68, 2}, [][]byte{{0x67, 1}, {0x68, 2}}, nil},
		{"3-byte", []byte{0, 0, 1, 0x67, 1, 0, 0, 1, 0x68, 2}, [][]byte{{0x67, 1}, {0x68, 2}}, nil},
		{"trailing zeros", []byte{0, 0, 1, 0x65, 9, 0, 0, 0, 0, 0, 1, 0x41, 3, 0, 0}, [][]byte{{0x65, 9}, {0x41, 3}}, nil},
		{"emulation kept", []byte{0, 0, 1, 0x65, 0, 0, 3, 1}, [][]byte{{0x65, 0, 0, 3, 1}}, nil},
		{"empty", nil, nil, ErrCorrupt},
		{"no start code", []byte{1, 0, 0, 1, 0x65}, nil, ErrCorrupt},
		{"two zeros only", []byte{0, 0, 2, 0x65}, nil, ErrCorrupt},
		{"start code only", []byte{0, 0, 0, 1}, nil, ErrCorrupt},
		{"empty unit", []byte{0, 0, 1, 0, 0, 1, 0x65}, nil, ErrCorrupt},
		{"empty last unit", []byte{0, 0, 1, 0x65, 0, 0, 1}, nil, ErrCorrupt},
	} {
		got, err := splitAnnexB(c.in, 10)
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("%s: %v, want %v", c.name, err, c.err)
			}
			continue
		}
		if err != nil || len(got) != len(c.want) {
			t.Fatalf("%s: %v %x", c.name, err, got)
		}
		for i := range got {
			if !bytes.Equal(got[i], c.want[i]) {
				t.Errorf("%s: unit %d is %x, want %x", c.name, i, got[i], c.want[i])
			}
		}
	}
	many := bytes.Repeat([]byte{0, 0, 1, 9, 0x10}, 11)
	if _, err := splitAnnexB(many, 10); !errors.Is(err, ErrTooLarge) {
		t.Errorf("11 units with a cap of 10: %v", err)
	}
	if got, err := splitAnnexB(many[:50], 10); err != nil || len(got) != 10 {
		t.Errorf("10 units with a cap of 10: %v %d", err, len(got))
	}
	if maxNALs(DefaultMaxPixels) != 36864+288 {
		t.Errorf("maxNALs %d", maxNALs(DefaultMaxPixels))
	}
}

func TestSplitAVCC(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      []byte
		lenSize int
		n       int
		ok      bool
	}{
		{"4", []byte{0, 0, 0, 2, 0x65, 1, 0, 0, 0, 1, 6}, 4, 2, true},
		{"2", []byte{0, 2, 0x65, 1, 0, 1, 6}, 2, 2, true},
		{"1", []byte{2, 0x65, 1, 1, 6}, 1, 2, true},
		{"empty", nil, 4, 0, true},
		{"length 0", []byte{0, 0, 0, 0}, 4, 0, false},
		{"past the end", []byte{0, 0, 0, 3, 0x65, 1}, 4, 0, false},
		{"truncated length", []byte{0, 0, 0, 1, 6, 0, 0}, 4, 0, false},
		{"length size 3", []byte{0, 0, 1, 6}, 3, 0, false},
		{"length size 0", []byte{1, 6}, 0, 0, false},
		{"2^32-1", []byte{0xff, 0xff, 0xff, 0xff, 1}, 4, 0, false},
		{"too many", bytes.Repeat([]byte{1, 6}, 11), 1, 0, false},
	} {
		got, err := splitAVCC(c.in, c.lenSize, 10)
		if c.ok != (err == nil) || c.ok && len(got) != c.n {
			t.Errorf("%s: %v %d", c.name, err, len(got))
		}
		if !c.ok && !errors.Is(err, ErrCodec) {
			t.Errorf("%s: %v is not ErrCodec", c.name, err)
		}
	}
	units := [][]byte{{0x65, 1, 2}, {6}}
	got, err := splitAVCC(appendAVCC(nil, units), 4, 10)
	if err != nil || len(got) != 2 || !bytes.Equal(got[0], units[0]) || !bytes.Equal(got[1], units[1]) {
		t.Errorf("appendAVCC round trip: %v %x", err, got)
	}
	if b := appendAnnexB(nil, units[:1], units[1:]); !bytes.Equal(b, []byte{0, 0, 0, 1, 0x65, 1, 2, 0, 0, 0, 1, 6}) {
		t.Errorf("appendAnnexB gives %x", b)
	}
}

func TestEscapeRBSP(t *testing.T) {
	for _, c := range []struct{ rbsp, nal []byte }{
		{[]byte{0, 0, 0}, []byte{0, 0, 3, 0}},
		{[]byte{0, 0, 1}, []byte{0, 0, 3, 1}},
		{[]byte{0, 0, 2}, []byte{0, 0, 3, 2}},
		{[]byte{0, 0, 3}, []byte{0, 0, 3, 3}},
		{[]byte{0, 0, 4}, []byte{0, 0, 4}},
		{[]byte{0, 0}, []byte{0, 0, 3}},
		{[]byte{0, 0, 0, 0, 0}, []byte{0, 0, 3, 0, 0, 3, 0}},
		{[]byte{1, 0, 0, 0, 1}, []byte{1, 0, 0, 3, 0, 1}},
	} {
		if got := escapeRBSP(nil, c.rbsp); !bytes.Equal(got, c.nal) {
			t.Errorf("escape %x: %x, want %x", c.rbsp, got, c.nal)
		}
		if got := unescapeRBSP(nil, c.nal); !bytes.Equal(got, c.rbsp) {
			t.Errorf("unescape %x: %x, want %x", c.nal, got, c.rbsp)
		}
	}
}

func TestBits(t *testing.T) {
	w := &bitWriter{}
	ues := []uint32{0, 1, 2, 3, 7, 8, 255, 65535, 1<<31 - 1, 1<<32 - 2}
	ses := []int32{0, 1, -1, 2, -2, 127, -128, 1<<31 - 1, -(1<<31 - 1)}
	for _, v := range ues {
		w.ue(v)
	}
	for _, v := range ses {
		w.se(v)
	}
	w.u(0xabcdef12, 32)
	w.u(5, 3)
	w.trailing()
	r := &bitReader{b: w.b}
	for _, v := range ues {
		if got := r.ue(); got != v {
			t.Errorf("ue %d read as %d", v, got)
		}
	}
	for _, v := range ses {
		if got := r.se(); got != v {
			t.Errorf("se %d read as %d", v, got)
		}
	}
	if r.u(32) != 0xabcdef12 || r.u(3) != 5 {
		t.Error("u")
	}
	if r.moreData() {
		t.Error("more data before the stop bit")
	}
	r.trailing()
	if r.err != nil {
		t.Error(r.err)
	}
	if r.u(16); !errors.Is(r.err, errTruncated) {
		t.Errorf("read past the end: %v", r.err)
	}
	if r.ue() != 0 || r.se() != 0 || r.flag() {
		t.Error("reads after an error are not 0")
	}
	// Exp-Golomb codes: 32 leading zeros are refused, 31 are read.
	r = &bitReader{b: []byte{0, 0, 0, 0, 0x80, 0, 0, 0, 0}}
	if r.ue(); r.err == nil {
		t.Error("32 leading zeros accepted")
	}
	r = &bitReader{b: []byte{0, 0, 0, 1, 0xff, 0xff, 0xff, 0xfe}}
	if v := r.ue(); r.err != nil || v != 1<<32-2 {
		t.Errorf("31 leading zeros: %d %v", v, r.err)
	}
	// The largest code is 2^32-2, so se never wraps: it ends at -(2^31-1).
	r = &bitReader{b: []byte{0, 0, 0, 1, 0xff, 0xff, 0xff, 0xfe}}
	if v := r.se(); r.err != nil || v != -(1<<31-1) {
		t.Errorf("se of the largest code: %d %v", v, r.err)
	}
	if r := (&bitReader{b: []byte{0}}); r.lastOne() != -1 || r.moreData() {
		t.Error("no stop bit")
	}
	r = &bitReader{b: []byte{0x40}}
	if r.trailing(); r.err == nil {
		t.Error("stop bit not at the read position")
	}
	var c bitWriter
	c.copyBits([]byte{0xb5, 0xff}, 11)
	if !bytes.Equal(c.b, []byte{0xb5, 0xe0}) || c.n != 3 {
		t.Errorf("copyBits gives %x %d", c.b, c.n)
	}
	if nalType(nil) != -1 || nalType([]byte{0x65}) != nalIDR {
		t.Error("nalType")
	}
}
