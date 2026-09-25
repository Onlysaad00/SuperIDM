//go:build !windows

package main

import (
	"fmt"
	"os"

	"github.com/Onlysaad00/SuperIDM/internal/api"
	"github.com/Onlysaad00/SuperIDM/internal/service"
)

type runModeKind int

const (
	modeDesktop runModeKind = iota
	modeTray
	modeHeadless
)

func runMode(tray, headless, desktop bool) runModeKind { return modeHeadless }

// runDesktop is a no-op outside Windows: the API server keeps running so the
// UI can be opened in any browser at the printed address.
func runDesktop(svc *service.Service, srv *api.Server, trayOnly bool) error {
	fmt.Printf("SuperIDM %s\nOpen %s in your browser.\n", svc.Version, srv.URL())
	select {}
}

var _ = os.Stdout
