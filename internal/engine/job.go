package engine

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Logf is the package-level log sink. The service layer points it at the log
// file so the UI can show per-connection events.
var Logf = func(format string, args ...any) {}

// Job is a single download with its own connection pool, progress accounting
// and resilience state.
type Job struct {
	ID   string
	opts Options

	mu          sync.Mutex
	state       State
	total       int64
	completed   []Range // merged-ish list of fully written ranges (resume map)
	finalURL    string
	fileName    string
	savePath    string
	contentType string
	rangesOK    bool
	err         error
	checksumOK  *bool
	startedAt   time.Time
	finishedAt  time.Time
	addedAt     time.Time
	conns       int
	retries     int
	active      int
	segDone     int
	segTotal    int

	keyMu    sync.Mutex
	keyCache map[string][]byte

	done atomic.Int64

	cancel  context.CancelFunc
	doneCh  chan struct{}
	wg      sync.WaitGroup
	prog    ProgressFunc
	limiter *rateLimiter

	speedMu     sync.Mutex
	lastBytes   int64
	lastSample  time.Time
	smoothSpeed float64
	peakSpeed   float64
}

// NewJob creates a job in the queued state.
func NewJob(id string, opts Options, limiter *rateLimiter, prog ProgressFunc) *Job {
	if limiter == nil {
		limiter = newRateLimiter(opts.SpeedLimit)
	}
	now := time.Now()
	opts = opts.withDefaults()
	return &Job{
		ID:    id,
		opts:  opts,
		state: StateQueued,
		// An explicit name/path always wins over what the URL suggests; the
		// prober only fills these in when they are still empty.
		fileName: opts.FileName,
		savePath: opts.SavePath,
		addedAt:  now,
		doneCh:   make(chan struct{}),
		prog:     prog,
		limiter:  limiter,
	}
}

// Options returns the effective options (defaults applied).
func (j *Job) Options() Options { return j.opts }

// MarkPaused puts a job that has never run into the paused state, so a
// download added with "start paused" - or restored from a previous session -
// is reported correctly instead of looking merely queued.
func (j *Job) MarkPaused() {
	j.mu.Lock()
	if j.state == StateQueued {
		j.state = StatePaused
	}
	j.mu.Unlock()
}

// Restore seeds resume state recovered from a previous session.
func (j *Job) Restore(total, done int64, completed []Range, finalURL, fileName, savePath, contentType string, rangesOK bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if total > 0 {
		j.total = total
	}
	if done > 0 {
		j.done.Store(done)
	}
	j.completed = append(j.completed, completed...)
	if finalURL != "" {
		j.finalURL = finalURL
	}
	if fileName != "" {
		j.fileName = fileName
	}
	if savePath != "" {
		j.savePath = savePath
	}
	j.contentType = contentType
	j.rangesOK = rangesOK
}

func (j *Job) setState(s State) {
	j.mu.Lock()
	j.state = s
	j.mu.Unlock()
}

func (j *Job) setStateIfRunning(s State) {
	j.mu.Lock()
	if !j.state.IsFinal() {
		j.state = s
	}
	j.mu.Unlock()
}

