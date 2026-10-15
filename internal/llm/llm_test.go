package llm

import (
	"context"
	"testing"
)

func TestNewMock(t *testing.T) {
	m, err := New(context.Background(), Config{Provider: "mock"})
	if err != nil {
		t.Fatalf("New mock: %v", err)
	}
	if m == nil {
		t.Fatal("mock model is nil")
	}
}

func TestNewOpenAI(t *testing.T) {
	// NewChatModel 仅构造客户端、不发起网络请求，因此可离线验证。
	m, err := New(context.Background(), Config{
		Provider:  "openai",
		APIKey:    "test-key",
		ModelName: "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("New openai: %v", err)
	}
	if m == nil {
		t.Fatal("openai model is nil")
	}
}

func TestNewUnsupported(t *testing.T) {
	if _, err := New(context.Background(), Config{Provider: "unknown"}); err == nil {
		t.Fatal("want error for unsupported provider")
	}
}
