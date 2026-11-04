package tools

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const v2HarnessFailingDispatchEnv = "OPENMESSAGE_TEST_V2_HARNESS_FAILING_DISPATCH"

// A tool test whose dispatcher fails must fail promptly and say why. The send
// tools wait on the caller's context with no deadline, so when the dispatcher
// stopped on 2026-11-03 a test like this could only hang until go test's
// timeout. The child run here fails every lease; it must end within seconds,
// with the harness naming the failed pass and the tool call returning.
func TestV2ToolHarnessFailsPromptlyWhenDispatchFails(t *testing.T) {
	if os.Getenv(v2HarnessFailingDispatchEnv) == "1" {
		sendThroughFailingDispatch(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestV2ToolHarnessFailsPromptlyWhenDispatchFails$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), v2HarnessFailingDispatchEnv+"=1")
	output, err := child.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the child test hung instead of failing:\n%s", output)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("the child test did not fail (%v):\n%s", err, output)
	}
	for _, want := range []string{
		"MessageService dispatch pass failed",
		"injected lease failure",
		"send_to_conversation returned",
	} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("child output lacks %q:\n%s", want, output)
		}
	}
}

// sendThroughFailingDispatch is the child: a send whose every lease fails.
func sendThroughFailingDispatch(t *testing.T) {
	harness := newV2ToolHarness(t)
	raw, err := sql.Open("sqlite", "file:"+harness.v2Path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open a second connection to the v2 store: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`
		CREATE TRIGGER fail_lease
		BEFORE UPDATE OF state ON outbox
		WHEN NEW.state = 'dispatching'
		BEGIN
			SELECT RAISE(ABORT, 'injected lease failure');
		END
	`); err != nil {
		t.Fatalf("create lease failure trigger: %v", err)
	}
	result, err := sendToConversationHandler(harness.app, &harness.deps)(harness.ctx, v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "never leaves the outbox",
		"idempotency_key": "harness-failing-dispatch",
	}))
	t.Logf("send_to_conversation returned: error %v, result %+v", err, result)
}