// State returns the current state.
func (j *Job) State() State {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

// Start launches the transfer in the background.
func (j *Job) Start(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	j.mu.Lock()
	if j.state.IsFinal() || j.state == StateDownloading || j.state == StateConnecting {
		j.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	j.cancel = cancel
	j.state = StateConnecting
	j.mu.Unlock()

	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		defer cancel()
		j.run(ctx)
	}()
}

// Pause stops the transfer, keeping the partial file and the resume map.
func (j *Job) Pause() {
	j.mu.Lock()
	if j.state.IsFinal() || j.state == StatePaused || j.state == StateQueued {
		j.mu.Unlock()
		return
	}
	j.state = StatePaused
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	j.wg.Wait()
	j.mu.Lock()
	j.state = StatePaused
	j.mu.Unlock()
	j.report(true)
}

// Cancel aborts the job and removes the partial file.
func (j *Job) Cancel() {
	j.mu.Lock()
	if j.state.IsFinal() {
		j.mu.Unlock()
		return
	}
	j.state = StateCanceled
	c := j.cancel
	path := j.savePath
	j.mu.Unlock()
	if c != nil {
		c()
	}
	j.wg.Wait()
	if path != "" {
		_ = os.Remove(path)
	}
	j.mu.Lock()
	j.state = StateCanceled
	j.finishedAt = time.Now()
	j.mu.Unlock()
	j.report(true)
}

// Wait blocks until the job reaches a final state.
func (j *Job) Wait() { j.wg.Wait() }

// SetSpeedLimit changes the per-job cap at runtime (0 = unlimited).
func (j *Job) SetSpeedLimit(bps int64) {
	j.mu.Lock()
	j.opts.SpeedLimit = bps
	j.mu.Unlock()
	j.limiter.setLimit(bps)
}

// ---------------------------------------------------------------------------
// main run loop
// ---------------------------------------------------------------------------

func (j *Job) run(ctx context.Context) {
	j.mu.Lock()
	j.startedAt = time.Now()
	j.mu.Unlock()
	j.report(true)

	var info *probeInfo
	var err error
	if j.opts.Kind == "hls" {
		info, err = probeHLS(ctx, j.opts)
	} else {
		info, err = probe(ctx, j.opts)
	}
	if err != nil {
		j.fail(err)
		return
	}

	j.mu.Lock()
	j.finalURL = info.FinalURL
	j.total = info.Size
	j.rangesOK = info.Ranges
	j.contentType = info.ContentType
	if j.fileName == "" {
		j.fileName = info.FileName
	}
	j.mu.Unlock()

	if err := j.prepareFile(); err != nil {
		j.fail(err)
		return
	}

	j.setState(StateDownloading)
	j.report(true)

	if j.opts.Kind == "hls" {
		j.runHLS(ctx, info)
		return
	}
	if !j.rangesOK || j.total <= 0 {
		if j.total > SmallFileThreshold {
			j.note("server does not support byte ranges - using a single connection")
		}
		j.runSingle(ctx)
		return
	}
	j.runParallel(ctx)
}

// prepareFile creates the destination file and pre-allocates its final size.
// Pre-allocation lets every connection write its range with pwrite at an
// arbitrary offset, which removes the per-segment temp files and the final
// merge pass that classic download managers have to perform.
func (j *Job) prepareFile() error {
	j.mu.Lock()
	dir := j.opts.Dir
	cat := j.opts.Category
	name := j.fileName
	path := j.savePath
	total := j.total
	j.mu.Unlock()

	if path == "" {
		if dir == "" {
			dir = DefaultDownloadDir()
		}
		if cat != "" && !strings.EqualFold(filepath.Base(dir), cat) {
			dir = filepath.Join(dir, cat)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("cannot create folder %s: %w", dir, err)
		}
		if name == "" {
			name = "download"
		}
		path = UniquePath(filepath.Join(dir, name), j.opts.Overwrite)
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer f.Close()

	j.mu.Lock()
	j.savePath = path
	j.fileName = filepath.Base(path)
	j.mu.Unlock()

	if total > 0 {
		if err := f.Truncate(total); err != nil {
			return fmt.Errorf("cannot pre-allocate file: %w", err)
		}
	}
	return nil
}

// runSingle downloads over one connection: servers without range support,
// unknown-length streams and very small files.
func (j *Job) runSingle(ctx context.Context) {
	sess := openSession(sharedTransport())
	defer sess.close()

	attempt := 0
	for {
		if ctx.Err() != nil {
			j.setStateIfRunning(StatePaused)
			j.report(true)
			return
		}
		attempt++
		if attempt > j.opts.MaxRetries {
			j.fail(fmt.Errorf("giving up after %d attempts", attempt-1))
			return
		}
		err := j.singlePass(ctx, sess)
		if err == nil {
			j.finish()
			return
		}
		if ctx.Err() != nil {
			j.completeOrPause()
			return
		}
		j.mu.Lock()
		j.retries++
		j.mu.Unlock()
		j.note("retrying after error: " + err.Error())
		if !sleepCtx(ctx, backoff(attempt)) {
			j.setStateIfRunning(StatePaused)
			return
		}
	}
}

func (j *Job) singlePass(ctx context.Context, sess *httpSession) error {
	j.mu.Lock()
	start := j.done.Load()
	total := j.total
	j.mu.Unlock()

	stream, cancel, err := sess.open(ctx, j.opts, Range{Start: start, End: -1})
	if err != nil {
		return err
	}
	defer cancel()
	defer stream.Body.Close()

	off := start
	if stream.Status == 200 && start > 0 {
		off = 0 // the server ignored our resume offset and restarted the body
		j.done.Store(0)
	}
	if stream.Total > 0 && total <= 0 {
		j.mu.Lock()
		j.total = stream.Total
		j.mu.Unlock()
	}

	f, err := os.OpenFile(j.savePath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, j.opts.BufferSize)
	activity := &atomic.Int64{}
	activity.Store(time.Now().UnixNano())
	stopWatch := j.watch(ctx, activity, cancel)
	defer stopWatch()
	defer j.report(true)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, rerr := stream.Body.Read(buf)
		if n > 0 {
			activity.Store(time.Now().UnixNano())
			if err := j.limiter.wait(ctx, n); err != nil {
				return err
			}
			if _, werr := f.WriteAt(buf[:n], off); werr != nil {
				return fmt.Errorf("disk write failed: %w", werr)
			}
			off += int64(n)
			j.done.Store(off)
			j.recordCompleted(Range{Start: off - int64(n), End: off})
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// runParallel is the fast path: N independent connections fed by the adaptive
// work queue.
func (j *Job) runParallel(ctx context.Context) {
	j.mu.Lock()
	total := j.total
	conns := j.opts.Connections
	completed := MergeRanges(j.completed)
	j.mu.Unlock()

	if total <= SmallFileThreshold {
		// Tiny files: one request beats handshaking dozens of sockets.
		conns = 1
	}

	regions := subtractRanges(Range{Start: 0, End: total}, completed)
	if len(regions) == 0 {
		j.finish()
		return
	}

	var already int64
	for _, c := range completed {
		already += c.Len()
	}
	j.done.Store(already)

	q := newWorkQueueRegions(regions, conns, j.opts.MinChunk)
	j.mu.Lock()
	j.conns = conns
	j.completed = completed
	j.mu.Unlock()

	var workers sync.WaitGroup
	for i := 0; i < conns; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			sess := openSession(sharedTransport())
			defer sess.close()
			j.worker(ctx, q, sess)
		}()
	}
	workers.Wait()
	q.close()

	if ctx.Err() != nil {
		// A pause that lands after the final byte must not leave the job
		// looking unfinished at 100%.
		if j.done.Load() >= total {
			j.finish()
			return
		}
		j.completeOrPause()
		return
	}
	if j.hardFailed() {
		j.fail(j.error())
		return
	}
	if j.done.Load() >= total {
		j.finish()
		return
	}
	j.fail(fmt.Errorf("incomplete transfer: %s of %s written", HumanBytes(j.done.Load()), HumanBytes(total)))
}

// worker claims units until the pool is drained.
func (j *Job) worker(ctx context.Context, q *workQueue, sess *httpSession) {
	for {
		if ctx.Err() != nil {
			return
		}
		item := q.claim()
		if item == nil {
			return
		}
		j.downloadUnit(ctx, q, sess, item)
		if j.hardFailed() {
			q.close()
			return
		}
	}
}

// downloadUnit transfers one work unit, retrying and re-queuing the unfinished
// tail on stall or error.
func (j *Job) downloadUnit(ctx context.Context, q *workQueue, sess *httpSession, item *workItem) {
	r := item.Range
	attempts := item.Attempts

	for {
		if ctx.Err() != nil {
			q.requeue(r, attempts)
			return
		}
		if attempts > j.opts.MaxRetries {
			j.setErr(fmt.Errorf("range %s failed after %d attempts on every connection", r.String(), attempts-1))
			q.requeue(r, attempts)
			return
		}

		stream, cancel, err := sess.open(ctx, j.opts, r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// 416: the server says this range is already satisfied.
				j.recordCompleted(r)
				q.release()
				return
			}
			attempts++
			j.bumpRetry()
			j.note(fmt.Sprintf("connect failed (%v); retry %d/%d", err, attempts-1, j.opts.MaxRetries))
			if !sleepCtx(ctx, backoff(attempts)) {
				q.requeue(r, attempts)
				return
			}
			continue
		}

		written, cerr := j.consume(ctx, r, stream, cancel)
		cancel()
		_ = stream.Body.Close()
		q.broadcast()

		if written > 0 {
			j.recordCompleted(Range{Start: r.Start, End: r.Start + written})
		}
		rest := Range{Start: r.Start + written, End: r.End}

		if cerr == nil && r.End < 0 {
			// Unknown-length stream ended: nothing left to fetch.
			q.release()
			return
		}
		if cerr == nil && rest.Len() <= 0 {
			q.release()
			return
		}
		if ctx.Err() != nil {
			// Paused: keep the tail for the next session.
			q.requeue(rest, attempts)
			return
		}

		attempts++
		j.bumpRetry()
		if cerr != nil {
			j.note(fmt.Sprintf("connection dropped at %s (%v); re-queued %s",
				HumanBytes(rest.Start), cerr, HumanBytes(rest.Len())))
		} else {
			j.note(fmt.Sprintf("short read at %s; re-queued %s", HumanBytes(rest.Start), HumanBytes(rest.Len())))
		}

		if !sleepCtx(ctx, backoff(attempts)) {
			q.requeue(rest, attempts)
			return
		}
		// Don't keep re-walking the whole tail on one connection: hand half of
		// it to the pool so a healthy connection can race ahead. Ranges are
		// disjoint, and writes are idempotent (same bytes, same offsets).
		if rest.Len() >= 2*j.opts.MinChunk {
			half := rest.Start + rest.Len()/2
			q.partial(Range{Start: half, End: rest.End}, attempts)
			rest = Range{Start: rest.Start, End: half}
		}
		r = rest
	}
}

// consume streams a response body into the file at the right offsets and
// returns how many bytes were durably written.
func (j *Job) consume(ctx context.Context, r Range, stream *rangeStream, cancel context.CancelFunc) (int64, error) {
	f, err := os.OpenFile(j.savePath, os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	offset := r.Start
	if stream.Status == 200 && r.Start > 0 {
		// The server ignored the Range header and started from byte 0.
		// Discard the prefix so the remaining bytes still land correctly.
		if _, err := io.CopyN(io.Discard, stream.Body, r.Start); err != nil {
			return 0, err
		}
	}

	buf := make([]byte, j.opts.BufferSize)
	activity := &atomic.Int64{}
	activity.Store(time.Now().UnixNano())
	stopWatch := j.watch(ctx, activity, cancel)
	defer stopWatch()

	var written int64
	for {
		if ctx.Err() != nil {
			return written, ctx.Err()
		}
		if r.End >= 0 {
			remaining := r.End - offset
			if remaining <= 0 {
				return written, nil
			}
			if int64(len(buf)) > remaining {
				buf = buf[:remaining]
			}
		}
		n, rerr := stream.Body.Read(buf)
		if n > 0 {
			activity.Store(time.Now().UnixNano())
			if err := j.limiter.wait(ctx, n); err != nil {
				return written, err
			}
			if _, werr := f.WriteAt(buf[:n], offset); werr != nil {
				return written, fmt.Errorf("disk write failed: %w", werr)
			}
			offset += int64(n)
			written += int64(n)
			j.done.Add(int64(n))
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, rerr
		}
	}
}

// watch arms a watchdog that aborts the request when no bytes arrive within
// the idle timeout. This converts a silently throttled or dead socket into an
// instant retry on a fresh connection instead of a multi-minute hang - the
// single most common cause of a "stuck" download in other managers.
func (j *Job) watch(ctx context.Context, activity *atomic.Int64, cancel context.CancelFunc) func() {
	done := make(chan struct{})
	j.mu.Lock()
	j.active++
	j.mu.Unlock()
	if j.opts.IdleTimeout > 0 {
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-t.C:
					if time.Since(time.Unix(0, activity.Load())) > j.opts.IdleTimeout {
						cancel()
						return
					}
				}
			}
		}()
	}
	return func() {
		close(done)
		j.mu.Lock()
		if j.active > 0 {
			j.active--
		}
		j.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// completion / failure
// ---------------------------------------------------------------------------

func (j *Job) finish() {
	j.mu.Lock()
	path := j.savePath
	sum := j.opts.Checksum
	total := j.total
	j.mu.Unlock()

	if f, err := os.OpenFile(path, os.O_RDWR, 0o644); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}

	if sum != "" {
		j.setState(StateVerifying)
		j.report(true)
		ok, got, err := VerifyChecksum(path, sum)
		j.mu.Lock()
		j.checksumOK = &ok
		j.mu.Unlock()
		if err != nil {
			j.fail(fmt.Errorf("checksum error: %w", err))
			return
		}
		if !ok {
			j.fail(fmt.Errorf("checksum mismatch: expected %s, got %s", sum, got))
			return
		}
	}

	j.mu.Lock()
	if total > 0 {
		j.done.Store(total)
	}
	j.state = StateCompleted
	j.finishedAt = time.Now()
	j.mu.Unlock()
	j.report(true)
}

