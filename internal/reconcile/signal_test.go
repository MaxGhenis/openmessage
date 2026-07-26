package reconcile

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	testSignalConversation = "signal:+16505550100"
	testSignalPeer         = "+16505550100"
	testIncomingTimestamp  = int64(1_700_000_001_000)
	testOutgoingTimestamp  = int64(1_700_000_006_000)
)

func TestSignalLegacySourceIDParity(t *testing.T) {
	t.Parallel()

	incoming := db.Message{
		ConversationID: testSignalConversation,
		SenderNumber:   testSignalPeer,
		TimestampMS:    testIncomingTimestamp,
		SourceID: legacySignalSourceID(
			testSignalConversation,
			testSignalPeer,
			testIncomingTimestamp,
			false,
		),
	}
	if got, want := incoming.SourceID, v2keys.SignalIncomingSourceID(
		incoming.ConversationID,
		incoming.SenderNumber,
		incoming.TimestampMS,
	); got != want {
		t.Fatalf("incoming legacy source_id = %q, want v2 key %q", got, want)
	}

	outgoing := db.Message{
		ConversationID: testSignalConversation,
		TimestampMS:    testOutgoingTimestamp,
		IsFromMe:       true,
		SourceID: legacySignalSourceID(
			testSignalConversation,
			"me",
			testOutgoingTimestamp,
			true,
		),
	}
	if got, want := outgoing.SourceID, v2keys.SignalLocalAlias(
		outgoing.ConversationID,
		outgoing.TimestampMS,
	); got != want {
		t.Fatalf("outgoing legacy source_id = %q, want v2 alias %q", got, want)
	}
}

func TestSignalImportsMissingMessagesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	legacy, v2 := openSignalStores(t)
	incomingSourceID := v2keys.SignalIncomingSourceID(
		testSignalConversation,
		testSignalPeer,
		testIncomingTimestamp,
	)
	outgoingAlias := v2keys.SignalLocalAlias(
		testSignalConversation,
		testOutgoingTimestamp,
	)
	seedLegacySignalConversation(t, legacy, testOutgoingTimestamp)
	seedLegacySignalMessage(t, legacy, db.Message{
		MessageID:      "signal:legacy-incoming",
		ConversationID: testSignalConversation,
		SenderName:     "Signal Peer",
		SenderNumber:   testSignalPeer,
		Body:           "missing incoming",
		TimestampMS:    testIncomingTimestamp,
		MediaID:        "legacy-photo-ref",
		MimeType:       "image/jpeg",
		SourcePlatform: signalPlatform,
		SourceID:       incomingSourceID,
	})
	seedLegacySignalMessage(t, legacy, db.Message{
		MessageID:      "signal:legacy-outgoing",
		ConversationID: testSignalConversation,
		SenderName:     "Me",
		Body:           "missing outgoing",
		TimestampMS:    testOutgoingTimestamp,
		IsFromMe:       true,
		SourcePlatform: signalPlatform,
		SourceID:       outgoingAlias,
	})

	first, err := Signal(ctx, Options{
		Legacy: legacy,
		V2:     v2,
		Logger: zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("first Signal(): %v", err)
	}
	if first.ConversationsScanned != 1 || first.ConversationsCreated != 1 ||
		first.MessagesScanned != 2 || first.MessagesImported != 2 ||
		first.MessagesAlreadyPresent != 0 || first.MediaDeferred != 1 ||
		first.Skipped != 0 {
		t.Fatalf("first report = %+v", first)
	}

	conversation, err := v2.GetConversationByRemote(
		signalAccountID,
		testSignalConversation,
	)
	if err != nil {
		t.Fatalf("GetConversationByRemote(): %v", err)
	}
	wantConversationID := v2keys.DeriveID(
		"conversation",
		signalAccountID,
		testSignalConversation,
	)
	if conversation.ConversationID != wantConversationID ||
		conversation.Kind != sqlite.ConversationKindDirect ||
		conversation.LastMessageAtMS != testOutgoingTimestamp {
		t.Fatalf("reconciled conversation = %+v", conversation)
	}
	participants, err := v2.ListParticipants(conversation.ConversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 1 || participants[0].DisplayName != "Signal Peer" {
		t.Fatalf("direct participants = %+v", participants)
	}

	repository := newSignalMessageRepository(t, v2)
	incoming, err := repository.GetMessageByRemote(
		ctx,
		signalAccountID,
		conversation.ConversationID,
		incomingSourceID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(incoming): %v", err)
	}
	if incoming.Body != "missing incoming" ||
		incoming.Direction != sqlite.MessageDirectionIncoming ||
		incoming.SenderIdentityID == nil {
		t.Fatalf("incoming message = %+v", incoming)
	}
	// A fresh repair keeps the byte-identical legacy source ID. The worker
	// resolves a later bare-timestamp delivery onto this local alias.
	outgoing, err := repository.GetMessageByRemote(
		ctx,
		signalAccountID,
		conversation.ConversationID,
		outgoingAlias,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(outgoing): %v", err)
	}
	if outgoing.Body != "missing outgoing" ||
		outgoing.Direction != sqlite.MessageDirectionOutgoing ||
		outgoing.SenderIdentityID != nil {
		t.Fatalf("outgoing message = %+v", outgoing)
	}

	second, err := Signal(ctx, Options{
		Legacy: legacy,
		V2:     v2,
		Logger: zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("second Signal(): %v", err)
	}
	if second.ConversationsCreated != 0 || second.MessagesImported != 0 ||
		second.MessagesAlreadyPresent != 2 || second.MessagesScanned != 2 {
		t.Fatalf("second report = %+v", second)
	}
	stored, err := repository.ListMessagesByConversation(
		ctx,
		conversation.ConversationID,
		0,
		"",
		10,
	)
	if err != nil {
		t.Fatalf("ListMessagesByConversation(): %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored messages = %d, want 2: %+v", len(stored), stored)
	}
	participants, err = v2.ListParticipants(conversation.ConversationID)
	if err != nil {
		t.Fatalf("ListParticipants(second run): %v", err)
	}
	if len(participants) != 1 {
		t.Fatalf("participants after second run = %d, want 1", len(participants))
	}
}

func TestSignalCountsExistingTwinWithoutReimporting(t *testing.T) {
	ctx := context.Background()
	legacy, v2 := openSignalStores(t)
	sourceID := v2keys.SignalIncomingSourceID(
		testSignalConversation,
		testSignalPeer,
		testIncomingTimestamp,
	)
	seedLegacySignalConversation(t, legacy, testIncomingTimestamp)
	seedLegacySignalMessage(t, legacy, db.Message{
		MessageID:      "signal:legacy-existing",
		ConversationID: testSignalConversation,
		SenderName:     "Signal Peer",
		SenderNumber:   testSignalPeer,
		Body:           "legacy body must not overwrite",
		TimestampMS:    testIncomingTimestamp,
		SourcePlatform: signalPlatform,
		SourceID:       sourceID,
	})
	seedSignalAccount(t, v2, testIncomingTimestamp)
	conversationID := seedV2SignalConversation(
		t,
		v2,
		"preexisting-conversation-pk",
		testIncomingTimestamp-1,
	)
	repository := newSignalMessageRepository(t, v2)
	if err := repository.ImportMessage(ctx, sqlite.MessageProjection{Message: sqlite.Message{
		MessageID:       "preexisting-message-pk",
		ConversationID:  conversationID,
		AccountID:       signalAccountID,
		RemoteMessageID: sourceID,
		Direction:       sqlite.MessageDirectionIncoming,
		Body:            "v2 body remains",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    testIncomingTimestamp,
	}}); err != nil {
		t.Fatalf("seed v2 message: %v", err)
	}

	report, err := Signal(ctx, Options{
		Legacy: legacy,
		V2:     v2,
		Logger: zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("Signal(): %v", err)
	}
	if report.MessagesImported != 0 || report.MessagesAlreadyPresent != 1 {
		t.Fatalf("report = %+v", report)
	}
	got, err := repository.GetMessageByRemote(
		ctx,
		signalAccountID,
		conversationID,
		sourceID,
	)
	if err != nil {
		t.Fatalf("GetMessageByRemote(): %v", err)
	}
	if got.MessageID != "preexisting-message-pk" || got.Body != "v2 body remains" {
		t.Fatalf("existing twin was rewritten: %+v", got)
	}
	stored, err := repository.ListMessagesByConversation(ctx, conversationID, 0, "", 10)
	if err != nil {
		t.Fatalf("ListMessagesByConversation(): %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored messages = %d, want 1", len(stored))
	}
}

func TestSignalDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	legacy, v2 := openSignalStores(t)
	sourceID := v2keys.SignalIncomingSourceID(
		testSignalConversation,
		testSignalPeer,
		testIncomingTimestamp,
	)
	seedLegacySignalConversation(t, legacy, testIncomingTimestamp)
	seedLegacySignalMessage(t, legacy, db.Message{
		MessageID:      "signal:legacy-dry-run",
		ConversationID: testSignalConversation,
		SenderName:     "Signal Peer",
		SenderNumber:   testSignalPeer,
		Body:           "[Photo]",
		TimestampMS:    testIncomingTimestamp,
		MediaID:        "legacy-media",
		SourcePlatform: signalPlatform,
		SourceID:       sourceID,
	})
	conversationID := v2keys.DeriveID(
		"conversation",
		signalAccountID,
		testSignalConversation,
	)
	repository := newSignalMessageRepository(t, v2)
	before, err := repository.ListMessagesByConversation(ctx, conversationID, 0, "", 10)
	if err != nil {
		t.Fatalf("list messages before dry run: %v", err)
	}

	report, err := Signal(ctx, Options{
		Legacy: legacy,
		V2:     v2,
		DryRun: true,
		Logger: zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("Signal(dry run): %v", err)
	}
	if !report.DryRun || report.ConversationsCreated != 1 ||
		report.MessagesImported != 1 || report.MediaDeferred != 1 {
		t.Fatalf("dry-run report = %+v", report)
	}
	after, err := repository.ListMessagesByConversation(ctx, conversationID, 0, "", 10)
	if err != nil {
		t.Fatalf("list messages after dry run: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("dry run changed message row count: before=%d after=%d", len(before), len(after))
	}
	if _, err := v2.GetConversationByRemote(
		signalAccountID,
		testSignalConversation,
	); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("dry run conversation lookup error = %v, want ErrNotFound", err)
	}
	accounts, err := v2.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts(): %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("dry run created accounts: %+v", accounts)
	}
	participants, err := v2.ListParticipants(conversationID)
	if err != nil {
		t.Fatalf("ListParticipants(): %v", err)
	}
	if len(participants) != 0 {
		t.Fatalf("dry run created participants: %+v", participants)
	}
}

func legacySignalSourceID(
	conversationID string,
	actor string,
	timestampMS int64,
	outgoing bool,
) string {
	sum := sha1.Sum([]byte(strings.Join([]string{
		conversationID,
		actor,
		strconv.FormatInt(timestampMS, 10),
	}, "\x1f")))
	encoded := hex.EncodeToString(sum[:])
	if outgoing {
		return "local:" + encoded
	}
	return encoded
}

func openSignalStores(t *testing.T) (*db.Store, *sqlite.Store) {
	t.Helper()
	legacy, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() {
		if err := legacy.Close(); err != nil {
			t.Errorf("legacy.Close(): %v", err)
		}
	})
	v2, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := v2.Close(); err != nil {
			t.Errorf("v2.Close(): %v", err)
		}
	})
	return legacy, v2
}

