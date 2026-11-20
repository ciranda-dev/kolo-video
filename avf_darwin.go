//go:build darwin

package kolovideo

import (
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

// The real AVFoundation layer, called through purego and the Objective-C
// runtime. purego has no autorelease pools, so every use of Objective-C runs
// in avfPool, on one locked OS thread. The delegate class is registered once
// per process and finds its capture by an ivar; purego never frees callbacks
// and allows 2000 per process.

var (
	cfRelease func(p uintptr)
	cfEqual   func(a, b uintptr) bool
	cfTypeID  func(p uintptr) uintptr

	poolPush func() uintptr
	poolPop  func(p uintptr)

	dispatchQueueCreate func(label *byte, attr uintptr) uintptr
	dispatchSyncF       func(q, ctx, fn uintptr)
	dispatchRelease     func(obj uintptr)
	uptimeNanos         func(clock int32) uint64

	avMediaTypeVideo      objc.ID
	avTypeBuiltInWide     objc.ID
	avTypeExternal        objc.ID
	avDisconnectedNote    objc.ID
	avRuntimeErrorNote    objc.ID
	avSessionErrorKey     objc.ID
	cvPixelFormatTypeKey  objc.ID
	avDelegateClass       objc.Class
	avDelegateIdent       objc.Ivar
	avSyncDone            uintptr
	avSampleBufferHandler = objc.RegisterName("captureOutput:didOutputSampleBuffer:fromConnection:")
)

var (
	avfOnce sync.Once
	avfInst *avfReal
	avfErr  error
)

// loadAVF loads the frameworks and registers the delegate class on first use.
func loadAVF() (*avfReal, error) {
	avfOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				avfErr = fmt.Errorf("%w: avfoundation: %v", ErrUnsupported, r)
			}
		}()
		open := func(path string) uintptr {
			h, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
			if err != nil {
				panic(err)
			}
			return h
		}
		frameworks := "/System/Library/Frameworks/"
		av := open(frameworks + "AVFoundation.framework/AVFoundation")
		cm := open(frameworks + "CoreMedia.framework/CoreMedia")
		cv := open(frameworks + "CoreVideo.framework/CoreVideo")
		cf := open(frameworks + "CoreFoundation.framework/CoreFoundation")
		sys := open("/usr/lib/libSystem.B.dylib")
		ob := open("/usr/lib/libobjc.A.dylib")
		purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
		purego.RegisterLibFunc(&cfEqual, cf, "CFEqual")
		purego.RegisterLibFunc(&cfTypeID, cf, "CFGetTypeID")
		purego.RegisterLibFunc(&poolPush, ob, "objc_autoreleasePoolPush")
		purego.RegisterLibFunc(&poolPop, ob, "objc_autoreleasePoolPop")
		purego.RegisterLibFunc(&dispatchQueueCreate, sys, "dispatch_queue_create")
		purego.RegisterLibFunc(&dispatchSyncF, sys, "dispatch_sync_f")
		purego.RegisterLibFunc(&dispatchRelease, sys, "dispatch_release")
		purego.RegisterLibFunc(&uptimeNanos, sys, "clock_gettime_nsec_np")
		bindSampleFuncs(cm, cv)

		avMediaTypeVideo = avfConstant(av, "AVMediaTypeVideo")
		avTypeBuiltInWide = avfConstant(av, "AVCaptureDeviceTypeBuiltInWideAngleCamera")
		// AVCaptureDeviceTypeExternal replaced the Unknown name in macOS 14.
		if _, err := purego.Dlsym(av, "AVCaptureDeviceTypeExternal"); err == nil {
			avTypeExternal = avfConstant(av, "AVCaptureDeviceTypeExternal")
		} else {
			avTypeExternal = avfConstant(av, "AVCaptureDeviceTypeExternalUnknown")
		}
		avDisconnectedNote = avfConstant(av, "AVCaptureDeviceWasDisconnectedNotification")
		avRuntimeErrorNote = avfConstant(av, "AVCaptureSessionRuntimeErrorNotification")
		avSessionErrorKey = avfConstant(av, "AVCaptureSessionErrorKey")
		cvPixelFormatTypeKey = avfConstant(cv, "kCVPixelBufferPixelFormatTypeKey")
		bindSampleConstants(cv)
		avSyncDone = purego.NewCallback(func(uintptr) {})

		proto := objc.GetProtocol("AVCaptureVideoDataOutputSampleBufferDelegate")
		var protos []*objc.Protocol
		if proto != nil {
			protos = append(protos, proto)
		}
		cls, err := objc.RegisterClass("KoloVideoDelegate", objc.GetClass("NSObject"), protos,
			[]objc.FieldDef{{Name: "ident", Type: reflect.TypeFor[uintptr](), Attribute: objc.ReadOnly}},
			[]objc.MethodDef{{Cmd: avSampleBufferHandler, Fn: avfDidOutputSampleBuffer}})
		if err != nil {
			panic(err)
		}
		avDelegateClass = cls
		avDelegateIdent = cls.InstanceVariable("ident")
		avfInst = &avfReal{}
	})
	return avfInst, avfErr
}

