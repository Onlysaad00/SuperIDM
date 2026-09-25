//go:build windows

package app

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// Minimal WebView2 host.
//
// SuperIDM renders its own UI (internal/api/web) inside the Microsoft Edge
// WebView2 control, which is present on Windows 11 and on Windows 10 through
// the Evergreen Runtime. Only the few COM calls needed for that are declared
// here, in the exact vtable order published in webview2.h, so the binary stays
// dependency free and CGO free.
//
// If the runtime is absent the caller falls back to the user's default browser
// (see app_windows.go), so SuperIDM always works.

var (
	webview2 = syscall.NewLazyDLL("WebView2Loader.dll")

	pCreateCoreWebView2EnvironmentWithOptions = webview2.NewProc("CreateCoreWebView2EnvironmentWithOptions")
	pGetAvailableCoreWebView2BrowserVersion   = webview2.NewProc("GetAvailableCoreWebView2BrowserVersionString")
)

// COM IIDs (webview2.h).
var (
	iidICoreWebView2Environment                      = guid{0xB96D755E, 0x0319, 0x4E92, [8]byte{0xA2, 0x96, 0x23, 0x43, 0x6F, 0x46, 0xA1, 0xFC}}
	iidICoreWebView2EnvironmentOptions               = guid{0x2FDE08A8, 0x1E9A, 0x4766, [8]byte{0x8C, 0x05, 0x95, 0xA9, 0xCE, 0xB9, 0xD1, 0xC5}}
	iidICoreWebView2CreateEnvCompletedHandler        = guid{0x4E8A3389, 0xC9D8, 0x4BD2, [8]byte{0xB6, 0xB5, 0x12, 0x4F, 0xEE, 0x6C, 0xC1, 0x4D}}
	iidICoreWebView2Controller                       = guid{0x4D00C0D1, 0x9434, 0x4EB6, [8]byte{0x80, 0x78, 0x86, 0x97, 0xA5, 0x60, 0x73, 0x45}}
	iidICoreWebView2Controller2                      = guid{0xC979903E, 0xD4CA, 0x4228, [8]byte{0x92, 0xEB, 0x47, 0xEE, 0x3F, 0xA9, 0x6E, 0xAB}}
	iidICoreWebView2CreateControllerCompletedHandler = guid{0x6C4819F3, 0xC9B7, 0x4260, [8]byte{0x81, 0x27, 0xCD, 0xE4, 0x36, 0x75, 0xC5, 0x9A}}
	iidICoreWebView2                                 = guid{0x76ECEACB, 0x0462, 0x4D94, [8]byte{0xAC, 0x83, 0x42, 0x3A, 0x67, 0x93, 0x77, 0x5E}}
	iidICoreWebView2Settings                         = guid{0xE562E4F0, 0xD7FA, 0x43B0, [8]byte{0x88, 0x15, 0x6F, 0x9A, 0x9D, 0x5D, 0x55, 0x40}}
	iidIUnknown                                      = guid{0x00000000, 0x0000, 0x0000, [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

// ---------------------------------------------------------------------------
// vtable slot numbers
//
// IUnknown always occupies slots 0..2, so for a COM interface the n-th method
// declared in webview2.h lives at slot n+2. These constants name every slot
// this file touches; the arithmetic is spelled out in the comments so the
// values can be checked against the header by hand.
// ---------------------------------------------------------------------------

const (
	// IUnknown
	slotQueryInterface = 0
	slotAddRef         = 1
	slotRelease        = 2

	// ICoreWebView2Environment: CreateCoreWebView2Controller is method 1.
	slotEnvCreateController = 3

	// ICoreWebView2Controller (methods 1..20):
	//  1 get_IsVisible     2 put_IsVisible
	//  3 get_Bounds        4 put_Bounds
	//  5 get_ZoomFactor    6 put_ZoomFactor
	//  7 add_ZoomFactorChanged   8 remove_ZoomFactorChanged
	//  9 SetBoundsAndZoomFactor 10 MoveFocus
	// 11 add_MoveFocusRequested 12 remove_MoveFocusRequested
	// 13 add_GotFocus     14 remove_GotFocus
	// 15 add_LostFocus    16 remove_LostFocus
	// 17 add_AcceleratorKeyPressed 18 remove_AcceleratorKeyPressed
	// 19 GetCoreWebView2  20 Close
	slotCtrlGetIsVisible = 3
	slotCtrlPutIsVisible = 4
	slotCtrlGetBounds    = 5
	slotCtrlPutBounds    = 6
	slotCtrlMoveFocus    = 12
	slotCtrlGetCore      = 21
	slotCtrlClose        = 22
	// ICoreWebView2Controller2 adds one method after the inherited 20:
	// 23 put_DefaultBackgroundColor, 24 get_DefaultBackgroundColor.
	slotCtrl2PutBackground = 23

	// ICoreWebView2 (methods 1..34+):
	//  1 get_Settings        2 get_Source        3 Navigate
	//  4 NavigateToString   13 add_NavigationCompleted
	// 19 add_PermissionRequested  25 add_WebMessageReceived
	// 28 Reload             29 PostWebMessageAsJson  31 add_WindowCloseRequested
	slotCoreGetSettings        = 3
	slotCoreNavigate           = 5
	slotCoreNavigationComplete = 15
	slotCoreReload             = 30
	slotCorePostMessageJSON    = 31
	slotCoreWindowCloseReq     = 33

	// ICoreWebView2Settings (methods 1..16):
	//  1 IsScriptEnabled 3 IsWebMessageEnabled 5 AreDefaultScriptDialogsEnabled
	//  7 IsStatusBarEnabled 9 AreDevToolsEnabled
	// 11 AreDefaultContextMenusEnabled 13 IsZoomControlEnabled
	// 15 IsBuiltInErrorPageEnabled (put_ variants are the odd-numbered twins)
	slotSettingsPutStatusBar    = 10
	slotSettingsPutDevTools     = 12
	slotSettingsPutContextMenus = 14
	slotSettingsPutZoomControl  = 16
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func (g guid) String() string {
	return fmt.Sprintf("{%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X}",
		g.Data1, g.Data2, g.Data3, g.Data4[0], g.Data4[1],
		g.Data4[2], g.Data4[3], g.Data4[4], g.Data4[5], g.Data4[6], g.Data4[7])
}

func guidEqual(p uintptr, want guid) bool {
	if p == 0 {
		return false
	}
	got := (*guid)(unsafe.Pointer(p))
	return got.Data1 == want.Data1 && got.Data2 == want.Data2 &&
		got.Data3 == want.Data3 && got.Data4 == want.Data4
}

// comCall invokes vtable slot `index` of a COM object.
func comCall(this uintptr, index int, args ...uintptr) uintptr {
	if this == 0 {
		return 0x80004003 // E_POINTER
	}
	vtbl := *(*uintptr)(unsafe.Pointer(this))
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(index)*unsafe.Sizeof(uintptr(0))))
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, this)
	all = append(all, args...)
	r1, _, _ := syscall.SyscallN(fn, all...)
	return r1
}

// comQueryInterface requests a different interface from a COM object.
func comQueryInterface(this uintptr, iid guid) uintptr {
	var out uintptr
	hr := comCall(this, slotQueryInterface, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&out)))
	if failed(hr) {
		return 0
	}
	return out
}

