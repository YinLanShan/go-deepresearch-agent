package service

import (
	"context"
	"strings"
	"testing"

	"github.com/YinLanShan/go-deepresearch-agent.git/internal/llm/mock"
)

func TestChatServiceStream(t *testing.T) {
	svc := NewChatService(mock.New())

	var b strings.Builder
	err := svc.Stream(context.Background(), "hello", func(delta string) error {
		b.WriteString(delta)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := b.String(); got != "mock reply to: hello" {
		t.Fatalf("unexpected result: %q", got)
	}
}

func TestChatServiceStreamAbortOnCallbackError(t *testing.T) {
	svc := NewChatService(mock.New())

	callCount := 0
	err := svc.Stream(context.Background(), "hello", func(delta string) error {
		callCount++
		return &sentinelError{}
	})
	if err == nil {
		t.Fatal("want error from callback")
	}
	if callCount != 1 {
		t.Fatalf("want callback called once, got %d", callCount)
	}
}

type sentinelError struct{}

func (e *sentinelError) Error() string { return "sentinel" }