// completeOrPause finishes the job when every byte is on disk, otherwise
// records it as paused so it can be continued later.
func (j *Job) completeOrPause() {
	j.mu.Lock()
	total := j.total
	done := j.done.Load()
	j.mu.Unlock()
	if total > 0 && done >= total {
		j.finish()
		return
	}
	j.setStateIfRunning(StatePaused)
	j.report(true)
}

func (j *Job) fail(err error) {
	j.mu.Lock()
	j.state = StateError
	if j.err == nil {
		j.err = err
	}
	j.finishedAt = time.Now()
	j.mu.Unlock()
	j.report(true)
}

func (j *Job) setErr(err error) {
	j.mu.Lock()
	if j.err == nil {
		j.err = err
	}
	j.mu.Unlock()
}

func (j *Job) error() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err == nil {
		return errors.New("download failed")
	}
	return j.err
}

func (j *Job) hardFailed() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err != nil
}

func (j *Job) bumpRetry() {
	j.mu.Lock()
	j.retries++
	j.mu.Unlock()
}

// note emits a human-readable log line for the UI.
func (j *Job) note(msg string) { Logf("[%s] %s", j.ID, msg) }

// recordCompleted merges a freshly written span into the resume map. Spans are
// always disjoint in the scheduler, so the live byte counter and the resume map
// stay consistent.
func (j *Job) recordCompleted(r Range) {
	if r.Len() <= 0 {
		return
	}
	j.mu.Lock()
	j.completed = append(j.completed, r)
	needCompact := len(j.completed) > 64 && len(j.completed)%64 == 0
	j.mu.Unlock()
	if needCompact {
		j.mu.Lock()
		j.completed = MergeRanges(j.completed)
		j.mu.Unlock()
	}
}

