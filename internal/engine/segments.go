package engine

import (
	"context"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Adaptive work queue
// ---------------------------------------------------------------------------
//
// A classic download manager splits a file into a fixed number of segments
// (Internet Download Manager uses 8 by default). The weakness of that design is
// ownership: a range belongs to the connection that was handed it, and no other
// connection may touch it until its owner finishes or is judged slow. Between
// those decisions a single straggler holds back the whole transfer, and with
// only 8 pieces the file stays coarsely divided for most of the download.
//
// SuperIDM instead treats the file as a pool of *work units* that idle
// connections claim on demand:
//
//   - The file starts as connections*ChunksPerWorker equally sized ranges.
//     Handing out big ranges first keeps per-request overhead negligible.
//   - Whenever the pool runs short, the next range is split in half and only
//     one half is handed out; the other half stays in the pool. The pool
//     therefore refines itself automatically as the transfer progresses.
//   - Near the end the units reach minChunk size, so every connection
//     finishes within one minChunk of the others instead of a single straggler
//     holding a 200 MB segment hostage.
//
// That self-balancing behaviour is what produces the observable speed-up on
// real-world (lossy, high latency, per-connection shaped) links: aggregate
// throughput ends up bounded by the *average* connection rather than the
// *slowest* one.

// workItem is one claimable byte range.
type workItem struct {
	Range
	Attempts int
}

// workQueue hands out byte ranges to the connection pool.
type workQueue struct {
	mu          sync.Mutex
	cond        *sync.Cond
	pending     []workItem
	minChunk    int64
	workers     int
	outstanding int // units currently owned by a worker
	retries     int
	closed      bool
}

// newWorkQueue seeds a pool covering [0,total). total <= 0 means the length is
// unknown and a single open-ended unit is created.
func newWorkQueue(total int64, workers int, minChunk int64) *workQueue {
	return newWorkQueueRegions([]Range{{Start: 0, End: total}}, workers, minChunk)
}

// newWorkQueueRegions seeds the pool from an arbitrary set of spans. This is
// how a resumed download only re-fetches the gaps in a partial file.
func newWorkQueueRegions(regions []Range, workers int, minChunk int64) *workQueue {
	if workers < 1 {
		workers = 1
	}
	if minChunk < 32*1024 {
		minChunk = 32 * 1024
	}
	q := &workQueue{minChunk: minChunk, workers: workers}
	q.cond = sync.NewCond(&q.mu)
	q.seedRegions(regions, workers)
	return q
}

// seedRegions splits every span into rough work units.
func (q *workQueue) seedRegions(regions []Range, workers int) {
	var total int64
	for _, r := range regions {
		if r.Len() > 0 {
			total += r.Len()
		}
	}
	units := int64(workers) * ChunksPerWorker
	if units < 1 {
		units = 1
	}
	chunk := total / units
	if chunk < q.minChunk {
		chunk = q.minChunk
	}
	for _, reg := range regions {
		if reg.Len() <= 0 {
			// Unknown length: one open-ended unit.
			q.pending = append(q.pending, workItem{Range: Range{Start: reg.Start, End: -1}})
			continue
		}
		off := reg.Start
		for off < reg.End {
			end := off + chunk
			if end > reg.End {
				end = reg.End
			}
			q.pending = append(q.pending, workItem{Range: Range{Start: off, End: end}})
			off = end
		}
	}
	if len(q.pending) == 0 {
		q.pending = append(q.pending, workItem{Range: Range{Start: 0, End: -1}})
	}
}

// claim returns the next work unit, or nil when the whole pool is drained and
// no worker still owns anything.
func (q *workQueue) claim() *workItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if q.closed {
			return nil
		}
		if len(q.pending) > 0 {
			q.outstanding++
			// Refine the pool when it runs short: split the head range and
			// hand out only its first half. The pool length stays constant, so
			// this keeps subdividing until units hit minChunk.
			if len(q.pending) <= q.workers && q.pending[0].Len() >= 2*q.minChunk {
				head := q.pending[0]
				mid := head.Start + head.Len()/2
				q.pending[0] = workItem{Range: Range{Start: mid, End: head.End}, Attempts: head.Attempts}
				it := workItem{Range: Range{Start: head.Start, End: mid}, Attempts: head.Attempts}
				return &it
			}
			it := q.pending[0]
			q.pending = q.pending[1:]
			return &it
		}
		if q.outstanding == 0 {
			// Nothing pending and nobody working: the transfer is done.
			return nil
		}
		q.cond.Wait()
	}
}

