package backends

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMock_Generate_Completion(t *testing.T) {
	m := NewMock("test", WithLatency(0, 5*time.Millisecond))
	resp, err := m.Generate(context.Background(), GenerateRequest{
		Model:     "mock-model",
		Prompt:    "Say something",
		MaxTokens: 50,
	})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if resp.Object != "text_completion" {
		t.Errorf("Object = %q, want text_completion", resp.Object)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Text == "" {
		t.Error("expected non-empty Text")
	}
	if resp.Choices[0].Message != nil {
		t.Error("plain completion should not populate Message")
	}
	if resp.Usage.CompletionTokens > 50 {
		t.Errorf("completion tokens %d exceed MaxTokens=50", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != resp.Usage.PromptTokens+resp.Usage.CompletionTokens {
		t.Error("TotalTokens != PromptTokens + CompletionTokens")
	}
	if !strings.HasPrefix(resp.ID, "mock-") {
		t.Errorf("ID %q should start with 'mock-'", resp.ID)
	}
}

func TestMock_Generate_Chat(t *testing.T) {
	m := NewMock("test", WithLatency(0, 5*time.Millisecond))
	resp, err := m.Generate(context.Background(), GenerateRequest{
		Model: "mock-model",
		Messages: []ChatMessage{
			{Role: "user", Content: "hi"},
		},
		MaxTokens: 30,
	})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("Object = %q, want chat.completion", resp.Object)
	}
	if resp.Choices[0].Message == nil {
		t.Fatal("chat response should populate Message")
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q, want assistant", resp.Choices[0].Message.Role)
	}
	if resp.Choices[0].Text != "" {
		t.Error("chat response should not populate Text")
	}
}

func TestMock_FinishReasonLength(t *testing.T) {
	m := NewMock("test",
		WithLatency(0, 5*time.Millisecond),
		WithOutputTokens(100, 200), // larger than MaxTokens
	)
	resp, err := m.Generate(context.Background(), GenerateRequest{
		Model:     "mock-model",
		Prompt:    "x",
		MaxTokens: 10,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Choices[0].FinishReason != "length" {
		t.Errorf("FinishReason = %q, want 'length' when MaxTokens hit", resp.Choices[0].FinishReason)
	}
	if resp.Usage.CompletionTokens != 10 {
		t.Errorf("CompletionTokens = %d, want 10 (clamped to MaxTokens)", resp.Usage.CompletionTokens)
	}
}

func TestMock_ContextCancellation(t *testing.T) {
	m := NewMock("test",
		WithLatency(500*time.Millisecond, 500*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := m.Generate(ctx, GenerateRequest{Model: "x", Prompt: "y"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected context.Canceled error, got nil")
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("expected cancel within ~20ms, took %v", elapsed)
	}
}

func TestMock_Health_Default(t *testing.T) {
	m := NewMock("test")
	if err := m.Health(context.Background()); err != nil {
		t.Errorf("default mock should be healthy, got: %v", err)
	}
}

func TestMock_Health_FailureInjection(t *testing.T) {
	// 100% failure rate means every Health() call should fail.
	m := NewMock("test", WithHealthFailRate(1.0))
	if err := m.Health(context.Background()); err == nil {
		t.Error("expected error with 100% fail rate")
	}
}

// Compile-time check: MockBackend implements Backend.
var _ Backend = (*MockBackend)(nil)
