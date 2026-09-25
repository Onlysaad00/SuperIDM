package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Onlysaad00/SuperIDM/internal/engine"
)

// Category is a download folder rule.
type Category struct {
	Name   string   `json:"name"`
	Folder string   `json:"folder"`
	Exts   []string `json:"exts"`
}

// Settings holds every user-visible option.
type Settings struct {
	DownloadDir string `json:"downloadDir"`

	// Performance. Connections default to 64: the single biggest reason
	// SuperIDM is faster than an 8-connection manager.
	Connections    int   `json:"connections"`
	MinChunkKB     int   `json:"minChunkKB"`
	BufferKB       int   `json:"bufferKB"`
	MaxRetries     int   `json:"maxRetries"`
	IdleTimeoutSec int   `json:"idleTimeoutSec"`
	SpeedLimitBps  int64 `json:"speedLimitBps"`
	MaxConcurrent  int   `json:"maxConcurrent"`

	// Behaviour
	UseCategories    bool       `json:"useCategories"`
	CustomCategories []Category `json:"customCategories"`
	ResumeOnLaunch   bool       `json:"resumeOnLaunch"`
	StartMinimized   bool       `json:"startMinimized"`
	MinimizeToTray   bool       `json:"minimizeToTray"`
	AutoStart        bool       `json:"autoStart"`
	NotifyOnComplete bool       `json:"notifyOnComplete"`
	ConfirmBeforeAdd bool       `json:"confirmBeforeAdd"`
	ClipboardMonitor bool       `json:"clipboardMonitor"`
	UserAgent        string     `json:"userAgent"`

	// Browser integration
	Port          int  `json:"port"`
	EnableAPI     bool `json:"enableAPI"`
	EnableSniffer bool `json:"enableSniffer"`

	// Appearance
	Theme string `json:"theme"`
}

// DefaultSettings returns the shipped defaults.
func DefaultSettings() Settings {
	return Settings{
		DownloadDir:      engine.DefaultDownloadDir(),
		Connections:      engine.DefaultConnections, // 64
		MinChunkKB:       1024,                      // 1 MiB work units
		BufferKB:         256,
		MaxRetries:       5,
		IdleTimeoutSec:   30,
		SpeedLimitBps:    0,
		MaxConcurrent:    3,
		UseCategories:    false,
		ResumeOnLaunch:   true,
		StartMinimized:   false,
		MinimizeToTray:   true,
		AutoStart:        false,
		NotifyOnComplete: false,
		ConfirmBeforeAdd: false,
		ClipboardMonitor: false,
		UserAgent:        engine.DefaultUserAgent,
		Port:             8765,
		EnableAPI:        true,
		EnableSniffer:    true,
		Theme:            "dark",
		CustomCategories: defaultCategories(),
	}
}

func defaultCategories() []Category {
	return []Category{
		{Name: "Video", Folder: "Video", Exts: []string{
			"mp4", "mkv", "avi", "mov", "wmv", "flv", "webm", "m4v", "mpg", "mpeg", "ts", "3gp", "m2ts", "vob", "ogv"}},
		{Name: "Music", Folder: "Music", Exts: []string{
			"mp3", "m4a", "aac", "flac", "wav", "ogg", "opus", "wma", "aiff", "alac", "mka"}},
		{Name: "Documents", Folder: "Documents", Exts: []string{
			"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "rtf", "csv", "epub", "mobi",
			"odt", "ods", "odp", "json", "xml", "yaml", "yml", "md", "log"}},
		{Name: "Compressed", Folder: "Compressed", Exts: []string{
			"zip", "rar", "7z", "tar", "gz", "bz2", "xz", "zst", "iso", "cab", "tgz", "tbz2"}},
		{Name: "Programs", Folder: "Programs", Exts: []string{
			"exe", "msi", "apk", "dmg", "deb", "rpm", "appx", "msix", "bat", "cmd", "jar", "xpi", "crx"}},
		{Name: "Pictures", Folder: "Pictures", Exts: []string{
			"jpg", "jpeg", "png", "gif", "bmp", "webp", "svg", "tif", "tiff", "ico", "heic", "avif", "raw"}},
	}
}

