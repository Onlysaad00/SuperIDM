// Package engine implements the SuperIDM multi-connection download core.
//
// Design goals (why this is faster than a classic 8-connection downloader):
//
//  1. Dynamic adaptive chunking: instead of splitting the file into a fixed
//     number of segments up front, the file is treated as a pool of work units
//     that idle connections claim on demand. A connection that finishes early
//     immediately takes more work, and the pool subdivides itself as it runs
//     short, so the slowest link cannot hold up the transfer.
//  2. Connection count defaults to 64 (configurable 1..128) instead of the
//     traditional 8/32.
//  3. In-place writes with WriteAt(pwrite) into a pre-allocated file: no
//     per-segment temp files, no final merge pass, and no re-seek storms.
//  4. HTTP/2 is used automatically on servers that support it, so many
//     concurrent byte ranges are multiplexed over one already-warm TLS
//     connection.
//  5. A per-job watchdog detects stalled sockets and instantly re-queues the
//     unfinished tail of that range onto a fresh connection.
package engine

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// urlQueryUnescape decodes RFC 3986 percent-encoding.
func urlQueryUnescape(s string) (string, error) { return url.QueryUnescape(s) }

// State is the lifecycle state of a job.
type State string

const (
	StateQueued      State = "queued"
	StateConnecting  State = "connecting"
	StateDownloading State = "downloading"
	StatePaused      State = "paused"
	StateVerifying   State = "verifying"
	StateCompleted   State = "completed"
	StateError       State = "error"
	StateCanceled    State = "canceled"
)

// IsFinal reports whether no further progress is expected for the state.
func (s State) IsFinal() bool {
	return s == StateCompleted || s == StateError || s == StateCanceled
}

// Range is a half-open byte range [Start, End).
type Range struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Len returns the number of bytes covered by the range.
func (r Range) Len() int64 { return r.End - r.Start }

// String renders the range as an HTTP byte-range specification.
func (r Range) String() string { return fmt.Sprintf("%d-%d", r.Start, r.End-1) }

// Options configures a single download.
type Options struct {
	URL        string            `json:"url"`
	Dir        string            `json:"dir"`
	FileName   string            `json:"fileName"`
	SavePath   string            `json:"savePath"`
	Category   string            `json:"category"`
	Referer    string            `json:"referer"`
	Cookie     string            `json:"cookie"`
	UserAgent  string            `json:"userAgent"`
	Headers    map[string]string `json:"headers"`
	Checksum   string            `json:"checksum"`
	ExpectedMB int64             `json:"expectedMB"`

	// Connections is the number of parallel sockets used for the transfer.
	Connections int `json:"connections"`
	// MinChunk is the smallest work unit handed to a connection. Smaller
	// units balance better but add per-request overhead.
	MinChunk int64 `json:"minChunk"`
	// BufferSize is the read/write buffer size for each connection.
	BufferSize int `json:"bufferSize"`
	// SpeedLimit is a per-job cap in bytes/second (0 = unlimited).
	SpeedLimit int64 `json:"speedLimit"`
	// MaxRetries per work unit before the job fails.
	MaxRetries int `json:"maxRetries"`
	// IdleTimeout cancels a connection that produces no bytes for this long.
	IdleTimeout time.Duration `json:"idleTimeout"`
	// Overwrite replaces an existing file instead of choosing "name (1).ext".
	Overwrite bool `json:"overwrite"`
	// StartPaused creates the job without starting the transfer.
	StartPaused bool `json:"startPaused"`
	// Kind is "http" (default) or "hls".
	Kind string `json:"kind"`
	// MediaTitle is used to derive a filename for HLS/DASH captures.
	MediaTitle string `json:"mediaTitle"`
}

// withDefaults returns a copy of the options with sane defaults applied.
func (o Options) withDefaults() Options {
	if o.Connections <= 0 {
		o.Connections = DefaultConnections
	}
	if o.Connections > MaxConnections {
		o.Connections = MaxConnections
	}
	if o.MinChunk <= 0 {
		o.MinChunk = DefaultMinChunk
	}
	if o.MinChunk < 32*1024 {
		o.MinChunk = 32 * 1024
	}
	if o.BufferSize <= 0 {
		o.BufferSize = DefaultBufferSize
	}
	if o.BufferSize < 16*1024 {
		o.BufferSize = 16 * 1024
	}
	if o.BufferSize > 4*1024*1024 {
		o.BufferSize = 4 * 1024 * 1024
	}
	if o.MaxRetries <= 0 {
		o.MaxRetries = DefaultRetries
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultIdleTimeout
	}
	if o.UserAgent == "" {
		o.UserAgent = DefaultUserAgent
	}
	if o.Kind == "" {
		o.Kind = "http"
	}
	return o
}

