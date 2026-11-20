//go:build windows && (amd64 || arm64)

package h264

// The COM objects of mftOS, called through their vtables. Slots, GUIDs and
// struct layouts are the ones of mft_ids.go.

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	mftOle32  = windows.NewLazySystemDLL("ole32.dll")
	mftPlat   = windows.NewLazySystemDLL("mfplat.dll")
	procCoCre = mftOle32.NewProc("CoCreateInstance")

	procMFStartup          = mftPlat.NewProc("MFStartup")
	procMFShutdown         = mftPlat.NewProc("MFShutdown")
	procMFCreateMediaType  = mftPlat.NewProc("MFCreateMediaType")
	procMFCreateSample     = mftPlat.NewProc("MFCreateSample")
	procMFCreateMemoryBuff = mftPlat.NewProc("MFCreateMemoryBuffer")
)

// mftLoad finds every function the engine calls. A Windows edition without
// Media Foundation (the N editions without the Media Feature Pack) fails
// here, naming the missing DLL.
var mftLoad = sync.OnceValue(func() error {
	for _, p := range []*windows.LazyProc{
		procCoCre, procMFStartup, procMFShutdown, procMFCreateMediaType, procMFCreateSample, procMFCreateMemoryBuff,
	} {
		if err := p.Find(); err != nil {
			return err
		}
	}
	return nil
})

func hresultOf(r uintptr) error {
	if int32(r) < 0 {
		return mftHRESULT(uint32(r))
	}
	return nil
}

// procCall calls an exported function that returns an HRESULT.
//
//go:uintptrescapes
func procCall(p *windows.LazyProc, args ...uintptr) error {
	r, _, _ := p.Call(args...)
	return hresultOf(r)
}

// call invokes vtable entry slot with the object as first argument. A
// failure HRESULT is returned as an mftHRESULT; success codes are nil.
//
//go:uintptrescapes
func (o *mftCom) call(slot int, args ...uintptr) error {
	if o == nil {
		return hrPointer
	}
	a := make([]uintptr, 1, len(args)+1)
	a[0] = uintptr(unsafe.Pointer(o))
	a = append(a, args...)
	r, _, _ := syscall.SyscallN(o.vtbl[slot], a...)
	return hresultOf(r)
}

func (o *mftCom) release() {
	if o != nil {
		o.call(slotRelease)
	}
}

// winMFT is the operating system behind mftEngine.
type winMFT struct{}

func (winMFT) coInit() (func(), error) {
	if err := mftLoad(); err != nil {
		return nil, fmt.Errorf("load Media Foundation: %w", err)
	}
	switch err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err {
	case nil, syscall.Errno(hrFalse):
		return windows.CoUninitialize, nil
	case syscall.Errno(hrChangedMode):
		// The thread already has an apartment of another kind, which also
		// serves these calls and is not ours to leave.
		return func() {}, nil
	default:
		var code syscall.Errno
		if errors.As(err, &code) {
			return nil, mftHRESULT(uint32(code))
		}
		return nil, err
	}
}

func (winMFT) startup() (func(), error) {
	if err := procCall(procMFStartup, mftVersion, mftStartupNoSocket); err != nil {
		return nil, err
	}
	return func() { procMFShutdown.Call() }, nil
}

func (winMFT) create(clsid *mftGUID) (mftTransform, error) {
	var o *mftCom
	if err := procCall(procCoCre, uintptr(unsafe.Pointer(clsid)), 0, clsctxInproc, uintptr(unsafe.Pointer(&iidTransform)), uintptr(unsafe.Pointer(&o))); err != nil {
		return nil, err
	}
	if o == nil {
		return nil, hrPointer
	}
	return &winTransform{o: o}, nil
}

func (winMFT) mediaType() (mftAttrs, error) {
	var o *mftCom
	if err := procCall(procMFCreateMediaType, uintptr(unsafe.Pointer(&o))); err != nil {
		return nil, err
	}
	return wrapAttrs(o)
}

