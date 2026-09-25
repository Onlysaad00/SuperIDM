// Command benchserver is a development-only HTTP origin used to measure
// SuperIDM's engine against a plain single-connection download.
//
// It deliberately models a hostile-but-realistic network:
//
//   - every request costs `latency` milliseconds of round-trip,
//   - every individual response is shaped to `perConnMiBps`, which is what an
//     ISP shaper or a congested long haul does to a *single* TCP connection,
//   - a full-body (non-ranged) request is additionally capped at `singleMiBps`
//     to model the throughput a browser gets today.
//
// The last point is the crux: when each connection is limited individually,
// total throughput scales with the number of connections until the link itself
// saturates - which is exactly why a download manager with 64 connections beats
// one with 8.
//
//	python3 tools/benchmark.sh
//
// or directly:
//
//	go run ./tools/benchserver -addr 127.0.0.1:8799 -size-mib 64 -single-mibps 8
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8799", "listen address")
	sizeMiB := flag.Int("size-mib", 64, "size of the served payload")
	latency := flag.Duration("latency", 12*time.Millisecond, "added latency per request")
	singleMiBps := flag.Float64("single-mibps", 8, "throughput cap for a full-body (non-ranged) transfer")
	perConnMiBps := flag.Float64("per-conn-mibps", 2, "throughput cap applied to every response (per TCP connection)")
	flag.Parse()

	// shape writes to a target rate per response
	shaper := func(w http.ResponseWriter, data []byte, mibps float64) error {
		if mibps <= 0 {
			_, err := w.Write(data)
			return err
		}
		const chunk = 64 << 10
		perChunk := time.Duration(float64(chunk) / (mibps * 1024 * 1024) * float64(time.Second))
		for off := 0; off < len(data); off += chunk {
			end := off + chunk
			if end > len(data) {
				end = len(data)
			}
			if _, err := w.Write(data[off:end]); err != nil {
				return err
			}
			if perChunk > 0 {
				time.Sleep(perChunk)
			}
		}
		return nil
	}

	size := *sizeMiB << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i * 7)
	}

	http.HandleFunc("/file.bin", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(*latency)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Type", "application/octet-stream")

		spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
		if spec == "" {
			// The slow path: one socket, shaped.
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			capped := *singleMiBps
			if capped <= 0 || (perConnMiBps != nil && *perConnMiBps > 0 && *perConnMiBps < capped) {
				capped = *perConnMiBps
			}
			_ = shaper(w, payload, capped)
			return
		}

		var start, end int64
		i := strings.IndexByte(spec, '-')
		start, _ = strconv.ParseInt(spec[:i], 10, 64)
		if spec[i+1:] == "" {
			end = int64(len(payload)) - 1
		} else {
			end, _ = strconv.ParseInt(spec[i+1:], 10, 64)
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
		_ = shaper(w, payload[start:end+1], *perConnMiBps)
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("bench origin on http://%s/file.bin (%d MiB, %.1f MiB/s per connection, %.0f MiB/s single stream, %s latency)",
		*addr, *sizeMiB, *perConnMiBps, *singleMiBps, *latency)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
