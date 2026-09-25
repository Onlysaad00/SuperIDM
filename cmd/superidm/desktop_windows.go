//go:build windows

package main

import (
	"os"

	"github.com/Onlysaad00/SuperIDM/internal/api"
	"github.com/Onlysaad00/SuperIDM/internal/app"
	"github.com/Onlysaad00/SuperIDM/internal/engine"
	"github.com/Onlysaad00/SuperIDM/internal/service"
)

type runModeKind int

const (
	modeDesktop runModeKind = iota
	modeTray
	modeHeadless
)

// runMode decides how the app should present itself.
func runMode(tray, headless, desktop bool) runModeKind {
	switch {
	case headless:
		return modeHeadless
	case tray:
		return modeTray
	case desktop:
		return modeDesktop
	default:
		// Default on Windows is the full desktop window unless the user asked
		// for the tray.
		if s := os.Getenv("SUPERIDM_HEADLESS"); s == "1" {
			return modeHeadless
		}
		return modeDesktop
	}
}

// runDesktop starts the native shell.
func runDesktop(svc *service.Service, srv *api.Server, trayOnly bool) error {
	cfg := app.Config{
		Title:    "SuperIDM",
		Version:  svc.Version,
		URL:      srv.URL(),
		Port:     srv.Port(),
		TrayOnly: trayOnly || svc.Settings().StartMinimized,
		StartMin: svc.Settings().StartMinimized,
		AddURL: func(url string) error {
			_, err := svc.Add(engine.Options{URL: url})
			return err
		},
		PauseAll:  svc.PauseAll,
		ResumeAll: svc.ResumeAll,
		OpenFolder: func() {
			dir := svc.Settings().DownloadDir
			_ = os.MkdirAll(dir, 0o755)
			app.OpenPath(dir)
		},
		OnSpeedLimit: func(bps int64) { svc.SetGlobalSpeedLimit(bps) },
		Stats: func() (float64, int, int) {
			var speed float64
			var active, done int
			for _, j := range svc.List() {
				switch j.State {
				case engine.StateDownloading, engine.StateConnecting:
					speed += j.Speed
					active++
				case engine.StateCompleted:
					done++
				}
			}
			return speed, active, done
		},
	}

	// Keep the autostart registry entry in sync with the setting.
	_ = app.SetAutoStart(svc.Settings().AutoStart)

	return app.Run(cfg)
}
