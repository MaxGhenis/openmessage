package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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
	"github.com/maxghenis/openmessage/internal/v2wire"
	"github.com/maxghenis/openmessage/internal/web"
)

const r5SignalAccountAddress = "+15550009999"

// TestR5SignalQuoteRepliesResolveFromV2OnV2Primary runs Signal quote-replies
// end to end on a migrated v2-primary stack with a scripted Signal account:
// submission (HTTP API and v2wire), the durable dispatcher, the bridge
// request, the real Signal adapter conversion and signallive's quote builder.
// It covers the message the legacy store never holds on v2-primary (this
// account's own send through the v2 outbox), a live-ingested incoming message
// (SHA-1 remote ID), migrated messages both stores hold (quotes must equal
// what the legacy row gives), and a reply submitted while its quoted send was
// still pending.
func TestR5SignalQuoteRepliesResolveFromV2OnV2Primary(t *testing.T) {
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
	signal := scripted.New(r5SignalAccountID, bridge.PlatformSignal)
	if err := stack.RegisterAdapter(signal); err != nil {
		t.Fatalf("register scripted Signal adapter: %v", err)
	}
	// The v2-primary daemon runs no legacy projector; this inert store is
	// what it would write a confirmed send into.
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
	handler := web.APIHandlerWithOptions(inertLegacy, nil, zerolog.Nop(), nil, web.APIOptions{
		Reads:     reads,
		V2Primary: true,
		V2: &web.V2Options{
			Service: stack.Service, Media: stack.Media, V2Store: stack.Store,
			Blobs: stack.Blobs, Registry: stack.Registry,
		},
	})
	deps := v2wire.NativeDeps{V2: stack.Store, Service: stack.Service, Registry: stack.Registry}
	signalConversation := v2keys.DeriveID("conversation", r5SignalAccountID, r5SignalConversation)

	waitConfirmed := func(description, outboxID, remoteID string) {
		t.Helper()
		waitR5(t, description, func() bool {
			delivery, getErr := stack.Service.Get(context.Background(), outboxID)
			return getErr == nil && delivery.State == messaging.OutboxConfirmed && delivery.RemoteMessageID == remoteID
		})
	}
	lastTextReply := func() *bridge.MessageRef {
		t.Helper()
		requests := signal.TextRequests()
		if len(requests) == 0 {
			t.Fatal("no Signal text requests")
		}
		return requests[len(requests)-1].ReplyTo
	}
	quote := func(ref *bridge.MessageRef) []string {
		t.Helper()
		// An identity resolver is what resolveContactAddress does for an
		// address with no known contact.
		args, quoteErr := signallive.QuoteArgs(signaladapter.ReplyTarget(ref), r5SignalAccountAddress, func(value string) string { return value })
		if quoteErr != nil {
			t.Fatalf("QuoteArgs(%+v): %v", ref, quoteErr)
		}
		return args
	}
	occurredAt := func(messageID string) time.Time {
		t.Helper()
		message, getErr := messages.GetMessage(context.Background(), messageID)
		if getErr != nil {
			t.Fatalf("GetMessage(%q): %v", messageID, getErr)
		}
		return time.UnixMilli(message.OccurredAtMS)
	}

	t.Run("reply to this account's own v2 outbox send, which no legacy row holds", func(t *testing.T) {
		signal.EnqueueTextResult(bridge.SendResult{RemoteMessageID: "1752800000001"})
		own, err := v2wire.SubmitTextV2(context.Background(), deps, v2wire.TextInput{
			ConversationID: signalConversation,
			Body:           "r5 own outbox send",
			IdempotencyKey: "r5-quote-own-send",
		})
		if err != nil {
			t.Fatalf("submit own send: %v", err)
		}
		waitConfirmed("own send", own.OutboxID, "1752800000001")
		if rows, err := inertLegacy.GetMessagesByConversation(r5SignalConversation, 10); err != nil || len(rows) != 0 {
			t.Fatalf("v2-primary wrote %d legacy rows (%v); the quoted send must exist only in v2", len(rows), err)
		}

		signal.EnqueueTextResult(bridge.SendResult{RemoteMessageID: "1752800000002"})
		body, _ := json.Marshal(map[string]string{
			"conversation_id": signalConversation,
			"body":            "r5 reply to own send",
			"reply_to_id":     own.LocalMessageID,
			"idempotency_key": "r5-quote-own-reply",
		})
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/outbox/messages", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("POST reply = %d: %s", recorder.Code, recorder.Body.String())
		}
		var submitted struct {
			OutboxID string `json:"outbox_id"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &submitted); err != nil || submitted.OutboxID == "" {
			t.Fatalf("decode reply submission %q: %v", recorder.Body.String(), err)
		}
		waitConfirmed("reply to own send", submitted.OutboxID, "1752800000002")

		ref := lastTextReply()
		want := bridge.MessageRef{RemoteID: "1752800000001", SentAt: occurredAt(own.LocalMessageID), Text: "r5 own outbox send"}
		if !r5MessageRefEqual(ref, want) {
			t.Fatalf("reply ref = %+v, want %+v", ref, want)
		}
		// The quote carries Signal's timestamp for the send, not the submit
		// time v2 stored as its occurred time.
		if got, want := quote(ref), r5QuoteArgs("1752800000001", r5SignalAccountAddress, "r5 own outbox send"); !slices.Equal(got, want) {
			t.Fatalf("quote = %q, want %q", got, want)
		}
	})

	t.Run("media reply to a live-ingested incoming message", func(t *testing.T) {
		const body = "r5 live incoming to quote"
		timestamp := fixture.BaseMS + 40_000
		line := []byte(fmt.Sprintf(
			`{"account":%q,"envelope":{"sourceServiceId":%q,"sourceName":"R5 Signal Quoted","timestamp":%d,"dataMessage":{"timestamp":%d,"message":%q}}}`,
			r5SignalAccountAddress, r5SignalACI, timestamp, timestamp, body,
		))
		record, ephemeral, err := ingest.BuildSignalIngress(
			r5SignalAccountID, 1, r5SignalAccountAddress, line, "", "", time.UnixMilli(timestamp),
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

		signal.EnqueueMediaResult(bridge.SendResult{RemoteMessageID: "1752800000003"})
		submission, err := v2wire.SubmitMediaV2(context.Background(), deps, v2wire.MediaInput{
			ConversationID: signalConversation,
			Content:        strings.NewReader("r5 reply photo"),
			Filename:       "reply.png",
			MIME:           "image/png",
			ReplyToID:      incoming.MessageID,
			IdempotencyKey: "r5-quote-incoming-media-reply",
		})
		if err != nil {
			t.Fatalf("submit media reply: %v", err)
		}
		waitConfirmed("media reply", submission.OutboxID, "1752800000003")

		calls := signal.MediaRequests()
		ref := calls[len(calls)-1].Request.ReplyTo
		want := bridge.MessageRef{
			RemoteID: v2keys.SignalIncomingSourceID(r5SignalConversation, r5SignalACI, timestamp),
			AuthorID: r5SignalACI,
			SentAt:   time.UnixMilli(timestamp),
			Text:     body,
		}
		if !r5MessageRefEqual(ref, want) {
			t.Fatalf("media reply ref = %+v, want %+v", ref, want)
		}
		if got, want := quote(ref), r5QuoteArgs(strconv.FormatInt(timestamp, 10), r5SignalACI, body); !slices.Equal(got, want) {
			t.Fatalf("quote = %q, want %q", got, want)
		}
	})

	t.Run("replies to migrated messages quote what their legacy rows give", func(t *testing.T) {
		legacyFixture, err := db.New(fixture.StorePath)
		if err != nil {
			t.Fatalf("open legacy fixture store: %v", err)
		}
		defer legacyFixture.Close()
		for index, migrated := range []struct{ legacyID, remoteID string }{
			{legacyID: "signal:1700000001000", remoteID: "1700000001000"},
			{legacyID: "signal:local:abc123r5", remoteID: "local:abc123r5"},
		} {
			row, err := legacyFixture.GetMessageByID(migrated.legacyID)
			if err != nil || row == nil {
				t.Fatalf("legacy fixture row %q = %v, %v", migrated.legacyID, row, err)
			}
			v2ID := v2keys.DeriveID("message", r5SignalAccountID, r5SignalConversation+"\x1f"+migrated.remoteID)
			result := strconv.Itoa(1752800000010 + index)
			signal.EnqueueTextResult(bridge.SendResult{RemoteMessageID: result})
			submission, err := v2wire.SubmitTextV2(context.Background(), deps, v2wire.TextInput{
				ConversationID: signalConversation,
				Body:           "r5 reply to migrated " + migrated.remoteID,
				ReplyToID:      v2ID,
				IdempotencyKey: "r5-quote-migrated-" + migrated.remoteID,
			})
			if err != nil {
				t.Fatalf("submit reply to %q: %v", v2ID, err)
			}
			waitConfirmed("reply to "+migrated.legacyID, submission.OutboxID, result)

			// The legacy lookup's rules: the row's own timestamp, this account
			// for a row it sent, otherwise the stored sender.
			author := row.SenderNumber
			if row.IsFromMe {
				author = r5SignalAccountAddress
			}
			want := r5QuoteArgs(strconv.FormatInt(row.TimestampMS, 10), author, row.Body)
			if got := quote(lastTextReply()); !slices.Equal(got, want) {
				t.Fatalf("quote for migrated %q = %q, legacy row gives %q", migrated.legacyID, got, want)
			}
		}
	})

	t.Run("reply submitted while its quoted send was still pending", func(t *testing.T) {
		signal.EnqueueTextResult(bridge.SendResult{RemoteMessageID: "1752800000020"})
		signal.EnqueueTextResult(bridge.SendResult{RemoteMessageID: "1752800000021"})
		start := time.Now()
		quoted, err := v2wire.SubmitTextV2(context.Background(), deps, v2wire.TextInput{
			ConversationID: signalConversation,
			Body:           "r5 scheduled quoted send",
			IdempotencyKey: "r5-quote-pending-quoted",
			NotBefore:      start.Add(1500 * time.Millisecond),
		})
		if err != nil {
			t.Fatalf("submit scheduled quoted send: %v", err)
		}
		reply, err := v2wire.SubmitTextV2(context.Background(), deps, v2wire.TextInput{
			ConversationID: signalConversation,
			Body:           "r5 reply queued behind it",
			ReplyToID:      quoted.LocalMessageID,
			IdempotencyKey: "r5-quote-pending-reply",
			NotBefore:      start.Add(2500 * time.Millisecond),
		})
		if err != nil {
			t.Fatalf("submit reply: %v", err)
		}
		stored, err := messages.GetMessage(context.Background(), reply.LocalMessageID)
		if err != nil || stored.ReplyToRemoteID == nil || *stored.ReplyToRemoteID == "1752800000020" {
			t.Fatalf("reply stored target %v (%v), want the quoted send's pending request ID", stored.ReplyToRemoteID, err)
		}
		waitConfirmed("quoted send", quoted.OutboxID, "1752800000020")
		waitConfirmed("reply", reply.OutboxID, "1752800000021")

		ref := lastTextReply()
		want := bridge.MessageRef{RemoteID: "1752800000020", SentAt: occurredAt(quoted.LocalMessageID), Text: "r5 scheduled quoted send"}
		if !r5MessageRefEqual(ref, want) {
			t.Fatalf("reply ref = %+v, want %+v", ref, want)
		}
		if got, want := quote(ref), r5QuoteArgs("1752800000020", r5SignalAccountAddress, "r5 scheduled quoted send"); !slices.Equal(got, want) {
			t.Fatalf("quote = %q, want %q", got, want)
		}
	})
}

func r5QuoteArgs(timestamp, author, message string) []string {
	return []string{"--quote-timestamp", timestamp, "--quote-author", author, "--quote-message", message}
}

func r5MessageRefEqual(got *bridge.MessageRef, want bridge.MessageRef) bool {
	return got != nil &&
		got.RemoteID == want.RemoteID &&
		got.AuthorID == want.AuthorID &&
		got.SentAt.Equal(want.SentAt) &&
		got.Text == want.Text &&
		got.HasAttachment == want.HasAttachment &&
		got.AttachmentMIME == want.AttachmentMIME
}
