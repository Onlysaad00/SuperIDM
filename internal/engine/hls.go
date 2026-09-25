package engine

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HLS progressive capture.
//
// Live and on-demand streams are delivered as a playlist of small segments.
// Downloading them sequentially (what a browser does while playing) is slow;
// SuperIDM fetches every segment over the same adaptive pool used by the file
// engine and reassembles them in order, decrypting AES-128 playlists in flight.

type hlsSegment struct {
	URI     string
	KeyURI  string
	KeyIV   string
	Enc     bool
	InitMap bool
	Seq     int64
}

type hlsVariant struct {
	URI       string
	Bandwidth int64
	Height    int
}

type hlsPlaylist struct {
	Base       *url.URL
	Segments   []hlsSegment
	Variants   []hlsVariant
	IsMaster   bool
	LiveStream bool
	TargetDur  float64
}

// probeHLS inspects a media URL and returns the same shape of information the
// HTTP prober produces, so the job runner stays uniform.
func probeHLS(ctx context.Context, opts Options) (*probeInfo, error) {
	pl, err := fetchPlaylist(ctx, opts.URL, opts, 0)
	if err != nil {
		return nil, err
	}
	if pl.IsMaster {
		if best := pickBestVariant(pl.Variants); best != "" {
			abs, err := pl.Base.Parse(best)
			if err != nil {
				return nil, err
			}
			pl, err = fetchPlaylist(ctx, abs.String(), opts, 0)
			if err != nil {
				return nil, err
			}
		}
	}
	if len(pl.Segments) == 0 {
		return nil, fmt.Errorf("no media segments found - the stream may need a valid Referer or cookie")
	}
	name := opts.FileName
	if name == "" {
		name = opts.MediaTitle
	}
	if name == "" {
		name = GuessFileName(opts.URL, "")
	}
	name = strings.TrimSuffix(name, filepath.Ext(name))
	if name == "" {
		name = "stream"
	}
	return &probeInfo{
		FinalURL:    opts.URL,
		Size:        -1,
		Ranges:      false,
		FileName:    sanitizeName(name) + streamExt(pl),
		ContentType: "video/mp2t",
	}, nil
}

func streamExt(pl *hlsPlaylist) string {
	if len(pl.Segments) > 0 {
		low := strings.ToLower(pl.Segments[0].URI)
		switch {
		case strings.Contains(low, ".m4s"), strings.Contains(low, ".mp4"), strings.Contains(low, ".cmfv"), strings.Contains(low, ".cmfa"):
			return ".mp4"
		case strings.Contains(low, ".aac"):
			return ".aac"
		case strings.Contains(low, ".mp3"):
			return ".mp3"
		case strings.Contains(low, ".vtt"):
			return ".vtt"
		}
	}
	return ".ts"
}

func pickBestVariant(vs []hlsVariant) string {
	best := ""
	var bestBW int64 = -1
	for _, v := range vs {
		if v.Bandwidth > bestBW {
			bestBW = v.Bandwidth
			best = v.URI
		}
	}
	return best
}

// fetchPlaylist downloads and parses an M3U8 document.
func fetchPlaylist(ctx context.Context, rawURL string, opts Options, depth int) (*hlsPlaylist, error) {
	if depth > 4 {
		return nil, fmt.Errorf("playlist redirects too deep")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := buildRequest(cctx, "GET", rawURL, opts)
	if err != nil {
		return nil, err
	}
	req.Header.Del("Range")
	resp, err := NewClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot load playlist: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("playlist returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	body := string(data)
	if !strings.Contains(body, "#EXTM3U") {
		if strings.Contains(body, "<MPD") {
			return nil, fmt.Errorf("this is a DASH (MPD) manifest; SuperIDM currently captures HLS/M3U8 streams")
		}
		return nil, fmt.Errorf("not an M3U8 playlist (missing #EXTM3U)")
	}
	return parseM3U8(body, resp.Request.URL), nil
}

func parseM3U8(text string, base *url.URL) *hlsPlaylist {
	pl := &hlsPlaylist{Base: base}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	var (
		seq            int64
		pendingVariant *hlsVariant
		haveEndList    bool
		curKeyURI      string
		curKeyIV       string
		encrypted      bool
	)

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			switch {
			case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
				attrs := parseAttrList(line[len("#EXT-X-STREAM-INF:"):])
				v := hlsVariant{Bandwidth: atoiSafe(attrs["BANDWIDTH"])}
				if res := attrs["RESOLUTION"]; res != "" {
					if i := strings.IndexByte(res, 'x'); i > 0 {
						v.Height = int(atoiSafe(res[i+1:]))
					}
				}
				pendingVariant = &v
			case strings.HasPrefix(line, "#EXT-X-KEY:"):
				attrs := parseAttrList(line[len("#EXT-X-KEY:"):])
				switch strings.ToUpper(attrs["METHOD"]) {
				case "NONE", "":
					encrypted, curKeyURI, curKeyIV = false, "", ""
				case "AES-128":
					encrypted, curKeyURI, curKeyIV = true, attrs["URI"], attrs["IV"]
				default:
					// SAMPLE-AES cannot be handled without the CENC boxes;
					// record it as unsupported so the user gets a clear error.
					encrypted, curKeyURI, curKeyIV = true, "[unsupported]"+attrs["METHOD"], ""
				}
			case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
				seq = atoiSafe(strings.TrimSpace(line[len("#EXT-X-MEDIA-SEQUENCE:"):]))
			case strings.HasPrefix(line, "#EXT-X-MAP:"):
				attrs := parseAttrList(line[len("#EXT-X-MAP:"):])
				if u := attrs["URI"]; u != "" {
					pl.Segments = append(pl.Segments, hlsSegment{URI: u, Seq: -1, InitMap: true})
				}
			case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
				pl.TargetDur = float64(atoiSafe(strings.TrimSpace(line[len("#EXT-X-TARGETDURATION:"):])))
			case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
				haveEndList = true
			}
			continue
		}
		if pendingVariant != nil {
			pendingVariant.URI = line
			pl.Variants = append(pl.Variants, *pendingVariant)
			pendingVariant = nil
			pl.IsMaster = true
			continue
		}
		pl.Segments = append(pl.Segments, hlsSegment{
			URI:    line,
			Seq:    seq,
			Enc:    encrypted,
			KeyURI: curKeyURI,
			KeyIV:  curKeyIV,
		})
		seq++
	}
	if !pl.IsMaster {
		pl.LiveStream = !haveEndList
	}
	return pl
}

