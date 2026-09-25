// Package api exposes the local SuperIDM control interface.
//
// It binds to 127.0.0.1 only and is used by three clients:
//   - the SuperIDM window/tray app (the embedded web UI),
//   - the Chrome/Edge/Firefox extension,
//   - scripts and installers.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Onlysaad00/SuperIDM/internal/engine"
	"github.com/Onlysaad00/SuperIDM/internal/service"
)

//go:embed web/*
var webFS embed.FS

// Server hosts the REST API and the embedded UI.
type Server struct {
	svc    *service.Service
	http   *http.Server
	ln     net.Listener
	port   int
	token  string
	sniff  *Sniffer
	logger *log.Logger
}

// Options configures the local server.
type Options struct {
	Port  int
	Token string
	Quiet bool
}

// New creates the API server.
func New(svc *service.Service, opts Options) *Server {
	s := &Server{
		svc:    svc,
		port:   opts.Port,
		token:  opts.Token,
		sniff:  NewSniffer(svc),
		logger: log.New(io.Discard, "", 0),
	}
	s.http = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: the SSE stream is long lived.
		IdleTimeout: 120 * time.Second,
	}
	return s
}

// Port returns the bound port (after Start).
func (s *Server) Port() int { return s.port }

// URL returns the local UI address.
func (s *Server) URL() string { return fmt.Sprintf("http://127.0.0.1:%d/", s.port) }

// Sniffer exposes the captured-media store.
func (s *Server) Sniffer() *Sniffer { return s.sniff }

// Start binds the socket. If the preferred port is busy the next few ports are
// tried so two installs can coexist.
func (s *Server) Start() error {
	var lastErr error
	for p := s.port; p < s.port+10; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			lastErr = err
			continue
		}
		s.ln = ln
		// Report the port the OS actually gave us (relevant when Port is 0,
		// which asks for any free port).
		if addr, ok := ln.Addr().(*net.TCPAddr); ok {
			s.port = addr.Port
		} else {
			s.port = p
		}
		p = s.port
		go func() {
			if err := s.http.Serve(ln); err != nil && err != http.ErrServerClosed {
				s.logger.Printf("api server stopped: %v", err)
			}
		}()
		return nil
	}
	return fmt.Errorf("cannot bind 127.0.0.1:%d-%d: %w", s.port, s.port+9, lastErr)
}

// Stop shuts the server down.
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.http.Shutdown(ctx)
}

// ---------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/limits", s.handleLimits)
	mux.HandleFunc("GET /api/jobs", s.handleListJobs)
	mux.HandleFunc("POST /api/jobs", s.handleAddJob)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("POST /api/jobs/{id}/{action}", s.handleJobAction)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleRemoveJob)
	mux.HandleFunc("POST /api/jobs/actions/{action}", s.handleBulkAction)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("POST /api/settings", s.handleSetSettings)
	mux.HandleFunc("POST /api/speedlimit", s.handleSpeedLimit)
	mux.HandleFunc("GET /api/inspect", s.handleInspect)
	mux.HandleFunc("POST /api/inspect", s.handleInspect)
	mux.HandleFunc("POST /api/clipboard", s.handleClipboard)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/log", s.handleLog)
	mux.HandleFunc("GET /api/extension-path", s.handleExtensionPath)

	// Browser-extension endpoints for media (video/audio) capture.
	mux.HandleFunc("POST /api/sniff", s.handleSniff)
	mux.HandleFunc("GET /api/sniff", s.handleSniffList)
	mux.HandleFunc("DELETE /api/sniff", s.handleSniffClear)
	mux.HandleFunc("POST /api/sniff/remove", s.handleSniffRemove)
	mux.HandleFunc("POST /api/sniff/download", s.handleSniffDownload)
	mux.HandleFunc("POST /api/sniff/clear", s.handleSniffClear)

	return s.cors(mux)
}

// cors reflects only extension origins and localhost, then enforces a strict
// content type on state-changing requests. A random website cannot send
// application/json cross-origin without a preflight, so this blocks the
// classic "any page can drive your localhost API" attack.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-SuperIDM-Token")
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/events" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func allowedOrigin(origin string) bool {
	switch {
	case strings.HasPrefix(origin, "chrome-extension://"),
		strings.HasPrefix(origin, "moz-extension://"),
		strings.HasPrefix(origin, "safari-web-extension://"),
		strings.HasPrefix(origin, "ms-browser-extension://"):
		return true
	case strings.HasPrefix(origin, "http://127.0.0.1:"),
		strings.HasPrefix(origin, "http://localhost:"),
		strings.HasPrefix(origin, "https://127.0.0.1:"),
		strings.HasPrefix(origin, "https://localhost:"):
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"version": s.svc.Version,
		"app":     "SuperIDM",
		"time":    time.Now().UnixMilli(),
	})
}

func (s *Server) handleLimits(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.svc.Limits())
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"jobs": s.svc.List()})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	snap, ok := s.svc.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "no such download")
		return
	}
	writeJSON(w, 200, snap)
}

