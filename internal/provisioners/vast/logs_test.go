package vast

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The S3 object appears a moment after the upload is requested, so the
// first fetch 403s. A single attempt would report no logs for every
// instance whose upload had not landed yet, which is most of them.
// Measured against a live instance: 403, then 200.
func TestInstanceLogsPollsThroughTheNotWrittenYet403(t *testing.T) {
	var fetches int
	logs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches++
		if fetches == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("Loading checkpoint shards 3/4\nValueError: max seq len too large\n"))
	}))
	t.Cleanup(logs.Close)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "request_logs") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"success":true,"result_url":"` + logs.URL + `"}`))
	}))
	t.Cleanup(api.Close)

	p := New(NewClient("k", WithBaseURL(api.URL)))
	got := p.instanceLogs(context.Background(), "42")

	if !strings.Contains(got, "max seq len too large") {
		t.Errorf("logs = %q, want the engine's own error", got)
	}
	if fetches < 2 {
		t.Errorf("fetched %d times; the 403 was treated as a refusal rather than as not-yet-written", fetches)
	}
}

// A logging failure must never replace the real failure with its own.
// This runs on a path that has already failed.
func TestInstanceLogsSaysNothingRatherThanErroring(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(api.Close)

	p := New(NewClient("k", WithBaseURL(api.URL)))
	if got := p.instanceLogs(context.Background(), "42"); got != "" {
		t.Errorf("logs = %q, want empty when the provider cannot say", got)
	}
}

// The engine's error has to survive sshd. A Vast container's stdout carries
// both, the deploy path connects over SSH repeatedly, and a fixed tail of the
// raw stream lands entirely inside the session bookkeeping. A failed 0.5B
// deploy reported twenty lines of "Accepted publickey for root" under the
// heading "--- engine said ---" and nothing from vLLM.
func TestDropSSHNoiseKeepsTheEngineError(t *testing.T) {
	body := `Connection from ::1 port 38616 on ::1 port 22 rdomain ""
ValueError: Model architectures ['Qwen2ForCausalLM'] failed to be inspected
Failed publickey for root from ::1 port 38616 ssh2: ED25519 SHA256:uV8
  File "/usr/lib/python3/vllm/engine.py", line 220, in _init
Postponed publickey for root from ::1 port 38616 ssh2 [preauth]
Accepted publickey for root from ::1 port 38616 ssh2: ED25519 SHA256:n/x
Starting session: command for root from ::1 port 38616 id 0
Received disconnect from ::1 port 38616:11: disconnected by user
Disconnected from user root ::1 port 38616`

	got := dropSSHNoise(body)

	for _, want := range []string{"ValueError: Model architectures", "vllm/engine.py"} {
		if !strings.Contains(got, want) {
			t.Errorf("filtered log lost the engine line %q\ngot:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"publickey", "Starting session", "Disconnected from user"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("filtered log still carries ssh chatter %q\ngot:\n%s", unwanted, got)
		}
	}
}

// A log that is nothing but ssh chatter must come back whole rather than
// empty. This runs on a path that has already failed, and an empty section
// replaces a real diagnosis with silence.
func TestDropSSHNoiseFallsBackWhenEverythingIsChatter(t *testing.T) {
	body := `Connection from ::1 port 38616 on ::1 port 22 rdomain ""
Accepted publickey for root from ::1 port 38616 ssh2: ED25519 SHA256:n/x
Disconnected from user root ::1 port 38616`

	if got := dropSSHNoise(body); got != body {
		t.Errorf("all-chatter log should fall back to the raw body\ngot:\n%s", got)
	}
}

// The tail is taken after filtering, so a long run of chatter cannot push the
// engine's error out of the window.
func TestInstanceLogTailSurvivesLongChatter(t *testing.T) {
	var b strings.Builder
	b.WriteString("RuntimeError: CUDA error: no kernel image is available\n")
	for range 200 {
		b.WriteString("Accepted publickey for root from ::1 port 38616 ssh2: ED25519\n")
	}

	got := tail(dropSSHNoise(b.String()), logTailLines)

	if !strings.Contains(got, "RuntimeError: CUDA error") {
		t.Errorf("engine error scrolled out of the tail\ngot:\n%s", got)
	}
}