func parseIV(ivHex string, seq int64) ([]byte, error) {
	iv := make([]byte, 16)
	if ivHex != "" {
		s := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(ivHex), "0x"), "0X")
		b, err := hex.DecodeString(s)
		if err != nil {
			return nil, fmt.Errorf("bad IV %q: %w", ivHex, err)
		}
		if len(b) > 16 {
			return nil, fmt.Errorf("IV longer than 16 bytes")
		}
		copy(iv[16-len(b):], b)
		return iv, nil
	}
	// Default IV is the media sequence number, big endian, in the low 8 bytes.
	for i := 0; i < 8; i++ {
		iv[15-i] = byte(seq >> (8 * uint(i)))
	}
	return iv, nil
}

// parseAttrList splits comma separated KEY=VALUE pairs, honouring quotes.
func parseAttrList(s string) map[string]string {
	out := map[string]string{}
	var field strings.Builder
	inQuote := false
	flush := func() {
		f := strings.TrimSpace(field.String())
		field.Reset()
		if f == "" {
			return
		}
		if i := strings.IndexByte(f, '='); i > 0 {
			k := strings.ToUpper(strings.TrimSpace(f[:i]))
			v := strings.Trim(strings.TrimSpace(f[i+1:]), `"`)
			out[k] = v
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			field.WriteRune(r)
		case r == ',' && !inQuote:
			flush()
		default:
			field.WriteRune(r)
		}
	}
	flush()
	return out
}

func atoiSafe(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// runHLS downloads every segment in parallel and concatenates them in order.
func (j *Job) runHLS(ctx context.Context, info *probeInfo) {
	pl, err := fetchPlaylist(ctx, j.opts.URL, j.opts, 0)
	if err != nil {
		j.fail(err)
		return
	}
	if pl.IsMaster {
		if best := pickBestVariant(pl.Variants); best != "" {
			if abs, perr := pl.Base.Parse(best); perr == nil {
				if p2, err2 := fetchPlaylist(ctx, abs.String(), j.opts, 0); err2 == nil {
					pl = p2
				}
			}
		}
	}
	if pl.LiveStream {
		j.note("live playlist detected - capturing the segments listed right now")
	}

	f, err := os.OpenFile(j.savePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		j.fail(err)
		return
	}
	defer f.Close()

	total := len(pl.Segments)
	j.mu.Lock()
	j.total = 0
	j.conns = j.opts.Connections
	j.mu.Unlock()
	j.setSegments(0, total)

	var (
		mu        sync.Mutex
		cond      = sync.NewCond(&mu)
		buffered  = map[int64][]byte{}
		next      int64
		wrote     int64
		writeErr  error
		segFailed error
		index     atomic.Int64
		inflight  = make(chan struct{}, j.opts.Connections*2)
	)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			mu.Lock()
			for {
				if writeErr != nil || segFailed != nil {
					mu.Unlock()
					return
				}
				if data, ok := buffered[next]; ok {
					delete(buffered, next)
					next++
					mu.Unlock()
					if len(data) > 0 {
						if _, err := f.Write(data); err != nil {
							mu.Lock()
							writeErr = err
							mu.Unlock()
							cond.Broadcast()
							return
						}
						wrote += int64(len(data))
						j.done.Store(wrote)
					}
					j.setSegments(int(next), total)
					mu.Lock()
					continue
				}
				if next >= int64(total) {
					mu.Unlock()
					return
				}
				cond.Wait()
			}
		}
	}()

	workers := j.opts.Connections
	if workers > total {
		workers = total
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess := openSession(sharedTransport())
			defer sess.close()
			for {
				if ctx.Err() != nil {
					return
				}
				i := int(index.Add(1)) - 1
				if i >= total {
					return
				}
				select {
				case inflight <- struct{}{}:
				case <-ctx.Done():
					return
				}
				data, err := j.fetchSegment(ctx, sess, pl, i, info.FinalURL)
				<-inflight
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					mu.Lock()
					if segFailed == nil {
						segFailed = fmt.Errorf("segment %d of %d failed: %w", i+1, total, err)
					}
					mu.Unlock()
					cond.Broadcast()
					return
				}
				mu.Lock()
				buffered[int64(i)] = data
				mu.Unlock()
				cond.Broadcast()
			}
		}()
	}

	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(400 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				j.report(true)
			}
		}
	}()

	wg.Wait()
	mu.Lock()
	cond.Broadcast()
	mu.Unlock()
	<-writerDone
	close(stop)

	mu.Lock()
	remaining := len(buffered)
	failed := segFailed
	werr := writeErr
	mu.Unlock()

	switch {
	case ctx.Err() != nil:
		j.setStateIfRunning(StatePaused)
		j.report(true)
	case failed != nil:
		j.fail(failed)
	case werr != nil:
		j.fail(werr)
	case remaining > 0:
		j.fail(fmt.Errorf("%d segments could not be written in order", remaining))
	default:
		j.mu.Lock()
		j.total = wrote
		j.mu.Unlock()
		j.finish()
	}
}