// avfConstant reads an exported object constant such as a notification name.
func avfConstant(lib uintptr, name string) objc.ID {
	addr, err := purego.Dlsym(lib, name)
	if err != nil {
		panic(err)
	}
	return *(*objc.ID)(*(*unsafe.Pointer)(unsafe.Pointer(&addr)))
}

// avfPool runs fn inside an autorelease pool. The pool belongs to the thread
// that pushed it, so the goroutine stays on that thread until it is popped.
func avfPool(fn func()) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	p := poolPush()
	defer poolPop(p)
	fn()
}

// avfDidOutputSampleBuffer is the delegate method. AVFoundation calls it on
// the session queue's thread.
func avfDidOutputSampleBuffer(self objc.ID, _ objc.SEL, _ objc.ID, sb uintptr, _ objc.ID) {
	avfPool(func() {
		if t := darwinCallbacks.get(uintptr(self.GetIvar(avDelegateIdent))); t != nil {
			t.sample(sb)
		}
	})
}

var sel = objc.RegisterName

func avfClass(name string) objc.ID { return objc.ID(objc.GetClass(name)) }

func avfCString(s string) []byte { return append([]byte(s), 0) }

func avfNSString(s string) objc.ID {
	b := avfCString(s)
	return avfClass("NSString").Send(sel("stringWithUTF8String:"), unsafe.Pointer(&b[0]))
}

func avfGoString(s objc.ID) string {
	if s == 0 {
		return ""
	}
	p := objc.Send[unsafe.Pointer](s, sel("UTF8String"))
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(p), n))
}

func avfArray(a objc.ID) []objc.ID {
	n := objc.Send[uint64](a, sel("count"))
	out := make([]objc.ID, n)
	for i := range out {
		out[i] = a.Send(sel("objectAtIndex:"), uint64(i))
	}
	return out
}

func avfRetain(id objc.ID) objc.ID { return id.Send(sel("retain")) }

func avfRelease(id objc.ID) {
	if id != 0 {
		id.Send(sel("release"))
	}
}

// avfReadFormats converts an array of AVCaptureDeviceFormat. Frame duration
// limits are read only when withDurations is set: listing needs the highest
// rate only.
func avfReadFormats(formats objc.ID, withDurations bool) []avfFormat {
	var out []avfFormat
	for _, f := range avfArray(formats) {
		if !objc.Send[bool](f.Send(sel("mediaType")), sel("isEqualToString:"), avMediaTypeVideo) {
			continue
		}
		desc := objc.Send[uintptr](f, sel("formatDescription"))
		d := cmVideoDimensions(desc)
		af := avfFormat{Width: int(d.Width), Height: int(d.Height), FourCC: cmMediaSubType(desc)}
		for _, r := range avfArray(f.Send(sel("videoSupportedFrameRateRanges"))) {
			ar := avfRange{MaxRate: objc.Send[float64](r, sel("maxFrameRate"))}
			if withDurations {
				ar.MinDuration = objc.Send[avfTime](r, sel("minFrameDuration"))
				ar.MaxDuration = objc.Send[avfTime](r, sel("maxFrameDuration"))
			}
			af.Ranges = append(af.Ranges, ar)
		}
		out = append(out, af)
	}
	return out
}

// avfReal is the system AVFoundation.
type avfReal struct{}

func (*avfReal) authorizationStatus() int {
	var st int64
	avfPool(func() {
		st = objc.Send[int64](avfClass("AVCaptureDevice"), sel("authorizationStatusForMediaType:"), avMediaTypeVideo)
	})
	return int(st)
}

// devices lists cameras and their formats. It never opens a camera.
func (*avfReal) devices() ([]avfDevice, error) {
	var out []avfDevice
	avfPool(func() {
		types := avfClass("NSMutableArray").Send(sel("array"))
		types.Send(sel("addObject:"), avTypeBuiltInWide)
		types.Send(sel("addObject:"), avTypeExternal)
		ds := avfClass("AVCaptureDeviceDiscoverySession").Send(
			sel("discoverySessionWithDeviceTypes:mediaType:position:"), types, avMediaTypeVideo, int64(0))
		def := avfClass("AVCaptureDevice").Send(sel("defaultDeviceWithMediaType:"), avMediaTypeVideo)
		defID := ""
		if def != 0 {
			defID = avfGoString(def.Send(sel("uniqueID")))
		}
		for _, d := range avfArray(ds.Send(sel("devices"))) {
			id := avfGoString(d.Send(sel("uniqueID")))
			out = append(out, avfDevice{
				ID:      id,
				Name:    avfGoString(d.Send(sel("localizedName"))),
				Default: def != 0 && id == defID,
				Builtin: objc.Send[bool](d.Send(sel("deviceType")), sel("isEqualToString:"), avTypeBuiltInWide),
				Formats: avfReadFormats(d.Send(sel("formats")), false),
			})
		}
	})
	return out, nil
}

