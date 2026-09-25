//go:build windows

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Onlysaad00/SuperIDM/internal/engine"
)

// Config describes how the native shell should come up.
type Config struct {
	Title    string
	Version  string
	URL      string // address of the local UI
	Port     int
	StartMin bool
	TrayOnly bool // run without a window (used with --tray)

	// Hooks into the service layer.
	AddURL       func(url string) error
	PauseAll     func()
	ResumeAll    func()
	OpenFolder   func()
	OnSpeedLimit func(bps int64)
	Stats        func() (speed float64, active int, done int)
}

type window struct {
	hwnd    uintptr
	wv      *webview
	tray    *tray
	cfg     Config
	useWeb  bool
	closing bool
}

var (
	win       *window
	wndProc   = syscall.NewCallback(windowProc)
	className = "SuperIDM_MainWindow"
)

// Run starts the native shell and blocks until the user closes the window.
func Run(cfg Config) error {
	// One instance only: focus the existing window instead.
	if !acquireSingleInstance() {
		if hwnd := findExistingWindow(); hwnd != 0 {
			pShowWindow.Call(hwnd, swRestore)
			pSetForegroundWindow.Call(hwnd)
		}
		return nil
	}

	enableDPIAwareness()

	hinst, _, _ := pGetModuleHandleW.Call(0)
	icon := createTrayIcon()

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		Style:         0x0002 | 0x0001, // CS_HREDRAW | CS_VREDRAW
		LpfnWndProc:   wndProc,
		HInstance:     hinst,
		HIcon:         icon,
		HIconSm:       icon,
		HCursor:       loadCursor(idcArrow),
		HbrBackground: 6, // COLOR_WINDOW+1
		LpszClassName: utf16Ptr(className),
	}
	if r, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		// ERROR_CLASS_ALREADY_EXISTS is fine.
		if e, ok := err.(syscall.Errno); !ok || e != 1410 {
			return fmt.Errorf("RegisterClassEx failed: %v", err)
		}
	}

	win = &window{cfg: cfg}

	if !cfg.TrayOnly {
		style := uintptr(wsOverlappedWindow | wsClipChildren)
		hwnd, _, err := pCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(utf16Ptr(className))),
			uintptr(unsafe.Pointer(utf16Ptr(cfg.Title))),
			style,
			cwUseDefault, cwUseDefault, 1180, 780,
			0, 0, hinst, 0,
		)
		if hwnd == 0 {
			return fmt.Errorf("CreateWindowEx failed: %v", err)
		}
		win.hwnd = hwnd
	} else {
		// Message-only window: keeps the tray icon and the message pump alive
		// without any visible UI.
		hwnd, _, err := pCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(utf16Ptr(className))),
			uintptr(unsafe.Pointer(utf16Ptr(cfg.Title))),
			0, 0, 0, 0, 0,
			0xFFFFFFFFFFFFFFFD, // HWND_MESSAGE = -3
			0, hinst, 0,
		)
		if hwnd == 0 {
			return fmt.Errorf("CreateWindowEx (message-only) failed: %v", err)
		}
		win.hwnd = hwnd
	}

	// Tray icon first so the user always has a way back in.
	win.tray = newTray(win.hwnd, handleTrayCommand)

	if win.hwnd != 0 && !cfg.TrayOnly {
		// Try WebView2; fall back to the default browser if unavailable.
		dataDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "SuperIDM", "WebView2")
		_ = os.MkdirAll(dataDir, 0o755)
		wv, err := newWebView(win.hwnd, dataDir, cfg.URL, func() {
			pSetWindowTextW.Call(win.hwnd, uintptr(unsafe.Pointer(utf16Ptr(cfg.Title))))
		})
		if err == nil {
			win.wv = wv
			win.useWeb = true
		} else {
			win.useWeb = false
			logToFile("WebView2 unavailable (%v); falling back to the default browser", err)
			openInBrowser(cfg.URL)
			// Keep the window: it shows a helpful page if the user restores it.
			win.wv = nil
		}

		pShowWindow.Call(win.hwnd, swShow)
		pUpdateWindow.Call(win.hwnd)
		if win.useWeb {
			win.wv.Focus()
		}
	} else if cfg.TrayOnly {
		logToFile("running in tray-only mode")
	}

	// Extra convenience: when the user starts the app with links as arguments
	// (or a browser hands them over), queue them immediately.
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
			if cfg.AddURL != nil {
				if err := cfg.AddURL(a); err != nil {
					logToFile("could not queue %s: %v", a, err)
				}
			}
		}
	}

	// Update the tray tooltip with live throughput.
	if cfg.Stats != nil {
		go func() {
			t := newTicker(2 * time.Second)
			for range t {
				if win == nil || win.tray == nil {
					return
				}
				speed, active, _ := cfg.Stats()
				tip := "SuperIDM — idle"
				if active > 0 {
					tip = fmt.Sprintf("SuperIDM — %s across %d download(s)", engine.HumanSpeed(speed), active)
				}
				win.tray.setTooltip(tip)
			}
		}()
	}

	return messageLoop()
}

