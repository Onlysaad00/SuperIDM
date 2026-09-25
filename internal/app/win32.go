//go:build windows

// Package app implements the native Windows shell for SuperIDM: the main
// window (hosting the embedded UI in WebView2, with a browser fallback), the
// system-tray icon and its menu, single-instance handling and autostart.
//
// Everything here talks to Win32 directly through syscall, so the final .exe
// is a single self-contained file with no CGO and no installer prerequisites
// beyond the WebView2 runtime that ships with Windows 10/11.
package app

import (
	"syscall"
	"unsafe"
)

// ---------------------------------------------------------------------------
// DLL procedures
// ---------------------------------------------------------------------------

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")
	shcore   = syscall.NewLazyDLL("shcore.dll")

	pRegisterClassExW     = user32.NewProc("RegisterClassExW")
	pCreateWindowExW      = user32.NewProc("CreateWindowExW")
	pDefWindowProcW       = user32.NewProc("DefWindowProcW")
	pDestroyWindow        = user32.NewProc("DestroyWindow")
	pShowWindow           = user32.NewProc("ShowWindow")
	pUpdateWindow         = user32.NewProc("UpdateWindow")
	pGetMessageW          = user32.NewProc("GetMessageW")
	pTranslateMessage     = user32.NewProc("TranslateMessage")
	pDispatchMessageW     = user32.NewProc("DispatchMessageW")
	pPostQuitMessage      = user32.NewProc("PostQuitMessage")
	pPostMessageW         = user32.NewProc("PostMessageW")
	pSetWindowPos         = user32.NewProc("SetWindowPos")
	pGetClientRect        = user32.NewProc("GetClientRect")
	pGetWindowRect        = user32.NewProc("GetWindowRect")
	pAdjustWindowRectEx   = user32.NewProc("AdjustWindowRectEx")
	pLoadCursorW          = user32.NewProc("LoadCursorW")
	pLoadImageW           = user32.NewProc("LoadImageW")
	pCreateIconFromRes    = user32.NewProc("CreateIconFromResourceEx")
	pDestroyIcon          = user32.NewProc("DestroyIcon")
	pSetForegroundWindow  = user32.NewProc("SetForegroundWindow")
	pGetSystemMetrics     = user32.NewProc("GetSystemMetrics")
	pMessageBoxW          = user32.NewProc("MessageBoxW")
	pIsWindowVisible      = user32.NewProc("IsWindowVisible")
	pSetWindowTextW       = user32.NewProc("SetWindowTextW")
	pCreatePopupMenu      = user32.NewProc("CreatePopupMenu")
	pAppendMenuW          = user32.NewProc("AppendMenuW")
	pDestroyMenu          = user32.NewProc("DestroyMenu")
	pTrackPopupMenu       = user32.NewProc("TrackPopupMenu")
	pGetCursorPos         = user32.NewProc("GetCursorPos")
	pRegisterWindowMsg    = user32.NewProc("RegisterWindowMessageW")
	pSetProcessDPIAware   = user32.NewProc("SetProcessDPIAware")
	pSetProcessDpiCtx     = user32.NewProc("SetProcessDpiAwarenessContext")
	pGetDpiForWindow      = user32.NewProc("GetDpiForWindow")
	pMonitorFromWindow    = user32.NewProc("MonitorFromWindow")
	pGetMonitorInfoW      = user32.NewProc("GetMonitorInfoW")
	pSystemParametersInfo = user32.NewProc("SystemParametersInfoW")
	pInvalidateRect       = user32.NewProc("InvalidateRect")
	pGetKeyState          = user32.NewProc("GetKeyState")
	pSetActiveWindow      = user32.NewProc("SetActiveWindow")

	pShellNotifyIconW  = shell32.NewProc("Shell_NotifyIconW")
	pShellExecuteW     = shell32.NewProc("ShellExecuteW")
	pSHGetFolderPathW  = shell32.NewProc("SHGetFolderPathW")
	pExtractIconExW    = shell32.NewProc("ExtractIconExW")
	pCommandLineToArgv = shell32.NewProc("CommandLineToArgvW")

	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	pCreateMutexW     = kernel32.NewProc("CreateMutexW")
	pGetLastError     = kernel32.NewProc("GetLastError")
	pGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	pGlobalFree       = kernel32.NewProc("GlobalFree")
	pGlobalLock       = kernel32.NewProc("GlobalLock")
	pGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	pGetTickCount64   = kernel32.NewProc("GetTickCount64")
	pLocalFree        = kernel32.NewProc("LocalFree")
	pCloseHandle      = kernel32.NewProc("CloseHandle")

	pRegCreateKeyExW = advapi32.NewProc("RegCreateKeyExW")
	pRegSetValueExW  = advapi32.NewProc("RegSetValueExW")
	pRegDeleteValueW = advapi32.NewProc("RegDeleteValueW")
	pRegCloseKey     = advapi32.NewProc("RegCloseKey")

	pCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	pCoUninitialize   = ole32.NewProc("CoUninitialize")
	pCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
	pOleInitialize    = ole32.NewProc("OleInitialize")
	pCreateStdAccess  = ole32.NewProc("CreateStdAccessibleObject")
	pSetErrorInfoNone = ole32.NewProc("CoFreeUnusedLibraries")
)