// requestAccess may show the system prompt. The completion block runs on a
// foreign thread.
func (*avfReal) requestAccess(done func(granted bool)) error {
	avfPool(func() {
		b := objc.NewBlock(func(_ objc.Block, granted bool) {
			avfPool(func() { done(granted) })
		})
		avfClass("AVCaptureDevice").Send(sel("requestAccessForMediaType:completionHandler:"), avMediaTypeVideo, b)
		b.Release()
	})
	return nil
}

func (*avfReal) openDevice(uid string) (avfSession, error) {
	if uid == "" {
		return nil, nil
	}
	var s *avfRealSession
	avfPool(func() {
		// An id that is not valid UTF-8 gives no string, and nil is not a valid id.
		name := avfNSString(uid)
		if name == 0 {
			return
		}
		dev := avfClass("AVCaptureDevice").Send(sel("deviceWithUniqueID:"), name)
		if dev != 0 {
			s = &avfRealSession{dev: avfRetain(dev)}
		}
	})
	if s == nil {
		return nil, nil
	}
	return s, nil
}

type avfObserver struct {
	obj   objc.ID
	block objc.Block
}

// avfRealSession owns the objects it retains; release frees them.
type avfRealSession struct {
	dev, fmts, input, session, output, delegate objc.ID
	queue                                       uintptr
	observers                                   []avfObserver

	stopOnce, releaseOnce sync.Once
}

func (s *avfRealSession) formats() []avfFormat {
	var out []avfFormat
	avfPool(func() {
		if s.fmts == 0 {
			s.fmts = avfRetain(s.dev.Send(sel("formats")))
		}
		out = avfReadFormats(s.fmts, true)
	})
	return out
}

func avfErrorCode(errp uintptr) int64 {
	if errp == 0 {
		return 0
	}
	return objc.Send[int64](objc.ID(errp), sel("code"))
}

func (s *avfRealSession) newInput() (code int64, ok bool) {
	avfPool(func() {
		var errp uintptr
		in := avfClass("AVCaptureDeviceInput").Send(sel("deviceInputWithDevice:error:"), s.dev, unsafe.Pointer(&errp))
		if in == 0 {
			code = avfErrorCode(errp)
			return
		}
		s.input = avfRetain(in)
		s.makeObjects()
		ok = true
	})
	return code, ok
}

// makeObjects creates the session, output, queue and delegate. The caller
// holds a pool.
func (s *avfRealSession) makeObjects() {
	s.session = avfClass("AVCaptureSession").Send(sel("new"))
	s.output = avfClass("AVCaptureVideoDataOutput").Send(sel("new"))
	s.output.Send(sel("setAlwaysDiscardsLateVideoFrames:"), true)
	label := avfCString("kolovideo.capture")
	s.queue = dispatchQueueCreate(&label[0], 0)
	s.delegate = objc.ID(avDelegateClass).Send(sel("new"))
}

func (s *avfRealSession) beginConfiguration() {
	avfPool(func() { s.session.Send(sel("beginConfiguration")) })
}
func (s *avfRealSession) commitConfiguration() {
	avfPool(func() { s.session.Send(sel("commitConfiguration")) })
}

func (s *avfRealSession) canAddInput() (ok bool) {
	avfPool(func() { ok = objc.Send[bool](s.session, sel("canAddInput:"), s.input) })
	return ok
}

func (s *avfRealSession) addInput() { avfPool(func() { s.session.Send(sel("addInput:"), s.input) }) }

func (s *avfRealSession) canAddOutput() (ok bool) {
	avfPool(func() { ok = objc.Send[bool](s.session, sel("canAddOutput:"), s.output) })
	return ok
}

func (s *avfRealSession) addOutput() { avfPool(func() { s.session.Send(sel("addOutput:"), s.output) }) }

func (s *avfRealSession) setDelegate(ident uintptr) {
	avfPool(func() {
		s.delegate.SetIvar(avDelegateIdent, objc.ID(ident))
		s.output.Send(sel("setSampleBufferDelegate:queue:"), s.delegate, s.queue)
	})
}

