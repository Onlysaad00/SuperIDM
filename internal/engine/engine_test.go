package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// makePayload returns deterministic pseudo-random bytes.
func makePayload(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// rangeServer serves payload with (or without) byte-range support.
func rangeServer(payload []byte, allowRanges bool, delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if allowRanges {
			w.Header().Set("Accept-Ranges", "bytes")
		} else {
			w.Header().Set("Accept-Ranges", "none")
		}
		rng := r.Header.Get("Range")
		if allowRanges && rng != "" && strings.HasPrefix(rng, "bytes=") {
			spec := strings.TrimPrefix(rng, "bytes=")
			var start, end int64
			if i := strings.IndexByte(spec, '-'); i > 0 {
				start, _ = strconv.ParseInt(spec[:i], 10, 64)
				if spec[i+1:] == "" {
					end = int64(len(payload)) - 1
				} else {
					end, _ = strconv.ParseInt(spec[i+1:], 10, 64)
				}
			}
			if start >= int64(len(payload)) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(payload)))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			if end >= int64(len(payload)) {
				end = int64(len(payload)) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[start : end+1])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
}

func testOpts(url, dir string) Options {
	return Options{
		URL:         url,
		Dir:         dir,
		Connections: 32,
		MinChunk:    64 << 10,
		Overwrite:   true,
	}
}

func TestParallelDownloadExactContent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	payload := makePayload(6<<20 + 12345) // deliberately not a round size
	srv := rangeServer(payload, true, 0)
	defer srv.Close()

	dir := t.TempDir()
	job := NewJob("t1", testOpts(srv.URL+"/file.bin", dir), nil, nil)
	job.Start(context.Background())
	job.Wait()

	snap := job.Snapshot()
	if snap.State != StateCompleted {
		t.Fatalf("state = %s, error = %q", snap.State, snap.Error)
	}
	if snap.Done != int64(len(payload)) {
		t.Fatalf("done = %d, want %d", snap.Done, len(payload))
	}
	got, err := os.ReadFile(snap.SavePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("file size = %d, want %d", len(got), len(payload))
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded content differs from the source (first mismatch at %d)",
			firstDiff(got, payload))
	}
	if snap.Segments < 2 {
		t.Fatalf("expected multiple segments, got %d", snap.Segments)
	}
}

func TestSingleConnectionFallback(t *testing.T) {
	payload := makePayload(3 << 20)
	srv := rangeServer(payload, false, 0) // server refuses ranges
	defer srv.Close()

	dir := t.TempDir()
	job := NewJob("t2", testOpts(srv.URL+"/f.bin", dir), nil, nil)
	job.Start(context.Background())
	job.Wait()

	snap := job.Snapshot()
	if snap.State != StateCompleted {
		t.Fatalf("state = %s, error = %q", snap.State, snap.Error)
	}
	got, _ := os.ReadFile(snap.SavePath)
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch in single-connection mode")
	}
}

func TestChecksumVerification(t *testing.T) {
	payload := makePayload(1 << 20)
	srv := rangeServer(payload, true, 0)
	defer srv.Close()

	sum := sha256.Sum256(payload)
	good := hex.EncodeToString(sum[:])

	dir := t.TempDir()
	opts := testOpts(srv.URL+"/f.bin", dir)
	opts.Checksum = "sha256:" + good
	job := NewJob("t3", opts, nil, nil)
	job.Start(context.Background())
	job.Wait()
	if s := job.Snapshot(); s.State != StateCompleted {
		t.Fatalf("valid checksum rejected: %s %s", s.State, s.Error)
	}

	// Now with a wrong digest.
	dir2 := t.TempDir()
	opts2 := testOpts(srv.URL+"/f.bin", dir2)
	opts2.Checksum = strings.Repeat("ab", 32)
	job2 := NewJob("t4", opts2, nil, nil)
	job2.Start(context.Background())
	job2.Wait()
	s := job2.Snapshot()
	if s.State != StateError {
		t.Fatalf("bad checksum accepted: state=%s", s.State)
	}
	if !strings.Contains(s.Error, "checksum mismatch") {
		t.Fatalf("unexpected error: %s", s.Error)
	}
}