func comRelease(this uintptr) {
	if this != 0 {
		comCall(this, slotRelease)
	}
}

// ---------------------------------------------------------------------------
// Hand-written COM handlers
// ---------------------------------------------------------------------------
//
// A handler is a Go struct whose first field is a pointer to its vtable: the
// address of the struct is handed to WebView2 as the `this` pointer, exactly
// as a C++ object would be laid out.

type iunknownVtbl struct {
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr
}

type completedVtbl struct {
	iunknownVtbl
	Invoke uintptr
}

type comHandler struct {
	vtbl  *completedVtbl
	fn    func(hr uintptr, obj uintptr)
	label string
}

var (
	handlerInvokeCallback = syscall.NewCallback(handlerInvoke)
	handlerQueryIICB      = syscall.NewCallback(handlerQueryInterface)
	handlerAddRef         = syscall.NewCallback(handlerAddRefCB)
	handlerRelease        = syscall.NewCallback(handlerReleaseCB)
)

func newHandler(label string, fn func(uintptr, uintptr)) (*comHandler, uintptr) {
	h := &comHandler{fn: fn, label: label}
	h.vtbl = &completedVtbl{
		iunknownVtbl: iunknownVtbl{
			QueryInterface: handlerQueryIICB,
			AddRef:         handlerAddRef,
			Release:        handlerRelease,
		},
		Invoke: handlerInvokeCallback,
	}
	return h, uintptr(unsafe.Pointer(h))
}

