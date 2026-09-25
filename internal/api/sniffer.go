package api

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Onlysaad00/SuperIDM/internal/engine"
	"github.com/Onlysaad00/SuperIDM/internal/service"
)

// MediaItem is one captured media stream reported by the browser extension
// (or discovered by the page's own DOM).
type MediaItem struct {
	ID       string  `json:"id"`
	URL      string  `json:"url"`
	Kind     string  `json:"kind"` // "hls" | "dash" | "video" | "audio" | "file"
	MIME     string  `json:"mime,omitempty"`
	Title    string  `json:"title,omitempty"`
	PageURL  string  `json:"pageUrl,omitempty"`
	Referer  string  `json:"referer,omitempty"`
	Cookie   string  `json:"cookie,omitempty"`
	Size     int64   `json:"size,omitempty"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	Duration float64 `json:"duration,omitempty"`
	AddedAt  int64   `json:"addedAt"`

	// State mirrors the download job created from this item, when there is one.
	JobID string `json:"jobId,omitempty"`
}

// Sniffer keeps the list of media candidates offered to the user.
type Sniffer struct {
	mu    sync.Mutex
	items map[string]*MediaItem
	order []string
	svc   *service.Service
	max   int
	seq   int64
}

// NewSniffer creates the capture store.
func NewSniffer(svc *service.Service) *Sniffer {
	return &Sniffer{items: map[string]*MediaItem{}, svc: svc, max: 400}
}

// Add merges captured items, ignoring duplicates by URL.
func (n *Sniffer) Add(items []MediaItem) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	added := 0
	for _, it := range items {
		it.URL = strings.TrimSpace(it.URL)
		if it.URL == "" || !isHTTP(it.URL) {
			continue
		}
		if existing, ok := n.items[it.URL]; ok {
			// Refresh metadata but keep the identity and any download link.
			if it.Title != "" {
				existing.Title = it.Title
			}
			if it.Size > 0 {
				existing.Size = it.Size
			}
			if it.MIME != "" {
				existing.MIME = it.MIME
			}
			if it.Kind != "" && existing.JobID == "" {
				existing.Kind = it.Kind
			}
			continue
		}
		n.seq++
		it.ID = fmt.Sprintf("m%d-%d", time.Now().UnixMilli(), n.seq)
		if it.AddedAt == 0 {
			it.AddedAt = time.Now().UnixMilli()
		}
		if it.Kind == "" {
			it.Kind = classifyKind(it.URL, it.MIME)
		}
		cp := it
		n.items[it.URL] = &cp
		n.order = append(n.order, it.URL)
		added++
	}
	if len(n.order) > n.max {
		drop := len(n.order) - n.max
		for _, u := range n.order[:drop] {
			delete(n.items, u)
		}
		n.order = n.order[drop:]
	}
	return added
}

// List returns the captured items, newest first.
func (n *Sniffer) List() []MediaItem {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]MediaItem, 0, len(n.order))
	for _, u := range n.order {
		if it, ok := n.items[u]; ok {
			out = append(out, *it)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].AddedAt > out[b].AddedAt })
	return out
}

// Get returns one item by id.
func (n *Sniffer) Get(id string) (MediaItem, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, it := range n.items {
		if it.ID == id {
			return *it, true
		}
	}
	return MediaItem{}, false
}

// Clear drops every captured item.
func (n *Sniffer) Clear() { n.ClearOrRemove("") }

// ClearOrRemove clears everything when id is empty, else removes one item.
func (n *Sniffer) ClearOrRemove(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if id == "" {
		n.items = map[string]*MediaItem{}
		n.order = nil
		return
	}
	for u, it := range n.items {
		if it.ID == id {
			delete(n.items, u)
			for i, ou := range n.order {
				if ou == u {
					n.order = append(n.order[:i], n.order[i+1:]...)
					break
				}
			}
			return
		}
	}
}

// Download turns a captured item into a real download job.
func (n *Sniffer) Download(id, dir string, connections int) (engine.Snapshot, error) {
	it, ok := n.Get(id)
	if !ok {
		return engine.Snapshot{}, fmt.Errorf("no such captured media")
	}
	kind := it.Kind
	if kind == "dash" {
		return engine.Snapshot{}, fmt.Errorf("DASH (MPD) capture is not implemented yet - try the HLS stream if the site offers one")
	}
	if kind == "file" || kind == "video" || kind == "audio" {
		kind = "http"
	}
	name := it.Title
	if name == "" {
		name = engine.GuessFileName(it.URL, "")
	}
	opts := engine.Options{
		URL:         it.URL,
		Dir:         dir,
		FileName:    name,
		Referer:     firstNonEmpty(it.Referer, it.PageURL),
		Cookie:      it.Cookie,
		Kind:        kind,
		MediaTitle:  it.Title,
		Connections: connections,
	}
	snap, err := n.svc.Add(opts)
	if err != nil {
		return engine.Snapshot{}, err
	}
	n.mu.Lock()
	for _, item := range n.items {
		if item.ID == id {
			item.JobID = snap.ID
		}
	}
	n.mu.Unlock()
	return snap, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isHTTP(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func classifyKind(rawURL, mime string) string {
	l := strings.ToLower(mime)
	switch {
	case strings.Contains(l, "mpegurl"):
		return "hls"
	case strings.Contains(l, "dash"):
		return "dash"
	case strings.HasPrefix(l, "video/"):
		return "video"
	case strings.HasPrefix(l, "audio/"):
		return "audio"
	}
	u := strings.ToLower(rawURL)
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	switch {
	case strings.HasSuffix(u, ".m3u8"):
		return "hls"
	case strings.HasSuffix(u, ".mpd"):
		return "dash"
	case strings.HasSuffix(u, ".mp4"), strings.HasSuffix(u, ".webm"), strings.HasSuffix(u, ".mkv"),
		strings.HasSuffix(u, ".mov"), strings.HasSuffix(u, ".avi"), strings.HasSuffix(u, ".flv"),
		strings.HasSuffix(u, ".ts"), strings.HasSuffix(u, ".m4s"):
		return "video"
	case strings.HasSuffix(u, ".mp3"), strings.HasSuffix(u, ".m4a"), strings.HasSuffix(u, ".aac"),
		strings.HasSuffix(u, ".flac"), strings.HasSuffix(u, ".opus"), strings.HasSuffix(u, ".wav"):
		return "audio"
	}
	return "file"
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// sniffRequest is what the extension posts.
type sniffRequest struct {
	Items []MediaItem `json:"items"`
	// Single-item shorthand.
	URL      string  `json:"url"`
	Kind     string  `json:"kind"`
	MIME     string  `json:"mime"`
	Title    string  `json:"title"`
	PageURL  string  `json:"pageUrl"`
	Referer  string  `json:"referer"`
	Cookie   string  `json:"cookie"`
	Size     int64   `json:"size"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Duration float64 `json:"duration"`
}

