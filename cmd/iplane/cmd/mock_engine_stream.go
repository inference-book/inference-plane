package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/inference-book/inference-plane/internal/backends"
)

// streamChatCompletion writes an OpenAI-compatible SSE stream for a
// completion the backend has already produced.
//
// It exists so the time-to-first-token path is exercisable without a GPU.
// Everything upstream of it (the router, `iplane load session --stream`,
// the TTFT percentiles) can now be driven end to end against the mock
// engine, which matters because the alternative was for a paid A/B run to
// be the first thing that ever exercised the measurement.
//
// The frame sequence deliberately mirrors what real engines send, including
// the opening role-only delta that carries no text. That frame is the one a
// TTFT measurement must NOT stop the clock on, so emitting it here means the
// GPU-free harness exercises the exact case the parser has to get right
// rather than a friendlier one.
//
// What it does not model is prefill. The backend has already generated the
// whole completion by the time this runs, so the first token arrives after
// the mock's configured latency and the rest follow after whatever gap
// tokenGap sets. Real TTFT separates queueing and prefill from decode; this
// only proves the plumbing.
//
// tokenGap is the pause between content frames. Zero emits them
// back-to-back, which is what every caller did before inter-token latency
// was a thing anyone measured, and which reports an inter-token latency of
// approximately nothing. Setting it is what makes decode speed visible
// without a GPU: the whole point of separating time-to-first-token from the
// gaps after it is that a growing batch stretches the second while leaving
// the first roughly alone, and a mock that emits its whole reply in one
// burst cannot show that.
// chatShape selects which of the two OpenAI streaming shapes to emit.
// /v1/chat/completions sends choices[].delta.content inside a
// "chat.completion.chunk"; /v1/completions sends choices[].text inside a
// "text_completion". The mock answered both endpoints in the chat shape for
// as long as it existed, which made a loadgen that only parsed chat deltas
// look complete while it was discarding every sample from the completions
// half of real traffic (#437). A mock that is wrong in the same direction as
// the code under test cannot fail.
func streamChatCompletion(w http.ResponseWriter, resp *backends.GenerateResponse, tokenGap time.Duration, chatShape bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flushing means no streaming; everything would buffer to the
		// end and the client would measure a first token that arrived with
		// the last one. Better to fail loudly than to emit a plausible
		// number that is silently wrong.
		http.Error(w, "streaming unsupported by this writer", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	send := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	pace := func() {
		if tokenGap > 0 {
			time.Sleep(tokenGap)
		}
	}

	type delta struct {
		Role    string `json:"role,omitempty"`
		Content string `json:"content,omitempty"`
	}
	type choice struct {
		Index        int    `json:"index"`
		Delta        *delta `json:"delta,omitempty"`
		Text         string `json:"text,omitempty"`
		FinishReason string `json:"finish_reason,omitempty"`
	}
	object := "text_completion"
	if chatShape {
		object = "chat.completion.chunk"
	}
	frame := func(c choice, usage *backends.Usage) map[string]any {
		f := map[string]any{
			"id":      resp.ID,
			"object":  object,
			"created": resp.Created,
			"model":   resp.Model,
			"choices": []choice{c},
		}
		if usage != nil {
			f["usage"] = usage
		}
		return f
	}
	// content builds the one frame that actually carries text, in whichever
	// shape this endpoint speaks.
	content := func(chunk string) choice {
		if chatShape {
			return choice{Delta: &delta{Content: chunk}}
		}
		return choice{Text: chunk}
	}

	// The role-only opener is a chat-protocol artifact and has no analogue
	// on /v1/completions. Emitting it only for chat keeps the mock honest
	// about the case a TTFT parser must skip.
	if chatShape {
		send(frame(choice{Delta: &delta{Role: "assistant"}}, nil))
	}

	for i, chunk := range chunkContent(completionText(resp)) {
		// After the first, not before it. The gap before the first
		// content frame is time-to-first-token and the backend has
		// already slept it; pausing here as well would charge prefill
		// twice.
		if i > 0 {
			pace()
		}
		send(frame(content(chunk), nil))
	}

	send(frame(choice{FinishReason: "stop"}, &resp.Usage))
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// completionText pulls the assistant text out of whichever shape the
// backend used: chat responses carry Message, completion responses Text.
func completionText(resp *backends.GenerateResponse) string {
	if len(resp.Choices) == 0 {
		return ""
	}
	c := resp.Choices[0]
	if c.Message != nil {
		return c.Message.Content
	}
	return c.Text
}

// chunkContent splits text into whitespace-delimited pieces, keeping the
// separator on each piece so concatenating them reproduces the original.
// Real engines emit roughly one token per frame; words are close enough to
// exercise a multi-frame stream without pulling in a tokenizer.
func chunkContent(text string) []string {
	if text == "" {
		return nil
	}
	words := strings.SplitAfter(text, " ")
	out := make([]string, 0, len(words))
	for _, w := range words {
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}