// Tick recomputes the speed estimate and fires the progress callback. The
// service layer calls it a few times per second so the UI has live numbers
// without the engine needing its own timer for idle jobs.
func (j *Job) Tick() { j.report(true) }

// ResumedRanges returns the coalesced resume map.
func (j *Job) ResumedRanges() []Range {
	j.mu.Lock()
	defer j.mu.Unlock()
	return MergeRanges(j.completed)
}

// ---------------------------------------------------------------------------
// snapshots & progress reporting
// ---------------------------------------------------------------------------

// Snapshot returns an immutable view of the job.
func (j *Job) Snapshot() Snapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	done := j.done.Load()
	total := j.total
	snap := Snapshot{
		ID:             j.ID,
		URL:            j.opts.URL,
		FinalURL:       j.finalURL,
		FileName:       j.fileName,
		SavePath:       j.savePath,
		Dir:            j.opts.Dir,
		Category:       j.opts.Category,
		Kind:           j.opts.Kind,
		Total:          total,
		Done:           done,
		State:          j.state,
		Active:         j.active,
		Segments:       len(j.completed),
		Retries:        j.retries,
		SupportsRanges: j.rangesOK,
		ContentType:    j.contentType,
		Checksum:       j.opts.Checksum,
		ChecksumOK:     j.checksumOK,
		Connections:    j.conns,
		AddedAt:        j.addedAt.UnixMilli(),
		SpeedLimit:     j.opts.SpeedLimit,
		SegmentsTotal:  j.segTotal,
		SegmentsDone:   j.segDone,
	}
	if j.err != nil {
		snap.Error = j.err.Error()
	}
	if !j.startedAt.IsZero() {
		snap.StartedAt = j.startedAt.UnixMilli()
	}
	if !j.finishedAt.IsZero() {
		snap.FinishedAt = j.finishedAt.UnixMilli()
	}
	if total > 0 {
		snap.Percent = float64(done) / float64(total) * 100
		if snap.Percent > 100 {
			snap.Percent = 100
		}
		snap.Left = total - done
		if snap.Left < 0 {
			snap.Left = 0
		}
	}
	snap.Speed = j.smoothSpeed
	if j.peakSpeed > snap.PeakSpeed {
		snap.PeakSpeed = j.peakSpeed
	}
	if !j.startedAt.IsZero() {
		if el := time.Since(j.startedAt).Seconds(); el > 0.5 {
			snap.AvgSpeed = float64(done) / el
		}
	}
	if snap.Speed > 1 && snap.Left > 0 {
		snap.ETA = int64(float64(snap.Left) / snap.Speed)
	} else {
		snap.ETA = -1
	}
	return snap
}

