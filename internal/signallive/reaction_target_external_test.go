package signallive_test

import (
	"cmp"
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// signalCLIRecorder stands in for signal-cli: it records every sendReaction
// argv, answers a text send with the next scripted timestamp, and answers
// contact refreshes with no contacts.
type signalCLIRecorder struct {
	mu             sync.Mutex
	reactions      [][]string
	sendTimestamps []int64
}

func installSignalCLIRecorder(t *testing.T) *signalCLIRecorder {
	t.Helper()
	recorder := &signalCLIRecorder{}
	restore := signallive.SetRunSignalCLIForTest(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		switch {
		case slices.Contains(args, "sendReaction"):
			recorder.reactions = append(recorder.reactions, slices.Clone(args))
			return []byte("ok"), nil
		case slices.Contains(args, "send") && len(recorder.sendTimestamps) > 0:
			timestamp := recorder.sendTimestamps[0]
			recorder.sendTimestamps = recorder.sendTimestamps[1:]
			return []byte(fmt.Sprintf(
				`{"timestamp":%d,"results":[{"recipientAddress":{"number":"+15551234567"},"type":"SUCCESS"}]}`,
				timestamp,
			)), nil
		case slices.Contains(args, "listContacts"):
			return []byte("[]"), nil
		}
		return nil, fmt.Errorf("unexpected signal-cli call %q", args)
	})
	t.Cleanup(restore)
	return recorder
}

func (r *signalCLIRecorder) reactionCalls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reactions)
}

func (r *signalCLIRecorder) scriptSend(timestamp int64) {
	r.mu.Lock()
	r.sendTimestamps = append(r.sendTimestamps, timestamp)
	r.mu.Unlock()
}

// newSignalReactionService wires a messaging service to the real Signal
// adapter over a connected signallive bridge whose legacy store is legacy.
func newSignalReactionService(
	t *testing.T,
	v2Store *sqlite.Store,
	legacy *db.Store,
	clock messaging.Clock,
) *messaging.MessageService {
	t.Helper()
	registry := bridge.NewRegistry()
	live := signallive.NewConnectedBridgeForTest(legacy, t.TempDir(), quoteDiffAccount, nil)
	if err := registry.Register(signaladapter.New(quoteDiffAccountID, live)); err != nil {
		t.Fatalf("Register(Signal adapter): %v", err)
	}
	service, err := messaging.NewMessageService(v2Store, registry, nil, clock, messaging.CryptoIDSource{})
	if err != nil {
		t.Fatalf("NewMessageService(): %v", err)
	}
	return service
}

// ingestSignalLine stores one signal-cli receive line through the durable
// tee, v2 decoder and ingest worker, as a v2-primary daemon does.
func ingestSignalLine(
	t *testing.T,
	sink *ingest.Sink,
	worker *ingest.Worker,
	messages *sqlite.MessageRepository,
	line []byte,
	resolvedSource string,
	receivedAt time.Time,
) {
	t.Helper()
	record, ephemeral, err := ingest.BuildSignalIngress(
		quoteDiffAccountID, 1, quoteDiffAccount, line, resolvedSource, "", receivedAt,
	)
	if err != nil || record == nil || ephemeral != nil {
		t.Fatalf("BuildSignalIngress = (%v, %v, %v)", record, ephemeral, err)
	}
	if err := sink.AppendIngress(context.Background(), *record); err != nil {
		t.Fatalf("AppendIngress: %v", err)
	}
	drainQuoteDiffWorker(t, worker, messages)
}

