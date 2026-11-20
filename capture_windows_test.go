//go:build windows && (amd64 || arm64)

package kolovideo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Vtable slots used only by these tests, from the same headers as the
// slots of mf_ids.go.
const (
	slotAddRef              = 1  // IMFAttributes::AddRef
	slotAttrGetStringLength = 11 // IMFAttributes::GetStringLength
	slotAttrSetBlob         = 26 // IMFAttributes::SetBlob
	slotSampleSetTime       = 36 // IMFSample::SetSampleTime
	slotSampleSetDuration   = 38 // IMFSample::SetSampleDuration
	slotSampleAddBuffer     = 42 // IMFSample::AddBuffer
	slotBufferSetLength     = 6  // IMFMediaBuffer::SetCurrentLength
	slotWriterAddStream     = 3  // IMFSinkWriter::AddStream
	slotWriterSetInput      = 4  // IMFSinkWriter::SetInputMediaType
	slotWriterBegin         = 5  // IMFSinkWriter::BeginWriting
	slotWriterWrite         = 6  // IMFSinkWriter::WriteSample
	slotWriterFinalize      = 11 // IMFSinkWriter::Finalize
	slotResolverFromURL     = 3  // IMFSourceResolver::CreateObjectFromURL

	mfResolutionMediaSource           = 0x1        // MF_RESOLUTION_MEDIASOURCE, mfidl.h:3648
	mfInterlaceProgressive            = 2          // MFVideoInterlace_Progressive, mfobjects.h:2678
	hrNoInterface           mfHRESULT = 0x80004002 // E_NOINTERFACE, winerror.h:3419
	hrCodecNotFound         mfHRESULT = 0xC00D5212 // MF_E_TOPO_CODEC_NOT_FOUND, mferror.h:222
)

var (
	iidMFMediaSource              = mfGUID{0x279A808D, 0xAEC7, 0x40C8, [8]byte{0x9C, 0x6B, 0xA6, 0xB4, 0x92, 0xC7, 0x8A, 0x66}}
	mfFmtH264                     = mfGUID{0x34363248, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	mfMTAvgBitrate                = mfGUID{0x20332624, 0xFB0D, 0x4D9E, [8]byte{0xBD, 0x0D, 0xCB, 0xF6, 0x78, 0x6C, 0x10, 0x2E}}
	mfMTInterlace                 = mfGUID{0xE2724BB8, 0xE676, 0x4806, [8]byte{0xB4, 0xB2, 0xA8, 0xD6, 0xEF, 0xB4, 0x4C, 0xCD}}
	mfMTPixelAspect               = mfGUID{0xC6376A1E, 0x8D0A, 0x4027, [8]byte{0xBE, 0x45, 0x6D, 0x9A, 0x0A, 0xD3, 0x9B, 0xB6}}
	procMFCreateSample            = mfplat.NewProc("MFCreateSample")
	procMFCreateMemoryBuffer      = mfplat.NewProc("MFCreateMemoryBuffer")
	procMFCreateSourceResolver    = mfplat.NewProc("MFCreateSourceResolver")
	procMFCreateSinkWriterFromURL = mfrw.NewProc("MFCreateSinkWriterFromURL")
)

func TestMFWindowsGUIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		g      *mfGUID
		header string
		want   string
	}{
		{"IID_IMFMediaSource", &iidMFMediaSource, "mfidl.h:2774", "279A808D-AEC7-40C8-9C6B-A6B492C78A66"},
		{"MFVideoFormat_H264", &mfFmtH264, "mfapi.h:406", "34363248-0000-0010-8000-00AA00389B71"},
		{"MF_MT_AVG_BITRATE", &mfMTAvgBitrate, "mfapi.h:593", "20332624-FB0D-4D9E-BD0D-CBF6786C102E"},
		{"MF_MT_INTERLACE_MODE", &mfMTInterlace, "mfapi.h:583", "E2724BB8-E676-4806-B4B2-A8D6EFB44CCD"},
		{"MF_MT_PIXEL_ASPECT_RATIO", &mfMTPixelAspect, "mfapi.h:578", "C6376A1E-8D0A-4027-BE45-6D9A0AD39BB6"},
	} {
		if got := tc.g.String(); got != tc.want {
			t.Errorf("%s (%s) is %s, want %s", tc.name, tc.header, got, tc.want)
		}
	}
}