// addRequest is the payload accepted from the UI and the extension.
type addRequest struct {
	URL         string `json:"url"`
	FileName    string `json:"fileName"`
	Dir         string `json:"dir"`
	Category    string `json:"category"`
	Referer     string `json:"referer"`
	Cookie      string `json:"cookie"`
	Checksum    string `json:"checksum"`
	Kind        string `json:"kind"`
	MediaTitle  string `json:"mediaTitle"`
	PageURL     string `json:"pageUrl"`
	Connections int    `json:"connections"`
	StartPaused bool   `json:"startPaused"`
	OnlyVideo   bool   `json:"onlyVideo"`
	Silent      bool   `json:"silent"`
}

func (s *Server) handleAddJob(w http.ResponseWriter, r *http.Request) {
	var req addRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid JSON body: "+err.Error())
		return
	}
	opts := engine.Options{
		URL:         req.URL,
		FileName:    req.FileName,
		Dir:         req.Dir,
		Category:    req.Category,
		Referer:     req.Referer,
		Cookie:      req.Cookie,
		Checksum:    req.Checksum,
		Kind:        req.Kind,
		MediaTitle:  req.MediaTitle,
		Connections: req.Connections,
		StartPaused: req.StartPaused,
	}
	if opts.Referer == "" {
		opts.Referer = req.PageURL
	}
	snap, err := s.svc.Add(opts)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.svc.Log("info", fmt.Sprintf("queued from %s: %s", clientName(r), snap.URL))
	writeJSON(w, 200, snap)
}

func (s *Server) handleJobAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	action := r.PathValue("action")
	var err error
	switch action {
	case "pause":
		err = s.svc.Pause(id)
	case "resume", "start":
		err = s.svc.Resume(id)
	case "cancel":
		err = s.svc.Cancel(id)
	case "open":
		err = s.svc.OpenPath(id)
	case "reveal", "folder":
		err = s.svc.RevealPath(id)
	case "restart":
		err = s.svc.Restart(id)
	default:
		writeErr(w, 400, "unknown action "+action)
		return
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleBulkAction(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("action") {
	case "pause-all":
		s.svc.PauseAll()
	case "resume-all":
		s.svc.ResumeAll()
	case "clear-completed":
		del := r.URL.Query().Get("delete") == "1"
		s.svc.ClearCompleted(del)
	default:
		writeErr(w, 400, "unknown bulk action")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleRemoveJob(w http.ResponseWriter, r *http.Request) {
	del := r.URL.Query().Get("delete") == "1"
	if err := s.svc.Remove(r.PathValue("id"), del); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"settings":  s.svc.Settings(),
		"limits":    s.svc.Limits(),
		"configDir": service.ConfigDir(),
		"port":      s.port,
	})
}

func (s *Server) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var set service.Settings
	if err := decodeJSON(r, &set); err != nil {
		writeErr(w, 400, "invalid JSON body: "+err.Error())
		return
	}
	out := s.svc.SaveSettings(set)
	writeJSON(w, 200, out)
}

func (s *Server) handleSpeedLimit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID  string `json:"id"`
		Bps int64  `json:"bps"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, 400, "invalid JSON body")
		return
	}
	if body.ID == "" {
		s.svc.SetGlobalSpeedLimit(body.Bps)
	} else {
		s.svc.SetSpeedLimit(body.ID, body.Bps)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("url")
	if r.Method == http.MethodPost {
		var body struct {
			URL string `json:"url"`
		}
		_ = decodeJSON(r, &body)
		if body.URL != "" {
			target = body.URL
		}
	}
	if target == "" {
		writeErr(w, 400, "url is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	info := s.svc.Inspect(ctx, target, r.URL.Query().Get("referer"), r.URL.Query().Get("cookie"))
	writeJSON(w, 200, info)
}

func (s *Server) handleClipboard(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	_ = decodeJSON(r, &body)
	if err := s.svc.CopyText(body.Text); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	data, err := readTail(s.svc.LogFilePath(), 128<<10)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

// handleExtensionPath reports where the bundled browser extension lives so the
// setup page can point the user straight at it.
func (s *Server) handleExtensionPath(w http.ResponseWriter, r *http.Request) {
	exe, err := os.Executable()
	if err != nil {
		writeErr(w, 500, "cannot locate the executable")
		return
	}
	dir := filepath.Dir(exe)
	candidates := []string{
		filepath.Join(dir, "chrome-extension"),
		filepath.Join(dir, "..", "chrome-extension"),
		filepath.Join(dir, "SuperIDM-extension"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			abs, _ := filepath.Abs(c)
			writeJSON(w, 200, map[string]any{"path": abs})
			return
		}
	}
	writeErr(w, 404, "chrome-extension folder not found next to the executable")
}

// handleEvents streams state to the UI as Server-Sent Events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	ch, cancel := s.svc.Subscribe()
	defer cancel()

	// Prime the client with the current state.
	if data, err := json.Marshal(service.Event{Type: "jobs", Jobs: s.svc.List(), Time: time.Now().UnixMilli()}); err == nil {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	keep := time.NewTicker(15 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keep.C:
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	return dec.Decode(v)
}

func clientName(r *http.Request) string {
	if o := r.Header.Get("Origin"); o != "" {
		return o
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func readTail(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	off := int64(0)
	if size > int64(max) {
		off = size - int64(max)
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}
