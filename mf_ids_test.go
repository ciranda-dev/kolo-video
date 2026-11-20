package kolovideo

import "testing"

// The tables below carry the value, and the header and line it was taken
// from, in the mingw-w64 14.0.0 copy of the Windows SDK headers.

func TestMFGUIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		g      *mfGUID
		header string
		want   string
	}{
		{"MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE", &mfDevSourceType, "mfidl.h:7356", "C60AC5FE-252A-478F-A0EF-BC8FA5F7CAD3"},
		{"MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID", &mfDevSourceVidcap, "mfidl.h:7357", "8AC3587A-4AE7-42D8-99E0-0A6013EEF90F"},
		{"MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_SYMBOLIC_LINK", &mfDevSymbolicLink, "mfidl.h:7355", "58F0AAD8-22BF-4F8A-BB3D-D2C4978C6E2F"},
		{"MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME", &mfDevFriendlyName, "mfidl.h:7347", "60D0E559-52F8-4FA2-BBCE-ACDB34A8EC01"},
		{"MF_MT_MAJOR_TYPE", &mfMTMajorType, "mfapi.h:554", "48EBA18E-F8C9-4687-BF11-0A74C9F96A8F"},
		{"MF_MT_SUBTYPE", &mfMTSubtype, "mfapi.h:555", "F7E34C9A-42E8-4714-B74B-CB29D72C35E5"},
		{"MF_MT_FRAME_SIZE", &mfMTFrameSize, "mfapi.h:576", "1652C33D-D6B2-4012-B834-72030849A37D"},
		{"MF_MT_FRAME_RATE", &mfMTFrameRate, "mfapi.h:577", "C459A2E8-3D2C-4E44-B132-FEE5156C7BB0"},
		{"MF_MT_YUV_MATRIX", &mfMTYUVMatrix, "mfapi.h:586", "3E23D450-2C75-4D25-A00E-B91670D12327"},
		{"MF_MT_MINIMUM_DISPLAY_APERTURE", &mfMTDisplayAperture, "mfapi.h:590", "D7388766-18FE-48C6-A177-EE894867C8C4"},
		{"MFMediaType_Video", &mfMediaVideo, "mfapi.h:628", "73646976-0000-0010-8000-00AA00389B71"},
		{"MFVideoFormat_I420", &mfFmtI420, "mfapi.h:372", "30323449-0000-0010-8000-00AA00389B71"},
		{"MFVideoFormat_IYUV", &mfFmtIYUV, "mfapi.h:373", "56555949-0000-0010-8000-00AA00389B71"},
		{"MFVideoFormat_NV12", &mfFmtNV12, "mfapi.h:369", "3231564E-0000-0010-8000-00AA00389B71"},
		{"MFVideoFormat_YUY2", &mfFmtYUY2, "mfapi.h:364", "32595559-0000-0010-8000-00AA00389B71"},
		{"MFVideoFormat_UYVY", &mfFmtUYVY, "mfapi.h:367", "59565955-0000-0010-8000-00AA00389B71"},
		{"MFVideoFormat_RGB32", &mfFmtRGB32, "mfapi.h:347", "00000016-0000-0010-8000-00AA00389B71"},
		{"MFVideoFormat_MJPG", &mfFmtMJPG, "mfapi.h:408", "47504A4D-0000-0010-8000-00AA00389B71"},
		{"MF_SOURCE_READER_ENABLE_ADVANCED_VIDEO_PROCESSING", &mfReaderAdvancedVideo, "mfreadwrite.h:179", "0F81DA2C-B537-4672-A8B2-A681B17307A3"},
		{"IID_IMF2DBuffer2", &iidMF2DBuffer2, "mfobjects.h:1564", "33AE5EA6-4316-436F-8DDD-D73D22F829EC"},
	} {
		if got := tc.g.String(); got != tc.want {
			t.Errorf("%s (%s) is %s, want %s", tc.name, tc.header, got, tc.want)
		}
	}
}