// TestV2ReactionToIncomingSignalMessageTargetsItsSentTimestamp dispatches a
// reaction to a live-ingested incoming Signal message through the v2 outbox,
// the real Signal adapter and signallive, and checks the signal-cli argv.
// Signal names the reacted-to message by author and sent timestamp; the v2
// remote ID of an incoming message is a SHA-1, so it must not reach -t.
func TestV2ReactionToIncomingSignalMessageTargetsItsSentTimestamp(t *testing.T) {
	recorder := installSignalCLIRecorder(t)
	dir := t.TempDir()
	emptyLegacy := openQuoteDiffLegacy(t, filepath.Join(dir, "legacy.sqlite3"))
	v2Store, messages, sink, worker := openQuoteDiffV2(t, filepath.Join(dir, "v2.sqlite3"))

	const sender = "+15551234567"
	const conversation = "signal:" + sender
	sentAt := int64(1_700_000_000_123)
	// The envelope timestamp differs from the data message's sent timestamp.
	line := []byte(fmt.Sprintf(
		`{"account":%q,"envelope":{"source":%q,"sourceNumber":%q,"sourceName":"Taylor","timestamp":%d,"dataMessage":{"timestamp":%d,"message":"react to me"}}}`,
		quoteDiffAccount, sender, sender, sentAt+7, sentAt,
	))
	ingestSignalLine(t, sink, worker, messages, line, sender, time.UnixMilli(sentAt+7))

	v2Conversation, err := v2Store.GetConversationByRemote(quoteDiffAccountID, conversation)
	if err != nil {
		t.Fatalf("GetConversationByRemote(%q): %v", conversation, err)
	}
	rows, err := messages.ListMessagesByConversation(context.Background(), v2Conversation.ConversationID, 0, "", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("v2 messages = %+v, %v; want the one ingested message", rows, err)
	}
	target := rows[0]
	if want := v2keys.SignalIncomingSourceID(conversation, sender, sentAt); target.RemoteMessageID != want {
		t.Fatalf("incoming remote ID = %q, want the SHA-1 %q", target.RemoteMessageID, want)
	}

	service := newSignalReactionService(t, v2Store, emptyLegacy, messaging.SystemClock{})
	submission, err := service.SendReaction(context.Background(), messaging.SendReactionCommand{
		CommonCommand: messaging.CommonCommand{
			AccountID:      quoteDiffAccountID,
			ConversationID: v2Conversation.ConversationID,
			IdempotencyKey: "react-to-incoming",
		},
		TargetMessageID: target.MessageID,
		Emoji:           "👍",
		Action:          bridge.ReactionAdd,
	})
	if err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}
	if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
	}

	calls := recorder.reactionCalls()
	want := []string{
		"-a", quoteDiffAccount,
		"sendReaction",
		"-e", "👍",
		"-a", sender,
		"-t", "1700000000123",
		sender,
	}
	if len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("signal-cli sendReaction calls = %q, want one %q", calls, want)
	}
	delivery, err := service.Get(context.Background(), submission.OutboxID)
	if err != nil || delivery.State != messaging.OutboxConfirmed {
		t.Fatalf("reaction delivery = %+v, %v; want confirmed", delivery, err)
	}
}

// TestV2ReactionMatchesLegacyReaction is the differential test for Signal
// reactions on a v2-primary daemon. Each seed feeds one random corpus of
// signal-cli receive lines (the quote differential's eight shapes) through
// both retained paths: the legacy receive handler and the durable tee, v2
// decoder and ingest worker. For every message both stores then hold it sends
// the same reaction twice and compares the signal-cli argv:
//
//   - the legacy path: SendReaction, which looks the target up in the legacy
//     store by message ID and sends its stored sender and timestamp;
//   - the new path: a reaction submitted to the messaging service with the v2
//     message's ID and dispatched through the real Signal adapter to a bridge
//     whose legacy store is empty.
//
// The argv must be identical: every shape here is one both paths can name (a
// decoded incoming message with a sender, or a sync message carrying Signal's
// timestamp). Before the fix the new path sent the v2 remote ID as -t, which
// for every incoming shape is a SHA-1.
func TestV2ReactionMatchesLegacyReaction(t *testing.T) {
	recorder := installSignalCLIRecorder(t)
	covered := make([]int, quoteDiffKindCount)
	compared := 0
	for seed := int64(1); seed <= 12; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			compared += checkReactionDifferential(t, seed, covered, recorder)
		})
	}
	for kind, count := range covered {
		if count == 0 {
			t.Errorf("corpus kind %d was never generated", kind)
		}
	}
	t.Logf("compared %d reactions; per-kind coverage %v", compared, covered)
}

type reactionDiffKey struct {
	conversation string
	timestamp    int64
	fromMe       bool
}

