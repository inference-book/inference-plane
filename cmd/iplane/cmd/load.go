package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/spf13/cobra"
)

// load flags. Defaults are tuned to populate dashboards quickly with
// modest demo traffic; turn --rps and --duration up for stress tests.
var (
	loadURL          string
	loadRPS          float64
	loadDuration     time.Duration
	loadModel        string
	loadMaxTokens    int
	loadChatFraction float64
	loadWorkers      int
)

var loadCmd = &cobra.Command{
	Use:   "load",
	Short: "Fire synthetic OpenAI requests at the running stack",
	Long: `Generates synthetic traffic against the control plane's HTTP
surface, mixing plain completion and chat completion requests by the
configured fraction. Reports actual rps and latency p50/p95/p99 at the
end.

This is the 'demo traffic for dashboards' tool, not a serious load
tester -- arrival pattern is constant-rate, prompts are picked from a
short fixed list. Reach for k6 or vegeta for capacity planning,
chaos-traffic, or anything that needs realistic arrival patterns.

Combined with the mock backend (engine: mock in deploy/config.yaml),
loadgen gives a self-contained demo path: 'iplane serve' in one shell,
'iplane load' in another, dashboards populate, no GPU required.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLoad(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(loadCmd)
	loadCmd.Flags().StringVar(&loadURL, "url", "http://localhost:8080",
		"control plane base URL")
	loadCmd.Flags().Float64Var(&loadRPS, "rps", 5,
		"target requests per second")
	loadCmd.Flags().DurationVar(&loadDuration, "duration", time.Minute,
		"total duration to generate traffic")
	loadCmd.Flags().StringVar(&loadModel, "model", "mock",
		"model name to send in requests")
	loadCmd.Flags().IntVar(&loadMaxTokens, "max-tokens", 100,
		"max output tokens per request")
	loadCmd.Flags().Float64Var(&loadChatFraction, "chat-fraction", 0.4,
		"fraction of requests sent as chat completions (0..1)")
	loadCmd.Flags().IntVar(&loadWorkers, "workers", 8,
		"concurrent worker count")
}

func runLoad(parent context.Context) error {
	if loadRPS <= 0 {
		return errors.New("--rps must be > 0")
	}
	if loadDuration <= 0 {
		return errors.New("--duration must be > 0")
	}

	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	deadlineCtx, cancelDeadline := context.WithTimeout(ctx, loadDuration)
	defer cancelDeadline()

	stats := &loadStats{}
	work := make(chan struct{}, loadWorkers*4)
	var wg sync.WaitGroup
	httpClient := &http.Client{Timeout: 5 * time.Minute}

	for i := 0; i < loadWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				fireLoadRequest(deadlineCtx, httpClient, stats)
			}
		}()
	}

	interval := time.Duration(float64(time.Second) / loadRPS)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	start := time.Now()
	fmt.Fprintf(os.Stderr, "iplane load: %v at %.2f rps -> %s\n", loadDuration, loadRPS, loadURL)

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
	stats.print(time.Since(start), loadRPS)

	if stats.errors > 0 {
		return fmt.Errorf("%d requests errored", stats.errors)
	}
	return nil
}

func fireLoadRequest(ctx context.Context, c *http.Client, st *loadStats) {
	chat := rand.Float64() < loadChatFraction
	path := "/v1/completions"
	body := loadCompletionBody()
	if chat {
		path = "/v1/chat/completions"
		body = loadChatBody()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loadURL+path, bytes.NewReader(body))
	if err != nil {
		st.recordError()
		return
	}
	req.Header.Set("Content-Type", "application/json")

	t0 := time.Now()
	resp, err := c.Do(req)
	dur := time.Since(t0)

	if err != nil {
		// Don't count cancellation against errors; it just means the
		// deadline expired while a request was in flight.
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

func loadCompletionBody() []byte {
	body := map[string]any{
		"model":      loadModel,
		"prompt":     pickLoadPrompt(),
		"max_tokens": loadMaxTokens,
	}
	b, _ := json.Marshal(body)
	return b
}

func loadChatBody() []byte {
	body := map[string]any{
		"model":      loadModel,
		"messages":   []map[string]string{{"role": "user", "content": pickLoadPrompt()}},
		"max_tokens": loadMaxTokens,
	}
	b, _ := json.Marshal(body)
	return b
}

func pickLoadPrompt() string {
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

type loadStats struct {
	mu        sync.Mutex
	successes int64
	errors    int64
	skipped   int64
	latencies []time.Duration
}

func (s *loadStats) recordSuccess(d time.Duration) {
	s.mu.Lock()
	s.successes++
	s.latencies = append(s.latencies, d)
	s.mu.Unlock()
}

func (s *loadStats) recordError() { atomic.AddInt64(&s.errors, 1) }

func (s *loadStats) print(elapsed time.Duration, targetRPS float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	total := s.successes + s.errors
	actualRPS := float64(total) / elapsed.Seconds()

	fmt.Fprintf(os.Stderr, "\n=== iplane load summary ===\n")
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
