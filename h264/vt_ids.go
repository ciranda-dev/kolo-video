package h264

// Values from the macOS SDK headers. vt_ids_test.go checks each against the
// header named in vtConstants and vtSymbols when the SDK is installed.

// OSStatus codes (VTErrors.h).
const (
	vtErrParameter       = -12902 // kVTParameterErr
	vtErrInvalidSession  = -12903 // kVTInvalidSessionErr
	vtErrCouldNotFindDec = -12906 // kVTCouldNotFindVideoDecoderErr
	vtErrCouldNotFindEnc = -12908 // kVTCouldNotFindVideoEncoderErr
	vtErrBadData         = -12909 // kVTVideoDecoderBadDataErr
	vtErrDecoderMalfunc  = -12911 // kVTVideoDecoderMalfunctionErr
	vtErrDecoderNotNow   = -12913 // kVTVideoDecoderNotAvailableNowErr
	vtErrEncoderNotNow   = -12915 // kVTVideoEncoderNotAvailableNowErr
)

const (
	vtCodecH264       = 0x61766331 // 'avc1', kCMVideoCodecType_H264
	vtPixelFormat420v = 0x34323076 // '420v', kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange

	vtInfoFrameDropped = 1 << 1 // kVTEncodeInfo_FrameDropped, kVTDecodeInfo_FrameDropped
	vtTimeValid        = 1      // kCMTimeFlags_Valid
	vtAssureMemoryNow  = 1      // kCMBlockBufferAssureMemoryNowFlag
	vtLockReadOnly     = 1      // kCVPixelBufferLock_ReadOnly
	vtNumberSInt32     = 3      // kCFNumberSInt32Type
	vtNumberFloat64    = 6      // kCFNumberFloat64Type

	// vtTimescale is the tick rate of the pts and dur values the engine interface passes.
	vtTimescale = 90000
)

type vtConstant struct {
	name   string
	value  int64
	header string
}

var vtConstants = []vtConstant{
	{"kVTParameterErr", vtErrParameter, "VTErrors.h"},
	{"kVTInvalidSessionErr", vtErrInvalidSession, "VTErrors.h"},
	{"kVTCouldNotFindVideoDecoderErr", vtErrCouldNotFindDec, "VTErrors.h"},
	{"kVTCouldNotFindVideoEncoderErr", vtErrCouldNotFindEnc, "VTErrors.h"},
	{"kVTVideoDecoderBadDataErr", vtErrBadData, "VTErrors.h"},
	{"kVTVideoDecoderMalfunctionErr", vtErrDecoderMalfunc, "VTErrors.h"},
	{"kVTVideoDecoderNotAvailableNowErr", vtErrDecoderNotNow, "VTErrors.h"},
	{"kVTVideoEncoderNotAvailableNowErr", vtErrEncoderNotNow, "VTErrors.h"},
	{"kVTEncodeInfo_FrameDropped", vtInfoFrameDropped, "VTErrors.h"},
	{"kVTDecodeInfo_FrameDropped", vtInfoFrameDropped, "VTErrors.h"},
	{"kCMVideoCodecType_H264", vtCodecH264, "CMFormatDescription.h"},
	{"kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange", vtPixelFormat420v, "CVPixelBuffer.h"},
	{"kCMTimeFlags_Valid", vtTimeValid, "CMTime.h"},
	{"kCMBlockBufferAssureMemoryNowFlag", vtAssureMemoryNow, "CMBlockBuffer.h"},
	{"kCVPixelBufferLock_ReadOnly", vtLockReadOnly, "CVPixelBuffer.h"},
	{"kCFNumberSInt32Type", vtNumberSInt32, "CFNumber.h"},
	{"kCFNumberFloat64Type", vtNumberFloat64, "CFNumber.h"},
}

// vtProp names a session property the engine sets or reads.
type vtProp int

