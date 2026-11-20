package h264

import (
	"testing"
	"unsafe"
)

// The tables below carry the value, and the header and line it was taken
// from, in the mingw-w64 14.0.0 copy of the Windows SDK headers.

func TestMFTGUIDs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		g      mftGUID
		header string
		want   string
	}{
		{"CLSID_CMSH264DecoderMFT", clsidH264Decoder, "wmcodecdsp.h:1758", "62CE7E72-4C71-4D20-B15D-452831A87D9D"},
		{"CLSID_CMSH264EncoderMFT", clsidH264Encoder, "wmcodecdsp.h:1771", "6CA50344-051A-4DED-9779-A43305165E35"},
		{"IID_IMFTransform", iidTransform, "mftransform.h:135", "BF94C121-5B05-4E6F-8000-BA598961414D"},
		{"IID_ICodecAPI", iidCodecAPI, "icodecapi.h:68", "901DB4C7-31CE-41A2-85DC-8FA0BF41B8DA"},
		{"MF_MT_MAJOR_TYPE", mtMajorType, "mfapi.h:554", "48EBA18E-F8C9-4687-BF11-0A74C9F96A8F"},
		{"MF_MT_SUBTYPE", mtSubtype, "mfapi.h:555", "F7E34C9A-42E8-4714-B74B-CB29D72C35E5"},
		{"MF_MT_FRAME_SIZE", mtFrameSize, "mfapi.h:576", "1652C33D-D6B2-4012-B834-72030849A37D"},
		{"MF_MT_FRAME_RATE", mtFrameRate, "mfapi.h:577", "C459A2E8-3D2C-4E44-B132-FEE5156C7BB0"},
		{"MF_MT_INTERLACE_MODE", mtInterlaceMode, "mfapi.h:583", "E2724BB8-E676-4806-B4B2-A8D6EFB44CCD"},
		{"MF_MT_MINIMUM_DISPLAY_APERTURE", mtDisplayAperture, "mfapi.h:590", "D7388766-18FE-48C6-A177-EE894867C8C4"},
		{"MF_MT_AVG_BITRATE", mtAvgBitrate, "mfapi.h:593", "20332624-FB0D-4D9E-BD0D-CBF6786C102E"},
		{"MF_MT_DEFAULT_STRIDE", mtDefaultStride, "mfapi.h:597", "644B4E48-1E02-4516-B0EB-C01CA9D49AC6"},
		{"MF_MT_MPEG2_PROFILE", mtMPEG2Profile, "mfapi.h:600", "AD76A80B-2D5C-4E0B-B375-64E520137036"},
		{"MF_MT_MPEG_SEQUENCE_HEADER", mtSequenceHeader, "mfapi.h:603", "3C036DE7-3AD0-4C9E-9216-EE6D6AC21CB3"},
		{"MFMediaType_Video", mediaVideo, "mfapi.h:628", "73646976-0000-0010-8000-00AA00389B71"},
		{"MFVideoFormat_H264", fmtH264, "mfapi.h:406", "34363248-0000-0010-8000-00AA00389B71"},
		{"MFVideoFormat_I420", fmtI420, "mfapi.h:372", "30323449-0000-0010-8000-00AA00389B71"},
		{"MFVideoFormat_IYUV", fmtIYUV, "mfapi.h:373", "56555949-0000-0010-8000-00AA00389B71"},
		{"MFSampleExtension_CleanPoint", sampleCleanPoint, "mfapi.h:237", "9CDF01D8-A0F0-43BA-B077-EAA06CBD728A"},
		{"MF_LOW_LATENCY", mfLowLatency, "mfapi.h:660", "9C27891A-ED7A-40E1-88E8-B22727A024EE"},
		{"CODECAPI_AVLowLatencyMode", apiLowLatencyMode, "codecapi.h:858", "9C27891A-ED7A-40E1-88E8-B22727A024EE"},
		{"CODECAPI_AVEncCommonRateControlMode", apiRateControlMode, "codecapi.h:621", "1C0608E9-370C-4710-8A58-CB6181C42423"},
		{"CODECAPI_AVEncCommonMeanBitRate", apiMeanBitRate, "codecapi.h:630", "F7222374-2144-4815-B550-A37F8E12EE52"},
		{"CODECAPI_AVEncVideoForceKeyFrame", apiForceKeyFrame, "codecapi.h:683", "398C1B98-8353-475A-9EF2-8F265D260345"},
		{"CODECAPI_AVEncMPVGOPSize", apiGOPSize, "codecapi.h:752", "95F31B26-95A4-41AA-9303-246A7FC6EEF1"},
		{"CODECAPI_AVEncMPVDefaultBPictureCount", apiBPictureCount, "codecapi.h:754", "8D390AAC-DC5C-4200-B57F-814D04BABAB2"},
		{"CODECAPI_AVDecVideoAcceleration_H264", apiDecAcceleration, "codecapi.h:851", "F7DB8A2F-4F48-4EE8-AE31-8B6EBE558AE2"},
		{"CODECAPI_AVDecVideoMaxCodedWidth", apiDecMaxWidth, "codecapi.h:865", "5AE557B8-77AF-41F5-9FA6-4DB2FE1D4BCA"},
		{"CODECAPI_AVDecVideoMaxCodedHeight", apiDecMaxHeight, "codecapi.h:866", "7262A16A-D2DC-4E75-9BA8-65C0C6D32B13"},
	} {
		if got := tc.g.String(); got != tc.want {
			t.Errorf("%s (%s) is %s, want %s", tc.name, tc.header, got, tc.want)
		}
	}
}