func handlerInvoke(this uintptr, hr uintptr, obj uintptr) uintptr {
	h := (*comHandler)(unsafe.Pointer(this))
	if h != nil && h.fn != nil {
		h.fn(hr, obj)
	}
	return 0
}

// The handlers live for the process lifetime and are only used on the UI
// thread, so reference counting is a formality that keeps COM happy.
func handlerQueryInterface(this, riid, ppv uintptr) uintptr {
	if ppv == 0 {
		return 0x80004003 // E_POINTER
	}
	if riid != 0 {
		// Handlers may be queried as IUnknown or as their specific completed
		// handler interface; both have the same layout here.
		if !guidEqual(riid, iidIUnknown) &&
			!guidEqual(riid, iidICoreWebView2CreateEnvCompletedHandler) &&
			!guidEqual(riid, iidICoreWebView2CreateControllerCompletedHandler) {
			return 0x80004002 // E_NOINTERFACE
		}
	}
	*(*uintptr)(unsafe.Pointer(ppv)) = this
	return 0
}
func handlerAddRefCB(this uintptr) uintptr  { return 2 }
func handlerReleaseCB(this uintptr) uintptr { return 1 }

// ---------------------------------------------------------------------------
// WebView2 host
// ---------------------------------------------------------------------------

type webview struct {
	ctrl      uintptr // ICoreWebView2Controller*
	core      uintptr // ICoreWebView2*
	env       uintptr // ICoreWebView2Environment*
	hwnd      uintptr
	envH      *comHandler
	ctrlH     *comHandler
	options   uintptr
	ready     bool
	navigated string
	failed    error
	onReady   func()
}

// webView2Available reports whether the Evergreen runtime is installed.
func webView2Available() (string, bool) {
	if err := webview2.Load(); err != nil {
		return "", false
	}
	var ver *uint16
	hr, _, _ := pGetAvailableCoreWebView2BrowserVersion.Call(0, uintptr(unsafe.Pointer(&ver)))
	if failed(hr) || ver == nil {
		return "", false
	}
	s := utf16ToString(ver)
	pCoTaskMemFree.Call(uintptr(unsafe.Pointer(ver)))
	return s, s != ""
}