func checkReactionDifferential(t *testing.T, seed int64, covered []int, recorder *signalCLIRecorder) int {
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

	legacyRows := map[reactionDiffKey]*db.Message{}
	v2Rows := map[reactionDiffKey]sqlite.Message{}
	conversations, err := legacy.ListConversations(1000)
	if err != nil {
		t.Fatalf("legacy ListConversations: %v", err)
	}
	for _, conversation := range conversations {
		rows, err := legacy.GetMessagesByConversation(conversation.ConversationID, 1000)
		if err != nil {
			t.Fatalf("legacy GetMessagesByConversation(%q): %v", conversation.ConversationID, err)
		}
		for _, row := range rows {
			legacyRows[reactionDiffKey{row.ConversationID, row.TimestampMS, row.IsFromMe}] = row
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
			v2Rows[reactionDiffKey{conversation.ConversationID, message.OccurredAtMS, message.Direction == sqlite.MessageDirectionOutgoing}] = message
		}
	}
	if len(legacyRows) != len(entries) || len(v2Rows) != len(entries) {
		t.Fatalf("stored %d legacy and %d v2 messages for %d receive lines", len(legacyRows), len(v2Rows), len(entries))
	}
	keys := make([]reactionDiffKey, 0, len(legacyRows))
	for key := range legacyRows {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b reactionDiffKey) int {
		return cmp.Or(
			strings.Compare(a.conversation, b.conversation),
			cmp.Compare(a.timestamp, b.timestamp),
		)
	})

	service := newSignalReactionService(t, v2Store, emptyLegacy, messaging.SystemClock{})
	emojis := []string{"👍", "❤️", "😂"}
	actions := []bridge.ReactionAction{bridge.ReactionAdd, bridge.ReactionRemove, bridge.ReactionSwitch}
	for index, key := range keys {
		legacyRow := legacyRows[key]
		v2Row, ok := v2Rows[key]
		if !ok {
			t.Fatalf("v2 store lacks the message legacy holds as %+v", legacyRow)
		}
		emoji := emojis[random.Intn(len(emojis))]
		action := actions[random.Intn(len(actions))]

		before := len(recorder.reactionCalls())
		if err := signallive.LegacySendReactionForTest(
			legacy, t.TempDir(), quoteDiffAccount, contacts,
			legacyRow.ConversationID, legacyRow.MessageID, emoji, string(action),
		); err != nil {
			t.Fatalf("legacy reaction to %+v: %v", legacyRow, err)
		}
		submission, err := service.SendReaction(context.Background(), messaging.SendReactionCommand{
			CommonCommand: messaging.CommonCommand{
				AccountID:      quoteDiffAccountID,
				ConversationID: v2Row.ConversationID,
				IdempotencyKey: fmt.Sprintf("reaction-diff-%d-%d", seed, index),
			},
			TargetMessageID: v2Row.MessageID,
			Emoji:           emoji,
			Action:          action,
		})
		if err != nil {
			t.Fatalf("SendReaction(%q): %v", v2Row.MessageID, err)
		}
		if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
			t.Fatalf("DispatchDue(reaction to %q) = %d, %v", v2Row.MessageID, processed, err)
		}
		calls := recorder.reactionCalls()[before:]
		if len(calls) != 2 {
			t.Fatalf("seed %d: reaction to %+v ran %d sendReaction calls, want legacy then v2: %q", seed, v2Row, len(calls), calls)
		}
		if !slices.Equal(calls[1], calls[0]) {
			t.Fatalf("seed %d: v2 reaction to %+v ran %q; legacy row %+v runs %q", seed, v2Row, calls[1], legacyRow, calls[0])
		}
		if delivery, err := service.Get(context.Background(), submission.OutboxID); err != nil ||
			delivery.State != messaging.OutboxConfirmed {
			t.Fatalf("seed %d: v2 reaction delivery = %+v, %v; want confirmed", seed, delivery, err)
		}
	}
	return len(keys)
}

