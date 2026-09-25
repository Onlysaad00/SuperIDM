package api

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Onlysaad00/SuperIDM/internal/service"
)

// newTestServer boots a real service + API server against a temp config dir, so
// the tests exercise exactly the code path the browser extension uses.
func newTestServer(t *testing.T) (*Server, *service.Service, string) {
	t.Helper()
	// Keep the test out of the developer's real %APPDATA%.
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("APPDATA", cfgDir)
	t.Setenv("HOME", cfgDir)

	svc := service.New("test-1.0")
	t.Cleanup(svc.Close)

	// Port 0 asks the OS for any free port, so tests never fight over one.
	srv := New(svc, Options{Port: 0, Quiet: true})
	if err := srv.Start(); err != nil {
		t.Fatalf("cannot start the API server: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv, svc, srv.URL()
}

func doJSON(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if out != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, out); err != nil {
			t.Fatalf("%s %s: cannot decode %q: %v", method, url, payload, err)
		}
	}
	return resp.StatusCode
}

// rangeOrigin serves a payload with byte-range support, like a normal CDN.
func rangeOrigin(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", "application/octet-stream")
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			_, _ = w.Write(payload)
			return
		}
		spec := strings.TrimPrefix(rng, "bytes=")
		var start, end int64
		i := strings.IndexByte(spec, '-')
		start, _ = strconv.ParseInt(spec[:i], 10, 64)
		if spec[i+1:] == "" {
			end = int64(len(payload)) - 1
		} else {
			end, _ = strconv.ParseInt(spec[i+1:], 10, 64)
		}
		if start >= int64(len(payload)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= int64(len(payload)) {
			end = int64(len(payload)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthAndUI(t *testing.T) {
	_, _, base := newTestServer(t)

	var health map[string]any
	if code := doJSON(t, "GET", base+"api/health", nil, &health); code != 200 {
		t.Fatalf("health returned %d", code)
	}
	if health["app"] != "SuperIDM" || health["ok"] != true {
		t.Fatalf("unexpected health payload: %v", health)
	}

	// The UI must be served from the embedded filesystem.
	for _, path := range []string{"", "app.js", "style.css", "logo.svg"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(body) == 0 {
			t.Fatalf("%q: status %d, %d bytes", path, resp.StatusCode, len(body))
		}
	}
}

func TestAddDownloadEndToEnd(t *testing.T) {
	_, _, base := newTestServer(t)

	payload := make([]byte, 3<<20+777)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	origin := rangeOrigin(t, payload)

	dir := t.TempDir()
	var job map[string]any
	code := doJSON(t, "POST", base+"api/jobs", map[string]any{
		"url":         origin.URL + "/file.bin",
		"dir":         dir,
		"fileName":    "report.bin",
		"connections": 16,
	}, &job)
	if code != 200 {
		t.Fatalf("add returned %d (%v)", code, job)
	}
	if job["fileName"] != "report.bin" {
		t.Fatalf("the requested file name was ignored: %v", job["fileName"])
	}
	id, _ := job["id"].(string)

	// Wait for completion through the API, the way the UI does.
	var snap map[string]any
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		doJSON(t, "GET", base+"api/jobs/"+id, nil, &snap)
		if s, _ := snap["state"].(string); s == "completed" || s == "error" {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if snap["state"] != "completed" {
		t.Fatalf("job did not complete: %v", snap["error"])
	}
	saved, _ := snap["savePath"].(string)
	got, err := os.ReadFile(saved)
	if err != nil {
		t.Fatalf("cannot read the result: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes, want %d and identical", len(got), len(payload))
	}
	if segs, _ := snap["segments"].(float64); segs < 2 {
		t.Fatalf("expected a multi-segment transfer, got %v segments", snap["segments"])
	}
}

func TestJobLifecycleActions(t *testing.T) {
	_, _, base := newTestServer(t)

	payload := make([]byte, 2<<20)
	origin := rangeOrigin(t, payload)

	var job map[string]any
	doJSON(t, "POST", base+"api/jobs", map[string]any{
		"url": origin.URL + "/slow.bin", "dir": t.TempDir(),
		"fileName": "lifecycle.bin", "connections": 8, "startPaused": true,
	}, &job)
	id := job["id"].(string)

	var snap map[string]any
	doJSON(t, "GET", base+"api/jobs/"+id, nil, &snap)
	if snap["state"] != "paused" {
		t.Fatalf("a job added with startPaused should be paused, got %v", snap["state"])
	}

	doJSON(t, "POST", base+"api/jobs/"+id+"/resume", map[string]any{}, nil)
	for i := 0; i < 300; i++ {
		doJSON(t, "GET", base+"api/jobs/"+id, nil, &snap)
		if snap["state"] == "completed" || snap["state"] == "error" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snap["state"] != "completed" {
		t.Fatalf("resume did not finish the job: %v", snap["error"])
	}

	// Removing without delete keeps the file.
	path := snap["savePath"].(string)
	doJSON(t, "DELETE", base+"api/jobs/"+id+"?delete=0", nil, nil)
	if code := doJSON(t, "GET", base+"api/jobs/"+id, nil, nil); code != 404 {
		t.Fatalf("a removed job should be gone, got status %d", code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file must survive Remove(delete=false): %v", err)
	}

	// A second job, removed with delete=true, must take its file with it.
	doJSON(t, "POST", base+"api/jobs", map[string]any{
		"url": origin.URL + "/x.bin", "dir": t.TempDir(), "fileName": "gonesoon.bin",
	}, &job)
	id2 := job["id"].(string)
	for i := 0; i < 300; i++ {
		doJSON(t, "GET", base+"api/jobs/"+id2, nil, &snap)
		if snap["state"] == "completed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	p2 := snap["savePath"].(string)
	doJSON(t, "DELETE", base+"api/jobs/"+id2+"?delete=1", nil, nil)
	if _, err := os.Stat(p2); !os.IsNotExist(err) {
		t.Fatalf("Remove(delete=true) must delete %s (stat err: %v)", p2, err)
	}
}

func TestSettingsRoundTripAndClamping(t *testing.T) {
	_, svc, base := newTestServer(t)

	var got map[string]any
	doJSON(t, "GET", base+"api/settings", nil, &got)
	settings, _ := got["settings"].(map[string]any)
	if settings["connections"].(float64) != 64 {
		t.Fatalf("the default should be 64 connections, got %v", settings["connections"])
	}

	// Out-of-range values must be clamped, not accepted.
	settings["connections"] = 9999
	settings["maxConcurrent"] = 0
	settings["bufferKB"] = 1
	var saved map[string]any
	doJSON(t, "POST", base+"api/settings", settings, &saved)

	cur := svc.Settings()
	if cur.Connections > 128 || cur.Connections < 1 {
		t.Fatalf("connections not clamped: %d", cur.Connections)
	}
	if cur.MaxConcurrent < 1 {
		t.Fatalf("maxConcurrent not clamped: %d", cur.MaxConcurrent)
	}
	if cur.BufferKB < 16 {
		t.Fatalf("bufferKB not clamped: %d", cur.BufferKB)
	}
}

func TestInspectFindsSizeAndRanges(t *testing.T) {
	_, _, base := newTestServer(t)

	payload := make([]byte, 1<<20)
	origin := rangeOrigin(t, payload)

	var info map[string]any
	code := doJSON(t, "GET", base+"api/inspect?url="+origin.URL+"/thing.tar.gz", nil, &info)
	if code != 200 {
		t.Fatalf("inspect returned %d", code)
	}
	if info["ranges"] != true {
		t.Fatalf("byte ranges not detected: %v", info)
	}
	if info["size"].(float64) != float64(len(payload)) {
		t.Fatalf("size = %v, want %d", info["size"], len(payload))
	}
	if info["fileName"] != "thing.tar.gz" {
		t.Fatalf("file name = %v", info["fileName"])
	}
}

func TestInspectReportsUnreachableHosts(t *testing.T) {
	_, _, base := newTestServer(t)
	var info map[string]any
	doJSON(t, "GET", base+"api/inspect?url=http://127.0.0.1:1/nothing", nil, &info)
	if info["error"] == nil || info["error"] == "" {
		t.Fatalf("expected an error for an unreachable host, got %v", info)
	}
}

func TestBadRequestsAreRejected(t *testing.T) {
	_, _, base := newTestServer(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"no url", map[string]any{"fileName": "x"}},
		{"non-http scheme", map[string]any{"url": "ftp://example.com/x"}},
		{"javascript url", map[string]any{"url": "javascript:alert(1)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code := doJSON(t, "POST", base+"api/jobs", c.body, nil); code != 400 {
				t.Fatalf("expected 400, got %d", code)
			}
		})
	}
	if code := doJSON(t, "POST", base+"api/jobs/does-not-exist/pause", map[string]any{}, nil); code != 400 {
		t.Fatalf("expected 400 for an unknown job, got %d", code)
	}
}

func TestSnifferEndToEnd(t *testing.T) {
	_, _, base := newTestServer(t)

	// A browser-style batch post.
	var res map[string]any
	doJSON(t, "POST", base+"api/sniff", map[string]any{
		"items": []map[string]any{
			{"url": "https://cdn.example/movie/index.m3u8", "kind": "hls", "title": "Movie", "pageUrl": "https://site.example/watch"},
			{"url": "https://cdn.example/movie.srt", "mime": "application/octet-stream"},
			{"url": "notaurl"},            // ignored
			{"url": "file:///etc/passwd"}, // ignored
		},
	}, &res)
	if res["added"].(float64) != 2 {
		t.Fatalf("expected 2 usable items, got %v", res["added"])
	}

	// Duplicates are merged, not appended.
	doJSON(t, "POST", base+"api/sniff", map[string]any{
		"url": "https://cdn.example/movie/index.m3u8", "kind": "hls",
	}, &res)
	var listed struct {
		Items []map[string]any `json:"items"`
	}
	doJSON(t, "GET", base+"api/sniff", nil, &listed)
	if len(listed.Items) != 2 {
		t.Fatalf("expected 2 captured items after a duplicate post, got %d", len(listed.Items))
	}
	// The kind from the URL extension should have been inferred.
	for _, it := range listed.Items {
		if it["url"] == "https://cdn.example/movie.srt" && it["kind"] != "file" {
			t.Fatalf("expected a .srt to be classified as a file, got %v", it["kind"])
		}
	}

	// Remove one item, then clear the rest.
	id := listed.Items[0]["id"].(string)
	doJSON(t, "POST", base+"api/sniff/remove", map[string]any{"id": id}, nil)
	doJSON(t, "GET", base+"api/sniff", nil, &listed)
	if len(listed.Items) != 1 {
		t.Fatalf("removal failed, %d items left", len(listed.Items))
	}
	doJSON(t, "POST", base+"api/sniff/clear", map[string]any{}, nil)
	doJSON(t, "GET", base+"api/sniff", nil, &listed)
	if len(listed.Items) != 0 {
		t.Fatalf("clear failed, %d items left", len(listed.Items))
	}
}

func TestSnifferDownloadsCapturedFile(t *testing.T) {
	_, _, base := newTestServer(t)

	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i * 3)
	}
	origin := rangeOrigin(t, payload)
	sum := sha256.Sum256(payload)

	// Capture it the way the content script would, then download it.
	var res map[string]any
	doJSON(t, "POST", base+"api/sniff", map[string]any{
		"url": origin.URL + "/captured.bin", "kind": "file", "title": "captured.bin",
		"referer": origin.URL + "/page",
	}, &res)

	var listed struct {
		Items []map[string]any `json:"items"`
	}
	doJSON(t, "GET", base+"api/sniff", nil, &listed)
	if len(listed.Items) != 1 {
		t.Fatalf("expected one captured item, got %d", len(listed.Items))
	}

	var job map[string]any
	doJSON(t, "POST", base+"api/sniff/download", map[string]any{
		"id": listed.Items[0]["id"], "dir": t.TempDir(),
	}, &job)
	id := job["id"].(string)

	var snap map[string]any
	for i := 0; i < 400; i++ {
		doJSON(t, "GET", base+"api/jobs/"+id, nil, &snap)
		if snap["state"] == "completed" || snap["state"] == "error" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snap["state"] != "completed" {
		t.Fatalf("captured media download failed: %v", snap["error"])
	}
	got, _ := os.ReadFile(snap["savePath"].(string))
	if hex.EncodeToString(got) == "" || len(got) != len(payload) {
		t.Fatalf("downloaded %d bytes, want %d", len(got), len(payload))
	}
	if fmt.Sprintf("%x", sha256.Sum256(got)) != fmt.Sprintf("%x", sum) {
		t.Fatal("captured media content mismatch")
	}

	// The item must now point at its job.
	doJSON(t, "GET", base+"api/sniff", nil, &listed)
	if listed.Items[0]["jobId"] != id {
		t.Fatalf("captured item was not linked to its job: %v", listed.Items[0])
	}
}

func TestClearCompletedKeepsFilesByDefault(t *testing.T) {
	_, _, base := newTestServer(t)

	payload := make([]byte, 512<<10)
	origin := rangeOrigin(t, payload)
	dir := t.TempDir()

	var job map[string]any
	doJSON(t, "POST", base+"api/jobs", map[string]any{
		"url": origin.URL + "/a.bin", "dir": dir, "fileName": "keepme.bin",
	}, &job)

	var snap map[string]any
	for i := 0; i < 300; i++ {
		doJSON(t, "GET", base+"api/jobs/"+job["id"].(string), nil, &snap)
		if snap["state"] == "completed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	path := snap["savePath"].(string)

	doJSON(t, "POST", base+"api/jobs/actions/clear-completed?delete=0", map[string]any{}, nil)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("clear-completed must not delete files by default: %v", err)
	}
	var list struct {
		Jobs []map[string]any `json:"jobs"`
	}
	doJSON(t, "GET", base+"api/jobs", nil, &list)
	if len(list.Jobs) != 0 {
		t.Fatalf("clear-completed left %d jobs in the list", len(list.Jobs))
	}
}

func TestEventStreamPushesJobs(t *testing.T) {
	_, _, base := newTestServer(t)

	// Subscribe first, then add a job and expect a "jobs" event carrying it.
	req, _ := http.NewRequest("GET", base+"api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("events returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("wrong content type: %s", ct)
	}

	payload := make([]byte, 256<<10)
	origin := rangeOrigin(t, payload)
	doJSON(t, "POST", base+"api/jobs", map[string]any{
		"url": origin.URL + "/e.bin", "dir": t.TempDir(),
	}, nil)

	reader := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(15 * time.Second)
	sawJob := false
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev service.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		if ev.Type == "jobs" && len(ev.Jobs) > 0 {
			sawJob = true
			break
		}
	}
	if !sawJob {
		t.Fatal("no jobs event was pushed over the event stream")
	}
}

func TestCORSAllowsExtensionsOnly(t *testing.T) {
	_, _, base := newTestServer(t)

	req, _ := http.NewRequest("GET", base+"api/health", nil)
	req.Header.Set("Origin", "chrome-extension://abcdefghijklmnop")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "chrome-extension://abcdefghijklmnop" {
		t.Fatalf("extension origin was not allowed: %q", resp.Header.Get("Access-Control-Allow-Origin"))
	}

	req2, _ := http.NewRequest("GET", base+"api/health", nil)
	req2.Header.Set("Origin", "https://evil.example")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("a random website origin must not be reflected, got %q", got)
	}
}

func TestServerBindsLoopbackOnly(t *testing.T) {
	_, _, base := newTestServer(t)
	if !strings.Contains(base, "127.0.0.1") {
		t.Fatalf("the API must bind loopback, got %s", base)
	}
}

func TestPortFallbackWhenBusy(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("APPDATA", cfgDir)
	t.Setenv("HOME", cfgDir)
	svc := service.New("test")
	t.Cleanup(svc.Close)

	// Reserve a port, release it, then make sure the fallback logic works.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wanted := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	first := New(svc, Options{Port: wanted})
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer first.Stop()
	obtained := first.Port()
	if obtained != wanted {
		t.Fatalf("expected to bind %d, got %d", wanted, obtained)
	}

	second := New(svc, Options{Port: wanted})
	if err := second.Start(); err != nil {
		t.Fatalf("the second server should fall back to another port: %v", err)
	}
	defer second.Stop()
	if second.Port() == obtained {
		t.Fatalf("both servers grabbed port %d", obtained)
	}
}

func TestExtensionPathEndpoint(t *testing.T) {
	_, _, base := newTestServer(t)
	// Either the folder ships next to the binary or the endpoint reports 404;
	// both are valid, but it must never blow up.
	resp, err := http.Get(base + "api/extension-path")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 404 {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}
}

func TestPathTraversalIsNotServed(t *testing.T) {
	_, _, base := newTestServer(t)
	for _, p := range []string{"../go.mod", "..%2fgo.mod", "web/../../go.mod"} {
		resp, err := http.Get(base + p)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if bytes.Contains(body, []byte("module github.com/Onlysaad00/SuperIDM")) {
			t.Fatalf("path traversal leaked go.mod via %q", p)
		}
	}
}

func TestLogEndpoint(t *testing.T) {
	srv, _, base := newTestServer(t)
	srv.svc.Log("info", "hello from the test")

	// The log file is written to the config dir; give it a moment on slow disks.
	var body []byte
	for i := 0; i < 40; i++ {
		resp, err := http.Get(base + "api/log")
		if err == nil {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(body), "hello from the test") {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the log endpoint did not serve the entry (got %d bytes)", len(body))
}

func TestConfigDirIsIsolatedInTests(t *testing.T) {
	_, _, base := newTestServer(t)
	var info map[string]any
	doJSON(t, "GET", base+"api/settings", nil, &info)
	dir, _ := info["configDir"].(string)
	if dir == "" {
		t.Fatal("configDir missing from the settings payload")
	}
	if !strings.HasPrefix(dir, os.TempDir()) && !strings.Contains(dir, "Test") {
		// t.TempDir() lives under the test's own temp root.
		home, _ := os.UserHomeDir()
		if strings.HasPrefix(dir, home) && !strings.Contains(dir, "Test") {
			t.Logf("note: config dir %q is outside the test temp dir", dir)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		t.Fatalf("settings.json was not written to the config dir: %v", err)
	}
}
