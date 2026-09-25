// Package service is the application layer: it owns the job list, the
// scheduler, settings, persistence, the log file and the event bus that feeds
// the UI and any interested browser extension.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Onlysaad00/SuperIDM/internal/engine"
)

// Event is a message pushed to UI subscribers.
type Event struct {
	Type    string            `json:"type"` // "jobs" | "log" | "toast" | "settings"
	Jobs    []engine.Snapshot `json:"jobs,omitempty"`
	Message string            `json:"message,omitempty"`
	Level   string            `json:"level,omitempty"`
	Time    int64             `json:"time,omitempty"`
	Title   string            `json:"title,omitempty"`
}

// Limits describes the tunables the UI needs for its sliders.
type Limits struct {
	MaxConnections int    `json:"maxConnections"`
	DefaultConns   int    `json:"defaultConns"`
	MinChunkKB     int    `json:"minChunkKB"`
	MaxChunkKB     int    `json:"maxChunkKB"`
	Version        string `json:"version"`
}

// Service is the single owner of all download state.
type Service struct {
	Version string

	settings *settingsStore

	mu    sync.RWMutex
	jobs  map[string]*entry
	order []string

	subsMu sync.Mutex
	subs   map[int]chan Event
	subID  int

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stateU  string
	logFile *os.File
	logMu   sync.Mutex

	dirty chan struct{}

	// OnAllDone fires when the last active download finishes (used for tray
	// notifications).
	OnAllDone func()
}

type entry struct {
	job      *engine.Job
	opts     engine.Options
	snapshot engine.Snapshot
	started  bool
	removed  bool
}

// New creates the service and restores the previous session.
func New(version string) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		Version:  version,
		settings: newSettingsStore(),
		jobs:     map[string]*entry{},
		subs:     map[int]chan Event{},
		ctx:      ctx,
		cancel:   cancel,
		stateU:   filepath.Join(ConfigDir(), "state.json"),
		dirty:    make(chan struct{}, 1),
	}
	s.openLog()
	engine.Logf = func(format string, args ...any) { s.Log("info", fmt.Sprintf(format, args...)) }
	s.restore()

	s.wg.Add(1)
	go s.tickLoop()
	return s
}

// Close stops everything and writes the session to disk.
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
	s.persist()
	s.logMu.Lock()
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
	s.logMu.Unlock()
}

// Context returns the service lifetime context.
func (s *Service) Context() context.Context { return s.ctx }

// ---------------------------------------------------------------------------
// settings
// ---------------------------------------------------------------------------

// Settings returns the current settings.
func (s *Service) Settings() Settings { return s.settings.get() }

// SaveSettings validates, stores and broadcasts new settings.
func (s *Service) SaveSettings(in Settings) Settings {
	s.settings.set(in)
	out := s.settings.get()
	for _, e := range s.snapshots() {
		if e.State == engine.StateDownloading || e.State == engine.StateConnecting {
			if j := s.entryJob(e.ID); j != nil {
				j.SetSpeedLimit(out.SpeedLimitBps)
			}
		}
	}
	s.broadcast(Event{Type: "settings"})
	return out
}

// Limits returns the tunable ranges for the UI.
func (s *Service) Limits() Limits {
	return Limits{
		MaxConnections: engine.MaxConnections,
		DefaultConns:   engine.DefaultConnections,
		MinChunkKB:     32,
		MaxChunkKB:     16 * 1024,
		Version:        s.Version,
	}
}

// ---------------------------------------------------------------------------
// job list
// ---------------------------------------------------------------------------

func (s *Service) entryJob(id string) *engine.Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.jobs[id]; ok {
		return e.job
	}
	return nil
}

// List returns snapshots ordered by insertion time (newest first).
func (s *Service) List() []engine.Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]engine.Snapshot, 0, len(s.order))
	for _, id := range s.order {
		if e, ok := s.jobs[id]; ok && !e.removed {
			out = append(out, e.snapshot)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].AddedAt > out[b].AddedAt })
	return out
}

