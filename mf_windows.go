//go:build windows && (amd64 || arm64)

package kolovideo

// The Media Foundation objects of mfAPI, called through their vtables.

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	mfplat   = windows.NewLazySystemDLL("mfplat.dll")
	mfdll    = windows.NewLazySystemDLL("mf.dll")
	mfrw     = windows.NewLazySystemDLL("mfreadwrite.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procMFStartup                           = mfplat.NewProc("MFStartup")
	procMFShutdown                          = mfplat.NewProc("MFShutdown")
	procMFCreateAttributes                  = mfplat.NewProc("MFCreateAttributes")
	procMFCreateMediaType                   = mfplat.NewProc("MFCreateMediaType")
	procMFEnumDeviceSources                 = mfdll.NewProc("MFEnumDeviceSources")
	procMFCreateDeviceSource                = mfdll.NewProc("MFCreateDeviceSource")
	procMFCreateSourceReaderFromMediaSource = mfrw.NewProc("MFCreateSourceReaderFromMediaSource")
	procQueryPerformanceCounter             = kernel32.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFrequency           = kernel32.NewProc("QueryPerformanceFrequency")
)

// mfLoad finds every Media Foundation function the backend calls. A Windows
// edition without Media Foundation (the N editions without the Media
// Feature Pack) fails here, naming the missing DLL.
var mfLoad = sync.OnceValue(func() error {
	for _, p := range []*windows.LazyProc{
		procMFStartup, procMFShutdown, procMFCreateAttributes, procMFCreateMediaType,
		procMFEnumDeviceSources, procMFCreateDeviceSource, procMFCreateSourceReaderFromMediaSource,
	} {
		if err := p.Find(); err != nil {
			return fmt.Errorf("%w: Media Foundation: %w", ErrUnsupported, err)
		}
	}
	return nil
})

func hresultOf(r uintptr) error {
	if int32(r) < 0 {
		return mfHRESULT(uint32(r))
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

// comObj is a COM interface pointer: an object whose first word points to
// its vtable. Slots are the constants of mf_ids.go.
type comObj struct{ vtbl *[64]uintptr }

// call invokes vtable entry slot with the object as first argument. A
// failure HRESULT is returned as an mfHRESULT; success codes are nil.
//
//go:uintptrescapes
func (o *comObj) call(slot int, args ...uintptr) error {
	if o == nil {
		return hrPointer
	}
	a := make([]uintptr, 1, len(args)+1)
	a[0] = uintptr(unsafe.Pointer(o))
	a = append(a, args...)
	r, _, _ := syscall.SyscallN(o.vtbl[slot], a...)
	return hresultOf(r)
}

func (o *comObj) release() {
	if o != nil {
		o.call(slotRelease)
	}
}

// allocatedString reads a string attribute; ok is false if it is absent.
func (o *comObj) allocatedString(key *mfGUID) (s string, ok bool) {
	var p *uint16
	var n uint32
	if o.call(slotAttrGetAllocatedString, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&n))) != nil || p == nil {
		return "", false
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(p))
	return windows.UTF16PtrToString(p), true
}

func newAttributes(size int) (*comObj, error) {
	var a *comObj
	if err := procCall(procMFCreateAttributes, uintptr(unsafe.Pointer(&a)), uintptr(size)); err != nil {
		return nil, err
	}
	return a, nil
}

func qpcNow() time.Duration {
	var c int64
	procQueryPerformanceCounter.Call(uintptr(unsafe.Pointer(&c)))
	return qpcDuration(c, qpcFreq())
}

var qpcFreq = sync.OnceValue(func() int64 {
	var f int64
	procQueryPerformanceFrequency.Call(uintptr(unsafe.Pointer(&f)))
	return f
})

type winMF struct{}

func (winMF) coInit() (func(), error) {
	switch err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err {
	case nil, syscall.Errno(hrFalse):
		return windows.CoUninitialize, nil
	case syscall.Errno(hrChangedMode):
		// The thread already has an apartment of another kind, which
		// also serves these calls and is not ours to leave.
		return func() {}, nil
	default:
		var code syscall.Errno
		if errors.As(err, &code) {
			return nil, mfHRESULT(uint32(code))
		}
		return nil, err
	}
}

func (winMF) startup() (func(), error) {
	if err := mfLoad(); err != nil {
		return nil, err
	}
	if err := procCall(procMFStartup, mfVersion, mfStartupNoSocket); err != nil {
		return nil, err
	}
	return func() { procMFShutdown.Call() }, nil
}

