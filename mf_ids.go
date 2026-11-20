package kolovideo

import "fmt"

// mfGUID has the memory layout of a Windows GUID.
type mfGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func (g mfGUID) String() string {
	return fmt.Sprintf("%08X-%04X-%04X-%X-%X", g.Data1, g.Data2, g.Data3, g.Data4[:2], g.Data4[2:])
}

// mfHRESULT is a failure HRESULT returned by a Media Foundation or COM call.
type mfHRESULT uint32

func (h mfHRESULT) Error() string { return fmt.Sprintf("HRESULT 0x%08x", uint32(h)) }

// GUIDs from the Windows SDK headers; mf_ids_test.go cites the header and
// line of each.
var (
	mfDevSourceType       = mfGUID{0xC60AC5FE, 0x252A, 0x478F, [8]byte{0xA0, 0xEF, 0xBC, 0x8F, 0xA5, 0xF7, 0xCA, 0xD3}}
	mfDevSourceVidcap     = mfGUID{0x8AC3587A, 0x4AE7, 0x42D8, [8]byte{0x99, 0xE0, 0x0A, 0x60, 0x13, 0xEE, 0xF9, 0x0F}}
	mfDevSymbolicLink     = mfGUID{0x58F0AAD8, 0x22BF, 0x4F8A, [8]byte{0xBB, 0x3D, 0xD2, 0xC4, 0x97, 0x8C, 0x6E, 0x2F}}
	mfDevFriendlyName     = mfGUID{0x60D0E559, 0x52F8, 0x4FA2, [8]byte{0xBB, 0xCE, 0xAC, 0xDB, 0x34, 0xA8, 0xEC, 0x01}}
	mfMTMajorType         = mfGUID{0x48EBA18E, 0xF8C9, 0x4687, [8]byte{0xBF, 0x11, 0x0A, 0x74, 0xC9, 0xF9, 0x6A, 0x8F}}
	mfMTSubtype           = mfGUID{0xF7E34C9A, 0x42E8, 0x4714, [8]byte{0xB7, 0x4B, 0xCB, 0x29, 0xD7, 0x2C, 0x35, 0xE5}}
	mfMTFrameSize         = mfGUID{0x1652C33D, 0xD6B2, 0x4012, [8]byte{0xB8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xA3, 0x7D}}
	mfMTFrameRate         = mfGUID{0xC459A2E8, 0x3D2C, 0x4E44, [8]byte{0xB1, 0x32, 0xFE, 0xE5, 0x15, 0x6C, 0x7B, 0xB0}}
	mfMTYUVMatrix         = mfGUID{0x3E23D450, 0x2C75, 0x4D25, [8]byte{0xA0, 0x0E, 0xB9, 0x16, 0x70, 0xD1, 0x23, 0x27}}
	mfMTDisplayAperture   = mfGUID{0xD7388766, 0x18FE, 0x48C6, [8]byte{0xA1, 0x77, 0xEE, 0x89, 0x48, 0x67, 0xC8, 0xC4}}
	mfMediaVideo          = mfGUID{0x73646976, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	mfFmtI420             = mfGUID{0x30323449, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	mfFmtIYUV             = mfGUID{0x56555949, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	mfFmtNV12             = mfGUID{0x3231564E, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	mfFmtYUY2             = mfGUID{0x32595559, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	mfFmtUYVY             = mfGUID{0x59565955, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	mfFmtRGB32            = mfGUID{0x00000016, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	mfFmtMJPG             = mfGUID{0x47504A4D, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	mfReaderAdvancedVideo = mfGUID{0x0F81DA2C, 0xB537, 0x4672, [8]byte{0xA8, 0xB2, 0xA6, 0x81, 0xB1, 0x73, 0x07, 0xA3}}
	iidMF2DBuffer2        = mfGUID{0x33AE5EA6, 0x4316, 0x436F, [8]byte{0x8D, 0xDD, 0xD7, 0x3D, 0x22, 0xF8, 0x29, 0xEC}}
)

// Values from the SDK headers, cited in mf_ids_test.go.
const (
	mfVersion          = 0x20070    // MF_VERSION
	mfStartupNoSocket  = 0x1        // MFSTARTUP_NOSOCKET
	mfFirstVideoStream = 0xFFFFFFFC // MF_SOURCE_READER_FIRST_VIDEO_STREAM
	mfAllStreams       = 0xFFFFFFFE // MF_SOURCE_READER_ALL_STREAMS
	mfReadError        = 0x1        // MF_SOURCE_READERF_ERROR
	mfReadEndOfStream  = 0x2        // MF_SOURCE_READERF_ENDOFSTREAM
	mfReadTypeChanged  = 0x20       // MF_SOURCE_READERF_CURRENTMEDIATYPECHANGED
	mf2DLockRead       = 0x1        // MF2DBuffer_LockFlags_Read
	mfMatrixBT709      = 1          // MFVideoTransferMatrix_BT709

	hrAccessDenied mfHRESULT = 0x80070005 // E_ACCESSDENIED
	hrInvalidArg   mfHRESULT = 0x80070057 // E_INVALIDARG
	hrPointer      mfHRESULT = 0x80004003 // E_POINTER
	hrUnexpected   mfHRESULT = 0x8000FFFF // E_UNEXPECTED
	hrFalse        mfHRESULT = 0x1        // S_FALSE
	hrChangedMode  mfHRESULT = 0x80010106 // RPC_E_CHANGED_MODE
)

// Vtable slots: IUnknown first, then the methods of each interface in the
// order of its C vtable in the SDK headers. mf_ids_test.go cites each.
const (
	slotQueryInterface = 0 // IUnknown
	slotRelease        = 2

	slotAttrGetUINT32          = 7 // IMFAttributes; also IMFActivate, IMFMediaType
	slotAttrGetUINT64          = 8
	slotAttrGetGUID            = 10
	slotAttrGetAllocatedString = 13
	slotAttrGetBlob            = 15
	slotAttrSetUINT32          = 21
	slotAttrSetUINT64          = 22
	slotAttrSetGUID            = 24
	slotAttrSetString          = 25

	slotSourceCreatePD = 8 // IMFMediaSource
	slotSourceShutdown = 12

	slotPDStreamByIndex = 34 // IMFPresentationDescriptor
	slotSDTypeHandler   = 34 // IMFStreamDescriptor

	slotHandlerCount      = 4 // IMFMediaTypeHandler
	slotHandlerByIndex    = 5
	slotHandlerSetCurrent = 6

	slotReaderGetCurrent = 6 // IMFSourceReader
	slotReaderSetCurrent = 7
	slotReaderReadSample = 9
	slotReaderFlush      = 10

	slotSampleContiguous = 41 // IMFSample

	slotBufferLock   = 3 // IMFMediaBuffer
	slotBufferUnlock = 4

	slot2DLockSize = 10 // IMF2DBuffer2
	slot2DUnlock   = 4
)

// mfPixelFormat maps the subtype of a native media type to a pixel format. The
// camera drivers use I420 and IYUV for the same layout.
func mfPixelFormat(sub mfGUID) PixelFormat {
	switch sub {
	case mfFmtI420, mfFmtIYUV:
		return FormatI420
	case mfFmtNV12:
		return FormatNV12
	case mfFmtYUY2:
		return FormatYUY2
	case mfFmtUYVY:
		return FormatUYVY
	case mfFmtRGB32:
		return FormatBGRA
	case mfFmtMJPG:
		return FormatMJPEG
	}
	return FormatUnknown
}