func (winMFT) sample(capacity int, fill func(dst []byte) int, pts, dur int64) (mftSample, error) {
	var buf, s *mftCom
	if err := procCall(procMFCreateMemoryBuff, uintptr(max(capacity, 1)), uintptr(unsafe.Pointer(&buf))); err != nil {
		return nil, err
	}
	defer buf.release()
	if fill != nil {
		var p *byte
		var maxLen, cur uint32
		if err := buf.call(slotBufferLock, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&cur))); err != nil {
			return nil, err
		}
		if p == nil || int64(maxLen) < int64(capacity) {
			buf.call(slotBufferUnlock)
			return nil, hrPointer
		}
		n := fill(unsafe.Slice(p, capacity))
		buf.call(slotBufferUnlock)
		if n < 0 || n > capacity {
			return nil, hrFail
		}
		if err := buf.call(slotBufferSetCurrentLn, uintptr(n)); err != nil {
			return nil, err
		}
	}
	if err := procCall(procMFCreateSample, uintptr(unsafe.Pointer(&s))); err != nil {
		return nil, err
	}
	w := &winSample{winAttrs{o: s}}
	for _, step := range []error{
		s.call(slotSampleAddBuffer, uintptr(unsafe.Pointer(buf))),
		s.call(slotSampleSetTime, uintptr(pts)),
		s.call(slotSampleSetDuration, uintptr(dur)),
	} {
		if step != nil {
			w.release()
			return nil, step
		}
	}
	return w, nil
}

// winAttrs is an IMFAttributes object. release frees it once.
type winAttrs struct{ o *mftCom }

func (a *winAttrs) setGUID(key, v *mftGUID) error {
	return a.o.call(slotAttrSetGUID, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(v)))
}

func (a *winAttrs) setU32(key *mftGUID, v uint32) error {
	return a.o.call(slotAttrSetUINT32, uintptr(unsafe.Pointer(key)), uintptr(v))
}

func (a *winAttrs) setU64(key *mftGUID, v uint64) error {
	return a.o.call(slotAttrSetUINT64, uintptr(unsafe.Pointer(key)), uintptr(v))
}

func (a *winAttrs) getGUID(key *mftGUID) (g mftGUID, err error) {
	err = a.o.call(slotAttrGetGUID, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&g)))
	return g, err
}

func (a *winAttrs) getU32(key *mftGUID) (v uint32, err error) {
	err = a.o.call(slotAttrGetUINT32, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v)))
	return v, err
}

func (a *winAttrs) getU64(key *mftGUID) (v uint64, err error) {
	err = a.o.call(slotAttrGetUINT64, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&v)))
	return v, err
}

func (a *winAttrs) getBlob(key *mftGUID, max int) ([]byte, error) {
	var n uint32
	if err := a.o.call(slotAttrGetBlobSize, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&n))); err != nil {
		return nil, err
	}
	if n == 0 || int64(n) > int64(max) {
		return nil, hrFail
	}
	b := make([]byte, n)
	var written uint32
	if err := a.o.call(slotAttrGetBlob, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&b[0])), uintptr(n), uintptr(unsafe.Pointer(&written))); err != nil {
		return nil, err
	}
	return b[:min(written, n)], nil
}

func (a *winAttrs) release() {
	o := a.o
	a.o = nil
	o.release()
}

// winSample is an IMFSample.
type winSample struct{ winAttrs }

func (s *winSample) data(dst []byte, max int) ([]byte, error) {
	var buf *mftCom
	if err := s.o.call(slotSampleContiguous, uintptr(unsafe.Pointer(&buf))); err != nil {
		return nil, err
	}
	defer buf.release()
	var p *byte
	var maxLen, cur uint32
	if err := buf.call(slotBufferLock, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&cur))); err != nil {
		return nil, err
	}
	defer buf.call(slotBufferUnlock)
	if p == nil || cur > maxLen || int64(cur) > int64(max) {
		return nil, hrFail
	}
	return append(dst[:0], unsafe.Slice(p, cur)...), nil
}

// winCodec is an ICodecAPI.
type winCodec struct{ o *mftCom }

func (c *winCodec) set(key *mftGUID, vt uint16, v uint64) error {
	x := mftVariant{VT: vt, Val: v}
	return c.o.call(slotCodecSetValue, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&x)))
}

func (c *winCodec) setU32(key *mftGUID, v uint32) error { return c.set(key, variantUI4, uint64(v)) }

func (c *winCodec) setBool(key *mftGUID, v bool) error {
	if v {
		return c.set(key, variantBool, variantTrue)
	}
	return c.set(key, variantBool, 0)
}

