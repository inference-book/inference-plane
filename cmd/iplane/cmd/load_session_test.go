package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// decodeSessionMessages pulls the chat messages array out of a request
// body the session driver posted. Used by the body-inspecting tests.
func decodeSessionMessages(t *testing.T, r *http.Request) []chatMessage {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var body struct {
		Messages []chatMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal body %q: %v", raw, err)
	}
	return body.Messages
}

const sessionOKResponse = `{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":1}}`

// TestLoadSession_StableSessionHeader: every turn in a conversation
// carries the same X-IPlane-Session value, and distinct sessions carry
// distinct values. This is the affinity key the PrefixAffinity policy
// (#172) routes on.
func TestLoadSession_StableSessionHeader(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := r.Header.Get("X-IPlane-Session")
		mu.Lock()
		counts[sid]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sessionOKResponse)
	}))
	defer srv.Close()

	cfg := &sessionFireConfig{base: srv.URL, chatPath: "/v1/chat/completions", model: "m", maxTokens: 16, systemPrompt: "sys"}
	stats := &loadStats{}
	runSession(t.Context(), &http.Client{}, cfg, "s-0001", 3, 0, stats)
	runSession(t.Context(), &http.Client{}, cfg, "s-0002", 3, 0, stats)

	if len(counts) != 2 {
		t.Fatalf("distinct session ids = %d, want 2 (%v)", len(counts), counts)
	}
	if counts["s-0001"] != 3 || counts["s-0002"] != 3 {
		t.Errorf("per-session request counts = %v, want each 3", counts)
	}
}

// TestLoadSession_PrefixGrowsPerTurn: the messages array resent each
// turn grows monotonically (system + the full running history), so the
// prefix a downstream engine can cache gets longer every turn.
func TestLoadSession_PrefixGrowsPerTurn(t *testing.T) {
	var mu sync.Mutex
	var lens []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msgs := decodeSessionMessages(t, r)
		mu.Lock()
		lens = append(lens, len(msgs))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sessionOKResponse)
	}))
	defer srv.Close()

	cfg := &sessionFireConfig{base: srv.URL, chatPath: "/v1/chat/completions", model: "m", maxTokens: 16, systemPrompt: "sys"}
	stats := &loadStats{}
	runSession(t.Context(), &http.Client{}, cfg, "s-0001", 4, 0, stats)

	// turn 0: [system, user]; turn 1: [system, user, assistant, user]; ...
	want := []int{2, 4, 6, 8}
	if len(lens) != len(want) {
		t.Fatalf("turn count = %d, want %d (%v)", len(lens), len(want), lens)
	}
	for i := range want {
		if lens[i] != want[i] {
			t.Errorf("turn %d messages = %d, want %d (all: %v)", i, lens[i], want[i], lens)
		}
	}
}

// TestLoadSession_AppendsRealAssistantResponse: the assistant content
// the server returned on turn N reappears in turn N+1's history. The
// driver replays a real conversation, not a fixed script, so the prefix
// is a stable cacheable string within a run.
func TestLoadSession_AppendsRealAssistantResponse(t *testing.T) {
	const reply = "PREFILL_ME"
	var mu sync.Mutex
	var turn1Messages []chatMessage
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msgs := decodeSessionMessages(t, r)
		mu.Lock()
		if n == 1 {
			turn1Messages = msgs
		}
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+reply+`"}}],"usage":{"completion_tokens":2}}`)
	}))
	defer srv.Close()

	cfg := &sessionFireConfig{base: srv.URL, chatPath: "/v1/chat/completions", model: "m", maxTokens: 16, systemPrompt: "sys"}
	stats := &loadStats{}
	runSession(t.Context(), &http.Client{}, cfg, "s-0001", 2, 0, stats)

	var sawAssistantReply bool
	for _, m := range turn1Messages {
		if m.Role == "assistant" && m.Content == reply {
			sawAssistantReply = true
		}
	}
	if !sawAssistantReply {
		t.Errorf("turn-1 history missing the real assistant reply %q; got %+v", reply, turn1Messages)
	}
}

// TestLoadSession_ClosedLoop: within a session, turn N+1 does not start
// until turn N's response returns. A buggy fire-all-turns-concurrently
// impl would show >1 in flight; closed-loop holds it at 1.
func TestLoadSession_ClosedLoop(t *testing.T) {
	var inFlight, maxInFlight int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sessionOKResponse)
		atomic.AddInt32(&inFlight, -1)
	}))
	defer srv.Close()

	cfg := &sessionFireConfig{base: srv.URL, chatPath: "/v1/chat/completions", model: "m", maxTokens: 16, systemPrompt: "sys"}
	stats := &loadStats{}
	runSession(t.Context(), &http.Client{}, cfg, "s-0001", 4, 0, stats)

	if got := atomic.LoadInt32(&maxInFlight); got != 1 {
		t.Errorf("max concurrent in-flight within a session = %d, want 1 (not closed-loop)", got)
	}
	if stats.successes != 4 {
		t.Errorf("successes = %d, want 4", stats.successes)
	}
}
