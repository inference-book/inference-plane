package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The bug this guards was invisible for exactly one reason: every piece was
// tested except the wiring between them.
//
// `iplane load`'s summary declared ttft_p50/p95/p99, loadStats had a working
// recordTTFT, and parseChatResponse measured TTFT correctly. All three had
// tests and all three passed. But the request path called a token-only
// parser, so recordTTFT was never invoked and `iplane load --stream` always
// reported ttft_samples=0. The metric looked supported and produced silence.
//
// Ch 10's fabric A/B reads TTFT specifically, so a silent zero there is not a
// cosmetic gap: prefill is where tensor-parallel traffic is heaviest, and an
// interconnect effect can appear in TTFT while throughput barely moves.
//
// This test goes through fireLoadRequest, the real request path, rather than
// through the parser, because the parser was never the broken part.
func TestFireLoadRequestRecordsTTFTOnStream(t *testing.T) {
	const gap = 25 * time.Millisecond
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":"hello "}}]}`,
		`{"choices":[{"delta":{"content":"world"}}],"usage":{"completion_tokens":7}}`,
	}, gap)
	defer srv.Close()

	st := &loadStats{}
	cfg := &loadFireConfig{
		base:            srv.URL,
		chatPath:        "/v1/chat/completions",
		completionsPath: "/v1/completions",
		stream:          true,
	}

	fireLoadRequest(context.Background(), srv.Client(), cfg, st)

	sum := st.summary(time.Second, 0)
	if sum.Successes != 1 {
		t.Fatalf("successes = %d, want 1", sum.Successes)
	}
	if sum.TTFTSamples != 1 {
		t.Fatalf("ttft_samples = %d, want 1: --stream did not reach recordTTFT", sum.TTFTSamples)
	}
	// First *content* frame is the second frame, so TTFT is about two gaps in.
	// Asserting a floor rather than an exact value keeps this from being a
	// timing-flake; the point is that a real measurement arrived.
	if sum.TTFTP50Ms <= 0 {
		t.Errorf("ttft p50 = %dms, want a positive measurement", sum.TTFTP50Ms)
	}
	if sum.Tokens != 7 {
		t.Errorf("completion_tokens = %d, want 7 (usage still parsed after the switch)", sum.Tokens)
	}
}

// Latency on the streaming path must cover the whole response, not the moment
// the headers arrived. http.Client.Do returns as soon as SSE headers land, so
// timing there records roughly TTFT and calls it latency -- which would make
// any A/B comparing two engines on latency meaningless.
func TestFireLoadRequestLatencyCoversTheWholeStream(t *testing.T) {
	const gap = 30 * time.Millisecond
	frames := []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[{"delta":{"content":"b"}}]}`,
		`{"choices":[{"delta":{"content":"c"}}],"usage":{"completion_tokens":3}}`,
	}
	srv := sseServer(t, frames, gap)
	defer srv.Close()

	st := &loadStats{}
	cfg := &loadFireConfig{
		base:            srv.URL,
		chatPath:        "/v1/chat/completions",
		completionsPath: "/v1/completions",
		stream:          true,
	}

	fireLoadRequest(context.Background(), srv.Client(), cfg, st)

	sum := st.summary(time.Second, 0)
	// Four frames at 30ms each means the full body takes ~120ms, while the
	// headers arrive after ~30ms. A latency at or under two gaps means the
	// timer stopped at the headers.
	minExpected := int64(2 * gap / time.Millisecond)
	if sum.LatencyP50Ms <= minExpected {
		t.Errorf("latency p50 = %dms, want > %dms: the timer stopped at the SSE headers rather than the end of the stream",
			sum.LatencyP50Ms, minExpected)
	}
	if sum.TTFTP50Ms >= sum.LatencyP50Ms {
		t.Errorf("ttft (%dms) >= latency (%dms): TTFT must be a prefix of the request, not the whole of it",
			sum.TTFTP50Ms, sum.LatencyP50Ms)
	}
}

// A non-streaming run has nothing to time, and must not invent a zero sample.
func TestFireLoadRequestNoTTFTWithoutStream(t *testing.T) {
	srv := jsonServer(t, `{"choices":[{"message":{"content":"hi"}}],"usage":{"completion_tokens":4}}`)
	defer srv.Close()

	st := &loadStats{}
	cfg := &loadFireConfig{
		base:            srv.URL,
		chatPath:        "/v1/chat/completions",
		completionsPath: "/v1/completions",
		stream:          false,
	}

	fireLoadRequest(context.Background(), srv.Client(), cfg, st)

	sum := st.summary(time.Second, 0)
	if sum.TTFTSamples != 0 {
		t.Errorf("ttft_samples = %d on a non-streaming run, want 0", sum.TTFTSamples)
	}
	if sum.Tokens != 4 {
		t.Errorf("completion_tokens = %d, want 4", sum.Tokens)
	}
}

func jsonServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}