func (winMF) devices() ([]mfDevice, error) {
	attr, err := newAttributes(1)
	if err != nil {
		return nil, err
	}
	defer attr.release()
	if err := attr.call(slotAttrSetGUID, uintptr(unsafe.Pointer(&mfDevSourceType)), uintptr(unsafe.Pointer(&mfDevSourceVidcap))); err != nil {
		return nil, err
	}
	var list **comObj
	var n uint32
	if err := procCall(procMFEnumDeviceSources, uintptr(unsafe.Pointer(attr)), uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&n))); err != nil {
		return nil, err
	}
	if list == nil {
		return nil, nil
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(list))
	var devs []mfDevice
	for _, a := range unsafe.Slice(list, n) {
		if id, ok := a.allocatedString(&mfDevSymbolicLink); ok {
			name, _ := a.allocatedString(&mfDevFriendlyName)
			devs = append(devs, mfDevice{id: id, name: name})
		}
		a.release()
	}
	return devs, nil
}

func (winMF) source(id string) (mfSource, error) {
	w, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return nil, hrInvalidArg
	}
	attr, err := newAttributes(2)
	if err != nil {
		return nil, err
	}
	defer attr.release()
	if err := attr.call(slotAttrSetGUID, uintptr(unsafe.Pointer(&mfDevSourceType)), uintptr(unsafe.Pointer(&mfDevSourceVidcap))); err != nil {
		return nil, err
	}
	if err := attr.call(slotAttrSetString, uintptr(unsafe.Pointer(&mfDevSymbolicLink)), uintptr(unsafe.Pointer(w))); err != nil {
		return nil, err
	}
	var s *comObj
	if err := procCall(procMFCreateDeviceSource, uintptr(unsafe.Pointer(attr)), uintptr(unsafe.Pointer(&s))); err != nil {
		return nil, err
	}
	return &winSource{o: s}, nil
}

// winSource is IMFMediaSource.
type winSource struct{ o *comObj }

func (s *winSource) release() { s.o.release() }

func (s *winSource) shutdown() error { return s.o.call(slotSourceShutdown) }

// handler returns the media type handler of the first stream.
func (s *winSource) handler() (*comObj, error) {
	var pd, sd, h *comObj
	var selected int32
	if err := s.o.call(slotSourceCreatePD, uintptr(unsafe.Pointer(&pd))); err != nil {
		return nil, err
	}
	defer pd.release()
	if err := pd.call(slotPDStreamByIndex, 0, uintptr(unsafe.Pointer(&selected)), uintptr(unsafe.Pointer(&sd))); err != nil {
		return nil, err
	}
	defer sd.release()
	if err := sd.call(slotSDTypeHandler, uintptr(unsafe.Pointer(&h))); err != nil {
		return nil, err
	}
	return h, nil
}

func (s *winSource) modes() ([]Mode, error) {
	h, err := s.handler()
	if err != nil {
		return nil, err
	}
	defer h.release()
	var count uint32
	if err := h.call(slotHandlerCount, uintptr(unsafe.Pointer(&count))); err != nil {
		return nil, err
	}
	return mfModes(count, func(i uint32) Mode { return nativeMode(h, i) }), nil
}

func nativeMode(h *comObj, index uint32) Mode {
	var t *comObj
	if h.call(slotHandlerByIndex, uintptr(index), uintptr(unsafe.Pointer(&t))) != nil {
		return Mode{}
	}
	defer t.release()
	return typeMode(t)
}

// typeMode reads the size, rate and subtype of a media type.
func typeMode(t *comObj) Mode {
	var size, rate uint64
	var sub mfGUID
	var ps, pr *uint64
	var pg *mfGUID
	if t.call(slotAttrGetUINT64, uintptr(unsafe.Pointer(&mfMTFrameSize)), uintptr(unsafe.Pointer(&size))) == nil {
		ps = &size
	}
	if t.call(slotAttrGetUINT64, uintptr(unsafe.Pointer(&mfMTFrameRate)), uintptr(unsafe.Pointer(&rate))) == nil {
		pr = &rate
	}
	if t.call(slotAttrGetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&sub))) == nil {
		pg = &sub
	}
	return mfMode(ps, pr, pg)
}

func (s *winSource) start(index int) (mfReader, mfLayout, error) {
	h, err := s.handler()
	if err != nil {
		return nil, mfLayout{}, err
	}
	defer h.release()
	var native, out, reader *comObj
	if err := h.call(slotHandlerByIndex, uintptr(index), uintptr(unsafe.Pointer(&native))); err != nil {
		return nil, mfLayout{}, err
	}
	defer native.release()
	if err := h.call(slotHandlerSetCurrent, uintptr(unsafe.Pointer(native))); err != nil {
		return nil, mfLayout{}, err
	}
	ra, err := newAttributes(1)
	if err != nil {
		return nil, mfLayout{}, err
	}
	defer ra.release()
	if err := ra.call(slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfReaderAdvancedVideo)), 1); err != nil {
		return nil, mfLayout{}, err
	}
	if err := procCall(procMFCreateSourceReaderFromMediaSource, uintptr(unsafe.Pointer(s.o)), uintptr(unsafe.Pointer(ra)), uintptr(unsafe.Pointer(&reader))); err != nil {
		return nil, mfLayout{}, err
	}
	fail := func(err error) (mfReader, mfLayout, error) {
		reader.release()
		return nil, mfLayout{}, err
	}
	if err := procCall(procMFCreateMediaType, uintptr(unsafe.Pointer(&out))); err != nil {
		return fail(err)
	}
	defer out.release()
	if err := out.call(slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTMajorType)), uintptr(unsafe.Pointer(&mfMediaVideo))); err != nil {
		return fail(err)
	}
	if err := out.call(slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&mfFmtNV12))); err != nil {
		return fail(err)
	}
	if err := reader.call(slotReaderSetCurrent, mfFirstVideoStream, 0, uintptr(unsafe.Pointer(out))); err != nil {
		return fail(err)
	}
	r := &winReader{o: reader}
	l, err := r.layout()
	if err != nil {
		return fail(err)
	}
	return r, l, nil
}