func (s *Service) snapshots() []engine.Snapshot { return s.List() }

// Get returns a single snapshot.
func (s *Service) Get(id string) (engine.Snapshot, bool) {
	for _, snap := range s.List() {
		if snap.ID == id {
			return snap, true
		}
	}
	return engine.Snapshot{}, false
}

// Add creates a download. It returns as soon as the job is registered; the
// transfer itself continues in the background.
func (s *Service) Add(opts engine.Options) (engine.Snapshot, error) {
	opts.URL = strings.TrimSpace(opts.URL)
	if opts.URL == "" {
		return engine.Snapshot{}, errors.New("a URL is required")
	}
	if !strings.HasPrefix(strings.ToLower(opts.URL), "http://") && !strings.HasPrefix(strings.ToLower(opts.URL), "https://") {
		return engine.Snapshot{}, errors.New("only http:// and https:// links are supported")
	}
	set := s.settings.get()
	opts = set.toOptions(opts)
	if opts.Kind == "" {
		if strings.Contains(strings.ToLower(opts.URL), ".m3u8") {
			opts.Kind = "hls"
		} else {
			opts.Kind = "http"
		}
	}

	id := newID()
	limiter := engine.NewRateLimiter(set.SpeedLimitBps)
	job := engine.NewJob(id, opts, limiter, nil)

	e := &entry{job: job, opts: opts, snapshot: job.Snapshot()}
	e.snapshot.ID = id
	e.snapshot.URL = opts.URL
	if e.snapshot.FileName == "" {
		e.snapshot.FileName = engine.GuessFileName(opts.URL, "")
	}
	if opts.StartPaused {
		job.MarkPaused()
		e.snapshot = job.Snapshot()
		e.snapshot.ID = id
		e.snapshot.URL = opts.URL
	}

	s.mu.Lock()
	s.jobs[id] = e
	s.order = append(s.order, id)
	s.mu.Unlock()

	s.Log("info", fmt.Sprintf("added %s (%d connections)", displayName(e.snapshot), opts.Connections))
	s.persist()
	if !opts.StartPaused {
		s.startJob(id)
	}
	s.publish()
	return e.snapshot, nil
}

// startJob respects the MaxConcurrent setting by leaving jobs queued.
func (s *Service) startJob(id string) {
	s.mu.Lock()
	e, ok := s.jobs[id]
	if !ok || e.removed {
		s.mu.Unlock()
		return
	}
	running := 0
	for _, other := range s.jobs {
		if other == e || other.removed {
			continue
		}
		if other.snapshot.State == engine.StateDownloading || other.snapshot.State == engine.StateConnecting {
			running++
		}
	}
	limit := s.settings.get().MaxConcurrent
	if running >= limit {
		e.snapshot.State = engine.StateQueued
		s.mu.Unlock()
		s.publish()
		return
	}
	e.started = true
	job := e.job
	e.snapshot.State = engine.StateConnecting
	s.mu.Unlock()

	s.publish()
	job.Start(s.ctx)
}

// nextQueued starts the oldest queued job, if there is capacity.
func (s *Service) nextQueued() {
	s.mu.RLock()
	type cand struct {
		id string
		at int64
	}
	var c []cand
	for id, e := range s.jobs {
		if e.removed {
			continue
		}
		if e.snapshot.State == engine.StateQueued {
			c = append(c, cand{id, e.snapshot.AddedAt})
		}
	}
	s.mu.RUnlock()
	if len(c) == 0 {
		return
	}
	sort.Slice(c, func(a, b int) bool { return c[a].at < c[b].at })
	s.startJob(c[0].id)
}

// Pause pauses a single job.
func (s *Service) Pause(id string) error {
	j := s.entryJob(id)
	if j == nil {
		return errors.New("no such download")
	}
	st := j.State()
	if st.IsFinal() || st == engine.StatePaused {
		return nil
	}
	go j.Pause()
	return nil
}

