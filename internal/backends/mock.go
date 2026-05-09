package backends

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mathrand "math/rand/v2"
	"strings"
	"time"
)

// MockBackend is a Backend implementation that returns synthetic
// OpenAI-shaped responses with configurable latency and token counts.
// Useful for:
//
//   - Local development on machines without an NVIDIA GPU (Apple Silicon, etc.)
//   - CI smoke tests that don't want to provision GPU instances
//   - Caching and rate-limit tests that need deterministic-shape responses
//   - Dashboard authoring with synthetic traffic
//   - Demos and book figures
//
// Generation is fast and free; the only thing it doesn't do is real
// model inference. Everything upstream of the backend boundary --
// middleware chain, request routing, OTel emission, the OpenAI HTTP
// surface -- exercises end-to-end against this backend exactly the
// same way it does against vLLM.
type MockBackend struct {
	name string
	rng  *mathrand.Rand

	// Latency configuration. Each Generate call sleeps for a duration
	// sampled uniformly between min and max. Defaults are tuned for
	// the bimodal LLM latency shape so the duration histogram has
	// realistic distribution.
	minLatency time.Duration
	maxLatency time.Duration

	// Output token count is sampled uniformly between these bounds,
	// clamped to req.MaxTokens if the caller specified one. Defaults
	// give a plausible mix.
	minOutputTokens int
	maxOutputTokens int

	// Health failure injection. Each Health() call returns a fake
	// error with this probability. Useful for exercising the
	// inference.backend.healthy gauge transitions in dashboards.
	// Default 0 (always healthy).
	healthFailRate float64
}

// MockOption customizes a MockBackend at construction.
type MockOption func(*MockBackend)

// WithLatency sets the latency range a Generate call samples from.
func WithLatency(min, max time.Duration) MockOption {
	return func(m *MockBackend) {
		m.minLatency = min
		m.maxLatency = max
	}
}

// WithOutputTokens sets the output-token-count range a response is
// sampled from. Clamped to the caller's MaxTokens at request time.
func WithOutputTokens(min, max int) MockOption {
	return func(m *MockBackend) {
		m.minOutputTokens = min
		m.maxOutputTokens = max
	}
}

// WithHealthFailRate injects synthetic health failures at the given
// probability (0.0 to 1.0). Useful for exercising the backend health
// gauge in dashboards.
func WithHealthFailRate(p float64) MockOption {
	return func(m *MockBackend) { m.healthFailRate = p }
}

// NewMock constructs a MockBackend with default settings tuned for a
// realistic-looking duration histogram and token throughput.
//
// Defaults:
//
//	latency:        100 ms .. 4 s   (most calls fast, some slow -- bimodal shape)
//	output tokens:  20 .. 200       (mix of short answers and longer ones)
//	health fail:    0%              (always serving)
func NewMock(name string, opts ...MockOption) *MockBackend {
	m := &MockBackend{
		name:            name,
		rng:             mathrand.New(mathrand.NewPCG(uint64(time.Now().UnixNano()), 42)),
		minLatency:      100 * time.Millisecond,
		maxLatency:      4 * time.Second,
		minOutputTokens: 20,
		maxOutputTokens: 200,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *MockBackend) Name() string { return m.name }

// Generate sleeps for a sampled duration (honoring context cancellation
// so the GPU-time-saver lever works), then returns a synthetic response
// shaped to match the request: Text for prompt-style requests, Message
// for chat-style.
func (m *MockBackend) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	// Sample a latency in [min, max].
	jitter := m.maxLatency - m.minLatency
	delay := m.minLatency + time.Duration(m.rng.Int64N(int64(jitter)+1))

	select {
	case <-ctx.Done():
		return GenerateResponse{}, ctx.Err()
	case <-time.After(delay):
	}

	// Sample output token count, clamped to MaxTokens if specified.
	tokens := m.minOutputTokens + m.rng.IntN(m.maxOutputTokens-m.minOutputTokens+1)
	finishReason := "stop"
	if req.MaxTokens > 0 && tokens >= req.MaxTokens {
		tokens = req.MaxTokens
		finishReason = "length"
	}

	text := syntheticText(tokens)
	promptTokens := approximateTokens(req)
	totalTokens := promptTokens + tokens

	resp := GenerateResponse{
		ID:      "mock-" + randomID(),
		Created: time.Now().Unix(),
		Model:   req.Model,
		Usage: Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: tokens,
			TotalTokens:      totalTokens,
		},
	}

	if len(req.Messages) > 0 {
		// Chat-style response.
		resp.Object = "chat.completion"
		resp.Choices = []Choice{{
			Index:        0,
			Message:      &ChatMessage{Role: "assistant", Content: text},
			FinishReason: finishReason,
		}}
	} else {
		// Plain completion.
		resp.Object = "text_completion"
		resp.Choices = []Choice{{
			Index:        0,
			Text:         text,
			FinishReason: finishReason,
		}}
	}
	return resp, nil
}

// Health reports the synthetic health of the mock backend. Returns nil
// most of the time; with WithHealthFailRate set, fails proportionally
// so dashboard health transitions can be exercised.
func (m *MockBackend) Health(ctx context.Context) error {
	if m.healthFailRate > 0 && m.rng.Float64() < m.healthFailRate {
		return fmt.Errorf("mock: synthetic health failure (rate=%.2f)", m.healthFailRate)
	}
	return nil
}

// approximateTokens estimates token count using the rough rule of thumb
// of one token per four characters of English text. Real tokenizers
// vary; this is good enough for synthetic traffic.
func approximateTokens(req GenerateRequest) int {
	n := len(req.Prompt)
	for _, msg := range req.Messages {
		n += len(msg.Content) + 4 // role + framing overhead
	}
	tokens := n / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// syntheticText returns approximately n tokens of canned text. Uses a
// fixed corpus repeated and truncated; enough variation that it doesn't
// look totally degenerate but cheap to generate.
func syntheticText(nTokens int) string {
	// Each "word" is approximately one token (rule of thumb for English).
	const corpus = "the quick brown fox jumps over the lazy dog while a curious cat watches " +
		"silently from atop the wooden fence enjoying the warm afternoon sun and " +
		"reflecting on whether dinner will arrive before the rain starts again "

	var b strings.Builder
	words := strings.Fields(corpus)
	for i := 0; i < nTokens; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(words[i%len(words)])
	}
	return b.String()
}

// randomID generates an 8-byte hex string for the response ID. Doesn't
// need to be cryptographically secure -- it's a synthetic ID for a
// synthetic response -- but using crypto/rand keeps the implementation
// simple and avoids managing a separate seeded RNG just for IDs.
func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
