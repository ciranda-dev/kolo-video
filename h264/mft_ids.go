package h264

import "fmt"

// mftGUID has the memory layout of a Windows GUID.
type mftGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func (g mftGUID) String() string {
	return fmt.Sprintf("%08X-%04X-%04X-%X-%X", g.Data1, g.Data2, g.Data3, g.Data4[:2], g.Data4[2:])
}

// mftHRESULT is a failure HRESULT returned by a COM or Media Foundation call.
type mftHRESULT uint32

func (h mftHRESULT) Error() string { return fmt.Sprintf("HRESULT 0x%08x", uint32(h)) }

// GUIDs from the Windows SDK headers; mft_ids_test.go cites the header and
// line of each.
var (
	clsidH264Decoder = mftGUID{0x62CE7E72, 0x4C71, 0x4D20, [8]byte{0xB1, 0x5D, 0x45, 0x28, 0x31, 0xA8, 0x7D, 0x9D}}
	clsidH264Encoder = mftGUID{0x6CA50344, 0x051A, 0x4DED, [8]byte{0x97, 0x79, 0xA4, 0x33, 0x05, 0x16, 0x5E, 0x35}}
	iidTransform     = mftGUID{0xBF94C121, 0x5B05, 0x4E6F, [8]byte{0x80, 0x00, 0xBA, 0x59, 0x89, 0x61, 0x41, 0x4D}}
	iidCodecAPI      = mftGUID{0x901DB4C7, 0x31CE, 0x41A2, [8]byte{0x85, 0xDC, 0x8F, 0xA0, 0xBF, 0x41, 0xB8, 0xDA}}

	mtMajorType       = mftGUID{0x48EBA18E, 0xF8C9, 0x4687, [8]byte{0xBF, 0x11, 0x0A, 0x74, 0xC9, 0xF9, 0x6A, 0x8F}}
	mtSubtype         = mftGUID{0xF7E34C9A, 0x42E8, 0x4714, [8]byte{0xB7, 0x4B, 0xCB, 0x29, 0xD7, 0x2C, 0x35, 0xE5}}
	mtFrameSize       = mftGUID{0x1652C33D, 0xD6B2, 0x4012, [8]byte{0xB8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xA3, 0x7D}}
	mtFrameRate       = mftGUID{0xC459A2E8, 0x3D2C, 0x4E44, [8]byte{0xB1, 0x32, 0xFE, 0xE5, 0x15, 0x6C, 0x7B, 0xB0}}
	mtAvgBitrate      = mftGUID{0x20332624, 0xFB0D, 0x4D9E, [8]byte{0xBD, 0x0D, 0xCB, 0xF6, 0x78, 0x6C, 0x10, 0x2E}}
	mtInterlaceMode   = mftGUID{0xE2724BB8, 0xE676, 0x4806, [8]byte{0xB4, 0xB2, 0xA8, 0xD6, 0xEF, 0xB4, 0x4C, 0xCD}}
	mtMPEG2Profile    = mftGUID{0xAD76A80B, 0x2D5C, 0x4E0B, [8]byte{0xB3, 0x75, 0x64, 0xE5, 0x20, 0x13, 0x70, 0x36}}
	mtSequenceHeader  = mftGUID{0x3C036DE7, 0x3AD0, 0x4C9E, [8]byte{0x92, 0x16, 0xEE, 0x6D, 0x6A, 0xC2, 0x1C, 0xB3}}
	mtDefaultStride   = mftGUID{0x644B4E48, 0x1E02, 0x4516, [8]byte{0xB0, 0xEB, 0xC0, 0x1C, 0xA9, 0xD4, 0x9A, 0xC6}}
	mtDisplayAperture = mftGUID{0xD7388766, 0x18FE, 0x48C6, [8]byte{0xA1, 0x77, 0xEE, 0x89, 0x48, 0x67, 0xC8, 0xC4}}
	mediaVideo        = mftGUID{0x73646976, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	fmtH264           = mftGUID{0x34363248, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	fmtI420           = mftGUID{0x30323449, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	fmtIYUV           = mftGUID{0x56555949, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	sampleCleanPoint  = mftGUID{0x9CDF01D8, 0xA0F0, 0x43BA, [8]byte{0xB0, 0x77, 0xEA, 0xA0, 0x6C, 0xBD, 0x72, 0x8A}}
	mfLowLatency      = mftGUID{0x9C27891A, 0xED7A, 0x40E1, [8]byte{0x88, 0xE8, 0xB2, 0x27, 0x27, 0xA0, 0x24, 0xEE}}

	apiRateControlMode = mftGUID{0x1C0608E9, 0x370C, 0x4710, [8]byte{0x8A, 0x58, 0xCB, 0x61, 0x81, 0xC4, 0x24, 0x23}}
	apiMeanBitRate     = mftGUID{0xF7222374, 0x2144, 0x4815, [8]byte{0xB5, 0x50, 0xA3, 0x7F, 0x8E, 0x12, 0xEE, 0x52}}
	apiForceKeyFrame   = mftGUID{0x398C1B98, 0x8353, 0x475A, [8]byte{0x9E, 0xF2, 0x8F, 0x26, 0x5D, 0x26, 0x03, 0x45}}
	apiGOPSize         = mftGUID{0x95F31B26, 0x95A4, 0x41AA, [8]byte{0x93, 0x03, 0x24, 0x6A, 0x7F, 0xC6, 0xEE, 0xF1}}
	apiBPictureCount   = mftGUID{0x8D390AAC, 0xDC5C, 0x4200, [8]byte{0xB5, 0x7F, 0x81, 0x4D, 0x04, 0xBA, 0xBA, 0xB2}}
	apiLowLatencyMode  = mfLowLatency
	apiDecAcceleration = mftGUID{0xF7DB8A2F, 0x4F48, 0x4EE8, [8]byte{0xAE, 0x31, 0x8B, 0x6E, 0xBE, 0x55, 0x8A, 0xE2}}
	apiDecMaxWidth     = mftGUID{0x5AE557B8, 0x77AF, 0x41F5, [8]byte{0x9F, 0xA6, 0x4D, 0xB2, 0xFE, 0x1D, 0x4B, 0xCA}}
	apiDecMaxHeight    = mftGUID{0x7262A16A, 0xD2DC, 0x4E75, [8]byte{0x9B, 0xA8, 0x65, 0xC0, 0xC6, 0xD3, 0x2B, 0x13}}
)

// Values from the SDK headers, cited in mft_ids_test.go.
const (
	mftVersion         = 0x20070 // MF_VERSION
	mftStartupNoSocket = 0x1     // MFSTARTUP_NOSOCKET
	clsctxInproc       = 0x1     // CLSCTX_INPROC_SERVER

	msgFlush          = 0x0        // MFT_MESSAGE_COMMAND_FLUSH
	msgDrain          = 0x1        // MFT_MESSAGE_COMMAND_DRAIN
	msgBeginStreaming = 0x10000000 // MFT_MESSAGE_NOTIFY_BEGIN_STREAMING
	msgEndStreaming   = 0x10000001 // MFT_MESSAGE_NOTIFY_END_STREAMING
	msgStartOfStream  = 0x10000003 // MFT_MESSAGE_NOTIFY_START_OF_STREAM

	outProvidesSamples = 0x100 // MFT_OUTPUT_STREAM_PROVIDES_SAMPLES

	profileConstrainedBase = 256 // eAVEncH264VProfile_ConstrainedBase
	profileMain            = 77  // eAVEncH264VProfile_Main
	profileHigh            = 100 // eAVEncH264VProfile_High
	rateControlCBR         = 0   // eAVEncCommonRateControlMode_CBR

	interlaceProgressive = 2 // MFVideoInterlace_Progressive
	interlaceMixed       = 7 // MFVideoInterlace_MixedInterlaceOrProgressive

	variantUI4   = 19     // VT_UI4
	variantBool  = 11     // VT_BOOL
	variantTrue  = 0xFFFF // VARIANT_TRUE (-1) as a 16-bit value
	videoAreaLen = 16     // sizeof(MFVideoArea)

	hrNeedMoreInput mftHRESULT = 0xC00D6D72 // MF_E_TRANSFORM_NEED_MORE_INPUT
	hrStreamChange  mftHRESULT = 0xC00D6D61 // MF_E_TRANSFORM_STREAM_CHANGE
	hrNotAccepting  mftHRESULT = 0xC00D36B5 // MF_E_NOTACCEPTING
	hrInvalidData   mftHRESULT = 0xC00D36CB // MF_E_INVALID_STREAM_DATA
	hrClassNotReg   mftHRESULT = 0x80040154 // REGDB_E_CLASSNOTREG
	hrFalse         mftHRESULT = 0x1        // S_FALSE
	hrChangedMode   mftHRESULT = 0x80010106 // RPC_E_CHANGED_MODE
	hrPointer       mftHRESULT = 0x80004003 // E_POINTER
	hrFail          mftHRESULT = 0x80004005 // E_FAIL
)

// Vtable slots: IUnknown first, then the methods of each interface in the
// order of its C vtable in the SDK headers. mft_ids_test.go cites each.
const (
	slotQueryInterface = 0 // IUnknown
	slotRelease        = 2

	slotAttrGetUINT32   = 7 // IMFAttributes; also IMFMediaType and IMFSample
	slotAttrGetUINT64   = 8
	slotAttrGetGUID     = 10
	slotAttrGetBlobSize = 14
	slotAttrGetBlob     = 15
	slotAttrSetUINT32   = 21
	slotAttrSetUINT64   = 22
	slotAttrSetGUID     = 24

	slotGetOutputStreamInfo = 7 // IMFTransform
	slotGetAttributes       = 8
	slotGetInputAvailable   = 13
	slotGetOutputAvailable  = 14
	slotSetInputType        = 15
	slotSetOutputType       = 16
	slotGetOutputCurrent    = 18
	slotProcessMessage      = 23
	slotProcessInput        = 24
	slotProcessOutput       = 25

	slotCodecSetValue = 9 // ICodecAPI

	slotSampleSetTime     = 36 // IMFSample
	slotSampleSetDuration = 38
	slotSampleContiguous  = 41
	slotSampleAddBuffer   = 42

	slotBufferLock         = 3 // IMFMediaBuffer
	slotBufferUnlock       = 4
	slotBufferSetCurrentLn = 6
)

// mftCom is a COM interface pointer: an object whose first word points to its
// vtable.
type mftCom struct{ vtbl *[64]uintptr }

// mftOutputStreamInfo is MFT_OUTPUT_STREAM_INFO.
type mftOutputStreamInfo struct{ Flags, Size, Alignment uint32 }

// mftOutputDataBuffer is MFT_OUTPUT_DATA_BUFFER.
type mftOutputDataBuffer struct {
	StreamID uint32
	Sample   *mftCom
	Status   uint32
	Events   *mftCom
}

// mftVariant is VARIANT on a 64-bit target.
type mftVariant struct {
	VT  uint16
	_   [3]uint16
	Val uint64
	_   uint64
}