// Resume restarts a paused/errored/queued job.
func (s *Service) Resume(id string) error {
	s.mu.RLock()
	e, ok := s.jobs[id]
	s.mu.RUnlock()
	if !ok {
		return errors.New("no such download")
	}
	st := e.job.State()
	if st == engine.StateCompleted || st == engine.StateCanceled {
		return errors.New("that download has already finished")
	}
	s.startJob(id)
	return nil
}

// PauseAll pauses everything in flight.
func (s *Service) PauseAll() {
	for _, snap := range s.List() {
		_ = s.Pause(snap.ID)
	}
}

// ResumeAll resumes every paused job.
func (s *Service) ResumeAll() {
	for _, snap := range s.List() {
		if snap.State == engine.StatePaused || snap.State == engine.StateError {
			_ = s.Resume(snap.ID)
		}
	}
}

// Cancel aborts a job and deletes the partial file.
func (s *Service) Cancel(id string) error {
	j := s.entryJob(id)
	if j == nil {
		return errors.New("no such download")
	}
	go j.Cancel()
	return nil
}

// Remove drops a job from the list, optionally deleting the file on disk.
func (s *Service) Remove(id string, deleteFile bool) error {
	s.mu.Lock()
	e, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return errors.New("no such download")
	}
	e.removed = true
	path := e.snapshot.SavePath
	j := e.job
	s.mu.Unlock()

	go j.Cancel()
	if deleteFile && path != "" {
		_ = os.Remove(path)
	}
	s.mu.Lock()
	delete(s.jobs, id)
	for i, oid := range s.order {
		if oid == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	s.persist()
	s.publish()
	s.nextQueued()
	return nil
}

// ClearCompleted removes every finished job (and optionally their files).
func (s *Service) ClearCompleted(deleteFiles bool) {
	for _, snap := range s.List() {
		if snap.State == engine.StateCompleted {
			_ = s.Remove(snap.ID, deleteFiles)
		}
	}
}

// Restart throws away the partial file and the resume map, then downloads the
// job again from the beginning.
func (s *Service) Restart(id string) error {
	s.mu.RLock()
	e, ok := s.jobs[id]
	s.mu.RUnlock()
	if !ok {
		return errors.New("no such download")
	}
	st := e.job.State()
	if st == engine.StateDownloading || st == engine.StateConnecting || st == engine.StateVerifying {
		return errors.New("pause the download before restarting it")
	}
	if e.snapshot.SavePath != "" {
		_ = os.Remove(e.snapshot.SavePath)
	}
	fresh := engine.NewJob(id, e.opts, engine.NewRateLimiter(e.opts.SpeedLimit), nil)
	s.mu.Lock()
	e.job = fresh
	e.snapshot = fresh.Snapshot()
	e.snapshot.ID = id
	e.snapshot.URL = e.opts.URL
	e.snapshot.AddedAt = time.Now().UnixMilli()
	s.mu.Unlock()
	s.startJob(id)
	return nil
}

// SetSpeedLimit overrides the cap for one running job.
func (s *Service) SetSpeedLimit(id string, bps int64) {
	if j := s.entryJob(id); j != nil {
		j.SetSpeedLimit(bps)
	}
}

// SetGlobalSpeedLimit updates the setting and applies it to running jobs.
func (s *Service) SetGlobalSpeedLimit(bps int64) {
	set := s.settings.get()
	set.SpeedLimitBps = bps
	s.SaveSettings(set)
}

// ---------------------------------------------------------------------------
// file helpers
// ---------------------------------------------------------------------------

// OpenPath opens a downloaded file with the default application.
func (s *Service) OpenPath(id string) error {
	snap, ok := s.Get(id)
	if !ok || snap.SavePath == "" {
		return errors.New("that download has no file yet")
	}
	return openWithShell(snap.SavePath)
}

