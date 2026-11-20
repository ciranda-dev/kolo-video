//go:build darwin

package kolovideo

import (
	"math"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

var (
	cmVideoDimensions func(desc uintptr) cmDimensions
	cmMediaSubType    func(desc uintptr) uint32
	cmSampleImage     func(sb uintptr) uintptr
	cmSamplePTS       func(sb uintptr) avfTime
	cmSyncConvert     func(t avfTime, from, to uintptr) avfTime
	cmHostClock       func() uintptr

	cvPixelBufferTypeID func() uintptr
	cvLock              func(pb uintptr, flags uint64) int32
	cvUnlock            func(pb uintptr, flags uint64) int32
	cvFormat            func(pb uintptr) uint32
	cvWidth             func(pb uintptr) uintptr
	cvHeight            func(pb uintptr) uintptr
	cvPlaneCount        func(pb uintptr) uintptr
	cvPlaneBase         func(pb, plane uintptr) unsafe.Pointer
	cvPlaneStride       func(pb, plane uintptr) uintptr
	cvPlaneHeight       func(pb, plane uintptr) uintptr
	cvBase              func(pb uintptr) unsafe.Pointer
	cvStride            func(pb uintptr) uintptr
	cvCopyAttachment    func(buf, key uintptr, mode *uint32) uintptr

	cvPixelBufferID uintptr
	cvMatrixKey     uintptr
	cvMatrix709     uintptr
)

type cmDimensions struct{ Width, Height int32 }

const (
	cvLockReadOnly = 1
	// clockUptimeRaw is CLOCK_UPTIME_RAW, the clock of the host time clock.
	clockUptimeRaw = 8
)

func bindSampleFuncs(cm, cv uintptr) {
	purego.RegisterLibFunc(&cmVideoDimensions, cm, "CMVideoFormatDescriptionGetDimensions")
	purego.RegisterLibFunc(&cmMediaSubType, cm, "CMFormatDescriptionGetMediaSubType")
	purego.RegisterLibFunc(&cmSampleImage, cm, "CMSampleBufferGetImageBuffer")
	purego.RegisterLibFunc(&cmSamplePTS, cm, "CMSampleBufferGetPresentationTimeStamp")
	purego.RegisterLibFunc(&cmSyncConvert, cm, "CMSyncConvertTime")
	purego.RegisterLibFunc(&cmHostClock, cm, "CMClockGetHostTimeClock")
	purego.RegisterLibFunc(&cvPixelBufferTypeID, cv, "CVPixelBufferGetTypeID")
	purego.RegisterLibFunc(&cvLock, cv, "CVPixelBufferLockBaseAddress")
	purego.RegisterLibFunc(&cvUnlock, cv, "CVPixelBufferUnlockBaseAddress")
	purego.RegisterLibFunc(&cvFormat, cv, "CVPixelBufferGetPixelFormatType")
	purego.RegisterLibFunc(&cvWidth, cv, "CVPixelBufferGetWidth")
	purego.RegisterLibFunc(&cvHeight, cv, "CVPixelBufferGetHeight")
	purego.RegisterLibFunc(&cvPlaneCount, cv, "CVPixelBufferGetPlaneCount")
	purego.RegisterLibFunc(&cvPlaneBase, cv, "CVPixelBufferGetBaseAddressOfPlane")
	purego.RegisterLibFunc(&cvPlaneStride, cv, "CVPixelBufferGetBytesPerRowOfPlane")
	purego.RegisterLibFunc(&cvPlaneHeight, cv, "CVPixelBufferGetHeightOfPlane")
	purego.RegisterLibFunc(&cvBase, cv, "CVPixelBufferGetBaseAddress")
	purego.RegisterLibFunc(&cvStride, cv, "CVPixelBufferGetBytesPerRow")
	purego.RegisterLibFunc(&cvCopyAttachment, cv, "CVBufferCopyAttachment")
}

func bindSampleConstants(cv uintptr) {
	cvPixelBufferID = cvPixelBufferTypeID()
	cvMatrixKey = uintptr(avfConstant(cv, "kCVImageBufferYCbCrMatrixKey"))
	cvMatrix709 = uintptr(avfConstant(cv, "kCVImageBufferYCbCrMatrix_ITU_R_709_2"))
}

// sample converts one sample buffer from the capture queue and reports it.
func (t *darwinTarget) sample(sb uintptr) {
	if sb == 0 {
		t.sink.invalid()
		return
	}
	t.image(cmSampleImage(sb), t.timestamp(sb))
}

// timestamp returns the presentation time of sb on the host clock in
// nanoseconds, or the current host time if the buffer has none.
func (t *darwinTarget) timestamp(sb uintptr) int64 {
	pts := cmSamplePTS(sb)
	if pts.numeric() {
		if clock := t.clock.Load(); clock != 0 {
			pts = cmSyncConvert(pts, clock, cmHostClock())
		}
		if ns, ok := pts.nanos(); ok {
			return ns
		}
	}
	return int64(uptimeNanos(clockUptimeRaw))
}

// image converts a pixel buffer that has timestamp ts.
func (t *darwinTarget) image(pb uintptr, ts int64) {
	if pb == 0 || cfTypeID(pb) != cvPixelBufferID || cvLock(pb, cvLockReadOnly) != 0 {
		t.sink.invalid()
		return
	}
	defer cvUnlock(pb, cvLockReadOnly)
	w, h := int(cvWidth(pb)), int(cvHeight(pb))
	if !fitsPixels(w, h, t.maxPixels) {
		t.sink.invalid()
		return
	}
	var f *Frame
	var err error
	switch cvFormat(pb) {
	case fccNV12:
		if cvPlaneCount(pb) != 2 {
			t.sink.invalid()
			return
		}
		ys, uvs := int(cvPlaneStride(pb, 0)), int(cvPlaneStride(pb, 1))
		y := avfBytes(cvPlaneBase(pb, 0), ys, int(cvPlaneHeight(pb, 0)))
		uv := avfBytes(cvPlaneBase(pb, 1), uvs, int(cvPlaneHeight(pb, 1)))
		if y == nil || uv == nil {
			t.sink.invalid()
			return
		}
		f, err = FromNV12(w, h, y, ys, uv, uvs)
		if err == nil && avfIs709(pb) {
			f.rec709to601()
		}
	case fccBGRA:
		stride := int(cvStride(pb))
		p := avfBytes(cvBase(pb), stride, h)
		if p == nil {
			t.sink.invalid()
			return
		}
		f, err = FromBGRA(w, h, p, stride)
	default:
		t.sink.invalid()
		return
	}
	if err != nil {
		t.sink.invalid()
		return
	}
	f.Timestamp = time.Duration(ts)
	f.Duration = time.Duration(t.dur.Load())
	t.sink.deliver(f)
}

// avfBytes views a locked plane, nil if its description is unusable.
func avfBytes(base unsafe.Pointer, stride, rows int) []byte {
	if base == nil || stride <= 0 || rows <= 0 || stride > math.MaxInt32 || rows > math.MaxInt32 {
		return nil
	}
	return unsafe.Slice((*byte)(base), stride*rows)
}

// avfIs709 reports whether the buffer is tagged with the BT.709 matrix.
func avfIs709(pb uintptr) bool {
	m := cvCopyAttachment(pb, cvMatrixKey, nil)
	if m == 0 {
		return false
	}
	defer cfRelease(m)
	return cfEqual(m, cvMatrix709)
}
