package sqlite

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"
)

// A send still waiting for its echo holds its transport request ID and this
// machine's time, so a phone frame stamped on that millisecond is not its
// re-delivery: not while it waits in the queue, nor once the transport has
// answered without a remote ID. When the echo binds it to the remote ID the phone
// served, a frame restating it under another ID is a re-delivery again.
func TestFindMessageContentDuplicateSkipsASendWaitingForItsEcho(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, outbox := openOutboxTestRepository(t, clock.Now)
	seedEchoMergeGraph(t, store)
	messages, err := NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	item := outboxTestItem("send")
	mustEnqueueOutgoingOutbox(t, outbox, item, "on my way")
	sender := "identity-a"
	duplicate := func(stage string) (Message, bool) {
		t.Helper()
		message, found, err := messages.FindMessageContentDuplicate(ctx, item.AccountID, item.ConversationID,
			"remote-other-echo", MessageDirectionOutgoing, &sender, outboxTestTimeMS, "on my way")
		if err != nil {
			t.Fatalf("FindMessageContentDuplicate(%s): %v", stage, err)
		}
		return message, found
	}
	if message, found := duplicate("queued"); found {
		t.Fatalf("queued send matched: %+v", message)
	}

	token := mustLeaseToken(t, mustLeaseOne(t, outbox, LeaseRequest{
		Owner: "worker", Now: clock.Now(), Duration: time.Minute, Limit: 1,
	}))
	if err := outbox.MarkTransportCalled(ctx, Attempt{OutboxID: item.OutboxID, LeaseToken: token}); err != nil {
		t.Fatalf("MarkTransportCalled(): %v", err)
	}
	if err := outbox.Confirm(ctx, Confirmation{
		OutboxID: item.OutboxID, LeaseToken: token, ResultRemoteID: item.TransportRequestID,
	}); err != nil {
		t.Fatalf("Confirm(provisional): %v", err)
	}
	if message, found := duplicate("confirmed without a remote ID"); found {
		t.Fatalf("send confirmed without a remote ID matched: %+v", message)
	}

	outcome, err := outbox.ReconcileConfirm(ctx, ReconcileRequest{
		AccountID: item.AccountID, TransportRequestID: item.TransportRequestID, ResultRemoteID: "remote-served",
	})
	if err != nil || outcome != ReconcileOutcomeEnriched {
		t.Fatalf("ReconcileConfirm() = %q, %v; want enriched", outcome, err)
	}
	if message, found := duplicate("bound"); !found || message.MessageID != item.LocalMessageID {
		t.Fatalf("bound send: duplicate = %+v, %v; want the send's row %q", message, found, item.LocalMessageID)
	}
}

const (
	contentDuplicatePhoneRow = iota
	contentDuplicateWaitingSend
	contentDuplicateBoundSend
)

// contentDuplicateScenario is up to six rows in two conversations whose
// content clashes often (two bodies, three milliseconds, two senders, both
// directions). Some rows are sends, still waiting for their echo or bound to
// one; sends are outgoing, as only a local send is.
type contentDuplicateScenario struct {
	rows []contentDuplicateRow
}

type contentDuplicateRow struct {
	conversation int // 0 or 1; the probes ask about conversation 0
	kind         int
	incoming     bool
	sender       bool  // identity-a, or no sender
	atMS         int64 // occurrence, after outboxTestTimeMS
	body         string
	createdMS    int64 // after outboxTestTimeMS
}

func (contentDuplicateScenario) Generate(r *rand.Rand, _ int) reflect.Value {
	var scenario contentDuplicateScenario
	for range 1 + r.Intn(6) {
		row := contentDuplicateRow{
			conversation: r.Intn(4) / 3,
			kind:         r.Intn(3),
			incoming:     r.Intn(2) == 0,
			sender:       r.Intn(2) == 0,
			atMS:         int64(r.Intn(3)),
			body:         []string{"a", "b"}[r.Intn(2)],
			createdMS:    int64(r.Intn(4)),
		}
		if row.kind != contentDuplicatePhoneRow {
			row.incoming = false
		}
		scenario.rows = append(scenario.rows, row)
	}
	return reflect.ValueOf(scenario)
}