func seedLegacySignalConversation(t *testing.T, legacy *db.Store, lastMessageMS int64) {
	t.Helper()
	if err := legacy.UpsertConversation(&db.Conversation{
		ConversationID: testSignalConversation,
		Name:           "Signal Peer",
		LastMessageTS:  lastMessageMS,
		SourcePlatform: signalPlatform,
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
}

func seedLegacySignalMessage(t *testing.T, legacy *db.Store, message db.Message) {
	t.Helper()
	if err := legacy.UpsertMessage(&message); err != nil {
		t.Fatalf("UpsertMessage(%q): %v", message.MessageID, err)
	}
}

func seedSignalAccount(t *testing.T, v2 *sqlite.Store, atMS int64) {
	t.Helper()
	if err := v2.UpsertAccount(sqlite.Account{
		AccountID:   signalAccountID,
		BridgeKey:   signalBridgeKey,
		DisplayName: "Signal",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  "{}",
		CreatedAtMS: atMS,
		UpdatedAtMS: atMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
}

func seedV2SignalConversation(
	t *testing.T,
	v2 *sqlite.Store,
	conversationID string,
	atMS int64,
) string {
	t.Helper()
	if err := v2.UpsertConversation(sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            signalAccountID,
		RemoteConversationID: testSignalConversation,
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Existing Signal Peer",
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      atMS,
		MetadataJSON:         "{}",
		CreatedAtMS:          atMS,
		UpdatedAtMS:          atMS,
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	return conversationID
}

func newSignalMessageRepository(
	t *testing.T,
	v2 *sqlite.Store,
) *sqlite.MessageRepository {
	t.Helper()
	repository, err := sqlite.NewMessageRepository(
		v2,
		func() time.Time { return time.UnixMilli(2_000_000_000_000) },
	)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	return repository
}

// The duplication defect this reconciler must never reintroduce, reproduced
// exactly as it appears on a real store: the live decoder keyed an outgoing
// message by its bare send timestamp, while legacy keys the same message
// "local:<sha1>". Deduping on the legacy key alone misses the v2 row and
// re-imports the user's own message as a duplicate — 50 of 50 outgoing rows on
// the thread that prompted this work.
func TestSignalDoesNotDuplicateOutgoingMessageAlreadyKeyedByTimestamp(t *testing.T) {
	ctx := context.Background()
	legacy, v2 := openSignalStores(t)
	seedSignalAccount(t, v2, testIncomingTimestamp)
	conversationID := seedV2SignalConversation(
		t,
		v2,
		v2keys.DeriveID("conversation", signalAccountID, testSignalConversation),
		testOutgoingTimestamp,
	)

	// v2 already holds the outgoing message under the live decoder's key.
	bareKey := strconv.FormatInt(testOutgoingTimestamp, 10)
	repository := newSignalMessageRepository(t, v2)
	if err := repository.ImportMessage(ctx, sqlite.MessageProjection{
		Message: sqlite.Message{
			MessageID: v2keys.DeriveID(
				"message",
				signalAccountID,
				testSignalConversation+"\x1f"+bareKey,
			),
			ConversationID:  conversationID,
			AccountID:       signalAccountID,
			RemoteMessageID: bareKey,
			Direction:       sqlite.MessageDirectionOutgoing,
			Body:            "already projected by the live decoder",
			State:           sqlite.MessageStateActive,
			OccurredAtMS:    testOutgoingTimestamp,
		},
	}); err != nil {
		t.Fatalf("seed live-decoder message: %v", err)
	}

	// Legacy holds the same message under the alias form.
	seedLegacySignalConversation(t, legacy, testOutgoingTimestamp)
	seedLegacySignalMessage(t, legacy, db.Message{
		MessageID:      "signal:legacy-outgoing",
		ConversationID: testSignalConversation,
		TimestampMS:    testOutgoingTimestamp,
		IsFromMe:       true,
		Body:           "already projected by the live decoder",
		SourcePlatform: signalPlatform,
		SourceID: v2keys.SignalLocalAlias(
			testSignalConversation,
			testOutgoingTimestamp,
		),
	})

	report, err := Signal(ctx, Options{Legacy: legacy, V2: v2, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("Signal(): %v", err)
	}
	if report.MessagesImported != 0 || report.MessagesAlreadyPresent != 1 {
		t.Fatalf("report = %+v, want the message recognized as already present", report)
	}
	stored, err := repository.ListMessagesByConversation(ctx, conversationID, 0, "", 100)
	if err != nil {
		t.Fatalf("ListMessagesByConversation(): %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored messages = %d (%+v), want exactly one — no duplicate", len(stored), stored)
	}
}

// The mirror case: v2 holds the message under the migration-era "local:" alias
// (what R2 wrote), and legacy has the same alias. Still one row.
func TestSignalDoesNotDuplicateOutgoingMessageAlreadyKeyedByAlias(t *testing.T) {
	ctx := context.Background()
	legacy, v2 := openSignalStores(t)
	seedSignalAccount(t, v2, testIncomingTimestamp)
	conversationID := seedV2SignalConversation(
		t,
		v2,
		v2keys.DeriveID("conversation", signalAccountID, testSignalConversation),
		testOutgoingTimestamp,
	)
	alias := v2keys.SignalLocalAlias(testSignalConversation, testOutgoingTimestamp)
	repository := newSignalMessageRepository(t, v2)
	if err := repository.ImportMessage(ctx, sqlite.MessageProjection{
		Message: sqlite.Message{
			MessageID: v2keys.DeriveID(
				"message",
				signalAccountID,
				testSignalConversation+"\x1f"+alias,
			),
			ConversationID:  conversationID,
			AccountID:       signalAccountID,
			RemoteMessageID: alias,
			Direction:       sqlite.MessageDirectionOutgoing,
			Body:            "migrated by R2",
			State:           sqlite.MessageStateActive,
			OccurredAtMS:    testOutgoingTimestamp,
		},
	}); err != nil {
		t.Fatalf("seed migrated message: %v", err)
	}
	seedLegacySignalConversation(t, legacy, testOutgoingTimestamp)
	seedLegacySignalMessage(t, legacy, db.Message{
		MessageID:      "signal:legacy-outgoing-alias",
		ConversationID: testSignalConversation,
		TimestampMS:    testOutgoingTimestamp,
		IsFromMe:       true,
		Body:           "migrated by R2",
		SourcePlatform: signalPlatform,
		SourceID:       alias,
	})

	report, err := Signal(ctx, Options{Legacy: legacy, V2: v2, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("Signal(): %v", err)
	}
	if report.MessagesImported != 0 || report.MessagesAlreadyPresent != 1 {
		t.Fatalf("report = %+v, want the alias row recognized", report)
	}
	stored, err := repository.ListMessagesByConversation(ctx, conversationID, 0, "", 100)
	if err != nil {
		t.Fatalf("ListMessagesByConversation(): %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored messages = %d, want exactly one — no duplicate", len(stored))
	}
}
