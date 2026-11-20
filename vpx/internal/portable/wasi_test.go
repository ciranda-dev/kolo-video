package portable

import (
	"encoding/binary"
	"errors"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

func TestHostErrno(t *testing.T) {
	var h host
	for name, got := range map[string]int32{
		"fd_close":            h.Xfd_close(1),
		"fd_fdstat_get":       h.Xfd_fdstat_get(1, 0),
		"fd_fdstat_set_flags": h.Xfd_fdstat_set_flags(1, 0),
		"fd_prestat_dir_name": h.Xfd_prestat_dir_name(3, 0, 0),
		"fd_prestat_get":      h.Xfd_prestat_get(3, 0),
		"fd_read":             h.Xfd_read(0, 0, 0, 0),
		"fd_seek":             h.Xfd_seek(1, 0, 0, 0),
		"fd_write":            h.Xfd_write(1, 0, 0, 0),
	} {
		if got != errnoBadf {
			t.Errorf("%s = %d, want EBADF", name, got)
		}
	}
	if got := h.Xpath_open(3, 0, 0, 0, 0, 0, 0, 0, 0); got != errnoNosys {
		t.Errorf("path_open = %d, want ENOSYS", got)
	}
}

func TestHostProcExit(t *testing.T) {
	defer func() {
		err, ok := recover().(error)
		var e exit
		if !ok || !errors.As(err, &e) || e != 3 {
			t.Fatalf("proc_exit panicked with %v, want exit(3)", err)
		}
		if want := "kolovideo/vpx: wasm module exited with status 3"; err.Error() != want {
			t.Fatalf("message %q, want %q", err, want)
		}
	}()
	(&host{}).Xproc_exit(3)
}

func TestHostDoesNotImportOS(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "wasi.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		switch imp.Path.Value {
		case `"os"`, `"syscall"`, `"os/exec"`:
			t.Fatalf("wasi.go imports %s", imp.Path.Value)
		}
	}
}

func TestHostClock(t *testing.T) {
	mem := make([]byte, 32)
	h := &host{mem: func() []byte { return mem }}
	before := time.Now().UnixNano()
	if r := h.Xclock_time_get(clockRealtime, 0, 8); r != 0 {
		t.Fatalf("realtime: errno %d", r)
	}
	got := int64(binary.LittleEndian.Uint64(mem[8:]))
	if got < before || got > time.Now().UnixNano() {
		t.Fatalf("realtime %d outside [%d, now]", got, before)
	}
	var last int64
	for range 3 {
		if r := h.Xclock_time_get(clockMonotonic, 0, 0); r != 0 {
			t.Fatalf("monotonic: errno %d", r)
		}
		now := int64(binary.LittleEndian.Uint64(mem))
		if now < last {
			t.Fatalf("monotonic clock went back: %d after %d", now, last)
		}
		last = now
	}
	for id, want := range map[int32]int32{2: errnoNosys, 3: errnoNosys, 4: errnoInval, -1: errnoInval} {
		if r := h.Xclock_time_get(id, 0, 0); r != want {
			t.Errorf("clock %d: errno %d, want %d", id, r, want)
		}
	}
	for _, p := range []int32{25, 32, 33, -1, -8} {
		if r := h.Xclock_time_get(clockRealtime, 0, p); r != errnoFault {
			t.Errorf("pointer %d: errno %d, want EFAULT", p, r)
		}
	}
	if r := h.Xclock_time_get(clockRealtime, 0, 24); r != 0 {
		t.Errorf("pointer 24 of 32 bytes: errno %d", r)
	}
}

func TestHostInit(t *testing.T) {
	h := &host{}
	m := New(h)
	if h.mem == nil || len(h.mem()) != len(*m.Slice()) {
		t.Fatal("host is not bound to the module memory")
	}
}