func (j *Job) setSegments(done, total int) {
	j.mu.Lock()
	j.segDone = done
	j.segTotal = total
	j.mu.Unlock()
}

// fetchSegment downloads and (when required) decrypts one segment.
func (j *Job) fetchSegment(ctx context.Context, sess *httpSession, pl *hlsPlaylist, i int, referer string) ([]byte, error) {
	seg := pl.Segments[i]
	abs, err := pl.Base.Parse(seg.URI)
	if err != nil {
		return nil, err
	}
	opts := j.opts
	opts.URL = abs.String()
	if opts.Referer == "" {
		opts.Referer = referer
	}

	var key, iv []byte
	if seg.Enc {
		if strings.HasPrefix(seg.KeyURI, "[unsupported]") {
			return nil, fmt.Errorf("playlist uses %s encryption, which needs DRM support SuperIDM does not implement",
				strings.TrimPrefix(seg.KeyURI, "[unsupported]"))
		}
		key, err = j.mediaKey(ctx, pl, seg)
		if err != nil {
			return nil, err
		}
		iv, err = parseIV(seg.KeyIV, seg.Seq)
		if err != nil {
			return nil, err
		}
	}

	var lastErr error
	for attempt := 1; attempt <= j.opts.MaxRetries; attempt++ {
		stream, cancel, err := sess.open(ctx, opts, Range{Start: 0, End: -1})
		if err != nil {
			lastErr = err
		} else {
			data, rerr := io.ReadAll(io.LimitReader(stream.Body, 256<<20))
			cancel()
			_ = stream.Body.Close()
			if rerr == nil {
				if seg.Enc {
					return decryptAES128(data, key, iv)
				}
				return data, nil
			}
			lastErr = rerr
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		j.bumpRetry()
		if !sleepCtx(ctx, backoff(attempt)) {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// mediaKey fetches (and caches) the AES key referenced by a segment.
func (j *Job) mediaKey(ctx context.Context, pl *hlsPlaylist, seg hlsSegment) ([]byte, error) {
	abs, err := pl.Base.Parse(seg.KeyURI)
	if err != nil {
		return nil, err
	}
	urlStr := abs.String()

	j.keyMu.Lock()
	if j.keyCache == nil {
		j.keyCache = map[string][]byte{}
	}
	if k, ok := j.keyCache[urlStr]; ok {
		j.keyMu.Unlock()
		return k, nil
	}
	j.keyMu.Unlock()

	req, err := buildRequest(ctx, "GET", urlStr, j.opts)
	if err != nil {
		return nil, err
	}
	resp, err := NewClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot fetch decryption key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("decryption key returned HTTP %d", resp.StatusCode)
	}
	key, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	if len(key) != 16 {
		return nil, fmt.Errorf("expected a 16-byte AES-128 key, got %d bytes", len(key))
	}
	j.keyMu.Lock()
	j.keyCache[urlStr] = key
	j.keyMu.Unlock()
	return key, nil
}

// decryptAES128 performs AES-128-CBC decryption and strips PKCS#7 padding.
func decryptAES128(data, key, iv []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	if len(key) != 16 {
		return nil, fmt.Errorf("AES-128 key must be 16 bytes, got %d", len(key))
	}
	if len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted segment is not block aligned (%d bytes)", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	if n := int(out[len(out)-1]); n > 0 && n <= aes.BlockSize && n <= len(out) {
		valid := true
		for _, b := range out[len(out)-n:] {
			if int(b) != n {
				valid = false
				break
			}
		}
		if valid {
			out = out[:len(out)-n]
		}
	}
	return out, nil
}
