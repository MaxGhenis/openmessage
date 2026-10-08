package tools

import (
	"strings"
	"testing"
)

func TestRejectSIMOnOutboxOnlyWhenRequested(t *testing.T) {
	if got := rejectSIMOnOutbox(map[string]any{"sim": " "}); got != nil {
		t.Fatalf("blank sim must not be rejected, got %+v", got)
	}
	got := rejectSIMOnOutbox(map[string]any{"sim": "2"})
	if got == nil || !got.IsError {
		t.Fatalf("explicit sim on the outbox must fail, got %+v", got)
	}
	if text := resultText(t, got); !strings.Contains(text, "not supported") {
		t.Fatalf("error text = %q", text)
	}
}