// RevealPath opens the containing folder and selects the file.
func (s *Service) RevealPath(id string) error {
	snap, ok := s.Get(id)
	if !ok || snap.SavePath == "" {
		return errors.New("that download has no file yet")
	}
	return revealWithShell(snap.SavePath)
}

// CopyText is a hook the UI uses for "copy link".
func (s *Service) CopyText(text string) error { return copyToClipboard(text) }

// Inspect probes a URL without downloading it.
func (s *Service) Inspect(ctx context.Context, url, referer, cookie string) engine.HeadInfo {
	return engine.InspectURL(ctx, url, engine.Options{
		Referer:   referer,
		Cookie:    cookie,
		UserAgent: s.settings.get().UserAgent,
		Kind:      "http",
	})
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

type persistedJob struct {
	Options  engine.Options  `json:"options"`
	Snapshot engine.Snapshot `json:"snapshot"`
	Resume   []engine.Range  `json:"resume"`
}

type persistedState struct {
	Saved int64          `json:"saved"`
	Jobs  []persistedJob `json:"jobs"`
	Log   []string       `json:"-"`
}

// persist writes the session so a restart can continue where it left off.
func (s *Service) persist() {
	s.mu.RLock()
	out := persistedState{Saved: time.Now().UnixMilli()}
	for _, id := range s.order {
		e, ok := s.jobs[id]
		if !ok || e.removed {
			continue
		}
		// Take a fresh view from the job (it owns its own locks) instead of
		// writing into the cached entry, so this stays a read-only operation.
		snap := e.job.Snapshot()
		snap.Completed = nil
		out.Jobs = append(out.Jobs, persistedJob{
			Options:  e.opts,
			Snapshot: snap,
			Resume:   e.job.ResumedRanges(),
		})
	}
	s.mu.RUnlock()

	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.stateU), 0o755)
	tmp := s.stateU + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.stateU)
}

func (s *Service) restore() {
	data, err := os.ReadFile(s.stateU)
	if err != nil {
		return
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return
	}
	set := s.settings.get()
	resumeOnLaunch := set.ResumeOnLaunch

	for _, pj := range st.Jobs {
		if pj.Snapshot.ID == "" {
			continue
		}
		opts := pj.Options
		if opts.Connections == 0 {
			opts = set.toOptions(opts)
		}
		limiter := engine.NewRateLimiter(opts.SpeedLimit)
		job := engine.NewJob(pj.Snapshot.ID, opts, limiter, nil)
		job.Restore(pj.Snapshot.Total, pj.Snapshot.Done, pj.Resume, pj.Snapshot.FinalURL,
			pj.Snapshot.FileName, pj.Snapshot.SavePath, pj.Snapshot.ContentType, pj.Snapshot.SupportsRanges)

		snap := job.Snapshot()
		snap.AddedAt = pj.Snapshot.AddedAt
		switch pj.Snapshot.State {
		case engine.StateCompleted:
			snap.State = engine.StateCompleted
		case engine.StateCanceled:
			continue
		default:
			snap.State = engine.StatePaused
			job.MarkPaused()
		}
		e := &entry{job: job, opts: opts, snapshot: snap}
		s.mu.Lock()
		s.jobs[snap.ID] = e
		s.order = append(s.order, snap.ID)
		s.mu.Unlock()

		if resumeOnLaunch && snap.State == engine.StatePaused && snap.SavePath != "" {
			if _, err := os.Stat(snap.SavePath); err == nil {
				go func(id string) {
					time.Sleep(1500 * time.Millisecond)
					s.startJob(id)
				}(snap.ID)
			}
		}
	}
	if len(s.order) > 0 {
		s.Log("info", fmt.Sprintf("restored %d download(s) from the previous session", len(s.order)))
	}
}

// ---------------------------------------------------------------------------
// periodic loop
// ---------------------------------------------------------------------------