func TestPauseAndResumeKeepsData(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	payload := makePayload(8 << 20)
	// Slow the server a little so the pause lands mid-transfer.
	srv := rangeServer(payload, true, 4*time.Millisecond)
	defer srv.Close()

	dir := t.TempDir()
	opts := testOpts(srv.URL+"/big.bin", dir)
	opts.Connections = 8
	job := NewJob("t5", opts, nil, nil)
	job.Start(context.Background())

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if job.Snapshot().Done > 1<<20 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	job.Pause()
	paused := job.Snapshot()
	if paused.State != StatePaused {
		t.Fatalf("state = %s, want paused", paused.State)
	}
	if paused.Done == 0 {
		t.Fatalf("no progress was made before the pause")
	}
	if paused.Done >= int64(len(payload)) {
		t.Skip("transfer finished before it could be paused")
	}
	partial, err := os.ReadFile(paused.SavePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != len(payload) {
		t.Fatalf("partial file should be pre-allocated to %d bytes, got %d", len(payload), len(partial))
	}

	// Resume: reuse the same job's resume map on a fresh job.
	resumed := NewJob("t5b", opts, nil, nil)
	resumed.Restore(paused.Total, paused.Done, job.ResumedRanges(), paused.FinalURL, paused.FileName, paused.SavePath, paused.ContentType, true)
	resumed.Start(context.Background())
	resumed.Wait()

	fin := resumed.Snapshot()
	if fin.State != StateCompleted {
		t.Fatalf("resume failed: %s %s", fin.State, fin.Error)
	}
	got, err := os.ReadFile(fin.SavePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("resumed file is corrupt (first mismatch at %d)", firstDiff(got, payload))
	}
}

func TestStalledConnectionIsRecovered(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	payload := makePayload(4 << 20)
	var stalled atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", "application/octet-stream")

		if spec == "0-0" {
			// The metadata probe always succeeds.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[:1])
			return
		}
		if spec == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
			return
		}

		// Stall the first two range requests: send headers, then never send a
		// body. Only the idle-timeout watchdog can rescue the transfer.
		if stalled.Add(1) <= 2 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %s/%d", spec, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
			return
		}

		var start, end int64
		if i := strings.IndexByte(spec, '-'); i > 0 {
			start, _ = strconv.ParseInt(spec[:i], 10, 64)
			end, _ = strconv.ParseInt(spec[i+1:], 10, 64)
		}
		if end >= int64(len(payload)) {
			end = int64(len(payload)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer srv.Close()

	dir := t.TempDir()
	opts := testOpts(srv.URL+"/stall.bin", dir)
	opts.Connections = 4
	opts.IdleTimeout = 800 * time.Millisecond
	opts.MaxRetries = 8
	job := NewJob("t6", opts, nil, nil)
	job.Start(context.Background())

	done := make(chan struct{})
	go func() { job.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("stalled download never recovered (watchdog did not fire)")
	}

	snap := job.Snapshot()
	if snap.State != StateCompleted {
		t.Fatalf("state=%s err=%q", snap.State, snap.Error)
	}
	if snap.Retries == 0 {
		t.Fatalf("expected the stall to be counted as a retry")
	}
	got, _ := os.ReadFile(snap.SavePath)
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch after stall recovery (first diff %d)", firstDiff(got, payload))
	}
}

func TestVerifyChecksumFormats(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.bin")
	data := []byte("hello world")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	hexSum := hex.EncodeToString(sum[:])

	for _, spec := range []string{hexSum, "sha256:" + hexSum, "SHA256:" + strings.ToUpper(hexSum)} {
		ok, got, err := VerifyChecksum(p, spec)
		if err != nil || !ok {
			t.Fatalf("spec %q: ok=%v err=%v got=%s", spec, ok, err, got)
		}
	}
	if _, _, err := VerifyChecksum(p, "nonsense"); err == nil {
		t.Fatal("expected an error for an unrecognised checksum")
	}
}

func TestMergeAndSubtractRanges(t *testing.T) {
	merged := MergeRanges([]Range{{5, 10}, {0, 3}, {9, 20}, {30, 40}})
	want := []Range{{0, 3}, {5, 20}, {30, 40}}
	if fmt.Sprint(merged) != fmt.Sprint(want) {
		t.Fatalf("merge = %v, want %v", merged, want)
	}

	gaps := subtractRanges(Range{0, 100}, []Range{{10, 20}, {50, 60}})
	wantGaps := []Range{{0, 10}, {20, 50}, {60, 100}}
	if fmt.Sprint(gaps) != fmt.Sprint(wantGaps) {
		t.Fatalf("gaps = %v, want %v", gaps, wantGaps)
	}

	if g := subtractRanges(Range{0, 100}, []Range{{0, 100}}); len(g) != 0 {
		t.Fatalf("expected no gaps, got %v", g)
	}
}

func TestGuessFileName(t *testing.T) {
	cases := []struct{ url, cd, want string }{
		{"https://x.test/a/b/movie.mp4", "", "movie.mp4"},
		{"https://x.test/a/b/movie.mp4?token=1", "", "movie.mp4"},
		{"https://x.test/", `attachment; filename="report final.pdf"`, "report final.pdf"},
		{"https://x.test/d", "attachment; filename*=UTF-8''caf%C3%A9.zip", "café.zip"},
		{"https://x.test/", "", ""},
	}
	for _, c := range cases {
		if got := GuessFileName(c.url, c.cd); got != c.want {
			t.Errorf("GuessFileName(%q,%q) = %q, want %q", c.url, c.cd, got, c.want)
		}
	}
}

