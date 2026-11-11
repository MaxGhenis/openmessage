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

// v2HarnessDispatchFaults are the ways the child's dispatch fails: a trigger
// on the send's lease, and what the failing child must print.
var v2HarnessDispatchFaults = []struct {
	name, trigger string
	want          []string
}{
	{
		// The lease fails on the row, so the store sets the row aside and the
		// send returns as retrying; the harness fails the test for the lease.
		name: "lease",
		trigger: `
			CREATE TRIGGER fail_lease
			BEFORE UPDATE OF state ON outbox
			WHEN NEW.state = 'dispatching'
			BEGIN
				SELECT RAISE(ABORT, 'injected lease failure');
			END`,
		want: []string{"MessageService could not lease outbox item", "injected lease failure"},
	},
	{
		// The lease fails on the store (an integer overflow, not a
		// constraint), so the whole pass fails.
		name: "store",
		trigger: `
			CREATE TRIGGER fail_store
			BEFORE UPDATE OF state ON outbox
			WHEN NEW.state = 'dispatching'
			BEGIN
				SELECT abs(-9223372036854775807 - 1);
			END`,
		want: []string{"MessageService dispatch pass failed", "integer overflow"},
	},
}

// A tool test whose dispatcher fails must fail promptly and say why. The send
// tools wait on the caller's context with no deadline, so when the dispatcher
// stopped on 2026-11-03 a test like this could only hang until go test's
// timeout. Each child run here fails every lease of its send, on the row or
// on the store; it must end within seconds, with the harness naming the
// failure and the tool call returning.
func TestV2ToolHarnessFailsPromptlyWhenDispatchFails(t *testing.T) {
	if name := os.Getenv(v2HarnessFailingDispatchEnv); name != "" {
		for _, fault := range v2HarnessDispatchFaults {
			if fault.name == name {
				sendThroughFailingDispatch(t, fault.trigger)
				return
			}
		}
		t.Fatalf("%s = %q names no fault", v2HarnessFailingDispatchEnv, name)
	}
	for _, fault := range v2HarnessDispatchFaults {
		t.Run(fault.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0],
				"-test.run=^TestV2ToolHarnessFailsPromptlyWhenDispatchFails$", "-test.count=1", "-test.v")
			child.Env = append(os.Environ(), v2HarnessFailingDispatchEnv+"="+fault.name)
			output, err := child.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("the child test hung instead of failing:\n%s", output)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("the child test did not fail (%v):\n%s", err, output)
			}
			for _, want := range append(fault.want, "send_to_conversation returned") {
				if !strings.Contains(string(output), want) {
					t.Fatalf("child output lacks %q:\n%s", want, output)
				}
			}
		})
	}
}

// sendThroughFailingDispatch is the child: a send whose every lease the
// trigger fails.
func sendThroughFailingDispatch(t *testing.T, trigger string) {
	harness := newV2ToolHarness(t)
	raw, err := sql.Open("sqlite", "file:"+harness.v2Path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open a second connection to the v2 store: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(trigger); err != nil {
		t.Fatalf("create dispatch failure trigger: %v", err)
	}
	result, err := sendToConversationHandler(harness.app, &harness.deps)(harness.ctx, v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "never leaves the outbox",
		"idempotency_key": "harness-failing-dispatch",
	}))
	t.Logf("send_to_conversation returned: error %v, result %+v", err, result)
}
