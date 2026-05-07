// Package backend defines the abstraction every inference engine must satisfy
// and the OpenAI-shaped request and response types that flow through the
// control plane.
//
// vLLM is the engine implementation in v0.1. SGLang and TensorRT-LLM
// implementations land in later chapters; both implement the same Backend
// interface, so the control plane code does not change when we swap.
package backend

// GenerateRequest is the OpenAI-compatible completion request shape.
// Field names and JSON tags match the OpenAI v1 API so existing client
// SDKs work unchanged against the control plane.
type GenerateRequest struct {
	Model       string   `json:"model"`
	Prompt      string   `json:"prompt,omitempty"`
	Messages    []ChatMessage `json:"messages,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature float64  `json:"temperature,omitempty"`
	TopP        float64  `json:"top_p,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Stream      bool     `json:"stream,omitempty"`
}

// ChatMessage is one entry in a chat-style request's message list.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// GenerateResponse is the OpenAI-compatible completion response shape.
type GenerateResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is one candidate output in the response.
type Choice struct {
	Index        int    `json:"index"`
	Text         string `json:"text,omitempty"`
	Message      *ChatMessage `json:"message,omitempty"`
	FinishReason string `json:"finish_reason"`
}

// Usage captures token accounting for the request, used downstream by
// throughput metrics and (in Chapter 11) per-tenant billing.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Status is the health-check verdict. Three states (not just up/down) so
// degraded backends can be drained or deprioritized without alerting.
type Status string

const (
	StatusHealthy   Status = "healthy"
	StatusDegraded  Status = "degraded"
	StatusUnhealthy Status = "unhealthy"
)

// CheckResult carries one health probe's outcome plus the metadata an
// operator needs to decide what to do with it.
type CheckResult struct {
	Name      string `json:"name"`
	Status    Status `json:"status"`
	Message   string `json:"message,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
}