func (c *winCodec) release() {
	o := c.o
	c.o = nil
	o.release()
}

// winTransform is an IMFTransform.
type winTransform struct{ o *mftCom }

func (x *winTransform) codecAPI() (mftCodec, error) {
	var o *mftCom
	if err := x.o.call(slotQueryInterface, uintptr(unsafe.Pointer(&iidCodecAPI)), uintptr(unsafe.Pointer(&o))); err != nil {
		return nil, err
	}
	if o == nil {
		return nil, hrPointer
	}
	return &winCodec{o: o}, nil
}

func wrapAttrs(o *mftCom) (mftAttrs, error) {
	if o == nil {
		return nil, hrPointer
	}
	return &winAttrs{o: o}, nil
}

func (x *winTransform) attributes() (mftAttrs, error) {
	var o *mftCom
	if err := x.o.call(slotGetAttributes, uintptr(unsafe.Pointer(&o))); err != nil {
		return nil, err
	}
	return wrapAttrs(o)
}

func (x *winTransform) inputType(i int) (mftAttrs, error) {
	var o *mftCom
	if err := x.o.call(slotGetInputAvailable, 0, uintptr(i), uintptr(unsafe.Pointer(&o))); err != nil {
		return nil, err
	}
	return wrapAttrs(o)
}

func (x *winTransform) outputType(i int) (mftAttrs, error) {
	var o *mftCom
	if err := x.o.call(slotGetOutputAvailable, 0, uintptr(i), uintptr(unsafe.Pointer(&o))); err != nil {
		return nil, err
	}
	return wrapAttrs(o)
}

func (x *winTransform) currentOutput() (mftAttrs, error) {
	var o *mftCom
	if err := x.o.call(slotGetOutputCurrent, 0, uintptr(unsafe.Pointer(&o))); err != nil {
		return nil, err
	}
	return wrapAttrs(o)
}

func (x *winTransform) typeArg(t mftAttrs) (*mftCom, error) {
	a, ok := t.(*winAttrs)
	if !ok || a.o == nil {
		return nil, hrPointer
	}
	return a.o, nil
}

func (x *winTransform) setInput(t mftAttrs) error {
	o, err := x.typeArg(t)
	if err != nil {
		return err
	}
	return x.o.call(slotSetInputType, 0, uintptr(unsafe.Pointer(o)), 0)
}

func (x *winTransform) setOutput(t mftAttrs) error {
	o, err := x.typeArg(t)
	if err != nil {
		return err
	}
	return x.o.call(slotSetOutputType, 0, uintptr(unsafe.Pointer(o)), 0)
}

func (x *winTransform) outputInfo() (flags, size uint32, err error) {
	var info mftOutputStreamInfo
	if err := x.o.call(slotGetOutputStreamInfo, 0, uintptr(unsafe.Pointer(&info))); err != nil {
		return 0, 0, err
	}
	return info.Flags, info.Size, nil
}

func (x *winTransform) message(m uint32) error { return x.o.call(slotProcessMessage, uintptr(m), 0) }

func (x *winTransform) processInput(s mftSample) error {
	w, ok := s.(*winSample)
	if !ok || w.o == nil {
		return hrPointer
	}
	return x.o.call(slotProcessInput, 0, uintptr(unsafe.Pointer(w.o)), 0)
}

func (x *winTransform) processOutput(s mftSample) (mftSample, error) {
	var ob mftOutputDataBuffer
	var supplied *winSample
	if s != nil {
		var ok bool
		if supplied, ok = s.(*winSample); !ok || supplied.o == nil {
			return nil, hrPointer
		}
		ob.Sample = supplied.o
	}
	var status uint32
	err := x.o.call(slotProcessOutput, 0, 1, uintptr(unsafe.Pointer(&ob)), uintptr(unsafe.Pointer(&status)))
	ob.Events.release()
	if err != nil {
		if supplied == nil {
			ob.Sample.release()
		}
		return nil, err
	}
	if supplied != nil {
		return supplied, nil
	}
	if ob.Sample == nil {
		return nil, hrPointer
	}
	return &winSample{winAttrs{o: ob.Sample}}, nil
}

func (x *winTransform) release() {
	o := x.o
	x.o = nil
	o.release()
}