// TestV2ReactionToOwnPendingSendWaitsForItsTransportTimestamp reacts to this
// account's own send while the send is still scheduled, so the message has no
// Signal timestamp yet: its remote ID is its outbox request ID and its
// occurred time the submit time. The reaction must not reach signal-cli with
// either; it is not dispatched and retries. Once the send is confirmed, its
// remote ID is the timestamp signal-cli reported, and the retry targets it.
func TestV2ReactionToOwnPendingSendWaitsForItsTransportTimestamp(t *testing.T) {
	recorder := installSignalCLIRecorder(t)
	dir := t.TempDir()
	emptyLegacy := openQuoteDiffLegacy(t, filepath.Join(dir, "legacy.sqlite3"))
	v2Store, messages, sink, worker := openQuoteDiffV2(t, filepath.Join(dir, "v2.sqlite3"))

	const peer = "+15551234567"
	start := time.UnixMilli(1_760_000_000_000)
	// One incoming line creates the conversation.
	line := []byte(fmt.Sprintf(
		`{"account":%q,"envelope":{"source":%q,"sourceNumber":%q,"timestamp":%d,"dataMessage":{"timestamp":%d,"message":"hello"}}}`,
		quoteDiffAccount, peer, peer, start.UnixMilli()-60_000, start.UnixMilli()-60_000,
	))
	ingestSignalLine(t, sink, worker, messages, line, peer, start.Add(-time.Minute))
	v2Conversation, err := v2Store.GetConversationByRemote(quoteDiffAccountID, "signal:"+peer)
	if err != nil {
		t.Fatalf("GetConversationByRemote: %v", err)
	}

	clock := &reactionTestClock{now: start}
	service := newSignalReactionService(t, v2Store, emptyLegacy, clock)
	common := func(key string) messaging.CommonCommand {
		return messaging.CommonCommand{
			AccountID: quoteDiffAccountID, ConversationID: v2Conversation.ConversationID, IdempotencyKey: key,
		}
	}
	sendCommand := common("own-pending-send")
	sendCommand.NotBefore = start.Add(time.Hour)
	own, err := service.SendText(context.Background(), messaging.SendTextCommand{
		CommonCommand: sendCommand,
		Body:          "scheduled own message",
	})
	if err != nil {
		t.Fatalf("SendText(scheduled): %v", err)
	}
	reaction, err := service.SendReaction(context.Background(), messaging.SendReactionCommand{
		CommonCommand:   common("react-to-own-pending"),
		TargetMessageID: own.LocalMessageID,
		Emoji:           "👍",
	})
	if err != nil {
		t.Fatalf("SendReaction(own pending): %v", err)
	}

	if processed, err := service.DispatchDue(context.Background(), 4); err != nil || processed != 1 {
		t.Fatalf("DispatchDue(reaction only) = %d, %v; want 1, nil", processed, err)
	}
	if calls := recorder.reactionCalls(); len(calls) != 0 {
		t.Fatalf("reaction to a send with no Signal timestamp ran signal-cli: %q", calls)
	}
	delivery, err := service.Get(context.Background(), reaction.OutboxID)
	if err != nil || delivery.State != messaging.OutboxNotDispatched || delivery.ErrorClass != string(bridge.FailureTransient) {
		t.Fatalf("reaction delivery = %+v, %v; want not dispatched and retryable", delivery, err)
	}

	recorder.scriptSend(1_760_003_600_555)
	clock.advance(time.Hour + time.Second)
	for attempt := 0; attempt < 4; attempt++ {
		if _, err := service.DispatchDue(context.Background(), 4); err != nil {
			t.Fatalf("DispatchDue(attempt %d): %v", attempt, err)
		}
		if delivery, err = service.Get(context.Background(), reaction.OutboxID); err != nil {
			t.Fatalf("Get(reaction): %v", err)
		}
		if delivery.State == messaging.OutboxConfirmed {
			break
		}
		clock.advance(10 * time.Second)
	}
	if sent, err := service.Get(context.Background(), own.OutboxID); err != nil ||
		sent.State != messaging.OutboxConfirmed || sent.RemoteMessageID != "1760003600555" {
		t.Fatalf("own send delivery = %+v, %v; want confirmed at signal-cli's timestamp", sent, err)
	}
	if delivery.State != messaging.OutboxConfirmed {
		t.Fatalf("reaction delivery after the send confirmed = %+v, want confirmed", delivery)
	}
	want := []string{
		"-a", quoteDiffAccount,
		"sendReaction",
		"-e", "👍",
		"-a", quoteDiffAccount,
		"-t", "1760003600555",
		peer,
	}
	if calls := recorder.reactionCalls(); len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("signal-cli sendReaction calls = %q, want one %q", calls, want)
	}
}

