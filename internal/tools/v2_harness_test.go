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

	"github.com/maxghenis/openmessage/internal/v2wire"
)

const v2HarnessFailingDispatchEnv = "OPENMESSAGE_TEST_V2_HARNESS_FAILING_DISPATCH"

// v2HarnessDispatchFaults are the ways the child's dispatch fails: a trigger
// on the outbox, and what the failing child must print.
var v2HarnessDispatchFaults = []struct {
	name, trigger string
	// strand has the child leave an expired lease in the outbox before it
	// sends, for the trigger to fail its recovery.
	strand bool
	want   []string
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
	{
		// The recovery of an expired lease fails on its row, so the row stays
		// dispatching and the pass goes on to the send; the harness fails the
		// test for the recovery.
		name: "recovery",
		trigger: `
			CREATE TRIGGER fail_recovery
			BEFORE UPDATE OF state ON outbox
			WHEN OLD.state = 'dispatching' AND NEW.state IN ('not_dispatched', 'uncertain')
			BEGIN
				SELECT RAISE(ABORT, 'injected recovery failure');
			END`,
		strand: true,
		want:   []string{"MessageService could not recover expired lease of outbox item", "injected recovery failure"},
	},
}

// A tool test whose dispatcher fails must fail promptly and say why. The send
// tools wait on the caller's context with no deadline, so when the dispatcher
// stopped on 2026-11-03 a test like this could only hang until go test's
// timeout. Each child run here fails every lease of its send, on the row or
// on the store, or fails the recovery of a lease left expired; it must end
// within seconds, with the harness naming the failure and the tool call
// returning.
func TestV2ToolHarnessFailsPromptlyWhenDispatchFails(t *testing.T) {
	if name := os.Getenv(v2HarnessFailingDispatchEnv); name != "" {
		for _, fault := range v2HarnessDispatchFaults {
			if fault.name == name {
				sendThroughFailingDispatch(t, fault.trigger, fault.strand)
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

// sendThroughFailingDispatch is the child: a send through a dispatcher whose
// writes the trigger fails.
func sendThroughFailingDispatch(t *testing.T, trigger string, strand bool) {
	harness := newV2ToolHarness(t)
	raw, err := sql.Open("sqlite", "file:"+harness.v2Path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open a second connection to the v2 store: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(trigger); err != nil {
		t.Fatalf("create dispatch failure trigger: %v", err)
	}
	if strand {
		strandExpiredLease(t, harness, raw)
	}
	result, err := sendToConversationHandler(harness.app, &harness.deps)(harness.ctx, v2ToolCall(map[string]any{
		"conversation_id": v2ToolConversationID,
		"message":         "never leaves the outbox",
		"idempotency_key": "harness-failing-dispatch",
	}))
	t.Logf("send_to_conversation returned: error %v, result %+v", err, result)
}

// strandExpiredLease leaves a text in the outbox the way a dispatcher that
// stopped mid-send would: dispatching, under a lease that has expired. The
// text is due in an hour, so Run never leases it first, and the trigger is
// already in place, so no pass can recover it before the send.
func strandExpiredLease(t *testing.T, harness *v2ToolHarness, raw *sql.DB) {
	t.Helper()
	const key = "harness-stranded-lease"
	if _, err := harness.deps.submitText(harness.ctx, harness.app, v2wire.TextInput{
		ConversationID: v2ToolConversationID,
		Body:           "stranded by a stopped dispatcher",
		IdempotencyKey: key,
		NotBefore:      time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("queue the text to strand: %v", err)
	}
	result, err := raw.Exec(`
		UPDATE outbox
		SET state = 'dispatching', lease_owner = 'stopped-dispatcher', lease_token = 'stranded',
			lease_expires_at_ms = ?, attempt_count = attempt_count + 1
		WHERE idempotency_key = ? AND state = 'queued'`,
		time.Now().Add(-time.Second).UnixMilli(), key)
	if err != nil {
		t.Fatalf("strand the text under an expired lease: %v", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("stranding changed %d rows (%v), want 1", rows, err)
	}
}
