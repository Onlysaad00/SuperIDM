//go:build windows

package service

import (
	"errors"
	"syscall"
	"unsafe"
)

// Clipboard access through user32.dll directly. Shelling out to `cmd /c clip`
// would flash a console window on a GUI app, so the Win32 API is used instead.

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procGetClipboardData = user32.NewProc("GetClipboardData")
	procIsClipboardFmtAv = user32.NewProc("IsClipboardFormatAvailable")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

func copyToClipboard(text string) error {
	utf16, err := syscall.UTF16FromString(text)
	if err != nil {
		return err
	}
	size := uintptr(len(utf16) * 2)

	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return errors.New("cannot open the clipboard")
	}
	defer procCloseClipboard.Call()

	if r, _, _ := procEmptyClipboard.Call(); r == 0 {
		return errors.New("cannot empty the clipboard")
	}
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return errors.New("out of memory allocating clipboard buffer")
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		procGlobalFree.Call(h)
		return errors.New("cannot lock the clipboard buffer")
	}
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), len(utf16))
	copy(dst, utf16)
	procGlobalUnlock.Call(h)

	if r, _, _ := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		procGlobalFree.Call(h)
		return errors.New("cannot place text on the clipboard")
	}
	// Ownership of h transferred to the clipboard.
	return nil
}

func clipboardText() (string, error) {
	if r, _, _ := procIsClipboardFmtAv.Call(cfUnicodeText); r == 0 {
		return "", nil
	}
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return "", errors.New("cannot open the clipboard")
	}
	defer procCloseClipboard.Call()

	h, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", nil
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return "", nil
	}
	defer procGlobalUnlock.Call(h)

	var out []uint16
	for i := 0; ; i++ {
		c := *(*uint16)(unsafe.Pointer(ptr + uintptr(i*2)))
		if c == 0 {
			break
		}
		out = append(out, c)
		if i > 1<<20 {
			break
		}
	}
	return syscall.UTF16ToString(out), nil
}
