package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/bridgeadapters/scripted"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2wire"
)

// TestR5RollbackLegacyPrimarySendsIntoMigratedThreads runs the rollback shape
// end to end: the real `openmessage migrate` builds the v2 store, then the
// daemon runs legacy-primary with v2 send on (OPENMESSAGES_V2_PRIMARY unset,
// OPENMESSAGES_V2_SEND=1), so the legacy mirror carries each send into the
// migrated store. For a reply to a migrated message on each platform it
// checks that the send reaches the scripted transport addressed by the legacy
// thread and quoting the migrated remote ID, that the v2 thread gains only the
// sent message, and that the legacy visibility projector shows the send in
// the legacy thread rather than under the v2 hash.
func TestR5RollbackLegacyPrimarySendsIntoMigratedThreads(t *testing.T) {
	fixture := buildR5LegacyFixture(t)
	now := time.Date(2026, 7, 17, 18, 0, 0, 0, time.UTC)
	targetDir := filepath.Join(fixture.DataDir, "v2")
	runR5Migrate(t, fixture.DataDir, targetDir, false, now)

	t.Setenv("OPENMESSAGES_DATA_DIR", fixture.DataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	t.Setenv("OPENMESSAGES_V2_PRIMARY", "")
	t.Setenv("OPENMESSAGES_V2_SEND", "1")
	t.Setenv("OPENMESSAGES_V2_INGEST", "")

	stack, err := newV2Stack(v2StackDeps{DataDir: fixture.DataDir, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("open migrated v2 stack: %v", err)
	}
	t.Cleanup(func() { _ = stack.Store.Close() })
	adapters := map[string]*scripted.Adapter{
		r5GoogleAccountID:   scripted.New(r5GoogleAccountID, bridge.PlatformGoogle),
		r5WhatsAppAccountID: scripted.New(r5WhatsAppAccountID, bridge.PlatformWhatsApp),
		r5SignalAccountID:   scripted.New(r5SignalAccountID, bridge.PlatformSignal),
	}
	for _, adapter := range adapters {
		if err := stack.RegisterAdapter(adapter); err != nil {
			t.Fatalf("register scripted %s adapter: %v", adapter.Platform(), err)
		}
	}
	legacy, err := db.New(fixture.StorePath)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	stopStack := stack.Start(ctx, legacy, nil, false)
	t.Cleanup(func() {
		stopStack()
		cancel()
	})
	deps := v2wire.Deps{Legacy: legacy, V2: stack.Store, Service: stack.Service, Registry: stack.Registry}
	messages, err := sqlite.NewMessageRepository(stack.Store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	conversationByLegacyID := map[string]r5FixtureConversation{}
	for _, conversation := range fixture.Conversations {
		conversationByLegacyID[conversation.LegacyID] = conversation
	}

	for _, test := range []struct {
		name            string
		accountID       string
		legacyThread    string
		replyTo         string
		wantQuotedID    string
		result          string
		wantLegacyID    string
		wantLegacyReply string
	}{
		{
			name: "google", accountID: r5GoogleAccountID, legacyThread: r5GoogleConversation,
			replyTo: "google-r5-outgoing", wantQuotedID: "google-r5-outgoing",
			result: "gm-rollback-permanent", wantLegacyReply: "google-r5-outgoing",
		},
		{
			name: "whatsapp", accountID: r5WhatsAppAccountID, legacyThread: r5WhatsAppConversation,
			replyTo: "whatsapp:wa-r5-lid", wantQuotedID: "whatsapp:wa-r5-explicit-source",
			result: "wa-rollback-sent", wantLegacyID: "whatsapp:wa-rollback-sent",
			wantLegacyReply: "whatsapp:wa-r5-explicit-source",
		},
		{
			name: "signal", accountID: r5SignalAccountID, legacyThread: r5SignalConversation,
			replyTo: "signal:1700000001000", wantQuotedID: "1700000001000",
			result: "1752800000000", wantLegacyID: "signal:1752800000000",
			wantLegacyReply: "signal:1700000001000",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			v2ID := conversationByLegacyID[test.legacyThread].V2ID()
			messagesBefore := r5ConversationMessageCount(t, messages, v2ID)
			body := "rollback reply on " + test.name
			adapter := adapters[test.accountID]
			result := bridge.SendResult{RemoteMessageID: test.result, AcceptedAt: time.Now()}
			if test.accountID == r5GoogleAccountID {
				result.EchoExpected = true
			}
			adapter.EnqueueTextResult(result)

			submission, err := v2wire.SubmitText(context.Background(), deps, v2wire.TextInput{
				ConversationID: test.legacyThread,
				Body:           body,
				ReplyToID:      test.replyTo,
				IdempotencyKey: "rollback-" + test.name,
			})
			if err != nil {
				t.Fatalf("SubmitText(%q): %v", test.legacyThread, err)
			}
			waitR5(t, test.name+" send confirmation", func() bool {
				delivery, err := stack.Service.Get(context.Background(), submission.OutboxID)
				return err == nil && delivery.State == messaging.OutboxConfirmed
			})
			requests := adapter.TextRequests()
			if len(requests) != 1 || requests[0].Conversation.RemoteID != test.legacyThread ||
				requests[0].ReplyTo == nil || requests[0].ReplyTo.RemoteID != test.wantQuotedID ||
				requests[0].Body != body {
				t.Fatalf("scripted %s text requests = %+v, want one to %q quoting %q", test.name, requests, test.legacyThread, test.wantQuotedID)
			}
			queued, err := messages.GetMessage(context.Background(), submission.LocalMessageID)
			if err != nil || queued.ConversationID != v2ID {
				t.Fatalf("queued v2 message = %+v, %v; want it in the migrated thread %q", queued, err, v2ID)
			}
			if got := r5ConversationMessageCount(t, messages, v2ID); got != messagesBefore+1 {
				t.Fatalf("v2 messages in the migrated thread = %d, want %d plus the sent message", got, messagesBefore)
			}

			waitR5(t, test.name+" legacy projection", func() bool {
				rows, err := legacy.GetMessagesByConversation(test.legacyThread, 50)
				if err != nil {
					return false
				}
				for _, row := range rows {
					if row.Body == body {
						return (test.wantLegacyID == "" || row.MessageID == test.wantLegacyID) &&
							row.ReplyToID == test.wantLegacyReply && row.IsFromMe
					}
				}
				return false
			})
			if stray, err := legacy.GetMessagesByConversation(v2ID, 10); err != nil || len(stray) != 0 {
				t.Fatalf("legacy messages under the v2 hash %q = %+v, %v; want none", v2ID, stray, err)
			}
		})
	}

	t.Run("google reply to a message migrated under its source id", func(t *testing.T) {
		v2ID := conversationByLegacyID[r5GoogleConversation].V2ID()
		before := r5ConversationMessageCount(t, messages, v2ID)
		_, err := v2wire.SubmitText(context.Background(), deps, v2wire.TextInput{
			ConversationID: r5GoogleConversation,
			Body:           "would duplicate the migrated target",
			ReplyToID:      "google-r5-explicit-source",
			IdempotencyKey: "rollback-google-source-id",
		})
		if !errors.Is(err, v2wire.ErrReplyTargetUnavailable) || !strings.Contains(err.Error(), "google-remote-r5-explicit") {
			t.Fatalf("SubmitText() error = %v, want ErrReplyTargetUnavailable naming the migrated remote id", err)
		}
		if got := r5ConversationMessageCount(t, messages, v2ID); got != before {
			t.Fatalf("v2 messages = %d, want unchanged %d", got, before)
		}
	})

	t.Run("mark read", func(t *testing.T) {
		v2ID := conversationByLegacyID[r5SignalConversation].V2ID()
		device, err := stack.Store.GetLocalInstallationDevice(context.Background(), r5SignalAccountID)
		if err != nil {
			t.Fatalf("GetLocalInstallationDevice(): %v", err)
		}
		// The migration already wrote this thread's cursor; read cursors only
		// move forward, so an older mark-read leaves it alone.
		migrated, err := stack.Store.GetReadCursor(device.DeviceID, v2ID)
		if err != nil {
			t.Fatalf("migrated cursor: %v", err)
		}
		readAtMS := migrated.LastReadAtMS + 60_000
		for _, atMS := range []int64{migrated.LastReadAtMS - 1, readAtMS} {
			if err := v2wire.MirrorReadCursor(context.Background(), legacy, stack.Store, r5SignalConversation, atMS); err != nil {
				t.Fatalf("MirrorReadCursor(%d): %v", atMS, err)
			}
			cursor, err := stack.Store.GetReadCursor(device.DeviceID, v2ID)
			if want := max(atMS, migrated.LastReadAtMS); err != nil || cursor.LastReadAtMS != want {
				t.Fatalf("cursor on the migrated device and thread after a read at %d = %+v, %v; want read at %d", atMS, cursor, err, want)
			}
		}
		devices, err := stack.Store.ListDevices(r5SignalAccountID)
		if err != nil || len(devices) != 1 {
			t.Fatalf("Signal devices = %+v, %v; want only the migrated one", devices, err)
		}
	})
}

func r5ConversationMessageCount(t *testing.T, messages *sqlite.MessageRepository, conversationID string) int {
	t.Helper()
	rows, err := messages.ListMessagesByConversation(context.Background(), conversationID, 0, "", 1_000)
	if err != nil {
		t.Fatalf("ListMessagesByConversation(%q): %v", conversationID, err)
	}
	return len(rows)
}