// release marks a claimed unit as no longer owned by the caller (completed).
func (q *workQueue) release() {
	q.mu.Lock()
	q.outstanding--
	q.mu.Unlock()
	q.cond.Broadcast()
}

// partial adds a *new*, unowned unit to the pool without giving up the
// caller's current claim. Used when a connection hands half of a straggling
// range to a healthier one so the tail cannot serialise the transfer.
func (q *workQueue) partial(r Range, attempts int) {
	if r.Len() <= 0 {
		return
	}
	q.mu.Lock()
	if !q.closed {
		q.pending = append(q.pending, workItem{Range: r, Attempts: attempts})
	}
	q.mu.Unlock()
	q.cond.Broadcast()
}

// requeue returns the unfinished remainder of a unit to the pool.
func (q *workQueue) requeue(r Range, attempts int) {
	q.mu.Lock()
	if q.outstanding > 0 {
		q.outstanding--
	}
	if !q.closed && r.Len() > 0 {
		q.pending = append(q.pending, workItem{Range: r, Attempts: attempts})
		q.retries++
	}
	q.mu.Unlock()
	q.cond.Broadcast()
}

// close abandons the pool; all parked workers wake up and exit.
func (q *workQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.pending = nil
	q.mu.Unlock()
	q.cond.Broadcast()
}

// broadcast wakes parked workers after another connection made progress.
func (q *workQueue) broadcast() { q.cond.Broadcast() }

// stats returns (pending units, pending bytes, retries).
func (q *workQueue) stats() (int, int64, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var n int64
	for _, w := range q.pending {
		if l := w.Len(); l > 0 {
			n += l
		}
	}
	return len(q.pending), n, q.retries
}

// ---------------------------------------------------------------------------
// Rate limiter (virtual-scheduling token bucket)
// ---------------------------------------------------------------------------

// rateLimiter smooths the aggregate throughput of a job across all of its
// connections. limit is in bytes/second; 0 disables limiting.
type rateLimiter struct {
	mu    sync.Mutex
	limit int64
	next  time.Time
}

func newRateLimiter(limit int64) *rateLimiter { return &rateLimiter{limit: limit} }

// NewRateLimiter creates a limiter for a shared or per-job byte-rate cap.
func NewRateLimiter(limit int64) *rateLimiter { return newRateLimiter(limit) }

func (rl *rateLimiter) setLimit(bytesPerSec int64) {
	if rl == nil {
		return
	}
	rl.mu.Lock()
	rl.limit = bytesPerSec
	rl.next = time.Time{}
	rl.mu.Unlock()
}

func (rl *rateLimiter) currentLimit() int64 {
	if rl == nil {
		return 0
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.limit
}

// wait blocks until transmitting n more bytes is allowed. It returns early if
// ctx is cancelled.
//
// The caller is given a fixed departure time ("ticket") that is computed under
// the lock, so the aggregate schedule advances by exactly n/limit seconds per
// call no matter how many connections are asking. The sleep below is only
// sliced for cancellation responsiveness - the target time is never shortened,
// because truncating it would silently raise the effective limit.
func (rl *rateLimiter) wait(ctx context.Context, n int) error {
	if rl == nil || n <= 0 {
		return nil
	}
	rl.mu.Lock()
	if rl.limit <= 0 {
		rl.mu.Unlock()
		return nil
	}
	now := time.Now()
	if rl.next.Before(now) {
		rl.next = now
	}
	rl.next = rl.next.Add(time.Duration(float64(n) / float64(rl.limit) * float64(time.Second)))
	target := rl.next
	rl.mu.Unlock()

	const slice = 500 * time.Millisecond
	for {
		d := time.Until(target)
		if d <= 0 {
			return nil
		}
		if d > slice {
			d = slice
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
			t.Stop()
		}
	}
}