func TestMFSlots(t *testing.T) {
	for _, tc := range []struct {
		iface, method string
		got, want     int
		header        string
	}{
		{"IMFAttributes", "QueryInterface", slotQueryInterface, 0, "mfobjects.h:451"},
		{"IMFAttributes", "Release", slotRelease, 2, "mfobjects.h:459"},
		{"IMFAttributes", "GetUINT32", slotAttrGetUINT32, 7, "mfobjects.h:485"},
		{"IMFAttributes", "GetUINT64", slotAttrGetUINT64, 8, "mfobjects.h:490"},
		{"IMFAttributes", "GetGUID", slotAttrGetGUID, 10, "mfobjects.h:500"},
		{"IMFAttributes", "GetAllocatedString", slotAttrGetAllocatedString, 13, "mfobjects.h:517"},
		{"IMFAttributes", "GetBlob", slotAttrGetBlob, 15, "mfobjects.h:528"},
		{"IMFAttributes", "SetUINT32", slotAttrSetUINT32, 21, "mfobjects.h:559"},
		{"IMFAttributes", "SetUINT64", slotAttrSetUINT64, 22, "mfobjects.h:564"},
		{"IMFAttributes", "SetGUID", slotAttrSetGUID, 24, "mfobjects.h:574"},
		{"IMFAttributes", "SetString", slotAttrSetString, 25, "mfobjects.h:579"},
		{"IMFMediaSource", "CreatePresentationDescriptor", slotSourceCreatePD, 8, "mfidl.h:2847"},
		{"IMFMediaSource", "Shutdown", slotSourceShutdown, 12, "mfidl.h:2863"},
		{"IMFPresentationDescriptor", "GetStreamDescriptorByIndex", slotPDStreamByIndex, 34, "mfidl.h:2574"},
		{"IMFStreamDescriptor", "GetMediaTypeHandler", slotSDTypeHandler, 34, "mfidl.h:2200"},
		{"IMFMediaTypeHandler", "GetMediaTypeCount", slotHandlerCount, 4, "mfidl.h:1910"},
		{"IMFMediaTypeHandler", "GetMediaTypeByIndex", slotHandlerByIndex, 5, "mfidl.h:1914"},
		{"IMFMediaTypeHandler", "SetCurrentMediaType", slotHandlerSetCurrent, 6, "mfidl.h:1919"},
		{"IMFSourceReader", "GetCurrentMediaType", slotReaderGetCurrent, 6, "mfreadwrite.h:518"},
		{"IMFSourceReader", "SetCurrentMediaType", slotReaderSetCurrent, 7, "mfreadwrite.h:523"},
		{"IMFSourceReader", "ReadSample", slotReaderReadSample, 9, "mfreadwrite.h:534"},
		{"IMFSourceReader", "Flush", slotReaderFlush, 10, "mfreadwrite.h:543"},
		{"IMFSample", "ConvertToContiguousBuffer", slotSampleContiguous, 41, "mfobjects.h:1163"},
		{"IMFMediaBuffer", "Lock", slotBufferLock, 3, "mfobjects.h:826"},
		{"IMFMediaBuffer", "Unlock", slotBufferUnlock, 4, "mfobjects.h:832"},
		{"IMF2DBuffer2", "Lock2DSize", slot2DLockSize, 10, "mfobjects.h:1632"},
		{"IMF2DBuffer2", "Unlock2D", slot2DUnlock, 4, "mfobjects.h:1605"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s::%s is slot %d, header %s says %d", tc.iface, tc.method, tc.got, tc.header, tc.want)
		}
	}
}

func TestMFConstants(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want uint64
		header    string
	}{
		{"MF_VERSION", mfVersion, 2<<16 | 0x70, "mfapi.h:25 to 31 (MF_SDK_VERSION 2, MF_API_VERSION 0x0070)"},
		{"MFSTARTUP_NOSOCKET", mfStartupNoSocket, 0x1, "mfapi.h:38"},
		{"MF_SOURCE_READER_FIRST_VIDEO_STREAM", mfFirstVideoStream, 0xfffffffc, "mfreadwrite.h:119"},
		{"MF_SOURCE_READER_ALL_STREAMS", mfAllStreams, 0xfffffffe, "mfreadwrite.h:116"},
		{"MF_SOURCE_READERF_ERROR", mfReadError, 0x1, "mfreadwrite.h:124"},
		{"MF_SOURCE_READERF_ENDOFSTREAM", mfReadEndOfStream, 0x2, "mfreadwrite.h:125"},
		{"MF_SOURCE_READERF_CURRENTMEDIATYPECHANGED", mfReadTypeChanged, 0x20, "mfreadwrite.h:128"},
		{"MF2DBuffer_LockFlags_Read", mf2DLockRead, 0x1, "mfobjects.h:1552"},
		{"MFVideoTransferMatrix_BT709", mfMatrixBT709, 1, "mfobjects.h:2744"},
		{"E_ACCESSDENIED", uint64(hrAccessDenied), 0x80070005, "winerror.h:3424"},
		{"E_INVALIDARG", uint64(hrInvalidArg), 0x80070057, "winerror.h:3418"},
		{"E_POINTER", uint64(hrPointer), 0x80004003, "winerror.h:3420"},
		{"E_UNEXPECTED", uint64(hrUnexpected), 0x8000FFFF, "winerror.h:3415"},
		{"S_FALSE", uint64(hrFalse), 0x1, "winerror.h:3475"},
		{"RPC_E_CHANGED_MODE", uint64(hrChangedMode), 0x80010106, "winerror.h:3940"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s is %#x, header %s says %#x", tc.name, tc.got, tc.header, tc.want)
		}
	}
}