// ---------------------------------------------------------------------------
// window procedure
// ---------------------------------------------------------------------------

func windowProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	if win != nil && win.tray != nil && win.tray.handleMessage(msg, wparam, lparam) {
		return 0
	}

	switch msg {
	case wmSize:
		if win != nil && win.wv != nil {
			win.wv.resize()
		}
		return 0
	case wmSetIcon:
		return 0
	case wmClose:
		// Minimise to tray instead of quitting, unless the user chose Exit.
		if win != nil && !win.closing {
			pShowWindow.Call(hwnd, swHide)
			if win.tray != nil {
				win.tray.notify("SuperIDM is still running",
					"Downloads continue in the background. Right-click the tray icon to exit.", false)
			}
			return 0
		}
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	case wmSuperIDM:
		if win != nil {
			pShowWindow.Call(hwnd, swRestore)
			pSetForegroundWindow.Call(hwnd)
		}
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return r
}

func messageLoop() error {
	var m msg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) == -1 {
			return fmt.Errorf("GetMessage failed")
		}
		if r == 0 { // WM_QUIT
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	if win != nil && win.wv != nil {
		win.wv.Close()
	}
	if win != nil && win.tray != nil {
		win.tray.remove()
	}
	return nil
}

// ---------------------------------------------------------------------------
// tray commands
// ---------------------------------------------------------------------------

func handleTrayCommand(cmd int) {
	if win == nil {
		return
	}
	cfg := win.cfg
	switch cmd {
	case cmdShow, cmdOpenUI:
		pShowWindow.Call(win.hwnd, swRestore)
		pSetForegroundWindow.Call(win.hwnd)
		if win.wv != nil {
			win.wv.Focus()
		} else {
			openInBrowser(cfg.URL)
		}
	case cmdAddURL:
		pShowWindow.Call(win.hwnd, swRestore)
		pSetForegroundWindow.Call(win.hwnd)
		if win.wv != nil {
			win.wv.PostJSON(`{"type":"addDownload"}`)
		} else {
			openInBrowser(cfg.URL + "#add")
		}
	case cmdPauseAll:
		if cfg.PauseAll != nil {
			cfg.PauseAll()
		}
	case cmdResumeAll:
		if cfg.ResumeAll != nil {
			cfg.ResumeAll()
		}
	case cmdSpeedLimit:
		showSpeedLimitMenu()
	case cmdOpenFolder:
		if cfg.OpenFolder != nil {
			cfg.OpenFolder()
		}
	case cmdExtFolder:
		openExtensionFolder()
	case cmdLogFile:
		openPath(logFilePath())
	case cmdAbout:
		showMessageBox(win.hwnd, "About SuperIDM",
			fmt.Sprintf("SuperIDM %s\n\nA multi-connection download accelerator.\n"+
				"Local interface: %s\n", cfg.Version, cfg.URL),
			mbOK|mbIconInfo)
	case cmdExit:
		win.closing = true
		if win.tray != nil {
			win.tray.remove()
		}
		pDestroyWindow.Call(win.hwnd)
	}
}

// showSpeedLimitMenu offers the same presets as the UI.
func showSpeedLimitMenu() {
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)
	const base = 2000
	presets := []struct {
		label string
		bps   int64
	}{
		{"Unlimited", 0}, {"256 KB/s", 256 << 10}, {"512 KB/s", 512 << 10},
		{"1 MB/s", 1 << 20}, {"2 MB/s", 2 << 20}, {"5 MB/s", 5 << 20},
		{"10 MB/s", 10 << 20}, {"50 MB/s", 50 << 20},
	}
	for i, p := range presets {
		pAppendMenuW.Call(menu, mfString, uintptr(base+i), uintptr(unsafe.Pointer(utf16Ptr(p.label))))
	}
	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(win.hwnd)
	r, _, _ := pTrackPopupMenu.Call(menu, tpmRightBtn|tpmRetCmd, uintptr(pt.X), uintptr(pt.Y), 0, win.hwnd, 0)
	if int32(r) >= base && int(r)-base < len(presets) {
		if win.cfg.OnSpeedLimit != nil {
			win.cfg.OnSpeedLimit(presets[int(r)-base].bps)
		}
	}
}

// ---------------------------------------------------------------------------
// plumbing
// ---------------------------------------------------------------------------

func enableDPIAwareness() {
	runtime.LockOSThread()
	if r, _, _ := pSetProcessDpiCtx.Call(dpiAwarenessPerMonitorV2); r != 0 {
		return
	}
	pSetProcessDPIAware.Call()
}

func loadCursor(id uintptr) uintptr {
	c, _, _ := pLoadCursorW.Call(0, id)
	return c
}