func TestMFTSlots(t *testing.T) {
	for _, tc := range []struct {
		iface, method string
		got, want     int
		header        string
	}{
		{"IUnknown", "QueryInterface", slotQueryInterface, 0, "mftransform.h:249"},
		{"IUnknown", "Release", slotRelease, 2, "mftransform.h:257"},
		{"IMFAttributes", "GetUINT32", slotAttrGetUINT32, 7, "mfobjects.h:485"},
		{"IMFAttributes", "GetUINT64", slotAttrGetUINT64, 8, "mfobjects.h:490"},
		{"IMFAttributes", "GetGUID", slotAttrGetGUID, 10, "mfobjects.h:500"},
		{"IMFAttributes", "GetBlobSize", slotAttrGetBlobSize, 14, "mfobjects.h:523"},
		{"IMFAttributes", "GetBlob", slotAttrGetBlob, 15, "mfobjects.h:528"},
		{"IMFAttributes", "SetUINT32", slotAttrSetUINT32, 21, "mfobjects.h:559"},
		{"IMFAttributes", "SetUINT64", slotAttrSetUINT64, 22, "mfobjects.h:564"},
		{"IMFAttributes", "SetGUID", slotAttrSetGUID, 24, "mfobjects.h:574"},
		{"IMFTransform", "GetOutputStreamInfo", slotGetOutputStreamInfo, 7, "mftransform.h:285"},
		{"IMFTransform", "GetAttributes", slotGetAttributes, 8, "mftransform.h:290"},
		{"IMFTransform", "GetInputAvailableType", slotGetInputAvailable, 13, "mftransform.h:313"},
		{"IMFTransform", "GetOutputAvailableType", slotGetOutputAvailable, 14, "mftransform.h:319"},
		{"IMFTransform", "SetInputType", slotSetInputType, 15, "mftransform.h:325"},
		{"IMFTransform", "SetOutputType", slotSetOutputType, 16, "mftransform.h:331"},
		{"IMFTransform", "GetOutputCurrentType", slotGetOutputCurrent, 18, "mftransform.h:342"},
		{"IMFTransform", "ProcessMessage", slotProcessMessage, 23, "mftransform.h:366"},
		{"IMFTransform", "ProcessInput", slotProcessInput, 24, "mftransform.h:371"},
		{"IMFTransform", "ProcessOutput", slotProcessOutput, 25, "mftransform.h:377"},
		{"ICodecAPI", "SetValue", slotCodecSetValue, 9, "icodecapi.h:185"},
		{"IMFSample", "SetSampleTime", slotSampleSetTime, 36, "mfobjects.h:1142"},
		{"IMFSample", "SetSampleDuration", slotSampleSetDuration, 38, "mfobjects.h:1150"},
		{"IMFSample", "ConvertToContiguousBuffer", slotSampleContiguous, 41, "mfobjects.h:1163"},
		{"IMFSample", "AddBuffer", slotSampleAddBuffer, 42, "mfobjects.h:1167"},
		{"IMFMediaBuffer", "Lock", slotBufferLock, 3, "mfobjects.h:826"},
		{"IMFMediaBuffer", "Unlock", slotBufferUnlock, 4, "mfobjects.h:832"},
		{"IMFMediaBuffer", "SetCurrentLength", slotBufferSetCurrentLn, 6, "mfobjects.h:839"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s::%s is slot %d, header %s says %d", tc.iface, tc.method, tc.got, tc.header, tc.want)
		}
	}
}