func (s *avfRealSession) pixelFormats() []uint32 {
	var out []uint32
	avfPool(func() {
		for _, n := range avfArray(s.output.Send(sel("availableVideoCVPixelFormatTypes"))) {
			out = append(out, objc.Send[uint32](n, sel("unsignedIntValue")))
		}
	})
	return out
}

func (s *avfRealSession) setPixelFormat(fourCC uint32) {
	avfPool(func() {
		num := avfClass("NSNumber").Send(sel("numberWithUnsignedInt:"), fourCC)
		settings := avfClass("NSDictionary").Send(sel("dictionaryWithObject:forKey:"), num, cvPixelFormatTypeKey)
		s.output.Send(sel("setVideoSettings:"), settings)
	})
}

func (s *avfRealSession) lockDevice() (code int64, ok bool) {
	avfPool(func() {
		var errp uintptr
		ok = objc.Send[bool](s.dev, sel("lockForConfiguration:"), unsafe.Pointer(&errp))
		if !ok {
			code = avfErrorCode(errp)
		}
	})
	return code, ok
}

func (s *avfRealSession) unlockDevice() {
	avfPool(func() { s.dev.Send(sel("unlockForConfiguration")) })
}

func (s *avfRealSession) setActiveFormat(index int) {
	avfPool(func() {
		if s.fmts == 0 {
			s.fmts = avfRetain(s.dev.Send(sel("formats")))
		}
		if index < 0 || uint64(index) >= objc.Send[uint64](s.fmts, sel("count")) {
			return
		}
		s.dev.Send(sel("setActiveFormat:"), s.fmts.Send(sel("objectAtIndex:"), uint64(index)))
	})
}

func (s *avfRealSession) setMinFrameDuration(d avfTime) {
	avfPool(func() { s.dev.Send(sel("setActiveVideoMinFrameDuration:"), d) })
}

func (s *avfRealSession) activeMinFrameDuration() (d avfTime) {
	avfPool(func() { d = objc.Send[avfTime](s.dev, sel("activeVideoMinFrameDuration")) })
	return d
}

func (s *avfRealSession) observe(lost func(kind int, code int64)) {
	avfPool(func() {
		nc := avfClass("NSNotificationCenter").Send(sel("defaultCenter"))
		add := func(name, object objc.ID, fn any) {
			b := objc.NewBlock(fn)
			o := nc.Send(sel("addObserverForName:object:queue:usingBlock:"), name, object, objc.ID(0), b)
			s.observers = append(s.observers, avfObserver{avfRetain(o), b})
		}
		add(avDisconnectedNote, s.dev, func(_ objc.Block, _ objc.ID) {
			avfPool(func() { lost(avfLostDisconnect, 0) })
		})
		add(avRuntimeErrorNote, s.session, func(_ objc.Block, note objc.ID) {
			avfPool(func() {
				e := note.Send(sel("userInfo")).Send(sel("objectForKey:"), avSessionErrorKey)
				code := int64(0)
				if e != 0 {
					code = objc.Send[int64](e, sel("code"))
				}
				if code == avfNotAuthorized {
					lost(avfLostAccess, code)
				} else {
					lost(avfLostSession, code)
				}
			})
		})
	})
}

func (s *avfRealSession) start() { avfPool(func() { s.session.Send(sel("startRunning")) }) }

func (s *avfRealSession) running() (ok bool) {
	avfPool(func() { ok = objc.Send[bool](s.session, sel("isRunning")) })
	return ok
}

func (s *avfRealSession) syncClock() (clock uintptr) {
	avfPool(func() {
		if objc.Send[bool](s.session, sel("respondsToSelector:"), sel("synchronizationClock")) {
			clock = objc.Send[uintptr](s.session, sel("synchronizationClock"))
		}
	})
	return clock
}

func (s *avfRealSession) stop() {
	s.stopOnce.Do(func() {
		if s.session == 0 {
			return
		}
		avfPool(func() {
			nc := avfClass("NSNotificationCenter").Send(sel("defaultCenter"))
			for _, o := range s.observers {
				nc.Send(sel("removeObserver:"), o.obj)
			}
			s.session.Send(sel("stopRunning"))
			s.output.Send(sel("setSampleBufferDelegate:queue:"), objc.ID(0), uintptr(0))
			// Wait for a callback already running on the queue.
			dispatchSyncF(s.queue, 0, avSyncDone)
		})
	})
}

func (s *avfRealSession) release() {
	s.stop()
	s.releaseOnce.Do(func() {
		avfPool(func() {
			for _, o := range s.observers {
				avfRelease(o.obj)
				o.block.Release()
			}
			s.observers = nil
			for _, o := range []objc.ID{s.delegate, s.output, s.session, s.input, s.fmts, s.dev} {
				avfRelease(o)
			}
			if s.queue != 0 {
				dispatchRelease(s.queue)
			}
		})
	})
}
