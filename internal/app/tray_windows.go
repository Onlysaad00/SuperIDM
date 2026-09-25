//go:build windows

package app

import (
	"sync"
	"unsafe"
)

// System tray icon: adds an icon next to the clock, a right-click menu and
// balloon notifications. Implemented over Shell_NotifyIconW.

type tray struct {
	hwnd       uintptr
	data       notifyIconDataW
	icon       uintptr
	added      bool
	menu       uintptr
	taskbarMsg uint32
	mu         sync.Mutex
	onCommand  func(cmd int)
	tooltip    string
}

func newTray(hwnd uintptr, onCommand func(int)) *tray {
	t := &tray{hwnd: hwnd, onCommand: onCommand}
	t.data.CbSize = uint32(unsafe.Sizeof(t.data))
	t.data.Hwnd = hwnd
	t.data.UID = 1
	t.data.UFlags = nifMsg | nifIcon | nifTip
	t.data.UCallbackMessage = wmTrayIcon
	t.tooltip = "SuperIDM"
	t.data.setTip(t.tooltip)
	t.data.HIcon = createTrayIcon()
	t.icon = t.data.HIcon

	// Explorer broadcasts TaskbarCreated when it restarts; re-add the icon then.
	name := utf16Ptr("TaskbarCreated")
	r, _, _ := pRegisterWindowMsg.Call(uintptr(unsafe.Pointer(name)))
	t.taskbarMsg = uint32(r)

	t.mu.Lock()
	t.add()
	t.mu.Unlock()
	return t
}

// add registers the icon with the shell.
func (t *tray) add() {
	if t.added {
		return
	}
	if r, _, _ := pShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&t.data))); r != 0 {
		t.added = true
	}
}

func (t *tray) remove() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.added {
		return
	}
	pShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&t.data)))
	t.added = false
}

// setTooltip updates the hover text, e.g. to show the live transfer rate.
func (t *tray) setTooltip(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s == "" || s == t.tooltip {
		return
	}
	t.tooltip = s
	t.data.setTip(s)
	if t.added {
		pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&t.data)))
	}
}

// notify shows a balloon message.
func (t *tray) notify(title, text string, warning bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	flags := uint32(niifInfo)
	if warning {
		flags = niifWarning
	}
	t.data.UFlags = nifMsg | nifIcon | nifTip | nifInfo
	t.data.setInfo(text)
	t.data.setInfoTitle(title)
	t.data.DwInfoFlags = flags
	if t.added {
		pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&t.data)))
	}
	t.data.UFlags = nifMsg | nifIcon | nifTip
}

// showMenu builds and displays the context menu; returns the chosen command.
func (t *tray) showMenu() {
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)

	add := func(id uintptr, label string) {
		pAppendMenuW.Call(menu, mfString, id, uintptr(unsafe.Pointer(utf16Ptr(label))))
	}
	add(cmdShow, "Open SuperIDM")
	pAppendMenuW.Call(menu, mfSeparator, 0, 0)
	add(cmdAddURL, "Add new download…")
	add(cmdPauseAll, "Pause all")
	add(cmdResumeAll, "Resume all")
	pAppendMenuW.Call(menu, mfSeparator, 0, 0)
	add(cmdSpeedLimit, "Speed limiter")
	pAppendMenuW.Call(menu, mfSeparator, 0, 0)
	add(cmdOpenFolder, "Open download folder")
	add(cmdExtFolder, "Open extension folder")
	add(cmdLogFile, "Open log file")
	pAppendMenuW.Call(menu, mfSeparator, 0, 0)
	add(cmdAbout, "About SuperIDM")
	add(cmdExit, "Exit")

	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Required so the menu closes when the user clicks elsewhere.
	pSetForegroundWindow.Call(t.hwnd)
	r, _, _ := pTrackPopupMenu.Call(menu, tpmRightBtn|tpmRetCmd, uintptr(pt.X), uintptr(pt.Y), 0, t.hwnd, 0)
	pPostMessageW.Call(t.hwnd, 0 /*WM_NULL*/, 0, 0)
	if int32(r) > 0 && t.onCommand != nil {
		t.onCommand(int(r))
	}
}

// handleMessage reports whether a window message belonged to the tray icon.
func (t *tray) handleMessage(m uint32, wparam, lparam uintptr) bool {
	if t.taskbarMsg != 0 && m == t.taskbarMsg {
		t.mu.Lock()
		t.added = false
		t.add()
		t.mu.Unlock()
		return true
	}
	if m != wmTrayIcon {
		return false
	}
	switch lowWord(lparam) {
	case wmLButtonUp:
		if t.onCommand != nil {
			t.onCommand(cmdShow)
		}
		return true
	case wmRButtonUp:
		t.showMenu()
		return true
	}
	return false
}

// createTrayIcon turns the embedded ICO bytes into a real HICON.
func createTrayIcon() uintptr {
	if len(trayIconICO) == 0 {
		ic, _, _ := pLoadImageW.Call(0, idiApplication, 1 /*IMAGE_ICON*/, 0, 0, 0x00008000 /*LR_SHARED*/)
		return ic
	}
	// Skip the ICONDIR and use the first entry's image data.
	// ICONDIR: 6 bytes header, then 16 bytes per entry.
	if len(trayIconICO) < 22 {
		return 0
	}
	entry := trayIconICO[6:22]
	size := uint32(entry[8]) | uint32(entry[9])<<8 | uint32(entry[10])<<16 | uint32(entry[11])<<24
	off := uint32(entry[12]) | uint32(entry[13])<<8 | uint32(entry[14])<<16 | uint32(entry[15])<<24
	if int(off)+int(size) > len(trayIconICO) || size == 0 {
		return 0
	}
	block := trayIconICO[off : off+size]
	// HICON CreateIconFromResourceEx(PBYTE, DWORD size, BOOL fIcon,
	//                                DWORD ver, int cx, int cy, UINT flags)
	icon, _, _ := pCreateIconFromRes.Call(
		uintptr(unsafe.Pointer(&block[0])),
		uintptr(size),
		1,          // fIcon = TRUE
		0x00030000, // version
		0, 0,       // default dimensions
		0, // LR_DEFAULTCOLOR
	)
	return icon
}