// winReader is a synchronous IMFSourceReader.
type winReader struct{ o *comObj }

func (r *winReader) release() { r.o.release() }

func (r *winReader) flush() error { return r.o.call(slotReaderFlush, mfAllStreams) }

func (r *winReader) layout() (mfLayout, error) {
	var t *comObj
	if err := r.o.call(slotReaderGetCurrent, mfFirstVideoStream, uintptr(unsafe.Pointer(&t))); err != nil {
		return mfLayout{}, err
	}
	defer t.release()
	return typeLayout(t)
}

// typeLayout reads the frame size, display aperture and YCbCr matrix of a
// media type.
func typeLayout(t *comObj) (mfLayout, error) {
	var size uint64
	if err := t.call(slotAttrGetUINT64, uintptr(unsafe.Pointer(&mfMTFrameSize)), uintptr(unsafe.Pointer(&size))); err != nil {
		return mfLayout{}, err
	}
	var area mfVideoArea
	var written uint32
	var pa *mfVideoArea
	if t.call(slotAttrGetBlob, uintptr(unsafe.Pointer(&mfMTDisplayAperture)), uintptr(unsafe.Pointer(&area)), unsafe.Sizeof(area), uintptr(unsafe.Pointer(&written))) == nil && written == uint32(unsafe.Sizeof(area)) {
		pa = &area
	}
	var m uint32
	var pm *uint32
	if t.call(slotAttrGetUINT32, uintptr(unsafe.Pointer(&mfMTYUVMatrix)), uintptr(unsafe.Pointer(&m))) == nil {
		pm = &m
	}
	return mfLayoutOf(size, pa, pm), nil
}

// read blocks for the next sample and locks its buffer for reading.
func (r *winReader) read() (mfSample, error) {
	var stream, flags uint32
	var ts int64
	var smp *comObj
	err := r.o.call(slotReaderReadSample, mfFirstVideoStream, 0, uintptr(unsafe.Pointer(&stream)), uintptr(unsafe.Pointer(&flags)), uintptr(unsafe.Pointer(&ts)), uintptr(unsafe.Pointer(&smp)))
	s := mfSample{flags: flags, ts: qpcNow()}
	if err != nil || smp == nil {
		return s, err
	}
	defer smp.release()
	var buf *comObj
	if err := smp.call(slotSampleContiguous, uintptr(unsafe.Pointer(&buf))); err != nil {
		return s, err
	}
	var b2 *comObj
	if buf.call(slotQueryInterface, uintptr(unsafe.Pointer(&iidMF2DBuffer2)), uintptr(unsafe.Pointer(&b2))) == nil {
		return lock2D(s, buf, b2)
	}
	var p *byte
	var maxLen, n uint32
	if err := buf.call(slotBufferLock, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&n))); err != nil {
		buf.release()
		return s, err
	}
	if p == nil {
		buf.call(slotBufferUnlock)
		buf.release()
		return s, hrPointer
	}
	s.data = unsafe.Slice(p, n)
	if s.data == nil {
		s.data = []byte{}
	}
	s.done = func() {
		buf.call(slotBufferUnlock)
		buf.release()
	}
	return s, nil
}

// lock2D locks a buffer that has a 2D layout and takes ownership of buf and
// b2.
func lock2D(s mfSample, buf, b2 *comObj) (mfSample, error) {
	var p, start *byte
	var pitch int32
	var n uint32
	if err := b2.call(slot2DLockSize, mf2DLockRead, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&pitch)), uintptr(unsafe.Pointer(&start)), uintptr(unsafe.Pointer(&n))); err != nil {
		b2.release()
		buf.release()
		return s, err
	}
	off := uintptr(unsafe.Pointer(p)) - uintptr(unsafe.Pointer(start))
	if p == nil || start == nil || uintptr(unsafe.Pointer(p)) < uintptr(unsafe.Pointer(start)) || off > uintptr(n) {
		b2.call(slot2DUnlock)
		b2.release()
		buf.release()
		return s, hrUnexpected
	}
	s.data = unsafe.Slice(p, uintptr(n)-off)
	if s.data == nil {
		s.data = []byte{}
	}
	s.pitch = int(pitch)
	s.done = func() {
		b2.call(slot2DUnlock)
		b2.release()
		buf.release()
	}
	return s, nil
}
