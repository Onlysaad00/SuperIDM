package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrRangeNotSupported is returned by probe when the server ignores Range
// headers; the job then falls back to a single-stream transfer.
var ErrRangeNotSupported = errors.New("server does not support byte ranges")

// probeInfo describes what the server told us about a resource.
type probeInfo struct {
	FinalURL     string
	Size         int64 // -1 when unknown
	Ranges       bool
	FileName     string
	ContentType  string
	ETag         string
	LastModified string
	Disposition  string
}

// buildRequest applies the job's identity/authorisation headers.
func buildRequest(ctx context.Context, method, rawURL string, opts Options) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, err
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity") // keep byte offsets exact
	req.Header.Set("Connection", "keep-alive")
	if opts.Referer != "" {
		req.Header.Set("Referer", opts.Referer)
	}
	if opts.Cookie != "" {
		req.Header.Set("Cookie", opts.Cookie)
	}
	for k, v := range opts.Headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	return req, nil
}

// probe asks the server for a single byte in order to learn the total size,
// whether ranges are honoured, and the real filename.
//
// Probing with a one-byte ranged GET (rather than HEAD) is deliberate: many
// CDNs answer HEAD with a different status, omit Content-Length or reject it
// outright, while a ranged GET always describes the actual download.
func probe(ctx context.Context, opts Options) (*probeInfo, error) {
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	req, err := buildRequest(cctx, http.MethodGet, opts.URL, opts)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes=0-0")

	resp, err := NewClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the server: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
	}()

	info := &probeInfo{
		FinalURL:     resp.Request.URL.String(),
		Size:         -1,
		FileName:     GuessFileName(resp.Request.URL.String(), resp.Header.Get("Content-Disposition")),
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		Disposition:  resp.Header.Get("Content-Disposition"),
	}
	acceptRanges := strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")

	switch resp.StatusCode {
	case http.StatusPartialContent:
		info.Ranges = true
		info.Size = totalFromContentRange(resp.Header.Get("Content-Range"))
	case http.StatusOK:
		info.Ranges = acceptRanges
		if cl, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil {
			info.Size = cl
		}
	case http.StatusRequestedRangeNotSatisfiable:
		info.Ranges = false
		info.Size = totalFromContentRange(resp.Header.Get("Content-Range"))
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("the server refused the request (HTTP %d) - the link may need a login, a Referer or a cookie", resp.StatusCode)
	case http.StatusNotFound:
		return nil, errors.New("HTTP 404 - file not found on the server")
	default:
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("server returned HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		if cl, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil {
			info.Size = cl
		}
	}

	// A 200 answer to a one-byte range request means the server ignored the
	// header even if it advertised Accept-Ranges. Trust the behaviour, not the
	// advertisement.
	if resp.StatusCode == http.StatusOK && !acceptRanges {
		info.Ranges = false
	}
	if info.Size == 0 && info.Ranges {
		info.Ranges = false
	}

	if opts.ExpectedMB > 0 && info.Size > 0 {
		want := opts.ExpectedMB << 20
		if info.Size < want/2 {
			return nil, fmt.Errorf("server reported %s but about %s was expected - refusing to save an HTML error page or a truncated file",
				HumanBytes(info.Size), HumanBytes(want))
		}
	}
	return info, nil
}

// totalFromContentRange parses "bytes 0-0/12345".
func totalFromContentRange(cr string) int64 {
	if cr == "" {
		return -1
	}
	i := strings.LastIndex(cr, "/")
	if i < 0 {
		return -1
	}
	total := strings.TrimSpace(cr[i+1:])
	if total == "*" || total == "" {
		return -1
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// resolveRedirects follows redirects and returns the final URL, used by the
// "Grab link" tool before a download starts.
func resolveRedirects(ctx context.Context, rawURL string, opts Options) (string, error) {
	if _, err := url.Parse(rawURL); err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	req, err := buildRequest(cctx, http.MethodGet, rawURL, opts)
	if err != nil {
		return "", err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := NewClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	return resp.Request.URL.String(), nil
}

// HeadInfo is a lightweight metadata lookup used by the UI's "Grab link" tool.
type HeadInfo struct {
	FinalURL    string `json:"finalUrl"`
	Size        int64  `json:"size"`
	Ranges      bool   `json:"ranges"`
	FileName    string `json:"fileName"`
	ContentType string `json:"contentType,omitempty"`
	Kind        string `json:"kind"`
	Error       string `json:"error,omitempty"`
}

// InspectURL probes a URL and reports what SuperIDM would do with it. It never
// downloads the payload.
func InspectURL(ctx context.Context, rawURL string, opts Options) HeadInfo {
	opts.URL = rawURL
	opts = opts.withDefaults()

	lower := strings.ToLower(rawURL)
	if strings.Contains(lower, ".m3u8") {
		opts.Kind = "hls"
		if info, err := probeHLS(ctx, opts); err != nil {
			return HeadInfo{FinalURL: rawURL, Kind: "hls", Error: err.Error()}
		} else {
			return HeadInfo{FinalURL: info.FinalURL, Size: -1, FileName: info.FileName, ContentType: info.ContentType, Kind: "hls"}
		}
	}

	final, err := resolveRedirects(ctx, rawURL, opts)
	if err != nil {
		return HeadInfo{FinalURL: rawURL, Kind: "http", Error: err.Error()}
	}
	opts.URL = final
	info, err := probe(ctx, opts)
	if err != nil {
		return HeadInfo{FinalURL: final, Kind: "http", Error: err.Error()}
	}
	name := info.FileName
	if name == "" {
		name = "download"
	}
	return HeadInfo{
		FinalURL:    info.FinalURL,
		Size:        info.Size,
		Ranges:      info.Ranges,
		FileName:    name,
		ContentType: info.ContentType,
		Kind:        chooseKind(info),
	}
}

func chooseKind(info *probeInfo) string {
	ct := strings.ToLower(info.ContentType)
	if strings.Contains(ct, "mpegurl") || strings.Contains(ct, "x-mpegurl") {
		return "hls"
	}
	return "http"
}