// newWebView creates the environment and controller inside hwnd.
func newWebView(hwnd uintptr, userDataDir, initialURL string, onReady func()) (*webview, error) {
	if _, ok := webView2Available(); !ok {
		return nil, errors.New("the WebView2 runtime is not installed")
	}
	w := &webview{hwnd: hwnd, onReady: onReady}
	w.options = newEnvOptions()

	envH, envHPtr := newHandler("environment", func(hr uintptr, env uintptr) {
		if failed(hr) || env == 0 {
			w.failed = fmt.Errorf("WebView2 environment creation failed (hr=0x%08X)", uint32(hr))
			return
		}
		w.env = env
		ctrlH, ctrlHPtr := newHandler("controller", func(hr uintptr, ctrl uintptr) {
			if failed(hr) || ctrl == 0 {
				w.failed = fmt.Errorf("WebView2 controller creation failed (hr=0x%08X)", uint32(hr))
				return
			}
			w.ctrl = ctrl
			w.afterController()
			if w.failed == nil {
				if initialURL != "" {
					w.Navigate(initialURL)
				}
				if onReady != nil {
					onReady()
				}
			}
		})
		w.ctrlH = ctrlH
		if hr2 := comCall(env, slotEnvCreateController, hwnd, ctrlHPtr); failed(hr2) {
			w.failed = fmt.Errorf("CreateCoreWebView2Controller failed (hr=0x%08X)", uint32(hr2))
		}
	})
	w.envH = envH

	hr, _, _ := pCreateCoreWebView2EnvironmentWithOptions.Call(
		0,
		uintptr(unsafe.Pointer(utf16Ptr(userDataDir))),
		w.options,
		envHPtr,
	)
	if failed(hr) {
		return nil, fmt.Errorf("CreateCoreWebView2EnvironmentWithOptions failed (hr=0x%08X)", uint32(hr))
	}
	return w, nil
}

// afterController configures the controller and grabs the core interface.
func (w *webview) afterController() {
	comCall(w.ctrl, slotCtrlPutIsVisible, 1)
	w.resize()

	// Dark background so resizing never flashes white.
	if c2 := comQueryInterface(w.ctrl, iidICoreWebView2Controller2); c2 != 0 {
		// COREWEBVIEW2_COLOR is BGRA.
		col := [4]byte{0x16, 0x11, 0x0e, 0xff}
		comCall(c2, slotCtrl2PutBackground, uintptr(unsafe.Pointer(&col)))
		comRelease(c2)
	}

	var core uintptr
	if hr := comCall(w.ctrl, slotCtrlGetCore, uintptr(unsafe.Pointer(&core))); failed(hr) || core == 0 {
		w.failed = errors.New("GetCoreWebView2 failed")
		return
	}
	w.core = core

	if settings := comQueryInterface(w.core, iidICoreWebView2Settings); settings != 0 {
		comCall(settings, slotSettingsPutStatusBar, 0)    // no hover status bar
		comCall(settings, slotSettingsPutContextMenus, 1) // keep copy/paste menu
		comCall(settings, slotSettingsPutZoomControl, 1)  // Ctrl +/-
		comRelease(settings)
	}
	w.ready = true
}

// Navigate loads a URL.
func (w *webview) Navigate(url string) {
	if w.core == 0 || url == "" {
		return
	}
	w.navigated = url
	comCall(w.core, slotCoreNavigate, uintptr(unsafe.Pointer(utf16Ptr(url))))
}

// Reload refreshes the page.
func (w *webview) Reload() {
	if w.core != 0 {
		comCall(w.core, slotCoreReload)
	}
}

// PostJSON sends a message into the page (used for tray-driven commands).
func (w *webview) PostJSON(jsonStr string) {
	if w.core != 0 {
		comCall(w.core, slotCorePostMessageJSON, uintptr(unsafe.Pointer(utf16Ptr(jsonStr))))
	}
}

// resize matches the webview to the client area of its window.
func (w *webview) resize() {
	if w.ctrl == 0 {
		return
	}
	var r rect
	pGetClientRect.Call(w.hwnd, uintptr(unsafe.Pointer(&r)))
	bounds := rect{0, 0, r.Right - r.Left, r.Bottom - r.Top}
	comCall(w.ctrl, slotCtrlPutBounds, uintptr(unsafe.Pointer(&bounds)))
}

// Focus moves keyboard focus into the page.
func (w *webview) Focus() {
	if w.ctrl != 0 {
		comCall(w.ctrl, slotCtrlMoveFocus, 0) // COREWEBVIEW2_MOVE_FOCUS_REASON_PROGRAMMATIC
	}
}