// Tunables. These are the numbers that make the difference versus a
// conventional downloader.
const (
	// DefaultConnections is deliberately higher than the 8 used by most
	// download managers. Most CDNs (Cloudflare, Fastly, Akamai, S3) have no
	// problem with 64 concurrent range requests and it is where a lot of the
	// extra throughput comes from on high-latency links.
	DefaultConnections = 64
	MaxConnections     = 128

	DefaultMinChunk    = 1 << 20 // 1 MiB work units
	DefaultMaxChunk    = 64 << 20
	DefaultBufferSize  = 256 << 10 // 256 KiB per connection
	DefaultRetries     = 5
	DefaultIdleTimeout = 30 * time.Second

	// SmallFileThreshold: files at or below this size are fetched with a
	// single request (parallel range requests only add handshake overhead).
	SmallFileThreshold = 2 << 20

	// ChunksPerWorker: how many work units to keep in flight per connection.
	// 4 gives the scheduler enough slack to keep every socket busy.
	ChunksPerWorker = 4

	DefaultUserAgent = "SuperIDM/1.0 (Windows; +https://github.com/Onlysaad00/SuperIDM)"
)

// Snapshot is an immutable view of a job, safe to serialise to JSON.
type Snapshot struct {
	ID             string  `json:"id"`
	URL            string  `json:"url"`
	FinalURL       string  `json:"finalUrl,omitempty"`
	FileName       string  `json:"fileName"`
	SavePath       string  `json:"savePath"`
	Dir            string  `json:"dir"`
	Category       string  `json:"category"`
	Kind           string  `json:"kind"`
	Total          int64   `json:"total"`
	Done           int64   `json:"done"`
	Percent        float64 `json:"percent"`
	State          State   `json:"state"`
	Speed          float64 `json:"speed"`
	AvgSpeed       float64 `json:"avgSpeed"`
	PeakSpeed      float64 `json:"peakSpeed"`
	ETA            int64   `json:"eta"` // seconds, -1 = unknown
	Active         int     `json:"active"`
	Segments       int     `json:"segments"`
	Retries        int     `json:"retries"`
	Error          string  `json:"error,omitempty"`
	SupportsRanges bool    `json:"supportsRanges"`
	Checksum       string  `json:"checksum,omitempty"`
	ChecksumOK     *bool   `json:"checksumOk,omitempty"`
	ContentType    string  `json:"contentType,omitempty"`
	StartedAt      int64   `json:"startedAt"`
	FinishedAt     int64   `json:"finishedAt,omitempty"`
	AddedAt        int64   `json:"addedAt"`
	Connections    int     `json:"connections"`

	// Transfer accounting for the UI.
	Left int64 `json:"left"`

	// SpeedLimit is the configured per-job cap in bytes/second (0 = none).
	SpeedLimit int64 `json:"speedLimit,omitempty"`
	// Completed is the resume map: byte spans already on disk.
	Completed []Range `json:"completed,omitempty"`
	// SegmentsTotal/SegmentsDone drive progress for playlist (HLS) jobs whose
	// total byte length is not known in advance.
	SegmentsTotal int `json:"segmentsTotal,omitempty"`
	SegmentsDone  int `json:"segmentsDone,omitempty"`
}

// ProgressFunc receives periodic snapshots.
type ProgressFunc func(Snapshot)

// TotalBytes returns the byte count of a list of ranges.
func TotalBytes(rs []Range) int64 {
	var n int64
	for _, r := range rs {
		n += r.Len()
	}
	return n
}

// HumanBytes renders a byte count using binary units.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// HumanSpeed renders a transfer rate.
func HumanSpeed(bps float64) string { return HumanBytes(int64(bps)) + "/s" }

// GuessFileName derives a file name from a URL and content-disposition header.
func GuessFileName(rawURL string, contentDisposition string) string {
	if n := fileNameFromDisposition(contentDisposition); n != "" {
		return sanitizeName(n)
	}
	// Only the path component may contribute a name: a bare host such as
	// https://example.com/ has no file name at all.
	path := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		path = u.Path
	} else if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	path = strings.TrimRight(path, "/")
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == ".." {
		return ""
	}
	if i := strings.LastIndex(base, ":"); i >= 0 && !strings.Contains(base, "]") {
		if _, err := fmt.Sscanf(base[i+1:], "%d"); err == nil {
			base = base[:i] // strip a port that leaked in
		}
	}
	return sanitizeName(base)
}

func fileNameFromDisposition(cd string) string {
	if cd == "" {
		return ""
	}
	lower := strings.ToLower(cd)
	// RFC 5987 / RFC 2231: filename*=UTF-8''name.ext
	if i := strings.Index(lower, "filename*="); i >= 0 {
		v := cd[i+len("filename*="):]
		if j := strings.IndexByte(v, ';'); j >= 0 {
			v = v[:j]
		}
		v = strings.TrimSpace(strings.Trim(v, `"`))
		if k := strings.LastIndex(v, "''"); k >= 0 {
			v = v[k+2:]
		}
		if dec, err := urlQueryUnescape(v); err == nil && dec != "" {
			return dec
		}
	}
	if i := strings.Index(lower, "filename="); i >= 0 {
		v := cd[i+len("filename="):]
		if j := strings.IndexByte(v, ';'); j >= 0 {
			v = v[:j]
		}
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"`)
		return v
	}
	return ""
}

// sanitizeName strips characters that are illegal in Windows file names.
func sanitizeName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return '_'
		}
		if r < 32 {
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if len(name) > 180 {
		ext := ""
		if i := strings.LastIndex(name, "."); i > 0 {
			ext = name[i:]
		}
		if len(ext) > 12 {
			ext = ""
		}
		name = name[:180-len(ext)] + ext
	}
	return name
}