func (s *Server) handleSniff(w http.ResponseWriter, r *http.Request) {
	var req sniffRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, 400, "invalid JSON body: "+err.Error())
		return
	}
	items := req.Items
	if req.URL != "" {
		items = append(items, MediaItem{
			URL: req.URL, Kind: req.Kind, MIME: req.MIME, Title: req.Title,
			PageURL: req.PageURL, Referer: req.Referer, Cookie: req.Cookie,
			Size: req.Size, Width: req.Width, Height: req.Height, Duration: req.Duration,
		})
	}
	added := s.sniff.Add(items)
	if added > 0 {
		s.svc.Log("info", fmt.Sprintf("captured %d media link(s) from %s", added, clientName(r)))
	}
	writeJSON(w, 200, map[string]any{"ok": true, "added": added, "total": len(s.sniff.List())})
}

func (s *Server) handleSniffList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"items": s.sniff.List()})
}

func (s *Server) handleSniffClear(w http.ResponseWriter, r *http.Request) {
	s.sniff.Clear()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleSniffRemove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, 400, "invalid JSON body")
		return
	}
	s.sniff.ClearOrRemove(body.ID)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleSniffDownload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          string `json:"id"`
		Dir         string `json:"dir"`
		Connections int    `json:"connections"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeErr(w, 400, "invalid JSON body")
		return
	}
	snap, err := s.sniff.Download(body.ID, body.Dir, body.Connections)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, snap)
}

// RefererFromPage derives a referer for a captured media URL.
func RefererFromPage(mediaURL string) string {
	u, err := url.Parse(mediaURL)
	if err != nil {
		return ""
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	return u.String()
}
