//go:build darwin && (amd64 || arm64)

package h264

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The real layer: C functions of CoreFoundation, CoreMedia, CoreVideo and
// VideoToolbox called through purego. No Objective-C object is used. The two
// output callbacks are created once per process; purego never frees a
// callback and allows 2000 per process.

// vtMaxParamSets bounds the parameter sets read from a format description.
const vtMaxParamSets = 64

var (
	cfRelease          func(obj uintptr)
	cfNumberCreate     func(alloc uintptr, typ int, v unsafe.Pointer) uintptr
	cfNumberGetValue   func(n uintptr, typ int, v unsafe.Pointer) bool
	cfDictCreate       func(alloc uintptr, capacity int, keyCB, valCB uintptr) uintptr
	cfDictSet          func(d, k, v uintptr)
	cfDictGet          func(d, k uintptr) uintptr
	cfArrayCount       func(a uintptr) int
	cfArrayAt          func(a uintptr, i int) uintptr
	cfBoolValue        func(b uintptr) bool
	sysMalloc          func(n uintptr) unsafe.Pointer
	sysFree            func(p unsafe.Pointer)
	vtCompCreate       func(alloc uintptr, w, h int32, codec uint32, spec, src, compAlloc, cb, ref uintptr, out *uintptr) int32
	vtSetProp          func(sess, key, val uintptr) int32
	vtCopyProp         func(sess, key, alloc uintptr, out *uintptr) int32
	vtCompPrepare      func(sess uintptr) int32
	vtCompPool         func(sess uintptr) uintptr
	vtCompEncode       func(sess, pb uintptr, pts, dur cmTime, props, srcRef uintptr, info *uint32) int32
	vtCompComplete     func(sess uintptr, t cmTime) int32
	vtCompInvalidate   func(sess uintptr)
	vtDecCreate        func(alloc, fd, spec, attrs uintptr, rec unsafe.Pointer, out *uintptr) int32
	vtDecDecode        func(sess, sb uintptr, flags uint32, srcRef uintptr, info *uint32) int32
	vtDecCanAccept     func(sess, fd uintptr) bool
	vtDecWait          func(sess uintptr) int32
	vtDecInvalidate    func(sess uintptr)
	vtIsHardwareDecode func(codec uint32) bool
	vtEncoderList      func(opts uintptr, out *uintptr) int32
	cmSampleData       func(sb uintptr) uintptr
	cmSampleFormat     func(sb uintptr) uintptr
	cmSampleAttach     func(sb uintptr, create bool) uintptr
	cmSampleCreate     func(alloc, bb, fd uintptr, nSamples, nTiming int, timing *cmTiming, nSizes int, sizes *uintptr, out *uintptr) int32
	cmBlockLen         func(bb uintptr) uintptr
	cmBlockCopy        func(bb, off, n uintptr, dst unsafe.Pointer) int32
	cmBlockCreate      func(alloc, mem, length, blockAlloc, cbs, off, dataLen uintptr, flags uint32, out *uintptr) int32
	cmBlockReplace     func(src unsafe.Pointer, bb, off, n uintptr) int32
	cmParamAt          func(fd, idx uintptr, ptr *unsafe.Pointer, size, count *uintptr, nalLen *int32) int32
	cmFormatCreate     func(alloc, count uintptr, ptrs, sizes unsafe.Pointer, nalLen int32, out *uintptr) int32
	cvPoolBuffer       func(alloc, pool uintptr, out *uintptr) int32
	cvBufferCreate     func(alloc, w, h uintptr, format uint32, attrs uintptr, out *uintptr) int32
	cvLock             func(pb uintptr, flags uint64) int32
	cvUnlock           func(pb uintptr, flags uint64) int32
	cvPlaneBase        func(pb, plane uintptr) unsafe.Pointer
	cvPlaneStride      func(pb, plane uintptr) uintptr
	cvPlaneRows        func(pb, plane uintptr) uintptr
	cvWidth            func(pb uintptr) uintptr
	cvHeight           func(pb uintptr) uintptr
	cvPlaneCount       func(pb uintptr) uintptr
	cvFormat           func(pb uintptr) uint32

	cfTrue, cfFalse                           uintptr
	dictKeyCB, dictValCB                      uintptr
	vtProps                                   [vtPropCount]uintptr
	vtLevels                                  [vtLevelCount]uintptr
	vtProfileLevelKey, vtEnableHWEnc          uintptr
	vtEnableHWDec, vtUsingHWDec, vtForceKey   uintptr
	vtListCodec, vtListHW, cmNotSync          uintptr
	cvPixelFormatKey, cvWidthKey, cvHeightKey uintptr
	cvIOSurfaceKey                            uintptr
	vtEncCallback, vtDecCallback              uintptr
)