// report recomputes the smoothed speed and fires the progress callback.
func (j *Job) report(force bool) {
	j.speedMu.Lock()
	now := time.Now()
	done := j.done.Load()
	if !j.lastSample.IsZero() {
		dt := now.Sub(j.lastSample).Seconds()
		if dt >= 0.25 {
			inst := float64(done-j.lastBytes) / dt
			if inst < 0 {
				inst = 0
			}
			alpha := 1 - 1.0/(1.0+dt)
			if j.smoothSpeed == 0 {
				j.smoothSpeed = inst
			} else {
				j.smoothSpeed = j.smoothSpeed*(1-alpha) + inst*alpha
			}
			j.mu.Lock()
			if j.smoothSpeed > j.peakSpeed {
				j.peakSpeed = j.smoothSpeed
			}
			j.mu.Unlock()
			j.lastBytes = done
			j.lastSample = now
		}
	} else {
		j.lastBytes = done
		j.lastSample = now
	}
	fn := j.prog
	j.speedMu.Unlock()

	if force && fn != nil {
		fn(j.Snapshot())
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// DefaultDownloadDir is where files land when no folder is configured.
func DefaultDownloadDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, "Downloads", "SuperIDM")
	}
	return filepath.Join(os.TempDir(), "SuperIDM")
}