func TestWorkQueueBalances(t *testing.T) {
	// 10 workers, 1 MiB min chunk, 100 MiB total. Every byte must be handed out
	// exactly once, and the pool must subdivide rather than leave one huge
	// straggler range behind.
	const total = 100 << 20
	q := newWorkQueue(total, 10, 1<<20)
	var claimed int64
	var biggest int64
	for {
		it := q.claim()
		if it == nil {
			break
		}
		if it.Len() > biggest {
			biggest = it.Len()
		}
		claimed += it.Len()
		q.release()
	}
	if claimed != total {
		t.Fatalf("claimed %d bytes, want %d", claimed, total)
	}
	if biggest > total/4 {
		t.Fatalf("largest unit was %d bytes; the pool did not subdivide (total %d)", biggest, total)
	}
}

func TestRateLimiterCapsThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	// 1 MiB/s should let exactly 2 MiB through in about 2 seconds. The check is
	// two sided: a limiter that is too slow is just as wrong as one that leaks.
	rl := newRateLimiter(1 << 20)
	start := time.Now()
	ctx := context.Background()
	chunk := 64 << 10
	passes := 32 // 32 * 64 KiB = 2 MiB
	for i := 0; i < passes; i++ {
		if err := rl.wait(ctx, chunk); err != nil {
			t.Fatal(err)
		}
	}
	el := time.Since(start)
	rate := float64(passes*chunk) / el.Seconds()
	t.Logf("limiter delivered %s in %s (%.2f MiB/s, cap 1.00 MiB/s)",
		HumanBytes(int64(passes*chunk)), el, rate/(1<<20))
	if rate > 1.25*(1<<20) {
		t.Fatalf("rate limiter leaked: %.2f MiB/s over a 1.00 MiB/s cap", rate/(1<<20))
	}
	if rate < 0.75*(1<<20) {
		t.Fatalf("rate limiter over-throttled: %.2f MiB/s against a 1.00 MiB/s cap", rate/(1<<20))
	}
}

func TestRateLimiterConcurrentDoesNotLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	// 64 workers asking in parallel must still share one 1 MiB/s budget.
	rl := newRateLimiter(1 << 20)
	const workers = 64
	const each = 8
	chunk := 16 << 10 // 64*8*16 KiB = 8 MiB -> ~8s at 1 MiB/s
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				if err := rl.wait(context.Background(), chunk); err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	el := time.Since(start)
	rate := float64(workers*each*chunk) / el.Seconds()
	t.Logf("concurrent limiter delivered %s in %s (%.2f MiB/s, cap 1.00 MiB/s)",
		HumanBytes(int64(workers*each*chunk)), el, rate/(1<<20))
	if rate > 1.3*(1<<20) {
		t.Fatalf("concurrent limiter leaked: %.2f MiB/s over a 1.00 MiB/s cap", rate/(1<<20))
	}
}

func TestParseM3U8(t *testing.T) {
	pl := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:5
#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x00000000000000000000000000000001
#EXTINF:10.0,
seg0.ts
#EXTINF:10.0,
seg1.ts
#EXT-X-ENDLIST
`
	p := parseM3U8(pl, mustURL("https://cdn.test/hls/index.m3u8"))
	if len(p.Segments) != 2 {
		t.Fatalf("segments = %d, want 2", len(p.Segments))
	}
	if p.LiveStream {
		t.Fatal("playlist with ENDLIST must not be flagged live")
	}
	if p.Segments[0].Seq != 5 || p.Segments[1].Seq != 6 {
		t.Fatalf("sequence numbers wrong: %d, %d", p.Segments[0].Seq, p.Segments[1].Seq)
	}
	if !p.Segments[0].Enc || p.Segments[0].KeyURI != "key.bin" {
		t.Fatalf("encryption metadata not parsed: %+v", p.Segments[0])
	}
	// The default IV comes from the sequence number.
	iv, err := parseIV("", 1)
	if err != nil || iv[15] != 1 {
		t.Fatalf("default IV wrong: %x err=%v", iv, err)
	}
}

func TestMasterPlaylistPicksHighestBandwidth(t *testing.T) {
	pl := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
high.m3u8
`
	p := parseM3U8(pl, mustURL("https://cdn.test/hls/master.m3u8"))
	if !p.IsMaster || len(p.Variants) != 2 {
		t.Fatalf("master playlist not detected: %+v", p)
	}
	if got := pickBestVariant(p.Variants); got != "high.m3u8" {
		t.Fatalf("picked %q, want high.m3u8", got)
	}
}

func neturl(s string) (*urlURL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	return (*urlURL)(u), nil
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

type urlURL = url.URL

func mustURL(s string) *urlURL { return parseURLForTest(s) }

func parseURLForTest(s string) *urlURL {
	u, err := neturl(s)
	if err != nil {
		panic(err)
	}
	return u
}
