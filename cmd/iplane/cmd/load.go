package cmd

import (
	"bufio"
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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// load flags. Defaults are tuned to populate dashboards quickly with
// modest demo traffic; turn --rps and --duration up for stress tests.
//
// Two URL modes:
//
//   - Legacy --url: direct base URL ("http://localhost:8080"). The
//     load tool POSTs to <url>/v1/chat/completions and the flat-URL
//     router resolves the deployment from the model field in the body.
//     Used by the `make load` smoke target against mock backends.
//
//   - v0.2 ch7-beat2.8 --target <deploy-id>: routes through the
//     explicit deploy-id URL. Requires --service-url (or
//     IPLANE_SERVICE_URL env) so the load tool knows the daemon's
//     HTTP base. POSTs to <service-url>/v1/<deploy-id>/v1/chat/completions.
//     Demos 05 + 06 use this path so traffic is unambiguously tied
//     to one deployment.
//
// When --target is set it takes precedence and --url is ignored.
var (
	loadURL          string
	loadServiceURL   string
	loadTarget       string
	loadRPS          float64
	loadDuration     time.Duration
	loadModel        string
	loadMaxTokens    int
	loadChatFraction float64
	loadWorkers      int
	loadPriority     string
	loadTenant       string
	loadStream       bool
	loadOutput       string
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

Two URL modes:

  --url <base-url>          flat-URL mode (model in body picks the
                            deployment). The Ch 6 default and what
                            'make load' uses against mock backends.

  --target <deploy-id>      deploy-id mode (explicit routing). Requires
                            --service-url. Demo 05 + 06 use this so
                            traffic is unambiguously tied to one
                            deployment.

v0.2 ch7-beat2 priority + tenant flags emit the X-IPlane-Priority and
X-IPlane-Tenant headers the router uses to lane and fair-share
incoming requests. Demo 05 fires two parallel load processes with
different priority classes to show "interactive cuts ahead of batch."`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLoad(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(loadCmd)
	loadCmd.Flags().StringVar(&loadURL, "url", "http://localhost:8080",
		"control plane base URL (flat-URL mode); ignored when --target is set")
	loadCmd.Flags().StringVar(&loadServiceURL, "service-url", os.Getenv("IPLANE_SERVICE_URL"),
		"daemon base URL for --target mode (default: IPLANE_SERVICE_URL env)")
	loadCmd.Flags().StringVar(&loadTarget, "target", "",
		"deployment id to route through (deploy-id URL mode); requires --service-url")
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
	loadCmd.Flags().StringVar(&loadPriority, "priority", "",
		"X-IPlane-Priority header value: interactive | batch (empty = no header, router falls back to default)")
	loadCmd.Flags().StringVar(&loadTenant, "tenant", "",
		"X-IPlane-Tenant header value (empty = no header, router treats as 'default')")
	loadCmd.Flags().BoolVar(&loadStream, "stream", false,
		"request streaming completions (sets stream=true in request body); SSE response is parsed for token counts")
	loadCmd.Flags().StringVar(&loadOutput, "output", "text",
		"final summary format: text | json")
}

// loadEndpoint returns the (base, chatPath, completionsPath) tuple
// the load tool fires against. In --target mode the paths embed the
// deployment id (explicit routing); in --url mode they use the flat
// /v1/{chat/}completions paths that the body-peek router maps to a
// deployment from the request body's model field.
//
// Returns an error when --target is set without --service-url (the
// only way to construct a deploy-id URL).
func loadEndpoint() (base, chatPath, completionsPath string, err error) {
	if loadTarget != "" {
		if loadServiceURL == "" {
			return "", "", "", errors.New("--target requires --service-url (or IPLANE_SERVICE_URL env) so the deploy-id URL can be constructed")
		}
		base = strings.TrimRight(loadServiceURL, "/")
		chatPath = "/v1/" + loadTarget + "/v1/chat/completions"
		completionsPath = "/v1/" + loadTarget + "/v1/completions"
		return base, chatPath, completionsPath, nil
	}
	base = strings.TrimRight(loadURL, "/")
	chatPath = "/v1/chat/completions"
	completionsPath = "/v1/completions"
	return base, chatPath, completionsPath, nil
}

func runLoad(parent context.Context) error {
	if loadRPS <= 0 {
		return errors.New("--rps must be > 0")
	}
	if loadDuration <= 0 {
		return errors.New("--duration must be > 0")
	}
	if loadPriority != "" && loadPriority != "interactive" && loadPriority != "batch" {
		return fmt.Errorf("--priority must be one of: interactive, batch (got %q)", loadPriority)
	}
	if loadOutput != "text" && loadOutput != "json" {
		return fmt.Errorf("--output must be one of: text, json (got %q)", loadOutput)
	}
	base, chatPath, completionsPath, err := loadEndpoint()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	deadlineCtx, cancelDeadline := context.WithTimeout(ctx, loadDuration)
	defer cancelDeadline()

	stats := &loadStats{}
	work := make(chan struct{}, loadWorkers*4)
	var wg sync.WaitGroup
	httpClient := &http.Client{Timeout: 5 * time.Minute}

	cfg := loadFireConfig{
		base:            base,
		chatPath:        chatPath,
		completionsPath: completionsPath,
		stream:          loadStream,
		priority:        loadPriority,
		tenant:          loadTenant,
	}

	for range loadWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				fireLoadRequest(deadlineCtx, httpClient, &cfg, stats)
			}
		}()
	}

	interval := time.Duration(float64(time.Second) / loadRPS)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	start := time.Now()
	fmt.Fprintf(os.Stderr, "iplane load: %v at %.2f rps -> %s (priority=%s tenant=%s stream=%v)\n",
		loadDuration, loadRPS, base, displayLabel(loadPriority), displayLabel(loadTenant), loadStream)

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
	stats.print(time.Since(start), loadRPS, loadOutput)

	if stats.errors > 0 {
		return fmt.Errorf("%d requests errored", stats.errors)
	}
	return nil
}

// displayLabel returns s if non-empty, "(default)" otherwise.
// Used in the startup log line so an empty priority/tenant shows up
// as "(default)" instead of an awkward blank field.
func displayLabel(s string) string {
	if s == "" {
		return "(default)"
	}
	return s
}

// loadFireConfig bundles per-request shape that doesn't change
// across the run. Passed to fireLoadRequest so each worker doesn't
// re-read package-level flags on the hot path (and so tests can
// inject a fake config without mutating globals).
type loadFireConfig struct {
	base            string
	chatPath        string
	completionsPath string
	stream          bool
	priority        string
	tenant          string
}

func fireLoadRequest(ctx context.Context, c *http.Client, cfg *loadFireConfig, st *loadStats) {
	chat := rand.Float64() < loadChatFraction
	path := cfg.completionsPath
	body := loadCompletionBody()
	if chat {
		path = cfg.chatPath
		body = loadChatBody()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.base+path, bytes.NewReader(body))
	if err != nil {
		st.recordError()
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.priority != "" {
		req.Header.Set("X-IPlane-Priority", cfg.priority)
	}
	if cfg.tenant != "" {
		req.Header.Set("X-IPlane-Tenant", cfg.tenant)
	}

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

	if resp.StatusCode >= 400 {
		_, _ = io.Copy(io.Discard, resp.Body)
		st.recordError()
		return
	}

	// Token-count parsing. For non-streaming JSON responses we look at
	// usage.completion_tokens; for streaming SSE we accumulate from
	// each frame's usage block (vLLM emits usage on the final delta;
	// older engines may not emit it at all -- treated as zero).
	tokens := parseTokens(resp, cfg.stream)
	st.recordSuccess(dur, tokens)
}

// parseTokens reads the response body and returns the
// completion_tokens count when the engine reported one. For
// non-streaming responses: one JSON object with usage. For
// streaming (SSE) responses: scan data: lines, look for usage on
// any frame.
//
// Returns 0 when the engine didn't emit a usage block. Errors during
// parse are silent -- iplane load isn't a correctness tool; mangled
// responses count as zero-token successes.
func parseTokens(resp *http.Response, stream bool) int64 {
	if !stream {
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return 0
		}
		return tokensFromJSON(raw)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var tokens int64
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		if t := tokensFromJSON(payload); t > tokens {
			tokens = t
		}
	}
	return tokens
}

func tokensFromJSON(raw []byte) int64 {
	var resp struct {
		Usage struct {
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0
	}
	return resp.Usage.CompletionTokens
}

func loadCompletionBody() []byte {
	body := map[string]any{
		"model":      loadModel,
		"prompt":     pickLoadPrompt(),
		"max_tokens": loadMaxTokens,
	}
	if loadStream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
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
	if loadStream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
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
	tokens    int64
	latencies []time.Duration
}

func (s *loadStats) recordSuccess(d time.Duration, tokens int64) {
	s.mu.Lock()
	s.successes++
	s.latencies = append(s.latencies, d)
	s.tokens += tokens
	s.mu.Unlock()
}

func (s *loadStats) recordError() { atomic.AddInt64(&s.errors, 1) }

// loadSummary is the structured form of the final stats line. JSON
// output mode marshals it directly; text output formats it as a
// human-readable block. Keeping the field set in one struct means
// new fields land in both outputs automatically.
type loadSummary struct {
	DurationSec  float64 `json:"duration_sec"`
	TargetRPS    float64 `json:"target_rps"`
	ActualRPS    float64 `json:"actual_rps"`
	Successes    int64   `json:"successes"`
	Errors       int64   `json:"errors"`
	Skipped      int64   `json:"skipped"`
	Tokens       int64   `json:"completion_tokens"`
	TokensPerSec float64 `json:"completion_tokens_per_sec"`
	LatencyP50Ms int64   `json:"latency_p50_ms"`
	LatencyP95Ms int64   `json:"latency_p95_ms"`
	LatencyP99Ms int64   `json:"latency_p99_ms"`
}

func (s *loadStats) summary(elapsed time.Duration, targetRPS float64) loadSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := s.successes + s.errors
	sum := loadSummary{
		DurationSec: elapsed.Seconds(),
		TargetRPS:   targetRPS,
		ActualRPS:   float64(total) / elapsed.Seconds(),
		Successes:   s.successes,
		Errors:      s.errors,
		Skipped:     s.skipped,
		Tokens:      s.tokens,
	}
	if elapsed.Seconds() > 0 {
		sum.TokensPerSec = float64(s.tokens) / elapsed.Seconds()
	}
	if len(s.latencies) > 0 {
		sort.Slice(s.latencies, func(i, j int) bool { return s.latencies[i] < s.latencies[j] })
		p := func(q float64) time.Duration {
			i := int(float64(len(s.latencies)) * q)
			if i >= len(s.latencies) {
				i = len(s.latencies) - 1
			}
			return s.latencies[i]
		}
		sum.LatencyP50Ms = p(0.50).Milliseconds()
		sum.LatencyP95Ms = p(0.95).Milliseconds()
		sum.LatencyP99Ms = p(0.99).Milliseconds()
	}
	return sum
}

func (s *loadStats) print(elapsed time.Duration, targetRPS float64, format string) {
	sum := s.summary(elapsed, targetRPS)
	if format == "json" {
		// JSON mode writes to stdout (machine-parsing path) so demos
		// + CI can capture it without mixing with the start-of-run
		// banner that goes to stderr.
		_ = json.NewEncoder(os.Stdout).Encode(sum)
		return
	}
	fmt.Fprintf(os.Stderr, "\n=== iplane load summary ===\n")
	fmt.Fprintf(os.Stderr, "duration              : %v\n", elapsed.Round(time.Millisecond))
	fmt.Fprintf(os.Stderr, "target rps            : %.2f\n", sum.TargetRPS)
	fmt.Fprintf(os.Stderr, "actual rps            : %.2f\n", sum.ActualRPS)
	fmt.Fprintf(os.Stderr, "successes             : %d\n", sum.Successes)
	fmt.Fprintf(os.Stderr, "errors                : %d\n", sum.Errors)
	fmt.Fprintf(os.Stderr, "skipped (full)        : %d\n", sum.Skipped)
	if sum.Tokens > 0 {
		fmt.Fprintf(os.Stderr, "completion tokens     : %d\n", sum.Tokens)
		fmt.Fprintf(os.Stderr, "completion tok/s      : %.1f\n", sum.TokensPerSec)
	}
	if sum.LatencyP50Ms > 0 {
		fmt.Fprintf(os.Stderr, "latency p50           : %dms\n", sum.LatencyP50Ms)
		fmt.Fprintf(os.Stderr, "latency p95           : %dms\n", sum.LatencyP95Ms)
		fmt.Fprintf(os.Stderr, "latency p99           : %dms\n", sum.LatencyP99Ms)
	}
}
