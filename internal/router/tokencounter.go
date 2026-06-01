package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// tokenCountingWriter wraps an http.ResponseWriter to extract
// completion-token counts from the engine's response while
// pass-through-writing every byte to the client. Two modes,
// selected at WriteHeader time by inspecting Content-Type:
//
//   - Non-streaming (application/json): buffer the body (capped at
//     FlatMaxBodyBytes), parse the OpenAI-shaped usage block at
//     close().
//   - Streaming (text/event-stream): scan inbound bytes for complete
//     SSE events (terminated by \n\n) and extract usage from each
//     event's `data:` field as it arrives. Last observed
//     completion_tokens wins -- some engines emit usage on the
//     terminal frame, some on every frame; either way the final
//     observation is the canonical count.
//
// The wrap does NOT block the proxy on parsing. SSE scanning runs
// inline in Write and operates on already-flushed bytes -- the
// client has already seen them before tokenCountingWriter parses
// anything. Token extraction is best-effort; a partial / unparseable
// stream yields zero tokens and the recorder skips the emission
// rather than poisoning the counter with garbage.
type tokenCountingWriter struct {
	http.ResponseWriter
	flusher http.Flusher

	statusCode  int
	contentType string

	// bodyBuf holds non-streaming response bytes for JSON parsing.
	// Nil for streaming responses.
	bodyBuf *bytes.Buffer

	// sseBuf holds in-flight bytes for the inline SSE parser. Bytes
	// past complete events stay buffered until the next chunk
	// completes them. Nil for non-streaming responses.
	sseBuf *bytes.Buffer

	// tokens is the latest observed completion_tokens count. For
	// streaming responses updated on each parsed event with a
	// usage field; for non-streaming computed at close().
	tokens int64
}

// newTokenCountingWriter wraps w. Returns a writer that also
// implements http.Flusher when w does (required for SSE pass-through
// to actually stream to the client).
func newTokenCountingWriter(w http.ResponseWriter) *tokenCountingWriter {
	tcw := &tokenCountingWriter{ResponseWriter: w}
	if f, ok := w.(http.Flusher); ok {
		tcw.flusher = f
	}
	return tcw
}