func (s *Service) tickLoop() {
	defer s.wg.Done()
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	save := time.NewTicker(10 * time.Second)
	defer save.Stop()
	lastActive := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			s.refresh()
			// When the last running job stops, promote the next queued one.
			active := s.activeCount()
			if active == 0 && lastActive > 0 {
				s.nextQueued()
				if s.OnAllDone != nil && s.pendingCount() == 0 {
					s.OnAllDone()
				}
			}
			if active > 0 || lastActive > 0 {
				s.publish()
			}
			lastActive = active
		case <-save.C:
			if s.activeCount() > 0 {
				s.persist()
			}
		}
	}
}

func (s *Service) refresh() {
	s.mu.RLock()
	ids := make([]string, 0, len(s.order))
	for _, id := range s.order {
		if e, ok := s.jobs[id]; ok && !e.removed {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()

	type upd struct {
		id   string
		snap engine.Snapshot
	}
	var updates []upd
	for _, id := range ids {
		j := s.entryJob(id)
		if j == nil {
			continue
		}
		st := j.State()
		if st == engine.StateDownloading || st == engine.StateConnecting || st == engine.StateVerifying {
			j.Tick()
		}
		updates = append(updates, upd{id, j.Snapshot()})
	}
	s.mu.Lock()
	for _, u := range updates {
		if e, ok := s.jobs[u.id]; ok {
			e.snapshot = u.snap
		}
	}
	s.mu.Unlock()
}

func (s *Service) activeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, e := range s.jobs {
		switch e.snapshot.State {
		case engine.StateDownloading, engine.StateConnecting, engine.StateVerifying:
			n++
		}
	}
	return n
}

func (s *Service) pendingCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, e := range s.jobs {
		switch e.snapshot.State {
		case engine.StateDownloading, engine.StateConnecting, engine.StateVerifying, engine.StateQueued:
			n++
		}
	}
	return n
}

func (s *Service) publish() {
	s.broadcast(Event{Type: "jobs", Jobs: s.List()})
}

// ---------------------------------------------------------------------------
// event bus
// ---------------------------------------------------------------------------

// Subscribe registers a listener. The returned cancel function must be called
// when the listener goes away.
func (s *Service) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	s.subsMu.Lock()
	s.subID++
	id := s.subID
	s.subs[id] = ch
	s.subsMu.Unlock()
	return ch, func() {
		s.subsMu.Lock()
		if c, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(c)
		}
		s.subsMu.Unlock()
	}
}

func (s *Service) broadcast(ev Event) {
	if ev.Time == 0 {
		ev.Time = time.Now().UnixMilli()
	}
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- ev:
		default: // slow consumer: drop rather than block a download
		}
	}
}

// Log writes a line to the log file and pushes it to the UI.
func (s *Service) Log(level, msg string) {
	s.logMu.Lock()
	if s.logFile != nil {
		fmt.Fprintf(s.logFile, "%s  %-5s %s\n", time.Now().Format("2006-01-02 15:04:05"), strings.ToUpper(level), msg)
	}
	s.logMu.Unlock()
	s.broadcast(Event{Type: "log", Level: level, Message: msg})
}

func (s *Service) openLog() {
	path := filepath.Join(ConfigDir(), "superidm.log")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		s.logMu.Lock()
		if s.logFile != nil {
			_ = s.logFile.Close()
		}
		s.logFile = f
		s.logMu.Unlock()
	}
}

// LogFilePath returns the log file location (used by the UI's "open log").
func (s *Service) LogFilePath() string { return filepath.Join(ConfigDir(), "superidm.log") }

func displayName(s engine.Snapshot) string {
	if s.FileName != "" {
		return s.FileName
	}
	return s.URL
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func newID() string {
	return fmt.Sprintf("%d-%04x", time.Now().UnixMilli(), randUint16())
}

func openWithShell(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

func revealWithShell(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", "/select,"+path).Start()
	case "darwin":
		return exec.Command("open", "-R", path).Start()
	default:
		return exec.Command("xdg-open", filepath.Dir(path)).Start()
	}
}
