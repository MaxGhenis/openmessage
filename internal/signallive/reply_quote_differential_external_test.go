package signallive_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/bridgeadapters/scripted"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const (
	quoteDiffAccountID = "signal-primary"
	quoteDiffAccount   = "+15551230000"
	quoteDiffKnownACI  = "9f4b50e3-ebf2-413c-a856-161756a6161a"
	quoteDiffKnownE164 = "+15557654321"
	quoteDiffOtherACI  = "11111111-2222-3333-4444-555555555555"
	quoteDiffGroupID   = "cXVvdGUtZGlmZmVyZW50aWFsLWdyb3Vw"
)

// The shapes of Signal traffic both receive paths store. Every kind must be
// generated at least once across the seeds (asserted below).
const (
	kindIncomingE164 = iota
	kindIncomingKnownACI
	kindIncomingUnknownACI
	kindIncomingGroup
	kindIncomingAttachmentOnly
	kindIncomingTextAndAttachment
	kindSyncSentText
	kindSyncSentAttachmentOnly
	quoteDiffKindCount
)

type quoteDiffEntry struct {
	kind      int
	line      []byte
	timestamp int64
	fromMe    bool
}

// TestV2DescribedQuoteMatchesLegacyLookup is the differential test for
// Signal quote-replies on a v2-primary daemon. Each seed feeds one random
// corpus of signal-cli receive lines through both retained paths: the legacy
// receive handler (writing the legacy store) and the durable tee, v2 decoder
// and ingest worker (writing the v2 store). For every message both stores
// then hold, it compares:
//
//   - the legacy path: the quote signalQuoteArgs builds from the legacy row;
//   - the new path: a reply submitted to the messaging service with the v2
//     message's ID, dispatched to a scripted Signal account, whose captured
//     bridge.MessageRef the real adapter conversion hands to signallive's
//     quote builder with an empty legacy store.
//
// The two must produce identical signal-cli arguments, and the new path must
// never need the legacy row (the ref is always described).
func TestV2DescribedQuoteMatchesLegacyLookup(t *testing.T) {
	restore := signallive.SetRunSignalCLIForTest(func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("signal-cli is not available in this test")
	})
	t.Cleanup(restore)

	covered := make([]int, quoteDiffKindCount)
	compared := 0
	for seed := int64(1); seed <= 12; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			compared += checkQuoteDifferential(t, seed, covered)
		})
	}
	for kind, count := range covered {
		if count == 0 {
			t.Errorf("corpus kind %d was never generated", kind)
		}
	}
	t.Logf("compared %d messages; per-kind coverage %v", compared, covered)
}

