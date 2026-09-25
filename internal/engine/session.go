package engine

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Why HTTP/1.1 for the range engine?
//
// HTTP/2 multiplexes every request over a single TCP connection. That is
// excellent for web browsing, but it is the opposite of what a download
// accelerator wants: all 64 byte-ranges then share one congestion window, so
// throughput becomes bounded by that single connection - exactly the ceiling
// Internet Download Manager avoids by using N separate sockets.
//
// The range engine therefore pins HTTP/1.1 so each worker owns a real,
// independent TCP connection (and its own congestion window), while metadata
// requests (probe, redirect resolution, HLS playlists, media sniffing) still
// use HTTP/2 where it is beneficial.

var (
	h1Once sync.Once
	h1Tr   *http.Transport

	h2Once sync.Once
	h2Tr   *http.Transport
)

func baseTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   20 * time.Second,
			KeepAlive: 25 * time.Second,
		}).DialContext,
		MaxIdleConns:          MaxConnections * 2,
		MaxIdleConnsPerHost:   MaxConnections,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true,
		WriteBufferSize:       64 << 10,
		ReadBufferSize:        64 << 10,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
}

// sharedTransport returns the HTTP/1.1 transport used by the parallel engine.
func sharedTransport() *http.Transport {
	h1Once.Do(func() {
		h1Tr = baseTransport()
		h1Tr.ForceAttemptHTTP2 = false
		// A non-nil, empty TLSNextProto map disables the automatic HTTP/2
		// upgrade while leaving ALPN negotiation functional.
		h1Tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	})
	return h1Tr
}

// metaTransport allows HTTP/2 for single-request operations.
func metaTransport() *http.Transport {
	h2Once.Do(func() {
		h2Tr = baseTransport()
		h2Tr.ForceAttemptHTTP2 = true
	})
	return h2Tr
}

// NewClient returns an HTTP/2-capable client for metadata operations.
func NewClient() *http.Client { return &http.Client{Transport: metaTransport()} }

// rangeStream is an open response body.
type rangeStream struct {
	Body   io.ReadCloser
	Status int
	Total  int64
}

// httpSession is the per-worker connection handle. Keeping one session per
// worker matters: it guarantees the transport's idle pool cannot silently
// funnel two workers onto the same socket, and it gives us a clean way to
// throw away a connection whose stream died mid-body.
type httpSession struct {
	client *http.Client
	tr     *http.Transport
	mu     sync.Mutex
	poison bool
}

func openSession(tr *http.Transport) *httpSession {
	return &httpSession{client: &http.Client{Transport: tr}, tr: tr}
}

// discard drops the pooled connections of this session after a stream error.
func (s *httpSession) discard() {
	s.mu.Lock()
	s.poison = true
	s.mu.Unlock()
	s.tr.CloseIdleConnections()
}

func (s *httpSession) close() { s.tr.CloseIdleConnections() }

// open performs a ranged (or whole-body) GET and returns the live stream.
//
//	r.End >= 0  -> "bytes=start-(end-1)"
//	r.End <  0  -> "bytes=start-"  (open ended; omitted entirely when start==0)
func (s *httpSession) open(ctx context.Context, opts Options, r Range) (*rangeStream, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := buildRequest(ctx, http.MethodGet, opts.URL, opts)
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	switch {
	case r.End >= 0:
		req.Header.Set("Range", "bytes="+r.String())
	case r.Start > 0:
		req.Header.Set("Range", "bytes="+strconv.FormatInt(r.Start, 10)+"-")
	}
	if opts.Headers != nil {
		if v := opts.Headers["If-Range"]; v != "" {
			req.Header.Set("If-Range", v)
		}
	}

	resp, err := s.client.Do(req)
	if err != nil {
		cancel()
		return nil, func() {}, err
	}

	// Servers occasionally report 200 to a ranged request. That is handled by
	// the caller (prefix discard), so it is not treated as an error here.
	switch resp.StatusCode {
	case http.StatusPartialContent, http.StatusOK:
		return &rangeStream{Body: resp.Body, Status: resp.StatusCode, Total: totalFromContentRange(resp.Header.Get("Content-Range"))}, cancel, nil
	case http.StatusRequestedRangeNotSatisfiable:
		_ = resp.Body.Close()
		cancel()
		return nil, func() {}, io.EOF
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		_ = resp.Body.Close()
		cancel()
		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			return nil, func() {}, &httpError{Code: resp.StatusCode, Retryable: true}
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return nil, func() {}, &httpError{Code: resp.StatusCode, Message: "link needs authentication"}
		default:
			return nil, func() {}, &httpError{Code: resp.StatusCode}
		}
	}
}

// httpError carries a status code so callers can decide whether to retry.
type httpError struct {
	Code      int
	Message   string
	Retryable bool
}

func (e *httpError) Error() string {
	m := e.Message
	if m == "" {
		m = http.StatusText(e.Code)
	}
	return "HTTP " + strconv.Itoa(e.Code) + " " + m
}