// Close tears the control down.
func (w *webview) Close() {
	if w.ctrl != 0 {
		comCall(w.ctrl, slotCtrlClose)
		comRelease(w.ctrl)
		w.ctrl = 0
	}
	if w.env != 0 {
		comRelease(w.env)
		w.env = 0
	}
}

// Ready reports whether the control finished initialising.
func (w *webview) Ready() bool { return w != nil && w.ready }

// Err returns the asynchronous failure, if any.
func (w *webview) Err() error { return w.failed }

// ---------------------------------------------------------------------------
// ICoreWebView2EnvironmentOptions (minimal implementation)
// ---------------------------------------------------------------------------

type envOptionsVtbl struct {
	iunknownVtbl
	putAdditionalBrowserArguments uintptr
	getAdditionalBrowserArguments uintptr
	putLanguage                   uintptr
	getLanguage                   uintptr
	putTargetCompatibleVersion    uintptr
	getTargetCompatibleVersion    uintptr
	getAllowSSO                   uintptr
}

type envOptions struct {
	vtbl *envOptionsVtbl
	args uintptr
	lang uintptr
}

var (
	envOptQI       = syscall.NewCallback(envOptQueryInterface)
	envOptAddRefCB = syscall.NewCallback(func(this uintptr) uintptr { return 2 })
	envOptRelCB    = syscall.NewCallback(func(this uintptr) uintptr { return 1 })
	envOptPutArgs  = syscall.NewCallback(func(this, v uintptr) uintptr {
		(*envOptions)(unsafe.Pointer(this)).args = v
		return 0
	})
	envOptGetArgs = syscall.NewCallback(func(this, out uintptr) uintptr {
		if out == 0 {
			return 0x80004003
		}
		*(*uintptr)(unsafe.Pointer(out)) = (*envOptions)(unsafe.Pointer(this)).args
		return 0
	})
	envOptPutLang = syscall.NewCallback(func(this, v uintptr) uintptr { return 0 })
	envOptGetLang = syscall.NewCallback(func(this, out uintptr) uintptr {
		if out == 0 {
			return 0x80004003
		}
		*(*uintptr)(unsafe.Pointer(out)) = 0
		return 0
	})
	envOptPutVer = syscall.NewCallback(func(this, v uintptr) uintptr { return 0 })
	envOptGetVer = syscall.NewCallback(func(this, out uintptr) uintptr {
		if out == 0 {
			return 0x80004003
		}
		*(*uintptr)(unsafe.Pointer(out)) = 0
		return 0
	})
	envOptGetSSO = syscall.NewCallback(func(this, out uintptr) uintptr {
		if out == 0 {
			return 0x80004003
		}
		*(*int32)(unsafe.Pointer(out)) = 0
		return 0
	})

	envOptionsInstance *envOptions // kept alive for the process lifetime
)

func envOptQueryInterface(this, riid, ppv uintptr) uintptr {
	if ppv == 0 {
		return 0x80004003
	}
	if riid != 0 && !guidEqual(riid, iidIUnknown) && !guidEqual(riid, iidICoreWebView2EnvironmentOptions) {
		return 0x80004002 // E_NOINTERFACE
	}
	*(*uintptr)(unsafe.Pointer(ppv)) = this
	return 0
}

func newEnvOptions() uintptr {
	o := &envOptions{}
	o.vtbl = &envOptionsVtbl{
		iunknownVtbl: iunknownVtbl{
			QueryInterface: envOptQI,
			AddRef:         envOptAddRefCB,
			Release:        envOptRelCB,
		},
		putAdditionalBrowserArguments: envOptPutArgs,
		getAdditionalBrowserArguments: envOptGetArgs,
		putLanguage:                   envOptPutLang,
		getLanguage:                   envOptGetLang,
		putTargetCompatibleVersion:    envOptPutVer,
		getTargetCompatibleVersion:    envOptGetVer,
		getAllowSSO:                   envOptGetSSO,
	}
	envOptionsInstance = o
	return uintptr(unsafe.Pointer(o))
}
