package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A stream that ends with [DONE] is a completed request.
func TestCompleteStreamIsMarkedComplete(t *testing.T) {
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[{"delta":{"content":"b"}}],"usage":{"completion_tokens":2}}`,
	}, time.Millisecond)

	got := parseChatResponse(getStream(t, srv.URL), true, time.Now())

	if !got.Complete {
		t.Error("a stream terminated by [DONE] should be Complete")
	}
	if got.Tokens != 2 {
		t.Errorf("Tokens = %d, want 2", got.Tokens)
	}
}

// A stream cut off before [DONE] is not a completed request. The engine is
// still working; we stopped listening because the measurement window closed.
func TestTruncatedStreamIsNotComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		flusher.Flush()
		// Hang up mid-stream: no further frames, no [DONE].
	}))
	t.Cleanup(srv.Close)

	got := parseChatResponse(getStream(t, srv.URL), true, time.Now())

	if got.Complete {
		t.Error("a stream with no [DONE] should not be Complete")
	}
	if !got.HasTTFT {
		t.Error("the first token did arrive; TTFT is still a real observation")
	}
}

// A non-streaming response that parsed is complete: there is no [DONE] on
// that path and the whole body arrived at once.
func TestNonStreamingResponseIsComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"completion_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)

	got := parseChatResponse(getStream(t, srv.URL), false, time.Now())

	if !got.Complete {
		t.Error("a parsed non-streaming body should be Complete")
	}
}

// The accounting consequence: a request the window closes on lands in
// truncated, not in successes, and contributes no latency and no tokens.
// Counting it as a success is what reported 6.2 completion tokens against a
// 512 cap on the 120k run.
func TestTruncatedRequestIsNotCountedAsSuccess(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		flusher.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	cfg := &loadFireConfig{
		base:            srv.URL,
		chatPath:        "/",
		completionsPath: "/",
		stream:          true,
	}
	st := &loadStats{}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	fireLoadRequest(ctx, srv.Client(), cfg, st)

	st.mu.Lock()
	successes, truncated, tokens, lats := st.successes, st.truncated, st.tokens, len(st.latencies)
	st.mu.Unlock()

	if successes != 0 {
		t.Errorf("successes = %d, want 0: the request never finished", successes)
	}
	if truncated != 1 {
		t.Errorf("truncated = %d, want 1", truncated)
	}
	if tokens != 0 {
		t.Errorf("tokens = %d, want 0: a truncated stream's count is not the engine's output", tokens)
	}
	if lats != 0 {
		t.Errorf("latencies = %d, want 0: the clock stopped when we stopped listening", lats)
	}
}
