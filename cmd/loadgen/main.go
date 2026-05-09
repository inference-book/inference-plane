// Command loadgen fires synthetic OpenAI-compatible HTTP requests at
// the control plane at a configurable rate. Combined with the mock
// backend (engine: mock in deploy/config.yaml), it gives a self-
// contained demo path: bring the stack up, generate traffic,
// dashboards populate, no GPU required.
//
// Usage:
//
//	make load          # default settings against http://localhost:8080
//	go run ./cmd/loadgen --rps=20 --duration=2m
//
// Flags:
//
//	--url             control plane base URL (default http://localhost:8080)
//	--rps             requests per second (default 5)
//	--duration        total duration to generate traffic (default 1m)
//	--model           model name to send in requests (default mock)
//	--max-tokens      max output tokens per request (default 100)
//	--chat-fraction   fraction of requests sent as chat completions
//	                  (default 0.4 -- mix of plain and chat)
//	--workers         concurrent workers (default 8)
//
// Reports a summary at the end: total sent, success / error counts,
// observed latency p50 / p95 / p99, requests per second.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	flagURL          = flag.String("url", "http://localhost:8080", "control plane base URL")
	flagRPS          = flag.Float64("rps", 5, "requests per second")
	flagDuration     = flag.Duration("duration", time.Minute, "total duration to generate traffic")
	flagModel        = flag.String("model", "mock", "model name to send")
	flagMaxTokens    = flag.Int("max-tokens", 100, "max output tokens per request")
	flagChatFraction = flag.Float64("chat-fraction", 0.4, "fraction of chat-style requests (0..1)")
	flagWorkers      = flag.Int("workers", 8, "concurrent worker count")
)

func main() {
	flag.Parse()

	if *flagRPS <= 0 {
		die("--rps must be > 0")
	}
	if *flagDuration <= 0 {
		die("--duration must be > 0")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	deadlineCtx, cancelDeadline := context.WithTimeout(ctx, *flagDuration)
	defer cancelDeadline()

	stats := &stats{}
	work := make(chan struct{}, *flagWorkers*4)
	var wg sync.WaitGroup

	httpClient := &http.Client{Timeout: 5 * time.Minute}

	// Worker pool. Each worker pulls a token from work and fires one
	// request. Bound by --workers so we don't drown the control plane
	// in goroutines if the configured rps gets stuck behind a slow
	// backend.
	for i := 0; i < *flagWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				fireOne(deadlineCtx, httpClient, stats)
			}
		}()
	}

	// Tick at the configured rate, scheduling one request per tick.
	// Drop ticks if work is backlogged (so we never exceed the worker
	// pool's queue) and surface that as 'skipped'.
	interval := time.Duration(float64(time.Second) / *flagRPS)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	start := time.Now()
	fmt.Fprintf(os.Stderr, "loadgen: %v at %.2f rps -> %s\n", *flagDuration, *flagRPS, *flagURL)

scheduleLoop:
	for {
		select {
		case <-deadlineCtx.Done():
			break scheduleLoop
		case <-ticker.C:
			select {
			case work <- struct{}{}:
			default:
				atomic.AddInt64(&stats.skipped, 1)
			}
		}
	}

	close(work)
	wg.Wait()
	stats.print(time.Since(start), *flagRPS)

	if stats.errors > 0 {
		os.Exit(1)
	}
}

// fireOne sends a single request. Picks completion vs chat by a
// uniform draw against --chat-fraction.
func fireOne(ctx context.Context, c *http.Client, st *stats) {
	chat := rand.Float64() < *flagChatFraction
	path := "/v1/completions"
	body := completionBody()
	if chat {
		path = "/v1/chat/completions"
		body = chatBody()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *flagURL+path, bytes.NewReader(body))
	if err != nil {
		st.recordError()
		return
	}
	req.Header.Set("Content-Type", "application/json")

	t0 := time.Now()
	resp, err := c.Do(req)
	dur := time.Since(t0)

	if err != nil {
		// Don't count cancellation against errors -- it just means
		// the deadline expired while a request was in flight.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			st.recordError()
		}
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 400 {
		st.recordError()
		return
	}
	st.recordSuccess(dur)
}

func completionBody() []byte {
	body := map[string]any{
		"model":      *flagModel,
		"prompt":     pickPrompt(),
		"max_tokens": *flagMaxTokens,
	}
	b, _ := json.Marshal(body)
	return b
}

func chatBody() []byte {
	body := map[string]any{
		"model":      *flagModel,
		"messages":   []map[string]string{{"role": "user", "content": pickPrompt()}},
		"max_tokens": *flagMaxTokens,
	}
	b, _ := json.Marshal(body)
	return b
}

// pickPrompt returns one of a small set of varied prompts so the
// trace search has something to differentiate by.
func pickPrompt() string {
	prompts := []string{
		"What is the capital of France?",
		"Explain the concept of autoregressive generation in one paragraph.",
		"List five strategies for reducing inference latency.",
		"Write a haiku about a control plane.",
		"Translate 'hello world' into Spanish, French, and Japanese.",
	}
	return prompts[rand.IntN(len(prompts))]
}

// ── stats ──────────────────────────────────────────────────────────

type stats struct {
	mu        sync.Mutex
	successes int64
	errors    int64
	skipped   int64
	latencies []time.Duration
}

func (s *stats) recordSuccess(d time.Duration) {
	s.mu.Lock()
	s.successes++
	s.latencies = append(s.latencies, d)
	s.mu.Unlock()
}

func (s *stats) recordError() {
	atomic.AddInt64(&s.errors, 1)
}

func (s *stats) print(elapsed time.Duration, targetRPS float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	total := s.successes + s.errors
	actualRPS := float64(total) / elapsed.Seconds()

	fmt.Fprintf(os.Stderr, "\n=== loadgen summary ===\n")
	fmt.Fprintf(os.Stderr, "duration       : %v\n", elapsed.Round(time.Millisecond))
	fmt.Fprintf(os.Stderr, "target rps     : %.2f\n", targetRPS)
	fmt.Fprintf(os.Stderr, "actual rps     : %.2f\n", actualRPS)
	fmt.Fprintf(os.Stderr, "successes      : %d\n", s.successes)
	fmt.Fprintf(os.Stderr, "errors         : %d\n", s.errors)
	fmt.Fprintf(os.Stderr, "skipped (full) : %d\n", s.skipped)

	if len(s.latencies) > 0 {
		sort.Slice(s.latencies, func(i, j int) bool { return s.latencies[i] < s.latencies[j] })
		p := func(q float64) time.Duration {
			i := int(float64(len(s.latencies)) * q)
			if i >= len(s.latencies) {
				i = len(s.latencies) - 1
			}
			return s.latencies[i]
		}
		fmt.Fprintf(os.Stderr, "latency p50    : %v\n", p(0.50).Round(time.Millisecond))
		fmt.Fprintf(os.Stderr, "latency p95    : %v\n", p(0.95).Round(time.Millisecond))
		fmt.Fprintf(os.Stderr, "latency p99    : %v\n", p(0.99).Round(time.Millisecond))
	}
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "loadgen:", msg)
	os.Exit(2)
}