// reactionTestClock is a messaging.Clock whose time moves only when told.
type reactionTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *reactionTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *reactionTestClock) advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	c.mu.Unlock()
}

func (c *reactionTestClock) NewTimer(delay time.Duration) messaging.Timer {
	return messaging.SystemClock{}.NewTimer(delay)
}

// TestV2ReactionRefusesATargetItCannotName covers stored messages whose
// author or Signal sent timestamp v2 cannot vouch for. Each would otherwise
// produce a well-formed signal-cli command naming a message that does not
// exist (the stored time, or this account as the author of someone else's
// message). The reaction must stop before signal-cli, stay retryable, and
// record why.
func TestV2ReactionRefusesATargetItCannotName(t *testing.T) {
	const peer = "+15551234567"
	const conversation = "signal:" + peer
	const createdAt = int64(1_700_000_000_500)
	const noTimestamp = "send_reaction: transient: signal reaction target timestamp is unavailable"
	const noAuthor = "send_reaction: transient: signal reaction target author is unavailable"
	tests := []struct {
		name       string
		message    sqlite.Message
		fromPeer   bool // stored with the peer's sender identity
		wantDetail string
	}{
		{
			// migration/transform.go imports a legacy scheduled message caught
			// in "sending" with a derived request ID, its creation time, and
			// no outbox row.
			name: "migrated scheduled send left in sending, with no outbox row",
			message: sqlite.Message{
				RemoteMessageID: v2keys.DeriveID("transport_request", quoteDiffAccountID, "legacy-scheduled-1"),
				Direction:       sqlite.MessageDirectionOutgoing,
				OccurredAtMS:    createdAt,
			},
			wantDetail: noTimestamp,
		},
		{
			// The legacy SendText stamped its row with the wall clock after
			// signal-cli returned, so a migrated "local:" row's time may not
			// be the timestamp Signal knows the message by.
			name: "migrated own message under a local alias",
			message: sqlite.Message{
				RemoteMessageID: v2keys.SignalLocalAlias(conversation, createdAt),
				Direction:       sqlite.MessageDirectionOutgoing,
				OccurredAtMS:    createdAt,
			},
			wantDetail: noTimestamp,
		},
		{
			// The legacy receive handler stored a group message with no source
			// under an empty sender; migration keeps it senderless.
			name: "migrated incoming message with no sender",
			message: sqlite.Message{
				RemoteMessageID: v2keys.SignalIncomingSourceID(conversation, "", createdAt),
				Direction:       sqlite.MessageDirectionIncoming,
				OccurredAtMS:    createdAt,
			},
			wantDetail: noAuthor,
		},
		{
			name: "migrated incoming message with no sender and a decimal ID",
			message: sqlite.Message{
				RemoteMessageID: "1700000000500",
				Direction:       sqlite.MessageDirectionIncoming,
				OccurredAtMS:    createdAt,
			},
			wantDetail: noAuthor,
		},
		{
			// The Signal Desktop importer stores a row with no sent time under
			// the time it was received and marks its source ID, which the
			// migration keeps as the remote ID.
			name: "imported Signal Desktop row stored under its received time",
			message: sqlite.Message{
				RemoteMessageID: v2keys.SignalReceivedSourceID(conversation, peer, createdAt),
				Direction:       sqlite.MessageDirectionIncoming,
				OccurredAtMS:    createdAt,
			},
			fromPeer:   true,
			wantDetail: noTimestamp,
		},
		{
			name: "incoming message from a known sender under a decimal ID",
			message: sqlite.Message{
				RemoteMessageID: "1700000000500",
				Direction:       sqlite.MessageDirectionIncoming,
				OccurredAtMS:    createdAt,
			},
			fromPeer:   true,
			wantDetail: noTimestamp,
		},
		{
			// v2wire.MirrorReplyTarget writes an incoming message under its
			// legacy ID with no sender identity.
			name: "mirrored incoming message with no sender",
			message: sqlite.Message{
				RemoteMessageID: "signal:1700000000500",
				Direction:       sqlite.MessageDirectionIncoming,
				OccurredAtMS:    createdAt,
			},
			wantDetail: noAuthor,
		},
	}
	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := installSignalCLIRecorder(t)
			dir := t.TempDir()
			emptyLegacy := openQuoteDiffLegacy(t, filepath.Join(dir, "legacy.sqlite3"))
			v2Store, messages, sink, worker := openQuoteDiffV2(t, filepath.Join(dir, "v2.sqlite3"))
			line := []byte(fmt.Sprintf(
				`{"account":%q,"envelope":{"source":%q,"sourceNumber":%q,"timestamp":%d,"dataMessage":{"timestamp":%d,"message":"hello"}}}`,
				quoteDiffAccount, peer, peer, createdAt-60_000, createdAt-60_000,
			))
			ingestSignalLine(t, sink, worker, messages, line, peer, time.UnixMilli(createdAt-60_000))
			v2Conversation, err := v2Store.GetConversationByRemote(quoteDiffAccountID, conversation)
			if err != nil {
				t.Fatalf("GetConversationByRemote: %v", err)
			}

			message := tc.message
			if tc.fromPeer {
				ingested, err := messages.ListMessagesByConversation(context.Background(), v2Conversation.ConversationID, 0, "", 10)
				if err != nil || len(ingested) != 1 || ingested[0].SenderIdentityID == nil {
					t.Fatalf("ingested peer message = %+v, %v; want one with a sender identity", ingested, err)
				}
				message.SenderIdentityID = ingested[0].SenderIdentityID
			}
			message.MessageID = fmt.Sprintf("message-unnameable-%d", index)
			message.ConversationID = v2Conversation.ConversationID
			message.AccountID = quoteDiffAccountID
			message.Body = "stored without a nameable Signal identity"
			message.State = sqlite.MessageStateActive
			if err := messages.ImportMessage(context.Background(), sqlite.MessageProjection{Message: message}); err != nil {
				t.Fatalf("ImportMessage: %v", err)
			}

			service := newSignalReactionService(t, v2Store, emptyLegacy, messaging.SystemClock{})
			submission, err := service.SendReaction(context.Background(), messaging.SendReactionCommand{
				CommonCommand: messaging.CommonCommand{
					AccountID:      quoteDiffAccountID,
					ConversationID: v2Conversation.ConversationID,
					IdempotencyKey: fmt.Sprintf("react-unnameable-%d", index),
				},
				TargetMessageID: message.MessageID,
				Emoji:           "👍",
			})
			if err != nil {
				t.Fatalf("SendReaction(): %v", err)
			}
			if processed, err := service.DispatchDue(context.Background(), 1); err != nil || processed != 1 {
				t.Fatalf("DispatchDue() = %d, %v; want 1, nil", processed, err)
			}
			if calls := recorder.reactionCalls(); len(calls) != 0 {
				t.Fatalf("signal-cli ran %q for a target v2 cannot name", calls)
			}
			delivery, err := service.Get(context.Background(), submission.OutboxID)
			if err != nil || delivery.State != messaging.OutboxNotDispatched ||
				delivery.ErrorClass != string(bridge.FailureTransient) {
				t.Fatalf("reaction delivery = %+v, %v; want not dispatched and retryable", delivery, err)
			}
			outbox, err := sqlite.NewOutboxRepository(v2Store, time.Now)
			if err != nil {
				t.Fatalf("NewOutboxRepository(): %v", err)
			}
			row, err := outbox.FindByID(context.Background(), submission.OutboxID)
			if err != nil || row.ErrorDetail == nil || *row.ErrorDetail != tc.wantDetail {
				t.Fatalf("reaction row = %+v, %v; want error detail %q", row, err, tc.wantDetail)
			}
		})
	}
}