var (
	vtOnce sync.Once
	vtInst *vtReal
	vtErr  error
)

// loadVT binds the frameworks on first use; a missing framework or symbol is
// the engine's absence reason.
func loadVT() (vtOS, error) {
	vtOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				vtErr = fmt.Errorf("VideoToolbox is not usable: %v", r)
			}
		}()
		open := func(path string) uintptr {
			h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
			if err != nil {
				panic(err)
			}
			return h
		}
		fw := "/System/Library/Frameworks/"
		cf := open(fw + "CoreFoundation.framework/CoreFoundation")
		cm := open(fw + "CoreMedia.framework/CoreMedia")
		cv := open(fw + "CoreVideo.framework/CoreVideo")
		vt := open(fw + "VideoToolbox.framework/VideoToolbox")
		sys := open("/usr/lib/libSystem.B.dylib")
		reg := func(fn any, lib uintptr, name string) { purego.RegisterLibFunc(fn, lib, name) }
		reg(&cfRelease, cf, "CFRelease")
		reg(&cfNumberCreate, cf, "CFNumberCreate")
		reg(&cfNumberGetValue, cf, "CFNumberGetValue")
		reg(&cfDictCreate, cf, "CFDictionaryCreateMutable")
		reg(&cfDictSet, cf, "CFDictionarySetValue")
		reg(&cfDictGet, cf, "CFDictionaryGetValue")
		reg(&cfArrayCount, cf, "CFArrayGetCount")
		reg(&cfArrayAt, cf, "CFArrayGetValueAtIndex")
		reg(&cfBoolValue, cf, "CFBooleanGetValue")
		reg(&sysMalloc, sys, "malloc")
		reg(&sysFree, sys, "free")
		reg(&vtCompCreate, vt, "VTCompressionSessionCreate")
		reg(&vtSetProp, vt, "VTSessionSetProperty")
		reg(&vtCopyProp, vt, "VTSessionCopyProperty")
		reg(&vtCompPrepare, vt, "VTCompressionSessionPrepareToEncodeFrames")
		reg(&vtCompPool, vt, "VTCompressionSessionGetPixelBufferPool")
		reg(&vtCompEncode, vt, "VTCompressionSessionEncodeFrame")
		reg(&vtCompComplete, vt, "VTCompressionSessionCompleteFrames")
		reg(&vtCompInvalidate, vt, "VTCompressionSessionInvalidate")
		reg(&vtDecCreate, vt, "VTDecompressionSessionCreate")
		reg(&vtDecDecode, vt, "VTDecompressionSessionDecodeFrame")
		reg(&vtDecCanAccept, vt, "VTDecompressionSessionCanAcceptFormatDescription")
		reg(&vtDecWait, vt, "VTDecompressionSessionWaitForAsynchronousFrames")
		reg(&vtDecInvalidate, vt, "VTDecompressionSessionInvalidate")
		reg(&vtIsHardwareDecode, vt, "VTIsHardwareDecodeSupported")
		reg(&vtEncoderList, vt, "VTCopyVideoEncoderList")
		reg(&cmSampleData, cm, "CMSampleBufferGetDataBuffer")
		reg(&cmSampleFormat, cm, "CMSampleBufferGetFormatDescription")
		reg(&cmSampleAttach, cm, "CMSampleBufferGetSampleAttachmentsArray")
		reg(&cmSampleCreate, cm, "CMSampleBufferCreateReady")
		reg(&cmBlockLen, cm, "CMBlockBufferGetDataLength")
		reg(&cmBlockCopy, cm, "CMBlockBufferCopyDataBytes")
		reg(&cmBlockCreate, cm, "CMBlockBufferCreateWithMemoryBlock")
		reg(&cmBlockReplace, cm, "CMBlockBufferReplaceDataBytes")
		reg(&cmParamAt, cm, "CMVideoFormatDescriptionGetH264ParameterSetAtIndex")
		reg(&cmFormatCreate, cm, "CMVideoFormatDescriptionCreateFromH264ParameterSets")
		reg(&cvPoolBuffer, cv, "CVPixelBufferPoolCreatePixelBuffer")
		reg(&cvBufferCreate, cv, "CVPixelBufferCreate")
		reg(&cvLock, cv, "CVPixelBufferLockBaseAddress")
		reg(&cvUnlock, cv, "CVPixelBufferUnlockBaseAddress")
		reg(&cvPlaneBase, cv, "CVPixelBufferGetBaseAddressOfPlane")
		reg(&cvPlaneStride, cv, "CVPixelBufferGetBytesPerRowOfPlane")
		reg(&cvPlaneRows, cv, "CVPixelBufferGetHeightOfPlane")
		reg(&cvWidth, cv, "CVPixelBufferGetWidth")
		reg(&cvHeight, cv, "CVPixelBufferGetHeight")
		reg(&cvPlaneCount, cv, "CVPixelBufferGetPlaneCount")
		reg(&cvFormat, cv, "CVPixelBufferGetPixelFormatType")

		cfTrue, cfFalse = vtObject(cf, "kCFBooleanTrue"), vtObject(cf, "kCFBooleanFalse")
		dictKeyCB, dictValCB = vtAddress(cf, "kCFTypeDictionaryKeyCallBacks"), vtAddress(cf, "kCFTypeDictionaryValueCallBacks")
		vtProps[vtPropRealTime] = vtObject(vt, "kVTCompressionPropertyKey_RealTime")
		vtProps[vtPropReorder] = vtObject(vt, "kVTCompressionPropertyKey_AllowFrameReordering")
		vtProps[vtPropBitrate] = vtObject(vt, "kVTCompressionPropertyKey_AverageBitRate")
		vtProps[vtPropFPS] = vtObject(vt, "kVTCompressionPropertyKey_ExpectedFrameRate")
		vtProps[vtPropKeyDuration] = vtObject(vt, "kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration")
		vtProps[vtPropKeyFrames] = vtObject(vt, "kVTCompressionPropertyKey_MaxKeyFrameInterval")
		vtProps[vtPropUsingHardware] = vtObject(vt, "kVTCompressionPropertyKey_UsingHardwareAcceleratedVideoEncoder")
		vtProfileLevelKey = vtObject(vt, "kVTCompressionPropertyKey_ProfileLevel")
		vtLevels[vtLevelConstrainedBaseline] = vtObject(vt, "kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel")
		vtLevels[vtLevelBaseline] = vtObject(vt, "kVTProfileLevel_H264_Baseline_AutoLevel")
		vtLevels[vtLevelMain] = vtObject(vt, "kVTProfileLevel_H264_Main_AutoLevel")
		vtLevels[vtLevelHigh] = vtObject(vt, "kVTProfileLevel_H264_High_AutoLevel")
		vtEnableHWEnc = vtObject(vt, "kVTVideoEncoderSpecification_EnableHardwareAcceleratedVideoEncoder")
		vtEnableHWDec = vtObject(vt, "kVTVideoDecoderSpecification_EnableHardwareAcceleratedVideoDecoder")
		vtUsingHWDec = vtObject(vt, "kVTDecompressionPropertyKey_UsingHardwareAcceleratedVideoDecoder")
		vtForceKey = vtObject(vt, "kVTEncodeFrameOptionKey_ForceKeyFrame")
		vtListCodec = vtObject(vt, "kVTVideoEncoderList_CodecType")
		vtListHW = vtObject(vt, "kVTVideoEncoderList_IsHardwareAccelerated")
		cmNotSync = vtObject(cm, "kCMSampleAttachmentKey_NotSync")
		cvPixelFormatKey = vtObject(cv, "kCVPixelBufferPixelFormatTypeKey")
		cvWidthKey = vtObject(cv, "kCVPixelBufferWidthKey")
		cvHeightKey = vtObject(cv, "kCVPixelBufferHeightKey")
		cvIOSurfaceKey = vtObject(cv, "kCVPixelBufferIOSurfacePropertiesKey")
		vtEncCallback = purego.NewCallback(vtEncoded)
		vtDecCallback = purego.NewCallback(vtDecoded)
		vtInst = &vtReal{}
	})
	if vtErr != nil {
		return nil, vtErr
	}
	return vtInst, nil
}

