package messaging

import (
	"context"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// A row whose lease always fails must not stop the sends queued after it.
// LeaseDue gave up its whole batch at the first failed lease, and every pass
// met that row first again, so after PR #274 Run kept retrying a pass that
// could never lease anything and no later text, media, reaction or read
// receipt left the outbox.
func TestDispatchPassSendsPastARowWhoseLeaseAlwaysFails(t *testing.T) {
	ctx := context.Background()
	clock := newManualClock(messagingTestTime)
	store, raw := openReconcileTestStore(t, clock.Now())
	sender := &scriptedTextSender{steps: []sendStep{
		{result: bridge.SendResult{RemoteMessageID: "remote-second"}},
		{result: bridge.SendResult{RemoteMessageID: "remote-third"}},
	}}
	registry := newScriptedRegistry("lease-failure", sender)
	registry.setAvailable(true)
	service := newMessagingTestService(t, store, registry, clock)

	var queued []Submission
	for _, key := range []string{"key-head", "key-second", "key-third"} {
		queued = append(queued, mustSendText(t, service, SendTextCommand{
			CommonCommand: testCommonCommand(key),
			Body:          "queued as " + key,
		}))
		clock.Advance(time.Millisecond)
	}
	head := queued[0]
	if _, err := raw.ExecContext(ctx, `
		CREATE TRIGGER fail_head_lease
		BEFORE UPDATE OF state ON outbox
		WHEN NEW.state = 'dispatching' AND OLD.outbox_id = '`+head.OutboxID+`'
		BEGIN
			SELECT RAISE(ABORT, 'injected lease failure');
		END
	`); err != nil {
		t.Fatalf("create lease failure trigger: %v", err)
	}

	for pass := range 3 {
		clock.Advance(time.Second)
		if err := service.dispatchPass(ctx); err != nil {
			t.Errorf("pass %d: %v", pass, err)
		}
	}
	for _, later := range queued[1:] {
		if got := mustOutboxItem(t, service, later.OutboxID); got.State != sqlite.OutboxConfirmed {
			t.Errorf("text queued after the row whose lease fails = %+v, want confirmed", got)
		}
	}
	if got := sender.requestCount(); got != 2 {
		t.Errorf("transport requests = %d, want 2 (the texts behind the failing row)", got)
	}
	if got := mustOutboxItem(t, service, head.OutboxID); got.State != sqlite.OutboxNotDispatched || got.AttemptCount != 0 {
		t.Errorf("row whose lease fails = %+v, want not_dispatched with no transport attempt", got)
	}
}
