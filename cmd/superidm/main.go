// Command superidm is the SuperIDM desktop application.
//
// On Windows it runs as a GUI app: a native window hosting the embedded UI,
// a tray icon, and a loopback REST API that the bundled browser extension (and
// any other tool) can drive:
//
//	superidm                          start the app
//	superidm --tray                   start minimised to the tray (autostart)
//	superidm --headless               no window, no tray: API only (for scripts)
//	superidm -d URL                   add a download and exit
//	superidm -p URL -o file.zip       add a download with an explicit name
//	superidm -c URL -n 64             add a download with N connections
//	superidm --inspect URL            print what the server reports, then exit
//	superidm --version                print the version
//
// The same binary runs on other platforms in headless/CLI mode, which is how
// the integration tests drive it during development.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Onlysaad00/SuperIDM/internal/api"
	"github.com/Onlysaad00/SuperIDM/internal/engine"
	"github.com/Onlysaad00/SuperIDM/internal/service"
)

// Version is stamped at build time with:
//
//	-ldflags "-X main.Version=v1.0.0"
var Version = "1.0.0"

func main() {
	var (
		flagTray     = flag.Bool("tray", false, "start minimised to the system tray")
		flagHeadless = flag.Bool("headless", false, "run without any window (API only)")
		flagDesktop  = flag.Bool("desktop", false, "force the full desktop window (default on Windows)")
		flagAdd      = flag.String("d", "", "add a download for this URL")
		flagOut      = flag.String("o", "", "file name or full path for -d")
		flagConns    = flag.Int("n", 0, "connections for -d (default from settings, 64)")
		flagDir      = flag.String("dir", "", "download folder for -d")
		flagReferer  = flag.String("referer", "", "Referer header for -d")
		flagCookie   = flag.String("cookie", "", "Cookie header for -d")
		flagChecksum = flag.String("checksum", "", "md5:/sha1:/sha256: digest to verify after -d")
		flagPaused   = flag.Bool("paused", false, "add the download in the paused state")
		flagInspect  = flag.String("inspect", "", "probe a URL and print its metadata")
		flagPort     = flag.Int("port", 0, "override the local API port")
		flagVersion  = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = usage
	flag.Parse()

	if *flagVersion {
		fmt.Printf("SuperIDM %s\n", Version)
		return
	}

	svc := service.New(Version)
	defer svc.Close()

	// --inspect is a one-shot probe.
	if *flagInspect != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		info := svc.Inspect(ctx, *flagInspect, *flagReferer, *flagCookie)
		printInspect(info)
		if info.Error != "" {
			os.Exit(1)
		}
		return
	}

	settings := svc.Settings()
	port := settings.Port
	if *flagPort > 0 {
		port = *flagPort
	}

	// Add a download from the command line.
	if *flagAdd != "" {
		opts := engine.Options{
			URL:         *flagAdd,
			Dir:         *flagDir,
			FileName:    *flagOut,
			Connections: *flagConns,
			Referer:     *flagReferer,
			Cookie:      *flagCookie,
			Checksum:    *flagChecksum,
			StartPaused: *flagPaused,
		}
		if *flagOut != "" && (strings.ContainsAny(*flagOut, `/\`) || strings.Contains(*flagOut, ":")) {
			opts.SavePath = *flagOut
		}
		snap, err := svc.Add(opts)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Printf("added %s -> %s\n", displayName(snap), snap.Dir)

		if *flagHeadless || *flagTray || *flagDesktop {
			// Fall through and keep the app running so the user can watch it.
		} else {
			// Wait for the transfer to finish, then exit: this makes
			// "superidm -d URL" behave like a normal CLI downloader.
			waitForJob(svc, snap.ID)
			svc.Close()
			final, _ := svc.Get(snap.ID)
			if final.State == engine.StateCompleted {
				fmt.Printf("saved to %s (%s)\n", final.SavePath, engine.HumanBytes(final.Done))
				return
			}
			fmt.Fprintf(os.Stderr, "download failed: %s\n", final.Error)
			os.Exit(1)
		}
	}

	if !settings.EnableAPI {
		fmt.Fprintln(os.Stderr, "The local API is disabled in the settings; enable it in the app to use the extension.")
	}

	srv := api.New(svc, api.Options{Port: port})
	if err := srv.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer srv.Stop()

	svc.Log("info", fmt.Sprintf("SuperIDM %s ready on %s", Version, srv.URL()))

	mode := runMode(*flagTray, *flagHeadless, *flagDesktop)
	if mode == modeHeadless {
		fmt.Printf("SuperIDM %s listening on %s (Ctrl+C to stop)\n", Version, srv.URL())
		select {}
	}

	// Desktop shell.
	if err := runDesktop(svc, srv, mode == modeTray); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func waitForJob(svc *service.Service, id string) {
	for {
		snap, ok := svc.Get(id)
		if !ok {
			return
		}
		if snap.State.IsFinal() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func printInspect(info engine.HeadInfo) {
	if info.Error != "" {
		fmt.Printf("x %s\n", info.Error)
		return
	}
	size := "unknown"
	if info.Size > 0 {
		size = fmt.Sprintf("%s (%d bytes)", engine.HumanBytes(info.Size), info.Size)
	}
	resumable := "no (single connection)"
	if info.Ranges {
		resumable = "yes (parallel download)"
	}
	fmt.Printf(`%s
  final URL   : %s
  file name   : %s
  size        : %s
  content type: %s
  kind        : %s
  resumable   : %s
`, "OK", info.FinalURL, info.FileName, size, orDash(info.ContentType), info.Kind, resumable)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func displayName(s engine.Snapshot) string {
	if s.FileName != "" {
		return s.FileName
	}
	return s.URL
}

func usage() {
	fmt.Fprintf(os.Stderr, `SuperIDM %s - multi-connection download accelerator

Usage:
  superidm                      open the app (window + tray + local API)
  superidm --tray               start hidden in the system tray
  superidm --headless           run the API without any window
  superidm -d URL [options]     download a URL from the command line
  superidm --inspect URL        show what the server reports about a link

Download options (-d):
  -o NAME|PATH     file name, or an absolute path to save to
  -dir FOLDER      destination folder (defaults to the configured folder)
  -n N             parallel connections (1-128, default 64)
  -referer URL     Referer header (needed by some hosts)
  -cookie STRING   Cookie header
  -checksum SPEC   md5:... sha1:... sha256:... to verify the result
  -paused          add it to the queue without starting

Other options:
  -port N          override the local API port (default from settings)
  -version         print the version

Links are also accepted as plain arguments: superidm "https://..." .
`, Version)
}