// vtAddress returns the address of an exported symbol.
func vtAddress(lib uintptr, name string) uintptr {
	a, err := purego.Dlsym(lib, name)
	if err != nil {
		panic(err)
	}
	return a
}

// vtObject reads an exported constant that holds an object reference.
func vtObject(lib uintptr, name string) uintptr {
	a := vtAddress(lib, name)
	return *(*uintptr)(*(*unsafe.Pointer)(unsafe.Pointer(&a)))
}

func vtNumber(v int32) uintptr  { return cfNumberCreate(0, vtNumberSInt32, unsafe.Pointer(&v)) }
func vtFloat(v float64) uintptr { return cfNumberCreate(0, vtNumberFloat64, unsafe.Pointer(&v)) }
func vtDict() uintptr           { return cfDictCreate(0, 0, dictKeyCB, dictValCB) }
func vtBool(v bool) uintptr {
	if v {
		return cfTrue
	}
	return cfFalse
}

// vtReal implements vtOS. Every Create call that returns an object the
// caller owns documents that; objects it makes for one call it releases
// before returning.
type vtReal struct{}

func (vtReal) release(obj uintptr) { cfRelease(obj) }

func (vtReal) encCreate(w, h int, hardware bool, ref uintptr) (uintptr, int32) {
	spec := vtDict()
	cfDictSet(spec, vtEnableHWEnc, vtBool(hardware))
	src, owned := vtPixelAttrs(w, h)
	var sess uintptr
	st := vtCompCreate(0, int32(w), int32(h), vtCodecH264, spec, src, 0, vtEncCallback, ref, &sess)
	for _, o := range owned {
		cfRelease(o)
	}
	cfRelease(spec)
	if st != 0 {
		return 0, st
	}
	return sess, 0
}