// DetectCategory returns the folder name that fits an extension.
func (s Settings) DetectCategory(ext string) string {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	if ext == "" {
		return ""
	}
	cats := s.CustomCategories
	if len(cats) == 0 {
		cats = defaultCategories()
	}
	for _, c := range cats {
		for _, e := range c.Exts {
			if strings.EqualFold(e, ext) {
				return c.Folder
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

// ConfigDir returns %APPDATA%\SuperIDM (Windows) or ~/.config/SuperIDM.
func ConfigDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return filepath.Join(os.TempDir(), "SuperIDM")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "SuperIDM")
}

// settingsStore owns settings.json.
type settingsStore struct {
	mu   sync.RWMutex
	path string
	cur  Settings
}

func newSettingsStore() *settingsStore {
	st := &settingsStore{path: filepath.Join(ConfigDir(), "settings.json")}
	st.cur = DefaultSettings()
	if _, err := os.Stat(st.path); os.IsNotExist(err) {
		// First run: materialise the defaults so users can read and edit the
		// file, and so support requests have something to look at.
		_ = st.save()
	}
	st.load()
	return st
}

func (st *settingsStore) load() {
	data, err := os.ReadFile(st.path)
	if err != nil {
		return
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return
	}
	st.cur = normalise(s)
}

func (st *settingsStore) save() error {
	st.mu.RLock()
	data, err := json.MarshalIndent(st.cur, "", "  ")
	st.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

func (st *settingsStore) get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.cur
}

func (st *settingsStore) set(s Settings) {
	st.mu.Lock()
	st.cur = normalise(s)
	st.mu.Unlock()
	_ = st.save()
}

// normalise clamps values that would otherwise break the engine.
func normalise(s Settings) Settings {
	d := DefaultSettings()
	if s.DownloadDir == "" {
		s.DownloadDir = d.DownloadDir
	}
	if s.Connections < 1 {
		s.Connections = d.Connections
	}
	if s.Connections > engine.MaxConnections {
		s.Connections = engine.MaxConnections
	}
	if s.MinChunkKB < 32 {
		s.MinChunkKB = d.MinChunkKB
	}
	if s.BufferKB < 16 {
		s.BufferKB = d.BufferKB
	}
	if s.BufferKB > 4096 {
		s.BufferKB = 4096
	}
	if s.MaxRetries < 1 {
		s.MaxRetries = d.MaxRetries
	}
	if s.IdleTimeoutSec < 5 {
		s.IdleTimeoutSec = d.IdleTimeoutSec
	}
	if s.MaxConcurrent < 1 {
		s.MaxConcurrent = d.MaxConcurrent
	}
	if s.MaxConcurrent > 16 {
		s.MaxConcurrent = 16
	}
	if s.SpeedLimitBps < 0 {
		s.SpeedLimitBps = 0
	}
	if s.Port < 1024 || s.Port > 65535 {
		s.Port = d.Port
	}
	if s.Theme == "" {
		s.Theme = d.Theme
	}
	if len(s.CustomCategories) == 0 {
		s.CustomCategories = d.CustomCategories
	}
	if s.UserAgent == "" {
		s.UserAgent = d.UserAgent
	}
	return s
}

// toOptions converts settings plus per-download overrides into engine options.
func (s Settings) toOptions(o engine.Options) engine.Options {
	if o.Dir == "" {
		o.Dir = s.DownloadDir
	}
	if o.Connections <= 0 {
		o.Connections = s.Connections
	}
	if o.MinChunk <= 0 {
		o.MinChunk = int64(s.MinChunkKB) << 10
	}
	if o.BufferSize <= 0 {
		o.BufferSize = int(s.BufferKB) << 10
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = s.MaxRetries
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = timeDurationSeconds(s.IdleTimeoutSec)
	}
	if o.SpeedLimit <= 0 {
		o.SpeedLimit = s.SpeedLimitBps
	}
	if o.UserAgent == "" {
		o.UserAgent = s.UserAgent
	}
	if s.UseCategories && o.Category == "" {
		o.Category = s.DetectCategory(filepath.Ext(o.FileName))
	}
	return o
}

func timeDurationSeconds(n int) time.Duration { return time.Duration(n) * time.Second }