func TestMFWindowsSlots(t *testing.T) {
	for _, tc := range []struct {
		iface, method string
		got, want     int
		header        string
	}{
		{"IMFAttributes", "AddRef", slotAddRef, 1, "mfobjects.h:456"},
		{"IMFAttributes", "GetStringLength", slotAttrGetStringLength, 11, "mfobjects.h:505"},
		{"IMFAttributes", "SetBlob", slotAttrSetBlob, 26, "mfobjects.h:584"},
		{"IMFSample", "SetSampleTime", slotSampleSetTime, 36, "mfobjects.h:1142"},
		{"IMFSample", "SetSampleDuration", slotSampleSetDuration, 38, "mfobjects.h:1150"},
		{"IMFSample", "AddBuffer", slotSampleAddBuffer, 42, "mfobjects.h:1167"},
		{"IMFMediaBuffer", "SetCurrentLength", slotBufferSetLength, 6, "mfobjects.h:839"},
		{"IMFSinkWriter", "AddStream", slotWriterAddStream, 3, "mfreadwrite.h:932"},
		{"IMFSinkWriter", "SetInputMediaType", slotWriterSetInput, 4, "mfreadwrite.h:937"},
		{"IMFSinkWriter", "BeginWriting", slotWriterBegin, 5, "mfreadwrite.h:943"},
		{"IMFSinkWriter", "WriteSample", slotWriterWrite, 6, "mfreadwrite.h:946"},
		{"IMFSinkWriter", "Finalize", slotWriterFinalize, 11, "mfreadwrite.h:969"},
		{"IMFSourceResolver", "CreateObjectFromURL", slotResolverFromURL, 3, "mfidl.h:5646"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s::%s is slot %d, header %s says %d", tc.iface, tc.method, tc.got, tc.header, tc.want)
		}
	}
}

// withMF runs f on the test's own thread, locked, in a multithreaded
// apartment with Media Foundation started, as the backend does.
func withMF(t *testing.T, f func()) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	uninit, err := winMF{}.coInit()
	if err != nil {
		t.Fatal(err)
	}
	defer uninit()
	shutdown, err := winMF{}.startup()
	if err != nil {
		t.Skip(err)
	}
	defer shutdown()
	f()
}

// TestMFEnumerate lists the cameras of this machine. It activates no
// device: the result may be empty, which still proves that COM, MFStartup,
// MFEnumDeviceSources and the release of every object work.
func TestMFEnumerate(t *testing.T) {
	devs, err := platformDevices()
	if errors.Is(err, ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s/%s: MFStartup ok, MFEnumDeviceSources returned %d video capture devices", runtime.GOOS, runtime.GOARCH, len(devs))
	defaults := 0
	for _, d := range devs {
		t.Logf("  default=%v name=%q link=%q", d.Default, d.Name, d.ID)
		if d.ID == "" || len(d.Modes) != 0 {
			t.Errorf("device %+v", d)
		}
		if d.Default {
			defaults++
		}
	}
	if len(devs) > 0 && (defaults != 1 || !devs[0].Default) {
		t.Errorf("%d default devices", defaults)
	}
	for range 20 {
		again, err := Devices()
		if err != nil || len(again) != len(devs) {
			t.Fatalf("second listing: %d devices, %v", len(again), err)
		}
	}
}

func TestMFLoad(t *testing.T) {
	if err := mfLoad(); err != nil {
		t.Skip(err)
	}
}

// TestMFCoInit checks the apartment rules on one locked thread.
func TestMFCoInit(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		first, err := winMF{}.coInit()
		if err != nil {
			t.Error(err)
			return
		}
		second, err := winMF{}.coInit() // S_FALSE: already entered
		if err != nil {
			t.Error(err)
		}
		second()
		first()
		if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil {
			t.Error(err)
			return
		}
		leave, err := winMF{}.coInit() // RPC_E_CHANGED_MODE: the apartment is not ours to leave
		if err != nil {
			t.Error(err)
		} else {
			leave()
		}
		windows.CoUninitialize()
	}()
	<-done
}

func newType(t *testing.T) *comObj {
	t.Helper()
	var o *comObj
	if err := procCall(procMFCreateMediaType, uintptr(unsafe.Pointer(&o))); err != nil {
		t.Fatal(err)
	}
	return o
}

func setSize(o *comObj, key *mfGUID, hi, lo uint32) error {
	return o.call(slotAttrSetUINT64, uintptr(unsafe.Pointer(key)), uintptr(uint64(hi)<<32|uint64(lo)))
}