// TestFindMessageContentDuplicateMatchesModelProperty checks the query
// against its contract, written out in Go: of the rows in the probe's
// conversation with the probe's direction, sender, millisecond and body under
// another remote ID, skipping sends still waiting for their echo, the oldest
// (created_at_ms, then message ID). The test probes each row's content
// under a fresh remote ID and under the row's own.
func TestFindMessageContentDuplicateMatchesModelProperty(t *testing.T) {
	ctx := context.Background()
	clock := newOutboxTestClock(outboxTestTimeMS)
	store, outbox := openOutboxTestRepository(t, clock.Now)
	seedMessageIdentity(t, store, "identity-a", "account-a")
	messages, err := NewMessageRepository(store, clock.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	coverage := map[string]int{}
	scenarioIndex := 0
	property := func(scenario contentDuplicateScenario) bool {
		scenarioIndex++
		conversations := [2]string{}
		for c := range conversations {
			conversations[c] = fmt.Sprintf("conversation-%04d-%d", scenarioIndex, c)
			seedMessageConversation(t, store, conversations[c], "account-a")
		}
		messageIDs := make([]string, len(scenario.rows))
		remoteIDs := make([]string, len(scenario.rows))
		for i, row := range scenario.rows {
			id := fmt.Sprintf("%04d-%d", scenarioIndex, i)
			messageIDs[i] = "message-" + id
			var sender *string
			if row.sender {
				identity := "identity-a"
				sender = &identity
			}
			if row.kind == contentDuplicatePhoneRow {
				remoteIDs[i] = "remote-" + id
				direction := MessageDirectionOutgoing
				if row.incoming {
					direction = MessageDirectionIncoming
				}
				mustExec(t, store.db, `
					INSERT INTO messages (
						message_id, conversation_id, account_id, remote_message_id,
						sender_identity_id, direction, body, reply_to_remote_id, state,
						occurred_at_ms, created_at_ms, updated_at_ms
					) VALUES (?, ?, 'account-a', ?, ?, ?, ?, NULL, 'active', ?, ?, ?)
				`, messageIDs[i], conversations[row.conversation], remoteIDs[i], sender, direction, row.body,
					outboxTestTimeMS+row.atMS, outboxTestTimeMS+row.createdMS, outboxTestTimeMS+row.createdMS)
				continue
			}
			item := outboxTestItem(id)
			item.ConversationID = conversations[row.conversation]
			clock.Set(outboxTestTimeMS + row.createdMS)
			if _, _, err := outbox.EnqueueOutgoingMessage(ctx, item, Message{
				MessageID:        item.LocalMessageID,
				ConversationID:   item.ConversationID,
				AccountID:        item.AccountID,
				RemoteMessageID:  item.TransportRequestID,
				SenderIdentityID: sender,
				Direction:        MessageDirectionOutgoing,
				Body:             row.body,
				State:            MessageStateActive,
				OccurredAtMS:     outboxTestTimeMS + row.atMS,
			}); err != nil {
				t.Fatalf("EnqueueOutgoingMessage(%s): %v", id, err)
			}
			remoteIDs[i] = item.TransportRequestID
			if row.kind == contentDuplicateBoundSend {
				// What a confirm with the echo's remote ID writes
				// (repointLocalMessage).
				remoteIDs[i] = "served-" + id
				mustExec(t, store.db, `UPDATE outbox SET state = 'confirmed', result_remote_id = ? WHERE outbox_id = ?`,
					remoteIDs[i], item.OutboxID)
				mustExec(t, store.db, `UPDATE messages SET remote_message_id = ? WHERE message_id = ?`,
					remoteIDs[i], item.LocalMessageID)
			}
		}

		older := func(a, b int) bool {
			if b < 0 || scenario.rows[a].createdMS != scenario.rows[b].createdMS {
				return b < 0 || scenario.rows[a].createdMS < scenario.rows[b].createdMS
			}
			return messageIDs[a] < messageIDs[b]
		}
		for i, probe := range scenario.rows {
			for _, probeRemote := range []string{"remote-fresh", remoteIDs[i]} {
				// want is the contract's answer; withWaiting is what the query
				// returned before it skipped sends waiting for their echo.
				want, withWaiting, matches := -1, -1, 0
				for j, row := range scenario.rows {
					if row.conversation != 0 || row.incoming != probe.incoming || row.sender != probe.sender ||
						row.atMS != probe.atMS || row.body != probe.body || remoteIDs[j] == probeRemote {
						continue
					}
					if older(j, withWaiting) {
						withWaiting = j
					}
					if row.kind == contentDuplicateWaitingSend {
						continue
					}
					matches++
					if older(j, want) {
						want = j
					}
				}
				direction := MessageDirectionOutgoing
				if probe.incoming {
					direction = MessageDirectionIncoming
				}
				var sender *string
				if probe.sender {
					identity := "identity-a"
					sender = &identity
				}
				got, found, err := messages.FindMessageContentDuplicate(ctx, "account-a", conversations[0],
					probeRemote, direction, sender, outboxTestTimeMS+probe.atMS, probe.body)
				if err != nil {
					t.Fatalf("FindMessageContentDuplicate(): %v", err)
				}
				gotID, wantID := "", ""
				if found {
					gotID = got.MessageID
				}
				if want >= 0 {
					wantID = messageIDs[want]
				}
				if gotID != wantID {
					t.Logf("probe of row %d under %q: got %q, want %q (rows %+v)", i, probeRemote, gotID, wantID, scenario.rows)
					return false
				}
				switch {
				case want >= 0 && scenario.rows[want].kind == contentDuplicateBoundSend:
					coverage["bound send found"]++
				case want >= 0:
					coverage["phone row found"]++
				}
				if matches > 1 {
					coverage["oldest of several"]++
				}
				if withWaiting != want {
					if want < 0 {
						coverage["only a waiting send matched"]++
					} else {
						coverage["an older waiting send skipped"]++
					}
				}
			}
		}
		return true
	}
	if err := quick.Check(property, repairPropertyConfig(t, 300, 100)); err != nil {
		t.Fatal(err)
	}
	t.Logf("coverage: %v", coverage)
	for _, name := range []string{
		"bound send found", "phone row found", "oldest of several",
		"only a waiting send matched", "an older waiting send skipped",
	} {
		if coverage[name] == 0 {
			t.Errorf("generator never produced %q; coverage = %v", name, coverage)
		}
	}
}