const (
	vtPropRealTime vtProp = iota
	vtPropReorder
	vtPropBitrate
	vtPropFPS
	vtPropKeyDuration
	vtPropKeyFrames
	vtPropUsingHardware
	vtPropCount
)

// vtLevel names a profile and level constant of ProfileLevel.
type vtLevel int

const (
	vtLevelConstrainedBaseline vtLevel = iota
	vtLevelBaseline
	vtLevelMain
	vtLevelHigh
	vtLevelCount
)

type vtSymbol struct {
	name   string
	header string
}

// vtSymbols lists every exported symbol the real layer reads, by header.
var vtSymbols = []vtSymbol{
	{"kVTCompressionPropertyKey_RealTime", "VTCompressionProperties.h"},
	{"kVTCompressionPropertyKey_AllowFrameReordering", "VTCompressionProperties.h"},
	{"kVTCompressionPropertyKey_AverageBitRate", "VTCompressionProperties.h"},
	{"kVTCompressionPropertyKey_ExpectedFrameRate", "VTCompressionProperties.h"},
	{"kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration", "VTCompressionProperties.h"},
	{"kVTCompressionPropertyKey_MaxKeyFrameInterval", "VTCompressionProperties.h"},
	{"kVTCompressionPropertyKey_UsingHardwareAcceleratedVideoEncoder", "VTCompressionProperties.h"},
	{"kVTCompressionPropertyKey_ProfileLevel", "VTCompressionProperties.h"},
	{"kVTProfileLevel_H264_ConstrainedBaseline_AutoLevel", "VTCompressionProperties.h"},
	{"kVTProfileLevel_H264_Baseline_AutoLevel", "VTCompressionProperties.h"},
	{"kVTProfileLevel_H264_Main_AutoLevel", "VTCompressionProperties.h"},
	{"kVTProfileLevel_H264_High_AutoLevel", "VTCompressionProperties.h"},
	{"kVTVideoEncoderSpecification_EnableHardwareAcceleratedVideoEncoder", "VTCompressionProperties.h"},
	{"kVTVideoDecoderSpecification_EnableHardwareAcceleratedVideoDecoder", "VTDecompressionProperties.h"},
	{"kVTDecompressionPropertyKey_UsingHardwareAcceleratedVideoDecoder", "VTDecompressionProperties.h"},
	{"kVTEncodeFrameOptionKey_ForceKeyFrame", "VTCompressionProperties.h"},
	{"kVTVideoEncoderList_CodecType", "VTVideoEncoderList.h"},
	{"kVTVideoEncoderList_IsHardwareAccelerated", "VTVideoEncoderList.h"},
	{"kCMSampleAttachmentKey_NotSync", "CMSampleBuffer.h"},
	{"kCVPixelBufferPixelFormatTypeKey", "CVPixelBuffer.h"},
	{"kCVPixelBufferWidthKey", "CVPixelBuffer.h"},
	{"kCVPixelBufferHeightKey", "CVPixelBuffer.h"},
	{"kCVPixelBufferIOSurfacePropertiesKey", "CVPixelBuffer.h"},
	{"kCFBooleanTrue", "CFNumber.h"},
	{"kCFBooleanFalse", "CFNumber.h"},
	{"kCFTypeDictionaryKeyCallBacks", "CFDictionary.h"},
	{"kCFTypeDictionaryValueCallBacks", "CFDictionary.h"},
}