func TestMFTConstants(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want uint64
		header    string
	}{
		{"MF_VERSION", mftVersion, 2<<16 | 0x70, "mfapi.h:25 to 31 (MF_SDK_VERSION 2, MF_API_VERSION 0x0070)"},
		{"MFSTARTUP_NOSOCKET", mftStartupNoSocket, 0x1, "mfapi.h:38"},
		{"CLSCTX_INPROC_SERVER", clsctxInproc, 0x1, "wtypesbase.h:270"},
		{"MFT_MESSAGE_COMMAND_FLUSH", msgFlush, 0x0, "mftransform.h:40"},
		{"MFT_MESSAGE_COMMAND_DRAIN", msgDrain, 0x1, "mftransform.h:41"},
		{"MFT_MESSAGE_NOTIFY_BEGIN_STREAMING", msgBeginStreaming, 0x10000000, "mftransform.h:45"},
		{"MFT_MESSAGE_NOTIFY_END_STREAMING", msgEndStreaming, 0x10000001, "mftransform.h:46"},
		{"MFT_MESSAGE_NOTIFY_START_OF_STREAM", msgStartOfStream, 0x10000003, "mftransform.h:48"},
		{"MFT_OUTPUT_STREAM_PROVIDES_SAMPLES", outProvidesSamples, 0x100, "mftransform.h:81"},
		{"eAVEncH264VProfile_Main", profileMain, 77, "codecapi.h:271"},
		{"eAVEncH264VProfile_High", profileHigh, 100, "codecapi.h:272"},
		{"eAVEncH264VProfile_ConstrainedBase", profileConstrainedBase, 256, "codecapi.h:281"},
		{"eAVEncCommonRateControlMode_CBR", rateControlCBR, 0, "codecapi.h:38"},
		{"MFVideoInterlace_Progressive", interlaceProgressive, 2, "mfobjects.h:2678"},
		{"MFVideoInterlace_MixedInterlaceOrProgressive", interlaceMixed, 7, "mfobjects.h:2683"},
		{"VT_BOOL", variantBool, 11, "wtypes.h:525"},
		{"VT_UI4", variantUI4, 19, "wtypes.h:532"},
		{"VARIANT_TRUE (-1 as 16 bits)", variantTrue, 0xffff, "wtypes.h:500"},
		{"MF_E_TRANSFORM_NEED_MORE_INPUT", uint64(hrNeedMoreInput), 0xc00d6d72, "mferror.h:254"},
		{"MF_E_TRANSFORM_STREAM_CHANGE", uint64(hrStreamChange), 0xc00d6d61, "mferror.h:238"},
		{"MF_E_NOTACCEPTING", uint64(hrNotAccepting), 0xc00d36b5, "mferror.h:26"},
		{"MF_E_INVALID_STREAM_DATA", uint64(hrInvalidData), 0xc00d36cb, "mferror.h:42"},
		{"REGDB_E_CLASSNOTREG", uint64(hrClassNotReg), 0x80040154, "winerror.h:3544"},
		{"S_FALSE", uint64(hrFalse), 0x1, "winerror.h:3475"},
		{"RPC_E_CHANGED_MODE", uint64(hrChangedMode), 0x80010106, "winerror.h:3940"},
		{"E_POINTER", uint64(hrPointer), 0x80004003, "winerror.h:3420"},
		{"E_FAIL", uint64(hrFail), 0x80004005, "winerror.h:3423"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s is %#x, header %s says %#x", tc.name, tc.got, tc.header, tc.want)
		}
	}
}

// The structs are read and written by the MFTs, so their layout is the C one
// on a 64-bit target: amd64 and arm64 share it.
func TestMFTStructLayouts(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the engine is built for 64-bit targets only")
	}
	var info mftOutputStreamInfo
	var ob mftOutputDataBuffer
	var v mftVariant
	var com mftCom
	for _, tc := range []struct {
		name      string
		got, want uintptr
		header    string
	}{
		{"MFT_OUTPUT_STREAM_INFO size", unsafe.Sizeof(info), 12, "mftransform.h:117 to 121"},
		{"MFT_OUTPUT_STREAM_INFO.cbSize", unsafe.Offsetof(info.Size), 4, "mftransform.h:119"},
		{"MFT_OUTPUT_DATA_BUFFER size", unsafe.Sizeof(ob), 32, "mftransform.h:122 to 127"},
		{"MFT_OUTPUT_DATA_BUFFER.pSample", unsafe.Offsetof(ob.Sample), 8, "mftransform.h:124"},
		{"MFT_OUTPUT_DATA_BUFFER.dwStatus", unsafe.Offsetof(ob.Status), 16, "mftransform.h:125"},
		{"MFT_OUTPUT_DATA_BUFFER.pEvents", unsafe.Offsetof(ob.Events), 24, "mftransform.h:126"},
		{"COM object vtable pointer offset", unsafe.Offsetof(com.vtbl), 0, "mftransform.h:387 to 389 (lpVtbl is the only member)"},
		{"VARIANT size", unsafe.Sizeof(v), 24, "oaidl.h:497 to 509 (vt, three reserved words, the 16-byte union with llVal at 505)"},
		{"VARIANT value offset", unsafe.Offsetof(v.Val), 8, "oaidl.h:500 to 505"},
		{"MFVideoArea size", videoAreaLen, 16, "mfobjects.h:2811 to 2819 (two MFOffset of 4 bytes, one SIZE of 8)"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s is %d, header %s says %d", tc.name, tc.got, tc.header, tc.want)
		}
	}
}