// findExistingWindow locates a running SuperIDM window so a second launch can
// simply raise it.
func findExistingWindow() uintptr {
	h, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(utf16Ptr(className))), 0)
	return h
}

// acquireSingleInstance returns false when another copy is already running.
func acquireSingleInstance() bool {
	pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(utf16Ptr("SuperIDM_SingleInstance_v1"))))
	e, _, _ := pGetLastError.Call()
	return e != 183 // ERROR_ALREADY_EXISTS
}

// OpenInBrowser opens the UI in the user's default browser (the fallback used
// when the WebView2 runtime is missing).
func OpenInBrowser(url string) { openInBrowser(url) }

func openInBrowser(url string) {
	verb := utf16Ptr("open")
	file := utf16Ptr(url)
	pShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), 0, 0, swShowNorm)
}

// OpenPath opens a file or folder with its default application.
func OpenPath(path string) { openPath(path) }

func openPath(path string) {
	if path == "" {
		return
	}
	verb := utf16Ptr("open")
	file := utf16Ptr(path)
	pShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), 0, 0, swShowNorm)
}

// openExtensionFolder reveals the browser-extension folder that ships beside
// the executable.
func openExtensionFolder() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(exe)
	for _, cand := range []string{
		filepath.Join(dir, "chrome-extension"),
		filepath.Join(dir, "..", "chrome-extension"),
	} {
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			openPath(cand)
			return
		}
	}
	showMessageBox(0, "SuperIDM",
		"The chrome-extension folder was not found next to SuperIDM.exe.\n"+
			"It is part of the release archive - extract the whole archive before running the app.",
		mbOK|mbIconWarning)
}

// ---------------------------------------------------------------------------
// autostart (HKCU\Software\Microsoft\Windows\CurrentVersion\Run)
// ---------------------------------------------------------------------------

// SetAutoStart enables or disables launching SuperIDM with Windows.
func SetAutoStart(enable bool) error {
	key := utf16Ptr(`Software\Microsoft\Windows\CurrentVersion\Run`)
	var hkey uintptr
	hr, _, _ := pRegCreateKeyExW.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(key)),
		0, 0,
		regOptionNonVolatile,
		keySetValue,
		0,
		uintptr(unsafe.Pointer(&hkey)),
		0,
	)
	if int32(hr) != 0 {
		return fmt.Errorf("cannot open the Run key (code %d)", int32(hr))
	}
	defer pRegCloseKey.Call(hkey)

	name := utf16Ptr("SuperIDM")
	if !enable {
		pRegDeleteValueW.Call(hkey, uintptr(unsafe.Pointer(name)))
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	value := utf16FromString(`"` + exe + `" --tray`)
	sz := uintptr(len(value) * 2)
	r, _, _ := pRegSetValueExW.Call(
		hkey,
		uintptr(unsafe.Pointer(name)),
		0,
		regSZ,
		uintptr(unsafe.Pointer(&value[0])),
		sz,
	)
	if int32(r) != 0 {
		return fmt.Errorf("cannot write the autostart value (code %d)", int32(r))
	}
	return nil
}

// IsAutoStartEnabled reports whether the Run entry exists.
func IsAutoStartEnabled() bool {
	key := utf16Ptr(`Software\Microsoft\Windows\CurrentVersion\Run`)
	var hkey uintptr
	hr, _, _ := pRegCreateKeyExW.Call(
		hkeyCurrentUser, uintptr(unsafe.Pointer(key)), 0, 0, regOptionNonVolatile, 0x0001 /*KEY_READ*/, 0,
		uintptr(unsafe.Pointer(&hkey)), 0)
	if int32(hr) != 0 {
		return false
	}
	defer pRegCloseKey.Call(hkey)
	var buf [1024]byte
	size := uintptr(len(buf))
	name := utf16Ptr("SuperIDM")
	r, _, _ := regQueryValueEx(hkey, uintptr(unsafe.Pointer(name)), 0, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	return int32(r) == 0
}

var pRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")

func regQueryValueEx(hkey, name, reserved, typ, data, size uintptr) (uintptr, uintptr, error) {
	r, r2, err := pRegQueryValueExW.Call(hkey, name, reserved, typ, data, size)
	return r, r2, err
}

// ---------------------------------------------------------------------------
// small utilities
// ---------------------------------------------------------------------------

func logToFile(format string, args ...any) {
	path := logFilePath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line := fmt.Sprintf(format, args...)
	_, _ = f.WriteString(fmt.Sprintf("%s  NATIVE %s\n", time.Now().Format("2006-01-02 15:04:05"), line))
}

func logFilePath() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "SuperIDM", "superidm.log")
}

// newTicker delivers a tick every d, dropping ticks when nobody is listening.
func newTicker(d time.Duration) <-chan struct{} {
	ch := make(chan struct{}, 1)
	go func() {
		t := time.NewTicker(d)
		defer t.Stop()
		for range t.C {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}()
	return ch
}