// vtPixelAttrs builds the 420v, size and IOSurface attributes; owned lists
// every object to release after use, the dictionary first.
func vtPixelAttrs(w, h int) (dict uintptr, owned []uintptr) {
	d, io := vtDict(), vtDict()
	nf, nw, nh := vtNumber(vtPixelFormat420v), vtNumber(int32(w)), vtNumber(int32(h))
	cfDictSet(d, cvPixelFormatKey, nf)
	cfDictSet(d, cvWidthKey, nw)
	cfDictSet(d, cvHeightKey, nh)
	cfDictSet(d, cvIOSurfaceKey, io)
	return d, []uintptr{d, io, nf, nw, nh}
}

func (vtReal) encSetBool(sess uintptr, p vtProp, v bool) int32 {
	return vtSetProp(sess, vtProps[p], vtBool(v))
}

func (vtReal) encSetInt(sess uintptr, p vtProp, v int32) int32 {
	n := vtNumber(v)
	defer cfRelease(n)
	return vtSetProp(sess, vtProps[p], n)
}

func (vtReal) encSetFloat(sess uintptr, p vtProp, v float64) int32 {
	n := vtFloat(v)
	defer cfRelease(n)
	return vtSetProp(sess, vtProps[p], n)
}

func (vtReal) encSetProfile(sess uintptr, l vtLevel) int32 {
	return vtSetProp(sess, vtProfileLevelKey, vtLevels[l])
}

func (vtReal) encPrepare(sess uintptr) int32 { return vtCompPrepare(sess) }

func (vtReal) encHardware(sess uintptr) bool {
	return vtCopyBool(sess, vtProps[vtPropUsingHardware])
}

func vtCopyBool(sess, key uintptr) bool {
	var v uintptr
	if vtCopyProp(sess, key, 0, &v) != 0 || v == 0 {
		return false
	}
	defer cfRelease(v)
	return cfBoolValue(v)
}

func (vtReal) encPixelBuffer(sess uintptr, pool bool, w, h int) (uintptr, int32) {
	var pb uintptr
	if pool {
		if p := vtCompPool(sess); p != 0 {
			if st := cvPoolBuffer(0, p, &pb); st != 0 {
				return 0, st
			}
			return pb, 0
		}
	}
	attrs, io := vtDict(), vtDict()
	cfDictSet(attrs, cvIOSurfaceKey, io)
	owned := []uintptr{attrs, io}
	st := cvBufferCreate(0, uintptr(w), uintptr(h), vtPixelFormat420v, attrs, &pb)
	for _, o := range owned {
		cfRelease(o)
	}
	if st != 0 {
		return 0, st
	}
	return pb, 0
}

