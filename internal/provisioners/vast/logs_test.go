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