// TestMFVtableLive calls every production vtable slot that needs no
// camera on objects made by Media Foundation, and reads the results back
// through the backend's own decoders.
func TestMFVtableLive(t *testing.T) {
	if err := mfLoad(); err != nil {
		t.Skip(err)
	}
	withMF(t, func() {
		a, err := newAttributes(4)
		if err != nil {
			t.Error(err)
			return
		}
		defer a.release()
		r, _, _ := syscall.SyscallN(a.vtbl[slotAddRef], uintptr(unsafe.Pointer(a)))
		if r != 2 {
			t.Errorf("AddRef returned %d, want 2", r)
		}
		r, _, _ = syscall.SyscallN(a.vtbl[slotRelease], uintptr(unsafe.Pointer(a)))
		if r != 1 {
			t.Errorf("Release returned %d, want 1", r)
		}
		var u32 uint32
		if err := a.call(slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfMTYUVMatrix)), 0xDEADBEEF); err != nil {
			t.Error(err)
		}
		if err := a.call(slotAttrGetUINT32, uintptr(unsafe.Pointer(&mfMTYUVMatrix)), uintptr(unsafe.Pointer(&u32))); err != nil || u32 != 0xDEADBEEF {
			t.Errorf("UINT32 %#x, %v", u32, err)
		}
		if err := a.call(slotAttrGetUINT32, uintptr(unsafe.Pointer(&mfMTFrameSize)), uintptr(unsafe.Pointer(&u32))); err == nil {
			t.Error("a missing attribute was found")
		}
		var u64 uint64
		if err := setSize(a, &mfMTFrameSize, 1920, 1080); err != nil {
			t.Error(err)
		}
		if err := a.call(slotAttrGetUINT64, uintptr(unsafe.Pointer(&mfMTFrameSize)), uintptr(unsafe.Pointer(&u64))); err != nil || u64 != 1920<<32|1080 {
			t.Errorf("UINT64 %#x, %v", u64, err)
		}
		var g mfGUID
		if err := a.call(slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&mfFmtH264))); err != nil {
			t.Error(err)
		}
		if err := a.call(slotAttrGetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&g))); err != nil || g != mfFmtH264 {
			t.Errorf("GUID %v, %v", g, err)
		}
		const link = `\\?\usb#vid_046d&pid_0825#6&1b#{e5323777-f976-4f5b-9b55-b94699c46e44}\global`
		w, _ := windows.UTF16PtrFromString(link)
		if err := a.call(slotAttrSetString, uintptr(unsafe.Pointer(&mfDevSymbolicLink)), uintptr(unsafe.Pointer(w))); err != nil {
			t.Error(err)
		}
		var n uint32
		if err := a.call(slotAttrGetStringLength, uintptr(unsafe.Pointer(&mfDevSymbolicLink)), uintptr(unsafe.Pointer(&n))); err != nil || n != uint32(len(link)) {
			t.Errorf("string length %d, %v", n, err)
		}
		if s, ok := a.allocatedString(&mfDevSymbolicLink); !ok || s != link {
			t.Errorf("string %q, %v", s, ok)
		}
		if _, ok := a.allocatedString(&mfDevFriendlyName); ok {
			t.Error("a missing string was found")
		}
		var q *comObj
		if err := a.call(slotQueryInterface, uintptr(unsafe.Pointer(&iidMF2DBuffer2)), uintptr(unsafe.Pointer(&q))); err != hrNoInterface || q != nil {
			t.Errorf("QueryInterface of an unsupported interface: %v, %p", err, q)
		}

		// A type with every attribute the backend reads.
		mt := newType(t)
		defer mt.release()
		area := mfVideoArea{cx: 1280, cy: 720}
		if err := setSize(mt, &mfMTFrameSize, 1280, 736); err != nil {
			t.Error(err)
		}
		if err := setSize(mt, &mfMTFrameRate, 30000, 1001); err != nil {
			t.Error(err)
		}
		if err := mt.call(slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(&mfFmtNV12))); err != nil {
			t.Error(err)
		}
		if err := mt.call(slotAttrSetBlob, uintptr(unsafe.Pointer(&mfMTDisplayAperture)), uintptr(unsafe.Pointer(&area)), unsafe.Sizeof(area)); err != nil {
			t.Error(err)
		}
		if err := mt.call(slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfMTYUVMatrix)), mfMatrixBT709); err != nil {
			t.Error(err)
		}
		if got, want := typeMode(mt), (Mode{1280, 736, 30000.0 / 1001, FormatNV12}); got != want {
			t.Errorf("mode %+v, want %+v", got, want)
		}
		if got, err := typeLayout(mt); err != nil || got != (mfLayout{1280, 720, 1280, 736, true}) {
			t.Errorf("layout %+v, %v", got, err)
		}

		// A type that has only a size.
		bare := newType(t)
		defer bare.release()
		if err := setSize(bare, &mfMTFrameSize, 640, 480); err != nil {
			t.Error(err)
		}
		if got, want := typeMode(bare), (Mode{640, 480, 0, FormatUnknown}); got != want {
			t.Errorf("mode %+v, want %+v", got, want)
		}
		if got, err := typeLayout(bare); err != nil || got != (mfLayout{640, 480, 640, 480, false}) {
			t.Errorf("layout %+v, %v", got, err)
		}
		if _, err := typeLayout(newType(t)); err == nil {
			t.Error("a type without a size has a layout")
		}
	})
}

