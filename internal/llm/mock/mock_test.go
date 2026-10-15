package mock

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestStreamConcatenatesReply(t *testing.T) {
	m := New()
	sr, err := m.Stream(context.Background(), []*schema.Message{
		schema.UserMessage("hello"),
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer sr.Close()

	var b strings.Builder
	for {
		chunk, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		b.WriteString(chunk.Content)
	}

	if got := b.String(); got != "mock reply to: hello" {
		t.Fatalf("unexpected stream result: %q", got)
	}
}

func TestGenerate(t *testing.T) {
	m := New()
	msg, err := m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("hi"),
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if msg.Content != "mock reply to: hi" {
		t.Fatalf("unexpected generate result: %q", msg.Content)
	}
	if msg.Role != schema.Assistant {
		t.Fatalf("unexpected role: %q", msg.Role)
	}
}