func (vtReal) encFrame(sess, pb uintptr, pts, dur int64, force bool, counter uintptr) (uint32, int32) {
	var props uintptr
	if force {
		props = vtDict()
		defer cfRelease(props)
		cfDictSet(props, vtForceKey, cfTrue)
	}
	var info uint32
	st := vtCompEncode(sess, pb, vtTime(pts), vtTime(dur), props, counter, &info)
	return info, st
}

func (vtReal) encComplete(sess uintptr, pts int64, all bool) int32 {
	if all {
		return vtCompComplete(sess, cmTime{})
	}
	return vtCompComplete(sess, vtTime(pts))
}

func (vtReal) encInvalidate(sess uintptr) { vtCompInvalidate(sess) }

func (vtReal) pbInfo(pb uintptr) (uint32, int, int, int) {
	return cvFormat(pb), int(cvPlaneCount(pb)), int(cvWidth(pb)), int(cvHeight(pb))
}

func (vtReal) pbLock(pb uintptr, readOnly bool) int32 {
	if readOnly {
		return cvLock(pb, vtLockReadOnly)
	}
	return cvLock(pb, 0)
}

func (vtReal) pbUnlock(pb uintptr, readOnly bool) int32 {
	if readOnly {
		return cvUnlock(pb, vtLockReadOnly)
	}
	return cvUnlock(pb, 0)
}

// pbPlane views a locked plane; the slice is valid until the buffer is
// unlocked.
func (vtReal) pbPlane(pb uintptr, plane int) ([]byte, int) {
	base, stride, rows := cvPlaneBase(pb, uintptr(plane)), int(cvPlaneStride(pb, uintptr(plane))), int(cvPlaneRows(pb, uintptr(plane)))
	if base == nil || stride <= 0 || rows <= 0 || stride > 1<<24 || rows > 1<<16 {
		return nil, 0
	}
	return unsafe.Slice((*byte)(base), stride*rows), stride
}

func (vtReal) sbSync(sb uintptr) bool {
	arr := cmSampleAttach(sb, false)
	if arr == 0 || cfArrayCount(arr) == 0 {
		return true
	}
	if v := cfDictGet(cfArrayAt(arr, 0), cmNotSync); v != 0 && cfBoolValue(v) {
		return false
	}
	return true
}

func (vtReal) sbLen(sb uintptr) int {
	bb := cmSampleData(sb)
	if bb == 0 {
		return 0
	}
	n := cmBlockLen(bb)
	if n > 1<<31-1 {
		return 1<<31 - 1
	}
	return int(n)
}

func (vtReal) sbCopy(sb uintptr, dst []byte) int32 {
	bb := cmSampleData(sb)
	if bb == 0 || len(dst) == 0 {
		return vtErrParameter
	}
	return cmBlockCopy(bb, 0, uintptr(len(dst)), unsafe.Pointer(&dst[0]))
}

func (vtReal) sbParams(sb uintptr) ([][]byte, int, int32) {
	fd := cmSampleFormat(sb)
	if fd == 0 {
		return nil, 0, vtErrParameter
	}
	var count uintptr
	var nalLen int32
	if st := cmParamAt(fd, 0, nil, nil, &count, &nalLen); st != 0 {
		return nil, 0, st
	}
	if count == 0 || count > vtMaxParamSets {
		return nil, 0, vtErrParameter
	}
	out := make([][]byte, 0, count)
	for i := uintptr(0); i < count; i++ {
		var p unsafe.Pointer
		var n uintptr
		if st := cmParamAt(fd, i, &p, &n, nil, nil); st != 0 {
			return nil, 0, st
		}
		if p == nil || n == 0 || n > 1<<16 {
			return nil, 0, vtErrParameter
		}
		out = append(out, append([]byte(nil), unsafe.Slice((*byte)(p), n)...))
	}
	return out, int(nalLen), 0
}