func checkQuoteDifferential(t *testing.T, seed int64, covered []int) int {
	random := rand.New(rand.NewSource(seed))
	contacts := map[string]string{quoteDiffKnownACI: quoteDiffKnownE164}
	dir := t.TempDir()
	legacy := openQuoteDiffLegacy(t, filepath.Join(dir, "legacy.sqlite3"))
	emptyLegacy := openQuoteDiffLegacy(t, filepath.Join(dir, "empty-legacy.sqlite3"))
	v2Store, messages, sink, worker := openQuoteDiffV2(t, filepath.Join(dir, "v2.sqlite3"))

	entries := quoteDiffCorpus(random)
	for index, entry := range entries {
		covered[entry.kind]++
		captured, processed, err := signallive.CaptureAndProcessReceiveLineForTest(
			legacy, t.TempDir(), contacts, quoteDiffAccount, entry.line,
		)
		if err != nil || !processed {
			t.Fatalf("entry %d legacy receive = (%v, %v), line %s", index, processed, err, entry.line)
		}
		record, ephemeral, err := ingest.BuildSignalIngress(
			quoteDiffAccountID, 1, captured.Account, captured.Line,
			captured.ResolvedSource, captured.ResolvedDestination, time.UnixMilli(entry.timestamp),
		)
		if err != nil || record == nil || ephemeral != nil {
			t.Fatalf("entry %d BuildSignalIngress = (%v, %v, %v)", index, record, ephemeral, err)
		}
		if err := sink.AppendIngress(context.Background(), *record); err != nil {
			t.Fatalf("entry %d AppendIngress: %v", index, err)
		}
	}
	drainQuoteDiffWorker(t, worker, messages)

	type key struct {
		conversation string
		timestamp    int64
		fromMe       bool
	}
	legacyRows := map[key]*db.Message{}
	conversations, err := legacy.ListConversations(1000)
	if err != nil {
		t.Fatalf("legacy ListConversations: %v", err)
	}
	v2Rows := map[key]sqlite.Message{}
	for _, conversation := range conversations {
		rows, err := legacy.GetMessagesByConversation(conversation.ConversationID, 1000)
		if err != nil {
			t.Fatalf("legacy GetMessagesByConversation(%q): %v", conversation.ConversationID, err)
		}
		for _, row := range rows {
			legacyRows[key{row.ConversationID, row.TimestampMS, row.IsFromMe}] = row
		}
		v2Conversation, err := v2Store.GetConversationByRemote(quoteDiffAccountID, conversation.ConversationID)
		if err != nil {
			t.Fatalf("v2 conversation for legacy %q: %v", conversation.ConversationID, err)
		}
		v2Messages, err := messages.ListMessagesByConversation(context.Background(), v2Conversation.ConversationID, 0, "", 1000)
		if err != nil {
			t.Fatalf("v2 ListMessagesByConversation(%q): %v", v2Conversation.ConversationID, err)
		}
		for _, message := range v2Messages {
			v2Rows[key{conversation.ConversationID, message.OccurredAtMS, message.Direction == sqlite.MessageDirectionOutgoing}] = message
		}
	}
	if len(legacyRows) != len(entries) || len(v2Rows) != len(entries) {
		t.Fatalf("stored %d legacy and %d v2 messages for %d receive lines", len(legacyRows), len(v2Rows), len(entries))
	}
	// occurred_at_ms is the Signal sent timestamp the decoder used, so the
	// dispatcher's SentAt is a valid quote timestamp for incoming messages.
	sentTimestamps := map[int64]bool{}
	for _, entry := range entries {
		sentTimestamps[entry.timestamp] = true
	}
	for rowKey, message := range v2Rows {
		if !sentTimestamps[message.OccurredAtMS] {
			t.Fatalf("v2 message %+v (key %+v) occurred at %d, not a sent timestamp of the corpus", message, rowKey, message.OccurredAtMS)
		}
	}

	registry := bridge.NewRegistry()
	adapter := scripted.New(quoteDiffAccountID, bridge.PlatformSignal)
	if err := registry.Register(adapter); err != nil {
		t.Fatalf("Register(scripted Signal): %v", err)
	}
	service, err := messaging.NewMessageService(v2Store, registry, nil, messaging.SystemClock{}, messaging.CryptoIDSource{})
	if err != nil {
		t.Fatalf("NewMessageService(): %v", err)
	}

	compared := 0
	for rowKey, legacyRow := range legacyRows {
		v2Row, ok := v2Rows[rowKey]
		if !ok {
			t.Fatalf("v2 store lacks the message legacy holds as %+v", legacyRow)
		}
		want, err := signallive.LegacyQuoteArgsForTest(legacy, contacts, quoteDiffAccount, legacyRow.MessageID)
		if err != nil {
			t.Fatalf("legacy quote for %+v: %v", legacyRow, err)
		}

		adapter.EnqueueTextResult(bridge.SendResult{RemoteMessageID: fmt.Sprintf("%d", 1_800_000_000_000+int64(compared))})
		if _, err := service.SendText(context.Background(), messaging.SendTextCommand{
			CommonCommand: messaging.CommonCommand{
				AccountID:      quoteDiffAccountID,
				ConversationID: v2Row.ConversationID,
				IdempotencyKey: fmt.Sprintf("quote-diff-%d-%d", seed, compared),
			},
			Body:             "reply",
			ReplyToMessageID: v2Row.MessageID,
		}); err != nil {
			t.Fatalf("SendText(reply to %q): %v", v2Row.MessageID, err)
		}
		if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(reply to %q) = %d, %v", v2Row.MessageID, processed, err)
		}
		requests := adapter.TextRequests()
		reply := requests[len(requests)-1].ReplyTo
		if reply == nil || reply.SentAt.IsZero() {
			t.Fatalf("reply to %q carried %+v, want a described ref", v2Row.MessageID, reply)
		}
		got, err := signallive.ReplyQuoteArgsForTest(
			emptyLegacy, contacts, quoteDiffAccount, signaladapter.ReplyTarget(reply),
		)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("seed %d: v2 quote for %+v = %q, %v; legacy row %+v quotes %q",
				seed, v2Row, got, err, legacyRow, want)
		}
		compared++
	}
	return compared
}