// TestMFOpenUnknownDevice activates a device that does not exist, which
// touches no camera.
func TestMFOpenUnknownDevice(t *testing.T) {
	if err := mfLoad(); err != nil {
		t.Skip(err)
	}
	cfg, _ := CaptureConfig{}.withDefaults()
	be, _, err := platformOpen(Device{ID: `\\?\kolovideo#none#{e5323777-f976-4f5b-9b55-b94699c46e44}\global`}, cfg, newMFSink())
	if be != nil {
		be.close()
		t.Fatal("opened a device that does not exist")
	}
	if !errors.Is(err, ErrNoDevice) {
		t.Errorf("error %v", err)
	}
	t.Logf("error: %v", err)
}

func buildType(t *testing.T, sub *mfGUID, w, h uint32, kbps uint32) *comObj {
	t.Helper()
	mt := newType(t)
	for _, step := range []error{
		mt.call(slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTMajorType)), uintptr(unsafe.Pointer(&mfMediaVideo))),
		mt.call(slotAttrSetGUID, uintptr(unsafe.Pointer(&mfMTSubtype)), uintptr(unsafe.Pointer(sub))),
		mt.call(slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfMTInterlace)), mfInterlaceProgressive),
		mt.call(slotAttrSetUINT32, uintptr(unsafe.Pointer(&mfMTAvgBitrate)), uintptr(kbps*1000)),
		setSize(mt, &mfMTFrameSize, w, h),
		setSize(mt, &mfMTFrameRate, 30, 1),
		setSize(mt, &mfMTPixelAspect, 1, 1),
	} {
		if step != nil {
			t.Fatal(step)
		}
	}
	return mt
}

// writeMP4 writes n solid NV12 frames, the luma of frame i being lumaOf(i),
// as H.264 in an MP4 file with the Sink Writer. It reports false, with the
// reason, when this Windows has no H.264 encoder.
func writeMP4(t *testing.T, path string, w, h, n int, lumaOf func(int) byte) (ok bool) {
	t.Helper()
	url, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	var wr *comObj
	if err := procCall(procMFCreateSinkWriterFromURL, uintptr(unsafe.Pointer(url)), 0, 0, uintptr(unsafe.Pointer(&wr))); err != nil {
		t.Fatal(err)
	}
	defer wr.release()
	out := buildType(t, &mfFmtH264, uint32(w), uint32(h), 800)
	defer out.release()
	in := buildType(t, &mfFmtNV12, uint32(w), uint32(h), 0)
	defer in.release()
	var stream uint32
	if err := wr.call(slotWriterAddStream, uintptr(unsafe.Pointer(out)), uintptr(unsafe.Pointer(&stream))); err != nil {
		if err == hrCodecNotFound {
			t.Logf("no H.264 encoder: %v", err)
			return false
		}
		t.Fatalf("AddStream: %v", err)
	}
	if err := wr.call(slotWriterSetInput, uintptr(stream), uintptr(unsafe.Pointer(in)), 0); err != nil {
		if err == hrCodecNotFound {
			t.Logf("no H.264 encoder: %v", err)
			return false
		}
		t.Fatalf("SetInputMediaType: %v", err)
	}
	if err := wr.call(slotWriterBegin); err != nil {
		t.Fatalf("BeginWriting: %v", err)
	}
	size := w * h * 3 / 2
	for i := range n {
		var buf, smp *comObj
		if err := procCall(procMFCreateMemoryBuffer, uintptr(size), uintptr(unsafe.Pointer(&buf))); err != nil {
			t.Fatal(err)
		}
		var p *byte
		var maxLen, cur uint32
		if err := buf.call(slotBufferLock, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&cur))); err != nil {
			t.Fatal(err)
		}
		data := unsafe.Slice(p, maxLen)[:size]
		for j := range data {
			data[j] = 128
		}
		for j := range w * h {
			data[j] = lumaOf(i)
		}
		buf.call(slotBufferUnlock)
		if err := buf.call(slotBufferSetLength, uintptr(size)); err != nil {
			t.Fatal(err)
		}
		if err := procCall(procMFCreateSample, uintptr(unsafe.Pointer(&smp))); err != nil {
			t.Fatal(err)
		}
		const unit = 10_000_000 // 100 ns per second
		for _, step := range []error{
			smp.call(slotSampleAddBuffer, uintptr(unsafe.Pointer(buf))),
			smp.call(slotSampleSetTime, uintptr(i*unit/30)),
			smp.call(slotSampleSetDuration, uintptr(unit/30)),
			wr.call(slotWriterWrite, uintptr(stream), uintptr(unsafe.Pointer(smp))),
		} {
			if step != nil {
				t.Fatalf("frame %d: %v", i, step)
			}
		}
		smp.release()
		buf.release()
	}
	if err := wr.call(slotWriterFinalize); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	return true
}

