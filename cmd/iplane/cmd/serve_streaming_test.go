package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The serve middleware chain must not cost a handler its ability to stream.
//
// It did once. skmw.RequestLogger wraps the ResponseWriter to capture the
// status code, and http.ResponseWriter carries only Header/Write/WriteHeader,
// so the wrap stripped http.Flusher. The router's tokenCountingWriter then
// found no flusher, its Flush became a no-op, and httputil.ReverseProxy's
// per-frame flushes went nowhere. Every streamed response was delivered as a
// single burst at the end (#441).
//
// Nothing about the response body changes when that happens, which is why no
// existing test caught it: same status, same bytes, same total latency. The
// only observable is timing, or this interface check.
func TestServeMiddlewarePreservesFlusher(t *testing.T) {
	var sawFlusher bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, sawFlusher = w.(http.Flusher)
	})

	rec := httptest.NewRecorder() // httptest.ResponseRecorder is an http.Flusher
	wrapServeMiddleware(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	if !sawFlusher {
		t.Fatal("handler behind the serve middleware chain cannot assert http.Flusher; " +
			"SSE responses will buffer to the end (#441)")
	}
}

// Same guarantee via http.ResponseController, which is what newer code
// reaches for and which needs an Unwrap chain rather than a Flush method.
func TestServeMiddlewareSupportsResponseController(t *testing.T) {
	var flushErr error
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flushErr = http.NewResponseController(w).Flush()
	})

	rec := httptest.NewRecorder()
	wrapServeMiddleware(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/completions", nil))

	if flushErr != nil {
		t.Fatalf("ResponseController could not flush through the serve chain: %v", flushErr)
	}
}