func quoteDiffCorpus(random *rand.Rand) []quoteDiffEntry {
	bodies := []string{"hi", "  padded text  ", "multi\nline", "emoji 🎉", "plain"}
	mimes := []string{"image/jpeg", "image/png", "video/mp4", "audio/aac", "application/pdf", ""}
	senders := []string{"+15551234567", "+15559876543"}
	base := int64(1_700_000_000_000) + random.Int63n(1_000_000_000)
	count := 6 + random.Intn(10)
	entries := make([]quoteDiffEntry, 0, count)
	for index := 0; index < count; index++ {
		kind := random.Intn(quoteDiffKindCount)
		// A minute apart, so the legacy sync-sent dedupe (15s drift) never
		// folds two lines into one row.
		timestamp := base + int64(index)*60_000 + random.Int63n(1_000)
		body := bodies[random.Intn(len(bodies))]
		attachment := map[string]any{
			"id":          fmt.Sprintf("att-%d-%d", index, random.Intn(1_000_000)),
			"contentType": mimes[random.Intn(len(mimes))],
			"filename":    "file.bin",
		}
		// Signal quotes by the sent timestamp, which signal-cli reports on the
		// data (or sent) message. The envelope's own timestamp can differ, and
		// a message without one falls back to the envelope's on both paths.
		envelopeTimestamp := timestamp
		if random.Intn(4) == 0 {
			envelopeTimestamp = timestamp + 1 + random.Int63n(500)
		}
		messageTimestamp := map[string]any{"timestamp": timestamp}
		if random.Intn(5) == 0 {
			messageTimestamp = map[string]any{}
			timestamp = envelopeTimestamp
		}
		envelope := map[string]any{"timestamp": envelopeTimestamp}
		data := map[string]any{}
		for field, value := range messageTimestamp {
			data[field] = value
		}
		fromMe := false
		switch kind {
		case kindIncomingE164:
			sender := senders[random.Intn(len(senders))]
			envelope["source"], envelope["sourceNumber"], envelope["sourceUuid"] = sender, sender, quoteDiffOtherACI
			envelope["sourceName"] = "E164 Friend"
			data["message"] = body
		case kindIncomingKnownACI:
			envelope["sourceServiceId"], envelope["sourceName"] = quoteDiffKnownACI, "Known ACI"
			data["message"] = body
		case kindIncomingUnknownACI:
			envelope["sourceServiceId"], envelope["sourceName"] = quoteDiffOtherACI, "Unknown ACI"
			data["message"] = body
		case kindIncomingGroup:
			if random.Intn(2) == 0 {
				envelope["sourceNumber"] = senders[random.Intn(len(senders))]
			} else {
				envelope["sourceServiceId"] = quoteDiffKnownACI
			}
			envelope["sourceName"] = "Group Member"
			data["message"] = body
			data["groupInfo"] = map[string]any{"groupId": quoteDiffGroupID, "type": "DELIVER"}
		case kindIncomingAttachmentOnly:
			envelope["sourceNumber"] = senders[random.Intn(len(senders))]
			data["attachments"] = []any{attachment}
		case kindIncomingTextAndAttachment:
			envelope["sourceNumber"] = senders[random.Intn(len(senders))]
			data["message"] = body
			data["attachments"] = []any{attachment}
		case kindSyncSentText, kindSyncSentAttachmentOnly:
			fromMe = true
			envelope["source"], envelope["sourceNumber"] = quoteDiffAccount, quoteDiffAccount
			sent := map[string]any{}
			for field, value := range messageTimestamp {
				sent[field] = value
			}
			if kind == kindSyncSentText {
				sent["message"] = body
			} else {
				sent["attachments"] = []any{attachment}
			}
			if random.Intn(3) == 0 {
				sent["groupInfo"] = map[string]any{"groupId": quoteDiffGroupID, "type": "DELIVER"}
			} else {
				destination := senders[random.Intn(len(senders))]
				sent["destination"], sent["destinationNumber"] = destination, destination
			}
			envelope["syncMessage"] = map[string]any{"sentMessage": sent}
		}
		if !fromMe {
			envelope["dataMessage"] = data
		}
		line, err := json.Marshal(map[string]any{"account": quoteDiffAccount, "envelope": envelope})
		if err != nil {
			panic(err)
		}
		entries = append(entries, quoteDiffEntry{kind: kind, line: line, timestamp: timestamp, fromMe: fromMe})
	}
	return entries
}

func openQuoteDiffLegacy(t *testing.T, path string) *db.Store {
	t.Helper()
	store, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func openQuoteDiffV2(
	t *testing.T,
	path string,
) (*sqlite.Store, *sqlite.MessageRepository, *ingest.Sink, *ingest.Worker) {
	t.Helper()
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	nowMS := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: quoteDiffAccountID, BridgeKey: "signal_cli", DisplayName: "Signal",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: `{}`,
		CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	reactions, err := sqlite.NewReactionRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	counters := &ingest.Counters{}
	worker, err := ingest.NewWorker(ingest.WorkerConfig{
		Store: store, Messages: messages, Reactions: reactions, Counters: counters,
		Decoders: []ingest.DecoderRegistration{{
			Codec:    ingest.SignalJSONRPCCodec,
			Platform: bridge.PlatformSignal,
			Decoder:  ingest.NewSignalDecoder(),
		}},
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	nextID := 0
	sink, err := ingest.NewSink(ingest.SinkConfig{
		Messages: messages, Worker: worker, Counters: counters,
		IDs: messaging.IDSourceFunc(func() (string, error) {
			nextID++
			return fmt.Sprintf("quote-diff-inbox-%d", nextID), nil
		}),
	})
	if err != nil {
		t.Fatalf("NewSink(): %v", err)
	}
	return store, messages, sink, worker
}

func drainQuoteDiffWorker(t *testing.T, worker *ingest.Worker, messages *sqlite.MessageRepository) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		pending, err := messages.Unprocessed(context.Background())
		if err == nil && len(pending) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ingest worker left %d frames unprocessed (last error %v)", len(pending), err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