// ---------------------------------------------------------------------------
// Win32 constants
// ---------------------------------------------------------------------------

const (
	wsOverlappedWindow = 0x00CF0000
	wsClipChildren     = 0x02000000
	wsMaximizeBox      = 0x00010000
	wsMinimizeBox      = 0x00020000
	cwUseDefault       = 0x80000000

	swShow     = 5
	swHide     = 0
	swRestore  = 9
	swShowNorm = 1
	swMinimize = 6

	wmDestroy     = 0x0002
	wmSize        = 0x0005
	wmClose       = 0x0010
	wmQuit        = 0x0012
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmApp         = 0x8000
	wmTrayIcon    = wmApp + 1
	wmTrayCommand = wmApp + 2
	wmSuperIDM    = wmApp + 10
	wmSetIcon     = 0x0080

	idiApplication = 32512
	idcArrow       = 32512

	hwndTopmost    = ^uintptr(0) // (HWND)-1
	hwndNotTopmost = ^uintptr(1) // (HWND)-2

	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010
	swpShowWindow = 0x0040

	mfString    = 0x00000000
	mfSeparator = 0x00000800
	mfChecked   = 0x00000008
	tpmRightBtn = 0x0002
	tpmRetCmd   = 0x0100

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002
	nifMsg    = 0x00000001
	nifIcon   = 0x00000002
	nifTip    = 0x00000004
	nifInfo   = 0x00000010

	niifInfo    = 0x00000001
	niifWarning = 0x00000002

	mbOK            = 0x00000000
	mbIconInfo      = 0x00000040
	mbIconWarning   = 0x00000030
	mbIconError     = 0x00000010
	mbSetForeground = 0x00010000

	regOptionNonVolatile = 0
	regSZ                = 1
	keySetValue          = 0x0002
	hkeyCurrentUser      = 0x80000001

	spiGetWorkArea = 0x0030

	dpiAwarenessPerMonitorV2 = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4

	monitorDefaultToNearest = 0x00000002

	swpNoSize = 0x0001
	swpNoMove = 0x0002

	// menu command ids
	cmdShow       = 1001
	cmdAddURL     = 1002
	cmdPauseAll   = 1003
	cmdResumeAll  = 1004
	cmdSpeedLimit = 1010
	cmdOpenFolder = 1011
	cmdExtFolder  = 1012
	cmdLogFile    = 1013
	cmdOpenUI     = 1014
	cmdAbout      = 1015
	cmdExit       = 1099
)

// ---------------------------------------------------------------------------
// Structs
// ---------------------------------------------------------------------------

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type point struct{ X, Y int32 }

type rect struct{ Left, Top, Right, Bottom int32 }

type msg struct {
	Hwnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32
}

type minMaxInfo struct {
	PtReserved     point
	PtMaxSize      point
	PtMaxPosition  point
	PtMinTrackSize point
	PtMaxTrackSize point
}

type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
}

type notifyIconDataW struct {
	CbSize           uint32
	Hwnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UTimeoutOrVer    uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

func (n *notifyIconDataW) setTip(s string) {
	n.SzTip = [128]uint16{}
	copy(n.SzTip[:], utf16FromString(s))
}

func (n *notifyIconDataW) setInfo(s string) {
	n.SzInfo = [256]uint16{}
	copy(n.SzInfo[:], utf16FromString(s))
}

func (n *notifyIconDataW) setInfoTitle(s string) {
	n.SzInfoTitle = [64]uint16{}
	copy(n.SzInfoTitle[:], utf16FromString(s))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func utf16FromString(s string) []uint16 {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		// The string contains a NUL; strip it rather than failing.
		out := make([]uint16, 0, len(s)+1)
		for _, r := range s {
			if r == 0 {
				continue
			}
			if r < 0x10000 {
				out = append(out, uint16(r))
			} else {
				r -= 0x10000
				out = append(out, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
			}
		}
		return append(out, 0)
	}
	return u
}

func utf16Ptr(s string) *uint16 { return &utf16FromString(s)[0] }

func utf16ToString(p *uint16) string {
	if p == nil {
		return ""
	}
	var n int
	for {
		if *(*uint16)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + uintptr(n*2))) == 0 {
			break
		}
		n++
		if n > 1<<16 {
			break
		}
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n+1))
}

func lowWord(v uintptr) uint16  { return uint16(v & 0xFFFF) }
func highWord(v uintptr) uint16 { return uint16((v >> 16) & 0xFFFF) }
func loWord(v uintptr) int32    { return int32(int16(v & 0xFFFF)) }
func hiWord(v uintptr) int32    { return int32(int16((v >> 16) & 0xFFFF)) }

func failed(hr uintptr) bool { return int32(hr) < 0 }

func lastErr() error {
	e, _, _ := pGetLastError.Call()
	if e == 0 {
		return nil
	}
	return syscall.Errno(e)
}

// showMessageBox displays a native message box.
func showMessageBox(hwnd uintptr, title, text string, flags uintptr) int {
	r, _, _ := pMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(utf16Ptr(text))),
		uintptr(unsafe.Pointer(utf16Ptr(title))), flags|mbSetForeground)
	return int(r)
}

// pFindWindowW locates an existing SuperIDM window (single-instance handoff).
var pFindWindowW = user32.NewProc("FindWindowW")
