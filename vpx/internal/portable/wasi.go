package portable

import (
	"encoding/binary"
	"fmt"
	"time"
)

// WASI errno values the module can receive.
const (
	errnoBadf  = 8
	errnoFault = 21
	errnoInval = 28
	errnoNosys = 52
)

// WASI clock ids.
const (
	clockRealtime  = 0
	clockMonotonic = 1
)

var epoch = time.Now()

// exit is the panic value of proc_exit.
type exit int32

func (e exit) Error() string {
	return fmt.Sprintf("kolovideo/vpx: wasm module exited with status %d", int32(e))
}

type memory = interface {
	Slice() *[]byte
	Grow(delta, max int64) int64
}

// host answers the WASI imports libvpx links. The module reads the clock for
// its timers and nothing else: there are no files, no environment and no
// process control.
type host struct{ mem func() []byte }

// Init gives the host its module's memory.
func (h *host) Init(m any) {
	mod := m.(interface{ Xmemory() memory })
	h.mem = func() []byte { return *mod.Xmemory().Slice() }
}

func (h *host) Xclock_time_get(id int32, _ int64, p int32) int32 {
	var ns int64
	switch id {
	case clockRealtime:
		ns = time.Now().UnixNano()
	case clockMonotonic:
		ns = int64(time.Since(epoch))
	case 2, 3:
		return errnoNosys
	default:
		return errnoInval
	}
	mem := h.mem()
	if uint32(p) > uint32(len(mem)) || uint32(len(mem))-uint32(p) < 8 {
		return errnoFault
	}
	binary.LittleEndian.PutUint64(mem[uint32(p):], uint64(ns))
	return 0
}

func (*host) Xfd_close(int32) int32                          { return errnoBadf }
func (*host) Xfd_fdstat_get(int32, int32) int32              { return errnoBadf }
func (*host) Xfd_fdstat_set_flags(int32, int32) int32        { return errnoBadf }
func (*host) Xfd_prestat_dir_name(int32, int32, int32) int32 { return errnoBadf }
func (*host) Xfd_prestat_get(int32, int32) int32             { return errnoBadf }
func (*host) Xfd_read(int32, int32, int32, int32) int32      { return errnoBadf }
func (*host) Xfd_seek(int32, int64, int32, int32) int32      { return errnoBadf }
func (*host) Xfd_write(int32, int32, int32, int32) int32     { return errnoBadf }

func (*host) Xpath_open(int32, int32, int32, int32, int32, int64, int64, int32, int32) int32 {
	return errnoNosys
}

func (*host) Xproc_exit(code int32) { panic(exit(code)) }
