package router

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// drive runs the input bytes through a tokenCountingWriter via a
// httptest.ResponseRecorder and returns the writer for assertions.
// The writer-under-test simulates what the reverse-proxy would do:
// set Content-Type via header, call WriteHeader, then call Write
// (possibly multiple times for streaming).
func drive(t *testing.T, contentType string, status int, chunks ...string) *tokenCountingWriter {
	t.Helper()
	rec := httptest.NewRecorder()
	tcw := newTokenCountingWriter(rec)
	tcw.Header().Set("Content-Type", contentType)
	tcw.WriteHeader(status)
	for _, c := range chunks {
		if _, err := io.WriteString(tcw, c); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return tcw
}

func TestTokenCounter_NonStreaming_ExtractsUsage(t *testing.T) {
	body := `{
		"id": "chatcmpl-test",
		"choices": [{"message": {"content": "hi"}}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 42, "total_tokens": 47}
	}`
	tcw := drive(t, "application/json", 200, body)
	if got := tcw.CompletionTokens(); got != 42 {
		t.Errorf("CompletionTokens = %d, want 42", got)
	}
	if got := tcw.StatusLabel(); got != "success" {
		t.Errorf("StatusLabel = %q, want success", got)
	}
}

func TestTokenCounter_Streaming_ExtractsFromUsageFrame(t *testing.T) {
	// Multi-chunk SSE stream; last frame before [DONE] carries usage.
	// vLLM and OpenAI both emit usage this way under
	// stream_options.include_usage=true.
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{}, \"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":17}}\n\n",
		"data: [DONE]\n\n",
	}
	tcw := drive(t, "text/event-stream", 200, chunks...)
	if got := tcw.CompletionTokens(); got != 17 {
		t.Errorf("CompletionTokens = %d, want 17", got)
	}
}

func TestTokenCounter_Streaming_NoUsage_ReturnsZero(t *testing.T) {
	// Stream without any usage field -- some clients don't set
	// stream_options.include_usage and don't get usage emitted.
	// Counter should yield zero (recorder will skip emission).
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	tcw := drive(t, "text/event-stream", 200, chunks...)
	if got := tcw.CompletionTokens(); got != 0 {
		t.Errorf("CompletionTokens = %d, want 0 (no usage frame)", got)
	}
}

func TestTokenCounter_Streaming_PartialEvent_Ignored(t *testing.T) {
	// Stream cut off mid-event (no terminating blank line). The
	// partial event stays in the buffer; CompletionTokens returns
	// whatever was last successfully parsed (zero in this case).
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"",
	}
	tcw := drive(t, "text/event-stream", 200, chunks...)
	if got := tcw.CompletionTokens(); got != 0 {
		t.Errorf("CompletionTokens on partial event = %d, want 0", got)
	}
}

func TestTokenCounter_MalformedJSON_ReturnsZero(t *testing.T) {
	tcw := drive(t, "application/json", 200, `{not json`)
	if got := tcw.CompletionTokens(); got != 0 {
		t.Errorf("CompletionTokens on malformed body = %d, want 0 (no panic)", got)
	}
}

func TestTokenCounter_ErrorResponse_NoBuffering(t *testing.T) {
	// 5xx body should not be buffered or parsed -- the router
	// emitted an error envelope, not a chat-completion. Asserts
	// CompletionTokens is zero and StatusLabel maps to engine_error.
	body := `{"usage":{"completion_tokens":999}}` // even if it has usage-shaped
	tcw := drive(t, "application/json", 502, body)
	if got := tcw.CompletionTokens(); got != 0 {
		t.Errorf("CompletionTokens on 5xx = %d, want 0", got)
	}
	if got := tcw.StatusLabel(); got != "engine_error" {
		t.Errorf("StatusLabel = %q, want engine_error", got)
	}
}

func TestTokenCounter_PlainTextResponse_NoParse(t *testing.T) {
	// Non-JSON, non-SSE content type. Counter is silent.
	tcw := drive(t, "text/plain", 200, "just some text")
	if got := tcw.CompletionTokens(); got != 0 {
		t.Errorf("CompletionTokens = %d, want 0", got)
	}
}

func TestTokenCounter_Streaming_LatestUsageWins(t *testing.T) {
	// Some engines emit usage progressively (e.g. vLLM in certain
	// modes). The latest observed usage should be the final value
	// reported, matching the engine's end-of-stream accounting.
	chunks := []string{
		"data: {\"usage\":{\"completion_tokens\":3}}\n\n",
		"data: {\"usage\":{\"completion_tokens\":7}}\n\n",
		"data: {\"usage\":{\"completion_tokens\":12}}\n\n",
		"data: [DONE]\n\n",
	}
	tcw := drive(t, "text/event-stream", 200, chunks...)
	if got := tcw.CompletionTokens(); got != 12 {
		t.Errorf("CompletionTokens = %d, want 12 (latest observation)", got)
	}
}

func TestTokenCounter_Streaming_CRLFTerminator(t *testing.T) {
	// WHATWG SSE spec permits CRLF event terminators. Some HTTP
	// stacks normalize, others don't. The parser must handle both.
	body := "data: {\"usage\":{\"completion_tokens\":99}}\r\n\r\ndata: [DONE]\r\n\r\n"
	tcw := drive(t, "text/event-stream", 200, body)
	if got := tcw.CompletionTokens(); got != 99 {
		t.Errorf("CompletionTokens with CRLF terminators = %d, want 99", got)
	}
}

func TestTokenCounter_Streaming_ChunkBoundaryAcrossEvents(t *testing.T) {
	// A Write boundary lands in the middle of an event. The parser
	// must accumulate across Writes until a full event arrives.
	tcw := drive(t, "text/event-stream", 200,
		"data: {\"usa",
		"ge\":{\"completion",
		"_tokens\":55}}\n\n",
		"data: [DONE]\n\n",
	)
	if got := tcw.CompletionTokens(); got != 55 {
		t.Errorf("CompletionTokens with split chunks = %d, want 55", got)
	}
}

func TestTokenCounter_StatusLabel_AllRanges(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{200, "success"},
		{204, "success"},
		{400, "client_error"},
		{404, "client_error"},
		{500, "engine_error"},
		{502, "engine_error"},
		{0, "unknown"},
	}
	for _, c := range cases {
		// Use plain-text to avoid the parse paths.
		rec := httptest.NewRecorder()
		tcw := newTokenCountingWriter(rec)
		tcw.Header().Set("Content-Type", "text/plain")
		if c.code != 0 {
			tcw.WriteHeader(c.code)
		}
		if got := tcw.StatusLabel(); got != c.want {
			t.Errorf("StatusLabel(%d) = %q, want %q", c.code, got, c.want)
		}
	}
}

// TestTokenCounter_PassThrough verifies bytes flow through to the
// underlying writer unchanged. The token counter must not eat, drop,
// or duplicate any data -- the proxy depends on it being a
// transparent wrap.
func TestTokenCounter_PassThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	tcw := newTokenCountingWriter(rec)
	tcw.Header().Set("Content-Type", "application/json")
	tcw.WriteHeader(200)
	body := `{"hello":"world","usage":{"completion_tokens":3}}`
	if _, err := io.WriteString(tcw, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := rec.Body.String()
	if got != body {
		t.Errorf("pass-through corrupted bytes; want %q, got %q", body, got)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type header lost in wrap")
	}
}