// UniquePath appends " (1)", " (2)", ... until the path is free.
func UniquePath(path string, overwrite bool) string {
	if overwrite {
		return path
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; i < 10000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return path
}

// MergeRanges sorts and coalesces overlapping/adjacent ranges.
func MergeRanges(in []Range) []Range {
	if len(in) == 0 {
		return nil
	}
	rs := append([]Range(nil), in...)
	sort.Slice(rs, func(a, b int) bool { return rs[a].Start < rs[b].Start })
	out := rs[:1]
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.Start <= last.End {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// subtractRanges returns the parts of whole not covered by done.
func subtractRanges(whole Range, done []Range) []Range {
	if len(done) == 0 {
		return []Range{whole}
	}
	done = MergeRanges(done)
	var out []Range
	cur := whole.Start
	for _, d := range done {
		if d.End <= cur {
			continue
		}
		if d.Start > cur {
			out = append(out, Range{Start: cur, End: min64(d.Start, whole.End)})
		}
		if d.End > cur {
			cur = d.End
		}
		if cur >= whole.End {
			break
		}
	}
	if cur < whole.End {
		out = append(out, Range{Start: cur, End: whole.End})
	}
	return out
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func backoff(attempt int) time.Duration {
	d := time.Duration(250*(1<<uint(minInt(attempt, 5)))) * time.Millisecond
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---------------------------------------------------------------------------
// checksums
// ---------------------------------------------------------------------------

// VerifyChecksum compares a file against "algo:hex" or a bare hex digest
// (length-sniffed: 32=md5, 40=sha1, 64=sha256).
func VerifyChecksum(path, spec string) (bool, string, error) {
	algo, want := "", spec
	if i := strings.Index(spec, ":"); i > 0 {
		algo = strings.ToLower(strings.TrimSpace(spec[:i]))
		want = strings.TrimSpace(spec[i+1:])
	}
	want = strings.ToLower(want)
	if algo == "" {
		switch len(want) {
		case 32:
			algo = "md5"
		case 40:
			algo = "sha1"
		case 64:
			algo = "sha256"
		default:
			return false, "", fmt.Errorf("unrecognised checksum %q (use md5:..., sha1:... or sha256:...)", spec)
		}
	}
	var h hash.Hash
	switch algo {
	case "md5":
		h = md5.New()
	case "sha1":
		h = sha1.New()
	case "sha256":
		h = sha256.New()
	default:
		return false, "", fmt.Errorf("unsupported checksum algorithm %q", algo)
	}
	f, err := os.Open(path)
	if err != nil {
		return false, "", err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return false, "", err
	}
	got := hex.EncodeToString(h.Sum(nil))
	return got == want, got, nil
}