// vtFunctions lists every function the real layer binds, by header.
var vtFunctions = []vtSymbol{
	{"VTCompressionSessionCreate", "VTCompressionSession.h"},
	{"VTSessionSetProperty", "VTSession.h"},
	{"VTSessionCopyProperty", "VTSession.h"},
	{"VTCompressionSessionPrepareToEncodeFrames", "VTCompressionSession.h"},
	{"VTCompressionSessionGetPixelBufferPool", "VTCompressionSession.h"},
	{"VTCompressionSessionEncodeFrame", "VTCompressionSession.h"},
	{"VTCompressionSessionCompleteFrames", "VTCompressionSession.h"},
	{"VTCompressionSessionInvalidate", "VTCompressionSession.h"},
	{"VTDecompressionSessionCreate", "VTDecompressionSession.h"},
	{"VTDecompressionSessionDecodeFrame", "VTDecompressionSession.h"},
	{"VTDecompressionSessionCanAcceptFormatDescription", "VTDecompressionSession.h"},
	{"VTDecompressionSessionWaitForAsynchronousFrames", "VTDecompressionSession.h"},
	{"VTDecompressionSessionInvalidate", "VTDecompressionSession.h"},
	{"VTIsHardwareDecodeSupported", "VTDecompressionSession.h"},
	{"VTCopyVideoEncoderList", "VTVideoEncoderList.h"},
	{"CMSampleBufferGetDataBuffer", "CMSampleBuffer.h"},
	{"CMSampleBufferGetFormatDescription", "CMSampleBuffer.h"},
	{"CMSampleBufferGetSampleAttachmentsArray", "CMSampleBuffer.h"},
	{"CMSampleBufferCreateReady", "CMSampleBuffer.h"},
	{"CMBlockBufferGetDataLength", "CMBlockBuffer.h"},
	{"CMBlockBufferCopyDataBytes", "CMBlockBuffer.h"},
	{"CMBlockBufferCreateWithMemoryBlock", "CMBlockBuffer.h"},
	{"CMBlockBufferReplaceDataBytes", "CMBlockBuffer.h"},
	{"CMVideoFormatDescriptionGetH264ParameterSetAtIndex", "CMFormatDescription.h"},
	{"CMVideoFormatDescriptionCreateFromH264ParameterSets", "CMFormatDescription.h"},
	{"CVPixelBufferPoolCreatePixelBuffer", "CVPixelBufferPool.h"},
	{"CVPixelBufferCreate", "CVPixelBuffer.h"},
	{"CVPixelBufferLockBaseAddress", "CVPixelBuffer.h"},
	{"CVPixelBufferUnlockBaseAddress", "CVPixelBuffer.h"},
	{"CVPixelBufferGetBaseAddressOfPlane", "CVPixelBuffer.h"},
	{"CVPixelBufferGetBytesPerRowOfPlane", "CVPixelBuffer.h"},
	{"CVPixelBufferGetHeightOfPlane", "CVPixelBuffer.h"},
	{"CVPixelBufferGetWidth", "CVPixelBuffer.h"},
	{"CVPixelBufferGetHeight", "CVPixelBuffer.h"},
	{"CVPixelBufferGetPlaneCount", "CVPixelBuffer.h"},
	{"CVPixelBufferGetPixelFormatType", "CVPixelBuffer.h"},
	{"CFRelease", "CFBase.h"},
	{"CFNumberCreate", "CFNumber.h"},
	{"CFDictionaryCreateMutable", "CFDictionary.h"},
	{"CFDictionarySetValue", "CFDictionary.h"},
	{"CFDictionaryGetValue", "CFDictionary.h"},
	{"CFArrayGetCount", "CFArray.h"},
	{"CFArrayGetValueAtIndex", "CFArray.h"},
	{"CFBooleanGetValue", "CFNumber.h"},
	{"CFNumberGetValue", "CFNumber.h"},
}

// cmTime is CMTime, 24 bytes (CMTime.h).
type cmTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

// cmTiming is CMSampleTimingInfo, 72 bytes (CMSampleBuffer.h).
type cmTiming struct{ Duration, PTS, DTS cmTime }

// vtDecodeRecord is VTDecompressionOutputCallbackRecord (VTDecompressionSession.h).
type vtDecodeRecord struct{ callback, refcon uintptr }

func vtTime(ticks int64) cmTime {
	return cmTime{Value: ticks, Timescale: vtTimescale, Flags: vtTimeValid}
}