// fdCreate copies the parameter sets into C memory, so no Go pointer sits
// inside memory the system reads.
func (vtReal) fdCreate(params [][]byte) (uintptr, int32) {
	n, total := len(params), 16*len(params)
	for _, p := range params {
		total += len(p)
	}
	mem := sysMalloc(uintptr(total))
	if mem == nil {
		return 0, vtErrParameter
	}
	defer sysFree(mem)
	block := unsafe.Slice((*byte)(mem), total)
	ptrs, sizes := unsafe.Slice((*uintptr)(mem), n), unsafe.Slice((*uintptr)(unsafe.Add(mem, 8*n)), n)
	off := 16 * n
	for i, p := range params {
		copy(block[off:], p)
		ptrs[i], sizes[i] = uintptr(unsafe.Pointer(&block[off])), uintptr(len(p))
		off += len(p)
	}
	var fd uintptr
	st := cmFormatCreate(0, uintptr(n), mem, unsafe.Add(mem, 8*n), 4, &fd)
	if st != 0 {
		return 0, st
	}
	return fd, 0
}

func (vtReal) decCreate(fd, ref uintptr, hardware bool) (uintptr, int32) {
	var spec uintptr
	if !hardware {
		spec = vtDict()
		defer cfRelease(spec)
		cfDictSet(spec, vtEnableHWDec, cfFalse)
	}
	attrs, owned := vtDict(), []uintptr(nil)
	pf := vtNumber(vtPixelFormat420v)
	cfDictSet(attrs, cvPixelFormatKey, pf)
	owned = append(owned, attrs, pf)
	rec := &vtDecodeRecord{callback: vtDecCallback, refcon: ref}
	var sess uintptr
	st := vtDecCreate(0, fd, spec, attrs, unsafe.Pointer(rec), &sess)
	runtime.KeepAlive(rec)
	for _, o := range owned {
		cfRelease(o)
	}
	if st != 0 {
		return 0, st
	}
	return sess, 0
}

func (vtReal) decCanAccept(sess, fd uintptr) bool { return vtDecCanAccept(sess, fd) }

func (vtReal) decHardware(sess uintptr) bool { return vtCopyBool(sess, vtUsingHWDec) }

func (vtReal) blockBuffer(data []byte) (uintptr, int32) {
	if len(data) == 0 {
		return 0, vtErrParameter
	}
	n := uintptr(len(data))
	var bb uintptr
	if st := cmBlockCreate(0, 0, n, 0, 0, 0, n, vtAssureMemoryNow, &bb); st != 0 {
		return 0, st
	}
	if st := cmBlockReplace(unsafe.Pointer(&data[0]), bb, 0, n); st != 0 {
		cfRelease(bb)
		return 0, st
	}
	return bb, 0
}

func (vtReal) sampleBuffer(bb, fd uintptr, size int, pts int64) (uintptr, int32) {
	tm := cmTiming{Duration: vtTime(3000), PTS: vtTime(pts)}
	sz := uintptr(size)
	var sb uintptr
	if st := cmSampleCreate(0, bb, fd, 1, 1, &tm, 1, &sz, &sb); st != 0 {
		return 0, st
	}
	return sb, 0
}

func (vtReal) decFrame(sess, sb, counter uintptr) (uint32, int32) {
	var info uint32
	st := vtDecDecode(sess, sb, 0, counter, &info)
	return info, st
}

func (vtReal) decWait(sess uintptr) int32 { return vtDecWait(sess) }

func (vtReal) decInvalidate(sess uintptr) { vtDecInvalidate(sess) }

func (vtReal) encoders() (avc, hardware bool, st int32) {
	var list uintptr
	if st = vtEncoderList(0, &list); st != 0 {
		return false, false, st
	}
	defer cfRelease(list)
	for i, n := 0, cfArrayCount(list); i < n; i++ {
		d := cfArrayAt(list, i)
		c := cfDictGet(d, vtListCodec)
		var codec uint32
		if c == 0 || !cfNumberGetValue(c, vtNumberSInt32, unsafe.Pointer(&codec)) || codec != vtCodecH264 {
			continue
		}
		avc = true
		if v := cfDictGet(d, vtListHW); v != 0 && cfBoolValue(v) {
			hardware = true
		}
	}
	return avc, hardware, 0
}

func (vtReal) hardwareDecode() bool { return vtIsHardwareDecode(vtCodecH264) }
