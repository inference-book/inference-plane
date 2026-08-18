package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// Session-mode flags. Unlike the open-loop `iplane load` firehose
// (constant arrival rate, stateless single-shot requests), session mode
// is closed-loop and stateful: each session holds a growing
// conversation and cannot send turn N+1 until turn N returns. That is
// what produces the long-lived, overlapping sessions that make routing
// locality matter -- the workload Ch 8's sticky routing is built for.
var (
	sessionCount        int
	sessionTurns        int
	sessionThinkTime    time.Duration
	sessionSystemTokens int
)

var loadSessionCmd = &cobra.Command{
	Use:   "session",
	Short: "Fire closed-loop multi-turn chat sessions (Ch 8 sticky-routing driver)",
	Long: `Drives multi-turn chat conversations against the control plane,
one goroutine per session. Each turn resends the full running history
(system prompt + every prior turn), so the prefix a downstream engine
can cache grows turn over turn. Every request carries an
X-IPlane-Session header -- the affinity key the prefix-cache-aware
router (Ch 8) routes on.

Closed-loop: a session waits for each turn's response before sending the
next (plus --think-time), the way a real conversation behaves. The run
ends when all --sessions have completed --turns turns.

Conversation content is a built-in synthetic generator (deterministic,
GPU-free for CI). Replaying a real multi-turn corpus (ShareGPT /
LMSYS-Chat-1M) is wired in the Ch 8 demo walkthrough, not here.

Targeting reuses the same flags as 'iplane load': --target <deploy-id>
with --service-url for explicit routing, or --url for flat-URL mode.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runLoadSession(cmd.Context())
	},
}

func init() {
	loadCmd.AddCommand(loadSessionCmd)

	loadSessionCmd.Flags().IntVar(&sessionCount, "sessions", 10,
		"number of concurrent conversations")
	loadSessionCmd.Flags().IntVar(&sessionTurns, "turns", 8,
		"user turns per conversation")
	loadSessionCmd.Flags().DurationVar(&sessionThinkTime, "think-time", time.Second,
		"delay between a turn's response and the next turn")
	loadSessionCmd.Flags().IntVar(&sessionSystemTokens, "system-prompt-tokens", 200,
		"approximate size of the shared system prompt, in whitespace tokens; a bigger shared prefix amplifies the cache story")

	// Request-shape + routing flags reused from `iplane load`. They bind
	// the same package-level vars; only one command parses per invocation.
	loadSessionCmd.Flags().StringVar(&loadURL, "url", "http://localhost:8080",
		"control plane base URL (flat-URL mode); ignored when --target is set")
	loadSessionCmd.Flags().StringVar(&loadServiceURL, "service-url", os.Getenv("IPLANE_SERVICE_URL"),
		"daemon base URL for --target mode (default: IPLANE_SERVICE_URL env)")
	loadSessionCmd.Flags().StringVar(&loadTarget, "target", "",
		"deployment id to route through (deploy-id URL mode); requires --service-url")
	loadSessionCmd.Flags().StringVar(&loadModel, "model", "",
		"model name in the request body (required); get the exact string from `iplane deployment list`")
	loadSessionCmd.Flags().IntVar(&loadMaxTokens, "max-tokens", 100,
		"max output tokens per turn")
	loadSessionCmd.Flags().BoolVar(&loadStream, "stream", false,
		"request streaming completions (content + usage accumulated from the SSE frames)")
	loadSessionCmd.Flags().StringVar(&loadOutput, "output", "text",
		"final summary format: text | json")
	loadSessionCmd.Flags().StringVar(&loadPriority, "priority", "",
		"X-IPlane-Priority header value: interactive | batch (empty = no header)")
	loadSessionCmd.Flags().StringVar(&loadTenant, "tenant", "",
		"X-IPlane-Tenant header value (empty = no header)")
}

// chatMessage (one entry in the OpenAI chat messages array) is shared
// with deployment_query.go.

// sessionFireConfig bundles the per-turn request shape that doesn't
// change across a run. Held once and shared by every session goroutine;
// the per-session identity (the X-IPlane-Session value) is passed
// separately so one config serves all sessions.
type sessionFireConfig struct {
	base         string
	chatPath     string
	stream       bool
	priority     string
	tenant       string
	maxTokens    int
	model        string
	systemPrompt string
}

func runLoadSession(parent context.Context) error {
	if sessionCount <= 0 {
		return errors.New("--sessions must be > 0")
	}
	if sessionTurns <= 0 {
		return errors.New("--turns must be > 0")
	}
	if loadModel == "" {
		return errors.New("--model is required (no default; get the exact string from `iplane deployment list`)")
	}
	if loadOutput != "text" && loadOutput != "json" {
		return fmt.Errorf("--output must be one of: text, json (got %q)", loadOutput)
	}
	if loadPriority != "" && loadPriority != "interactive" && loadPriority != "batch" {
		return fmt.Errorf("--priority must be one of: interactive, batch (got %q)", loadPriority)
	}
	base, chatPath, _, err := loadEndpoint()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := sessionFireConfig{
		base:         base,
		chatPath:     chatPath,
		stream:       loadStream,
		priority:     loadPriority,
		tenant:       loadTenant,
		maxTokens:    loadMaxTokens,
		model:        loadModel,
		systemPrompt: synthSystemPrompt(sessionSystemTokens),
	}

	stats := &loadStats{}
	httpClient := &http.Client{Timeout: 5 * time.Minute}

	start := time.Now()
	fmt.Fprintf(os.Stderr, "iplane load session: %d sessions x %d turns (think-time=%v) -> %s (priority=%s tenant=%s stream=%v)\n",
		sessionCount, sessionTurns, sessionThinkTime, base,
		displayLabel(loadPriority), displayLabel(loadTenant), loadStream)

	var wg sync.WaitGroup
	for i := range sessionCount {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			runSession(ctx, httpClient, &cfg, sessionID(idx), sessionTurns, sessionThinkTime, stats)
		}(i)
	}
	wg.Wait()

	stats.print(time.Since(start), 0, loadOutput)
	if stats.errors > 0 {
		return fmt.Errorf("%d turns errored", stats.errors)
	}
	return nil
}

// runSession drives one conversation to completion: turns user messages
// in, appends each real assistant reply, and resends the growing history
// every turn. Closed-loop -- it blocks on each turn's response before the
// next, plus thinkTime between turns. Aborts early (without erroring the
// command) on context cancellation or the first failed turn, which is
// already recorded in stats.
func runSession(ctx context.Context, c *http.Client, cfg *sessionFireConfig, id string, turns int, thinkTime time.Duration, st *loadStats) {
	history := make([]chatMessage, 0, turns*2)
	for turn := range turns {
		if ctx.Err() != nil {
			return
		}
		history = append(history, chatMessage{Role: "user", Content: synthUserTurn(turn)})

		messages := make([]chatMessage, 0, len(history)+1)
		messages = append(messages, chatMessage{Role: "system", Content: cfg.systemPrompt})
		messages = append(messages, history...)

		content, _, err := fireSessionTurn(ctx, c, cfg, id, messages, st)
		if err != nil {
			return
		}
		history = append(history, chatMessage{Role: "assistant", Content: content})

		if turn < turns-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(thinkTime):
			}
		}
	}
}

// fireSessionTurn posts one turn, stamps the X-IPlane-Session header,
// records the outcome in stats, and returns the assistant's reply so the
// caller can append it to the conversation. The returned error is for
// session-loop control only (abort this conversation); it has already
// been counted in stats.
func fireSessionTurn(ctx context.Context, c *http.Client, cfg *sessionFireConfig, sessionID string, messages []chatMessage, st *loadStats) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.base+cfg.chatPath, bytes.NewReader(sessionChatBody(cfg, messages)))
	if err != nil {
		st.recordError()
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-IPlane-Session", sessionID)
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
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			st.recordError()
		}
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		_, _ = io.Copy(io.Discard, resp.Body)
		st.recordError()
		return "", 0, fmt.Errorf("status %d", resp.StatusCode)
	}

	res := parseChatResponse(resp, cfg.stream, t0)
	st.recordSuccess(dur, res.Tokens)
	if res.HasTTFT {
		st.recordTTFT(res.TTFT)
	}
	return res.Content, res.Tokens, nil
}

func sessionChatBody(cfg *sessionFireConfig, messages []chatMessage) []byte {
	body := map[string]any{
		"model":      cfg.model,
		"messages":   messages,
		"max_tokens": cfg.maxTokens,
	}
	if cfg.stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	b, _ := json.Marshal(body)
	return b
}

// chatResult is one turn's outcome: what came back, how much of it, and
// how long the caller waited for the first token.
//
// TTFT is paired with HasTTFT rather than relying on a zero value, because
// zero is a legitimate reading on a fast local engine and "not measured" has
// to stay distinguishable from "measured as very fast". A summary that
// averaged unmeasured turns in as zero would report a flattering number that
// is not about the engine at all.
type chatResult struct {
	Content string
	Tokens  int64
	TTFT    time.Duration
	HasTTFT bool

	// ITLs is the gap between consecutive content frames, one entry per
	// frame after the first. Empty on a non-streamed response and on a
	// stream that produced one frame, both of which are "not measured"
	// rather than "zero" for the reason TTFT is.
	//
	// Separate from TTFT because the two answer different questions and
	// move independently: time to first token is dominated by queueing
	// and prefill, and the gap between tokens afterwards is decode. A
	// batch that doubles will often leave the first roughly alone and
	// stretch the second, which is invisible in a single latency number.
	ITLs []time.Duration
}

// parseChatResponse extracts the assistant reply text, completion token
// count, and time-to-first-token from a chat response. Non-streaming:
// choices[0].message.content + usage.completion_tokens. Streaming (SSE):
// accumulate choices[0].delta.content across frames, take the max reported
// usage. Returns zero values on a malformed body -- the load tool is not a
// correctness checker, so a mangled response is a zero-content, zero-token
// success rather than a hard failure.
//
// start is when the request went out, so TTFT covers queueing, prefill and
// the network, which is what the caller actually experienced.
//
// TTFT is only available on the streaming path, and that is a property of
// the protocol rather than a gap here: a non-streamed response arrives in
// one piece, so its first token and its last token land at the same instant
// and the number would just be total latency wearing a different label.
func parseChatResponse(resp *http.Response, stream bool, start time.Time) chatResult {
	if !stream {
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return chatResult{}
		}
		var r struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return chatResult{}
		}
		var content string
		if len(r.Choices) > 0 {
			content = r.Choices[0].Message.Content
		}
		return chatResult{Content: content, Tokens: r.Usage.CompletionTokens}
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var sb strings.Builder
	var tokens int64
	var ttft time.Duration
	var hasTTFT bool
	var itls []time.Duration
	last := start
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage struct {
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(payload, &frame); err != nil {
			continue
		}
		if len(frame.Choices) > 0 {
			// The clock stops on the first frame carrying actual text.
			// OpenAI-compatible streams open with a role-only delta, and
			// counting that as the first token would measure the moment
			// the engine acknowledged the request rather than the moment
			// it produced anything, understating TTFT by the whole
			// prefill on a long prompt.
			if chunk := frame.Choices[0].Delta.Content; chunk != "" {
				now := time.Now()
				if !hasTTFT {
					ttft = now.Sub(start)
					hasTTFT = true
				} else {
					itls = append(itls, now.Sub(last))
				}
				last = now
				sb.WriteString(chunk)
			}
		}
		if frame.Usage.CompletionTokens > tokens {
			tokens = frame.Usage.CompletionTokens
		}
	}
	return chatResult{Content: sb.String(), Tokens: tokens, TTFT: ttft, HasTTFT: hasTTFT, ITLs: itls}
}

// sessionID is the stable per-conversation identity stamped into
// X-IPlane-Session. Zero-padded so lexical and numeric order agree in
// logs and dashboards.
func sessionID(idx int) string { return fmt.Sprintf("s-%04d", idx) }

// synthSystemPrompt builds a deterministic system prompt of roughly
// `tokens` whitespace-separated words. Whitespace tokens approximate
// model tokens closely enough for the demo's purpose (sizing the shared
// prefix), without pulling in a tokenizer dependency.
func synthSystemPrompt(tokens int) string {
	return synthPrompt(tokens)
}

// synthUserTurn returns a deterministic user message for the given turn
// index, cycling through a fixed script. Deterministic content keeps the
// resent prefix a stable, cacheable string within a run.
func synthUserTurn(turn int) string {
	prompts := []string{
		"Walk me through how a control plane provisions a GPU instance.",
		"What happens next once the instance is running?",
		"How does the engine load model weights on first start?",
		"Explain how requests get routed to a replica.",
		"Now describe what changes when there are multiple replicas.",
		"How would prefix caching help a multi-turn conversation like this one?",
		"What breaks if every turn lands on a different replica?",
		"Summarize everything we have discussed so far.",
	}
	return prompts[turn%len(prompts)]
}