// WriteHeader captures the status code and Content-Type, then
// initializes the buffer for whichever parse mode the response shape
// implies. ReverseProxy calls WriteHeader before any Write, so the
// mode is committed before the first byte flows.
func (w *tokenCountingWriter) WriteHeader(code int) {
	w.statusCode = code
	w.contentType = w.Header().Get("Content-Type")
	if w.statusCode < 400 {
		if isStreamingContentType(w.contentType) {
			w.sseBuf = &bytes.Buffer{}
		} else if isJSONContentType(w.contentType) {
			w.bodyBuf = &bytes.Buffer{}
		}
		// Other content types (plain text, html, octet-stream) do
		// not carry usage data; skip buffering entirely.
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write tees bytes to the client first, then feeds them into the
// active parser (if any). Order matters: the client must see bytes
// promptly; parsing is opportunistic.
func (w *tokenCountingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n <= 0 {
		return n, err
	}
	switch {
	case w.bodyBuf != nil:
		// Non-streaming: accumulate up to the cap, then stop
		// (large bodies aren't realistic for chat-completions but
		// the cap is a defensive ceiling).
		if w.bodyBuf.Len()+n <= FlatMaxBodyBytes {
			w.bodyBuf.Write(p[:n])
		}
	case w.sseBuf != nil:
		w.sseBuf.Write(p[:n])
		w.parseSSEBuffer()
	}
	return n, err
}

// Flush forwards to the underlying flusher. ReverseProxy invokes
// this when streaming responses produce flushable boundaries; the
// pass-through is what makes SSE actually stream.
func (w *tokenCountingWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// CompletionTokens returns the parsed token count. For streaming
// responses returns the latest observation seen during Write calls;
// for non-streaming returns the result of parsing the buffered body.
// Zero means "no usage data observed" -- the caller should skip
// emitting the token metric rather than recording a misleading 0.
func (w *tokenCountingWriter) CompletionTokens() int64 {
	if w.bodyBuf != nil {
		return extractCompletionTokens(w.bodyBuf.Bytes())
	}
	return w.tokens
}

// StatusLabel returns a metric-friendly status label for the
// captured response code. Anything 2xx -> "success", 4xx ->
// "client_error", 5xx -> "engine_error", 0 (no WriteHeader fired) ->
// "unknown". The codes follow the OpenAI / iplane envelope mapping
// from router.go's forwardable().
func (w *tokenCountingWriter) StatusLabel() string {
	switch {
	case w.statusCode == 0:
		return "unknown"
	case w.statusCode >= 200 && w.statusCode < 300:
		return "success"
	case w.statusCode >= 400 && w.statusCode < 500:
		return "client_error"
	case w.statusCode >= 500:
		return "engine_error"
	}
	return "other"
}

// parseSSEBuffer scans sseBuf for complete events (delimited by a
// blank line, per the WHATWG SSE spec). For each event, looks at
// every `data:` line for a JSON payload carrying a `usage` field
// and updates w.tokens with the latest observed value.
//
// Partial events stay in the buffer until a subsequent Write completes
// them. The scan handles both \n\n and \r\n\r\n event terminators.
func (w *tokenCountingWriter) parseSSEBuffer() {
	for {
		idx := findEventTerminator(w.sseBuf.Bytes())
		if idx < 0 {
			return
		}
		event := w.sseBuf.Next(idx)
		// Skip the terminator itself so the next scan starts clean.
		w.sseBuf.Next(eventTerminatorLen(w.sseBuf.Bytes(), 0))
		w.extractFromEvent(event)
	}
}

// extractFromEvent walks a single SSE event's lines, accumulates
// data: payloads (per WHATWG, multi-line data is concatenated with
// \n), and parses the result as JSON looking for usage.
func (w *tokenCountingWriter) extractFromEvent(event []byte) {
	var dataLines [][]byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimPrefix(line, []byte("data:"))
			// SSE spec: a single leading space after `data:` is
			// stripped; anything else is preserved.
			if len(payload) > 0 && payload[0] == ' ' {
				payload = payload[1:]
			}
			dataLines = append(dataLines, payload)
		}
	}
	if len(dataLines) == 0 {
		return
	}
	joined := bytes.Join(dataLines, []byte("\n"))
	if n := extractCompletionTokens(joined); n > 0 {
		w.tokens = n
	}
}

// findEventTerminator returns the byte index of the SSE event
// terminator in buf, or -1 if not present. SSE spec allows either
// \n\n or \r\n\r\n. Returns the start position of the terminator.
func findEventTerminator(buf []byte) int {
	if i := bytes.Index(buf, []byte("\n\n")); i >= 0 {
		if j := bytes.Index(buf, []byte("\r\n\r\n")); j >= 0 && j < i {
			return j
		}
		return i
	}
	return bytes.Index(buf, []byte("\r\n\r\n"))
}

// eventTerminatorLen returns the length of the terminator at buf[at].
// Used to advance the buffer past the terminator after consuming
// the event body.
func eventTerminatorLen(buf []byte, at int) int {
	if at < len(buf) && buf[at] == '\r' {
		return 4 // \r\n\r\n
	}
	return 2 // \n\n
}

// extractCompletionTokens decodes a JSON blob and returns the
// usage.completion_tokens field if present. Returns zero when the
// blob is malformed, doesn't contain usage, or completion_tokens is
// missing. Used for both streaming SSE data payloads and
// non-streaming response bodies.
func extractCompletionTokens(data []byte) int64 {
	// Strip the OpenAI streaming terminal sentinel ("[DONE]") -- not
	// JSON, would yield a parse error otherwise.
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return 0
	}
	var probe struct {
		Usage struct {
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return 0
	}
	return probe.Usage.CompletionTokens
}

func isStreamingContentType(ct string) bool {
	return strings.HasPrefix(strings.ToLower(ct), "text/event-stream")
}

func isJSONContentType(ct string) bool {
	lower := strings.ToLower(ct)
	return strings.HasPrefix(lower, "application/json") || strings.HasPrefix(lower, "application/x-ndjson")
}
