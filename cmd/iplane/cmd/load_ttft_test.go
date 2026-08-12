package cmd

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// sseServer streams the given frames, pausing before each one, so a test can
// assert on when the clock stopped rather than only on what was parsed.
func sseServer(t *testing.T, frames []string, gap time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server does not support flushing; SSE timing would be meaningless")
			return
		}
		for _, f := range frames {
			time.Sleep(gap)
			fmt.Fprintf(w, "data: %s\n\n", f)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func getStream(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// OpenAI-compatible streams open with a role-only delta carrying no text.
// Stopping the clock there would measure when the engine acknowledged the
// request, not when it produced anything, understating TTFT by the whole
// prefill on a long prompt.
func TestTTFTIgnoresTheRoleOnlyOpeningFrame(t *testing.T) {
	const gap = 60 * time.Millisecond
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"delta":{"content":" there"}}],"usage":{"completion_tokens":2}}`,
	}, gap)

	start := time.Now()
	got := parseChatResponse(getStream(t, srv.URL), true, start)

	if !got.HasTTFT {
		t.Fatal("no TTFT measured on a streaming response")
	}
	// The clock must have run past the role-only frame into the first
	// content frame, so at least two gaps.
	if got.TTFT < 2*gap {
		t.Errorf("TTFT = %v, want >= %v (clock stopped on the role-only frame)", got.TTFT, 2*gap)
	}
	// And it must not have run to completion.
	if got.TTFT >= 3*gap {
		t.Errorf("TTFT = %v, want < %v (clock ran past the first content frame)", got.TTFT, 3*gap)
	}
	if got.Content != "Hello there" {
		t.Errorf("content = %q", got.Content)
	}
	if got.Tokens != 2 {
		t.Errorf("tokens = %d, want 2", got.Tokens)
	}
}

// A non-streamed response arrives in one piece, so its first and last token
// land together. Reporting that as TTFT would be total latency wearing a
// different label.
func TestNonStreamingReportsNoTTFT(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"completion_tokens":1}}`)
	}))
	defer srv.Close()

	got := parseChatResponse(getStream(t, srv.URL), false, time.Now())

	if got.HasTTFT {
		t.Error("TTFT reported for a non-streamed response")
	}
	if got.Content != "hi" || got.Tokens != 1 {
		t.Errorf("content/tokens = %q/%d", got.Content, got.Tokens)
	}
}

// A stream that carries no text at all has nothing to time.
func TestStreamWithoutContentReportsNoTTFT(t *testing.T) {
	srv := sseServer(t, []string{`{"choices":[{"delta":{"role":"assistant"}}]}`}, time.Millisecond)

	got := parseChatResponse(getStream(t, srv.URL), true, time.Now())

	if got.HasTTFT {
		t.Errorf("TTFT = %v reported for a stream with no content", got.TTFT)
	}
}

// Zero is a legitimate reading on a fast local engine, so "not measured"
// has to stay distinguishable from "measured as very fast". A summary that
// counted unmeasured turns as zero would report a flattering number that is
// not about the engine.
func TestUnmeasuredTurnsAreNotCountedAsZero(t *testing.T) {
	s := &loadStats{}
	s.recordSuccess(100*time.Millisecond, 10)
	s.recordSuccess(100*time.Millisecond, 10)
	s.recordTTFT(40 * time.Millisecond) // only one of the two was streamed

	sum := s.summary(time.Second, 0)

	if sum.TTFTSamples != 1 {
		t.Errorf("ttft samples = %d, want 1", sum.TTFTSamples)
	}
	if sum.TTFTP50Ms != 40 {
		t.Errorf("ttft p50 = %dms, want 40ms (an unmeasured turn dragged the percentile)", sum.TTFTP50Ms)
	}
	if sum.Successes != 2 {
		t.Errorf("successes = %d, want 2", sum.Successes)
	}
}

func TestNoTTFTSamplesLeavesTheFieldsZero(t *testing.T) {
	s := &loadStats{}
	s.recordSuccess(100*time.Millisecond, 10)

	sum := s.summary(time.Second, 0)

	if sum.TTFTSamples != 0 || sum.TTFTP50Ms != 0 || sum.TTFTP95Ms != 0 || sum.TTFTP99Ms != 0 {
		t.Errorf("ttft fields populated with no samples: %+v", sum)
	}
	// Latency percentiles are unaffected by the new field set.
	if sum.LatencyP50Ms != 100 {
		t.Errorf("latency p50 = %dms, want 100ms", sum.LatencyP50Ms)
	}
}

func TestTTFTPercentiles(t *testing.T) {
	s := &loadStats{}
	// Recorded out of order to prove the summary sorts.
	for _, ms := range []int{90, 10, 50, 30, 70} {
		s.recordTTFT(time.Duration(ms) * time.Millisecond)
	}

	sum := s.summary(time.Second, 0)

	if sum.TTFTSamples != 5 {
		t.Fatalf("samples = %d, want 5", sum.TTFTSamples)
	}
	if sum.TTFTP50Ms != 50 {
		t.Errorf("p50 = %dms, want 50ms", sum.TTFTP50Ms)
	}
	if sum.TTFTP99Ms != 90 {
		t.Errorf("p99 = %dms, want 90ms", sum.TTFTP99Ms)
	}
}
