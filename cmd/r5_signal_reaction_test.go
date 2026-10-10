package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/bridgeadapters/scripted"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// r5ReactingSignal gives the scripted Signal account, which has no reaction
// sender of its own, one that records each request and confirms it.
type r5ReactingSignal struct {
	*scripted.Adapter

	mu       sync.Mutex
	requests []bridge.ReactionRequest
}

func (a *r5ReactingSignal) SendReaction(_ context.Context, req bridge.ReactionRequest) (bridge.SendResult, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()
	return bridge.SendResult{}, nil
}

func (a *r5ReactingSignal) reactionRequests() []bridge.ReactionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

// TestR5SignalReactionsTargetTheSentTimestampOnV2Primary runs Signal reactions
// end to end on the quote-reply test's harness: a migrated v2-primary stack
// with a scripted Signal account. A reaction goes through the messaging
// service (no HTTP route submits v2 reactions; /api/react still calls the
// legacy bridge), the durable dispatcher and the bridge request, and the
// captured target then goes through the real Signal adapter conversion into
// signallive's argument builder. It covers a live-ingested incoming message
// (SHA-1 remote ID), this account's own send through the v2 outbox, and the
// migrated messages whose Signal identity v2 cannot vouch for, which must be
// refused: an incoming row under a decimal ID, an own "local:" row, and a
// scheduled send caught in "sending".
func TestR5SignalReactionsTargetTheSentTimestampOnV2Primary(t *testing.T) {
	fixture := buildR5LegacyFixture(t)
	now := time.Date(2026, 7, 17, 18, 0, 0, 0, time.UTC)
	runR5Migrate(t, fixture.DataDir, filepath.Join(fixture.DataDir, "v2"), false, now)

	t.Setenv("OPENMESSAGES_DATA_DIR", fixture.DataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	t.Setenv("OPENMESSAGES_V2_PRIMARY", "1")
	t.Setenv("OPENMESSAGES_V2_SEND", "")
	t.Setenv("OPENMESSAGES_V2_INGEST", "")

	stack, err := newV2Stack(v2StackDeps{DataDir: fixture.DataDir, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("open migrated v2 stack: %v", err)
	}
	t.Cleanup(func() { _ = stack.Store.Close() })
	signal := &r5ReactingSignal{Adapter: scripted.New(r5SignalAccountID, bridge.PlatformSignal)}
	if err := stack.RegisterAdapter(signal); err != nil {
		t.Fatalf("register scripted Signal adapter: %v", err)
	}
	inertLegacy, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create inert legacy store: %v", err)
	}
	t.Cleanup(func() { _ = inertLegacy.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	stopStack := stack.Start(ctx, inertLegacy, nil, true)
	t.Cleanup(func() {
		stopStack()
		cancel()
	})

	messages, err := sqlite.NewMessageRepository(stack.Store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	reads := v2read.New(stack.Store)
	signalConversation := v2keys.DeriveID("conversation", r5SignalAccountID, r5SignalConversation)

	// react submits one reaction, waits for the dispatcher to hand it to the
	// (scripted) transport, and returns the target the bridge request carried
	// with what the Signal transport makes of it: the signal-cli target
	// arguments, or the error it stops on before running signal-cli.
	react := func(key, targetMessageID string) (bridge.MessageRef, []string, error) {
		t.Helper()
		before := len(signal.reactionRequests())
		submission, err := stack.Service.SendReaction(context.Background(), messaging.SendReactionCommand{
			CommonCommand: messaging.CommonCommand{
				AccountID: r5SignalAccountID, ConversationID: signalConversation, IdempotencyKey: key,
			},
			TargetMessageID: targetMessageID,
			Emoji:           "👍",
		})
		if err != nil {
			t.Fatalf("SendReaction(%q): %v", targetMessageID, err)
		}
		waitR5(t, "reaction "+key, func() bool {
			delivery, getErr := stack.Service.Get(context.Background(), submission.OutboxID)
			return getErr == nil && delivery.State == messaging.OutboxConfirmed
		})
		requests := signal.reactionRequests()
		if len(requests) != before+1 {
			t.Fatalf("reaction %q reached the transport %d times, want once", key, len(requests)-before)
		}
		request := requests[before]
		if request.Conversation.RemoteID != r5SignalConversation {
			t.Fatalf("reaction %q conversation = %q, want %q", key, request.Conversation.RemoteID, r5SignalConversation)
		}
		args, err := signallive.ReactionTargetArgs(signaladapter.ReactionTarget(request.Target), r5SignalAccountAddress)
		return request.Target, args, err
	}
	targetArgs := func(author string, timestamp int64) []string {
		return []string{"-a", author, "-t", strconv.FormatInt(timestamp, 10)}
	}

	t.Run("reaction to a live-ingested incoming message", func(t *testing.T) {
		const body = "r5 live incoming to react to"
		timestamp := fixture.BaseMS + 50_000
		line := []byte(fmt.Sprintf(
			`{"account":%q,"envelope":{"sourceServiceId":%q,"sourceName":"R5 Signal Reacted","timestamp":%d,"dataMessage":{"timestamp":%d,"message":%q}}}`,
			r5SignalAccountAddress, r5SignalACI, timestamp+9, timestamp, body,
		))
		record, ephemeral, err := ingest.BuildSignalIngress(
			r5SignalAccountID, 1, r5SignalAccountAddress, line, "", "", time.UnixMilli(timestamp+9),
		)
		if err != nil || record == nil || ephemeral != nil {
			t.Fatalf("BuildSignalIngress = %v, %v, %v", record, ephemeral, err)
		}
		if err := stack.Sink.AppendIngress(context.Background(), *record); err != nil {
			t.Fatalf("AppendIngress: %v", err)
		}
		var incoming *db.Message
		waitR5(t, "incoming projection", func() bool {
			found, searchErr := reads.SearchMessagesFiltered(body, db.SearchFilter{Limit: 5})
			if searchErr != nil || len(found) != 1 {
				return false
			}
			incoming = found[0]
			return true
		})

		target, args, err := react("r5-react-incoming", incoming.MessageID)
		// The v2 remote ID is a SHA-1, which signal-cli's -t cannot take.
		if want := v2keys.SignalIncomingSourceID(r5SignalConversation, r5SignalACI, timestamp); target.RemoteID != want {
			t.Fatalf("reaction target remote ID = %q, want the SHA-1 %q", target.RemoteID, want)
		}
		if want := targetArgs(r5SignalACI, timestamp); err != nil || !slices.Equal(args, want) {
			t.Fatalf("reaction target = %q, %v; want %q", args, err, want)
		}
	})

	t.Run("reaction to a migrated incoming message under a decimal ID is refused", func(t *testing.T) {
		// The fixture's incoming Signal row has a numeric legacy ID that is
		// not its timestamp. No Signal receiver keys a message that way, so
		// nothing vouches that its stored time is the one Signal knows it by:
		// neither the ID nor the time may reach -t.
		legacyFixture, err := db.New(fixture.StorePath)
		if err != nil {
			t.Fatalf("open legacy fixture store: %v", err)
		}
		defer legacyFixture.Close()
		row, err := legacyFixture.GetMessageByID("signal:1700000001000")
		if err != nil || row == nil || row.IsFromMe || row.TimestampMS == 1700000001000 {
			t.Fatalf("legacy fixture row = %+v, %v; want an incoming row whose ID is not its timestamp", row, err)
		}
		v2ID := v2keys.DeriveID("message", r5SignalAccountID, r5SignalConversation+"\x1f1700000001000")
		target, args, err := react("r5-react-migrated-incoming", v2ID)
		if target.RemoteID != "1700000001000" || target.Outgoing || target.AuthorID != row.SenderNumber ||
			!target.SentAt.Equal(time.UnixMilli(row.TimestampMS)) {
			t.Fatalf("reaction target = %+v, want the migrated incoming row as stored", target)
		}
		if err == nil || err.Error() != "signal reaction target timestamp is unavailable" || args != nil {
			t.Fatalf("reaction target = %q, %v; want no timestamp to send", args, err)
		}
	})

	t.Run("reactions to migrated own messages with no known Signal timestamp are refused", func(t *testing.T) {
		// A "local:" row keeps whatever time the legacy store held, which for
		// a send the legacy SendText made is the wall clock after signal-cli
		// returned. A scheduled send caught in "sending" is imported under a
		// derived request ID with its creation time and no outbox row.
		sendingRequestID := v2keys.DeriveID("transport_request", r5SignalAccountID, "r5-sending")
		for _, migrated := range []struct{ name, remoteID string }{
			{name: "local alias", remoteID: "local:abc123r5"},
			{name: "scheduled send left in sending", remoteID: sendingRequestID},
		} {
			v2ID := v2keys.DeriveID("message", r5SignalAccountID, r5SignalConversation+"\x1f"+migrated.remoteID)
			stored, err := messages.GetMessage(context.Background(), v2ID)
			if err != nil || stored.Direction != sqlite.MessageDirectionOutgoing || stored.OccurredAtMS <= 0 {
				t.Fatalf("migrated %s = %+v, %v; want an own message with a stored time", migrated.name, stored, err)
			}
			target, args, err := react("r5-react-migrated-own-"+migrated.name, v2ID)
			if target.RemoteID != migrated.remoteID || !target.Outgoing ||
				!target.SentAt.Equal(time.UnixMilli(stored.OccurredAtMS)) {
				t.Fatalf("reaction target for migrated %s = %+v, want the own message as stored", migrated.name, target)
			}
			if err == nil || err.Error() != "signal reaction target timestamp is unavailable" || args != nil {
				t.Fatalf("reaction target for migrated %s = %q, %v; want no timestamp to send", migrated.name, args, err)
			}
		}
	})

	t.Run("reaction to this account's own v2 outbox send", func(t *testing.T) {
		signal.EnqueueTextResult(bridge.SendResult{RemoteMessageID: "1752800000030"})
		own, err := stack.Service.SendText(context.Background(), messaging.SendTextCommand{
			CommonCommand: messaging.CommonCommand{
				AccountID: r5SignalAccountID, ConversationID: signalConversation, IdempotencyKey: "r5-react-own-send",
			},
			Body: "r5 own outbox send to react to",
		})
		if err != nil {
			t.Fatalf("submit own send: %v", err)
		}
		waitR5(t, "own send", func() bool {
			delivery, getErr := stack.Service.Get(context.Background(), own.OutboxID)
			return getErr == nil && delivery.State == messaging.OutboxConfirmed && delivery.RemoteMessageID == "1752800000030"
		})
		stored, err := messages.GetMessage(context.Background(), own.LocalMessageID)
		if err != nil {
			t.Fatalf("GetMessage(own send): %v", err)
		}

		target, args, err := react("r5-react-own", own.LocalMessageID)
		// The stored occurred time is the submit time; the reaction targets
		// the timestamp signal-cli reported for the send.
		if !target.Outgoing || !target.SentAt.Equal(time.UnixMilli(stored.OccurredAtMS)) ||
			stored.OccurredAtMS == 1752800000030 {
			t.Fatalf("own send target = %+v, stored occurred at %d; want the submit time", target, stored.OccurredAtMS)
		}
		if want := targetArgs(r5SignalAccountAddress, 1752800000030); err != nil || !slices.Equal(args, want) {
			t.Fatalf("reaction target = %q, %v; want %q", args, err, want)
		}
	})
}