// fileMF is the real Media Foundation with a media source made from a
// file in place of a camera, so the reader path runs without a camera.
type fileMF struct {
	winMF
	path string
}

func (fileMF) devices() ([]mfDevice, error) {
	return []mfDevice{{id: "file", name: "generated file"}}, nil
}

func (f fileMF) source(string) (mfSource, error) {
	url, err := windows.UTF16PtrFromString(f.path)
	if err != nil {
		return nil, err
	}
	var res, unk, src *comObj
	if err := procCall(procMFCreateSourceResolver, uintptr(unsafe.Pointer(&res))); err != nil {
		return nil, err
	}
	defer res.release()
	var kind int32
	if err := res.call(slotResolverFromURL, uintptr(unsafe.Pointer(url)), mfResolutionMediaSource, 0, uintptr(unsafe.Pointer(&kind)), uintptr(unsafe.Pointer(&unk))); err != nil {
		return nil, err
	}
	defer unk.release()
	if err := unk.call(slotQueryInterface, uintptr(unsafe.Pointer(&iidMFMediaSource)), uintptr(unsafe.Pointer(&src))); err != nil {
		return nil, err
	}
	return fileSource{&winSource{o: src}}, nil
}

// fileSource offers the file's H.264 stream as NV12, the way a camera
// offers a compressed type that the reader converts.
type fileSource struct{ *winSource }

func (s fileSource) modes() ([]Mode, error) {
	modes, err := s.winSource.modes()
	for i := range modes {
		if modes[i].Format == FormatUnknown && modes[i].Width > 0 {
			modes[i].Format = FormatNV12
		}
	}
	return modes, err
}

func lumaOf(i int) byte { return byte(40 + 16*i) }

func meanLuma(f *Frame) float64 {
	var sum int
	for y := range f.Height {
		for _, v := range f.Y[y*f.YStride : y*f.YStride+f.Width] {
			sum += int(v)
		}
	}
	return float64(sum) / float64(f.Width*f.Height)
}

