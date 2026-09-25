//go:build !windows

package service

import (
	"bytes"
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// Non-Windows fallbacks, used for local development and the test suite.

func copyToClipboard(text string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	default:
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		} else if _, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command("wl-copy")
		} else {
			return errors.New("no clipboard tool available")
		}
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

func clipboardText() (string, error) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("pbpaste")
	default:
		if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard", "-o")
		} else if _, err := exec.LookPath("wl-paste"); err == nil {
			cmd = exec.Command("wl-paste", "--no-newline")
		} else {
			return "", errors.New("no clipboard tool available")
		}
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return buf.String(), nil
}