// TestMFReaderFile writes generated frames as H.264 MP4 with the Sink
// Writer in a temp dir and reads them back as NV12 through the backend's
// IMFSourceReader path: media type handler, advanced video processing,
// NV12 output, samples, 2D buffers, end of stream and Close from another
// thread. No camera is involved.
func TestMFReaderFile(t *testing.T) {
	if err := mfLoad(); err != nil {
		t.Skip(err)
	}
	const w, h, n = 320, 240, 12
	path := filepath.Join(t.TempDir(), "gen.mp4")
	wrote := false
	withMF(t, func() { wrote = writeMP4(t, path, w, h, n, lumaOf) })
	if !wrote {
		t.Skip("this Windows has no H.264 encoder")
	}
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		t.Fatalf("no MP4 written: %v", err)
	}
	api := fileMF{path: path}

	// The native types of the file, through the media type handler.
	withMF(t, func() {
		src, err := api.source("")
		if err != nil {
			t.Error(err)
			return
		}
		defer src.release()
		modes, err := src.(fileSource).winSource.modes()
		if err != nil || len(modes) != 1 {
			t.Errorf("modes %+v, %v", modes, err)
			return
		}
		if modes[0] != (Mode{w, h, 30, FormatUnknown}) {
			t.Errorf("native type %+v, want 320x240 at 30 fps, H.264 (not a raw format)", modes[0])
		}
		// Shutdown ends the source: its descriptors are refused afterwards.
		if err := src.shutdown(); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if _, err := src.(fileSource).winSource.modes(); err != testShutdown {
			t.Errorf("modes after Shutdown: %v, want %v", err, testShutdown)
		}
	})

	cfg, _ := CaptureConfig{Width: w, Height: h, FrameRate: 30}.withDefaults()
	sink := newMFSink()
	be, mode, err := mfOpen(api, Device{ID: "file"}, cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer be.close()
	if mode.Width != w || mode.Height != h || mode.FrameRate != 30 {
		t.Errorf("mode %+v", mode)
	}
	var last time.Duration
	for i := range n {
		var f *Frame
		select {
		case f = <-sink.frames:
		case err := <-sink.failed:
			t.Fatalf("frame %d: %v (invalid %d)", i, err, sink.invalidN.Load())
		case <-time.After(20 * time.Second):
			t.Fatalf("frame %d: timeout", i)
		}
		if err := f.Validate(); err != nil || f.Width != w || f.Height != h {
			t.Fatalf("frame %d: %dx%d, %v", i, f.Width, f.Height, err)
		}
		if got, want := meanLuma(f), float64(lumaOf(i)); got < want-8 || got > want+8 {
			t.Errorf("frame %d: mean luma %.1f, want %.0f", i, got, want)
		}
		if i > 0 && f.Timestamp <= last {
			t.Errorf("frame %d: timestamp %v after %v", i, f.Timestamp, last)
		}
		last = f.Timestamp
		if f.Duration != frameDuration(30) {
			t.Errorf("duration %v", f.Duration)
		}
	}
	select {
	case err := <-sink.failed:
		if !errors.Is(err, ErrDeviceLost) || !strings.Contains(err.Error(), "stream ended") {
			t.Errorf("at the end of the file: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no end of stream")
	}
	if sink.invalidN.Load() != 0 || len(sink.frames) != 0 {
		t.Errorf("invalid %d, extra frames %d", sink.invalidN.Load(), len(sink.frames))
	}
	if err := be.close(); err != nil {
		t.Error(err)
	}

	// Close from another thread while the reader runs, repeatedly.
	for i := range 20 {
		sink := newMFSink()
		be, _, err := mfOpen(api, Device{ID: "file"}, cfg, sink)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if i%2 == 0 {
			select {
			case <-sink.frames:
			case <-time.After(20 * time.Second):
				t.Fatalf("open %d: no frame", i)
			}
		}
		done := make(chan struct{})
		go func() { be.close(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("close %d did not return", i)
		}
		sink.closed.Store(true)
	}
}

// TestMFHardwareCapture opens the default camera. It turns the camera on
// and runs only with KOLO_VIDEO_HW_TEST=1.
func TestMFHardwareCapture(t *testing.T) {
	if os.Getenv("KOLO_VIDEO_HW_TEST") != "1" {
		t.Skip("set KOLO_VIDEO_HW_TEST=1 to capture from a real camera")
	}
	devs, err := Devices()
	if err != nil || len(devs) == 0 {
		t.Skipf("no camera: %v", err)
	}
	const want = 45
	c, err := OpenCapture(CaptureConfig{DeviceID: devs[0].ID, Width: 1280, Height: 720, FrameRate: 30, Buffer: want})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("device %q mode %+v", c.Device().Name, c.Mode())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var last time.Duration
	start := time.Now()
	for i := range want {
		f, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if err := f.Validate(); err != nil {
			t.Fatal(err)
		}
		if i > 0 && f.Timestamp <= last {
			t.Fatalf("timestamp %v after %v", f.Timestamp, last)
		}
		last = f.Timestamp
	}
	t.Logf("%d frames in %v, stats %+v", want, time.Since(start).Round(time.Millisecond), c.Stats())
	if err := c.Close(); err != nil {
		t.Error(err)
	}
}
