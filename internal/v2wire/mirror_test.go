package v2wire

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestAccountForConversationUsesLegacyRoutingPredicates(t *testing.T) {
	legacy := openLegacyTestStore(t)
	for _, conversation := range []*db.Conversation{
		{ConversationID: "whatsapp:prefixed", SourcePlatform: "sms"},
		{ConversationID: "signal:+15551230000", SourcePlatform: "sms"},
		{ConversationID: "signal-group:group-id", SourcePlatform: "sms", IsGroup: true},
		{ConversationID: "stored-whatsapp", SourcePlatform: "whatsapp"},
		{ConversationID: "stored-signal", SourcePlatform: "signal"},
		{ConversationID: "stored-google", SourcePlatform: "sms"},
		{ConversationID: "stored-imessage", SourcePlatform: "imessage"},
		{ConversationID: "stored-gchat", SourcePlatform: "gchat"},
	} {
		if err := legacy.UpsertConversation(conversation); err != nil {
			t.Fatalf("UpsertConversation(%q): %v", conversation.ConversationID, err)
		}
	}

	tests := []struct {
		conversationID string
		wantAccountID  string
		wantErr        error
	}{
		{conversationID: "whatsapp:prefixed", wantAccountID: whatsappAccountID},
		{conversationID: "signal:+15551230000", wantAccountID: signalAccountID},
		{conversationID: "signal-group:group-id", wantAccountID: signalAccountID},
		{conversationID: "stored-whatsapp", wantAccountID: whatsappAccountID},
		{conversationID: "stored-signal", wantAccountID: signalAccountID},
		{conversationID: "stored-google", wantAccountID: googleAccountID},
		{conversationID: "stored-imessage", wantErr: ErrPlatformNotSendable},
		{conversationID: "stored-gchat", wantErr: ErrPlatformNotSendable},
	}
	for _, test := range tests {
		t.Run(test.conversationID, func(t *testing.T) {
			got, err := AccountForConversation(legacy, test.conversationID)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("AccountForConversation() error = %v, want %v", err, test.wantErr)
			}
			if got != test.wantAccountID {
				t.Fatalf("AccountForConversation() = %q, want %q", got, test.wantAccountID)
			}
		})
	}
}

func TestMirrorConversationCreatesPerAccountDevicesAndCursors(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	seedLegacyConversation(t, legacy, "whatsapp:chat", "whatsapp", false)
	seedLegacyConversation(t, legacy, "signal-group:chat", "signal", true)

	for _, test := range []struct {
		conversationID string
		accountID      string
		bridgeKey      string
		kind           sqlite.ConversationKind
	}{
		{conversationID: "whatsapp:chat", accountID: whatsappAccountID, bridgeKey: "whatsapp", kind: sqlite.ConversationKindDirect},
		{conversationID: "signal-group:chat", accountID: signalAccountID, bridgeKey: "signal", kind: sqlite.ConversationKindGroup},
	} {
		accountID, conversationID, err := MirrorConversation(legacy, v2, test.conversationID)
		if err != nil {
			t.Fatalf("MirrorConversation(%q): %v", test.conversationID, err)
		}
		if accountID != test.accountID || conversationID != test.conversationID {
			t.Fatalf("MirrorConversation(%q) = (%q,%q), want (%q,%q)", test.conversationID, accountID, conversationID, test.accountID, test.conversationID)
		}
		account, err := v2.GetAccount(test.accountID)
		if err != nil {
			t.Fatalf("GetAccount(%q): %v", test.accountID, err)
		}
		if account.BridgeKey != test.bridgeKey || account.Mode != sqlite.AccountModeLive || !account.Enabled {
			t.Fatalf("mirrored account = %+v", account)
		}
		conversation, err := v2.GetConversation(test.conversationID)
		if err != nil {
			t.Fatalf("GetConversation(%q): %v", test.conversationID, err)
		}
		if conversation.AccountID != test.accountID ||
			conversation.RemoteConversationID != test.conversationID ||
			conversation.Kind != test.kind {
			t.Fatalf("mirrored conversation = %+v", conversation)
		}
		device, err := v2.GetDevice(localDeviceID(test.accountID))
		if err != nil {
			t.Fatalf("GetDevice(%q): %v", localDeviceID(test.accountID), err)
		}
		if device.AccountID != test.accountID || device.Kind != sqlite.DeviceKindLocalInstallation {
			t.Fatalf("mirrored device = %+v", device)
		}

		const readAtMS int64 = 1_910_000_000_000
		if err := MirrorReadCursor(context.Background(), legacy, v2, test.conversationID, readAtMS); err != nil {
			t.Fatalf("MirrorReadCursor(%q): %v", test.conversationID, err)
		}
		cursor, err := v2.GetReadCursor(localDeviceID(test.accountID), test.conversationID)
		if err != nil {
			t.Fatalf("GetReadCursor(%q): %v", test.conversationID, err)
		}
		if cursor.AccountID != test.accountID || cursor.LastReadAtMS != readAtMS || cursor.LastReadMessageID != nil {
			t.Fatalf("mirrored cursor = %+v", cursor)
		}
	}

	first, err := v2.GetDevice(localDeviceID(whatsappAccountID))
	if err != nil {
		t.Fatalf("GetDevice(WhatsApp) after Signal mirror: %v", err)
	}
	if first.AccountID != whatsappAccountID {
		t.Fatalf("WhatsApp device account = %q, want %q", first.AccountID, whatsappAccountID)
	}
}

func TestMirrorReplyTargetDerivesRemoteIDsAndTypedErrors(t *testing.T) {
	tests := []struct {
		name           string
		conversationID string
		platform       string
		message        *db.Message
		wantRemoteID   string
		wantErr        error
	}{
		{
			name:           "google message id",
			conversationID: "google-chat",
			platform:       "sms",
			message:        &db.Message{MessageID: "google-remote", Body: "google body", TimestampMS: 1_900_000_000_001},
			wantRemoteID:   "google-remote",
		},
		{
			name:           "whatsapp source id",
			conversationID: "whatsapp:chat",
			platform:       "whatsapp",
			message:        &db.Message{MessageID: "whatsapp:wa-remote", SourceID: "wa-remote", Body: "wa body", TimestampMS: 1_900_000_000_002},
			wantRemoteID:   "wa-remote",
		},
		{
			name:           "signal full legacy id",
			conversationID: "signal:+15551230000",
			platform:       "signal",
			message:        &db.Message{MessageID: "signal:1700000000123", SourceID: "1700000000123", Body: "signal body", TimestampMS: 1_700_000_000_123},
			wantRemoteID:   "signal:1700000000123",
		},
		{
			name:           "signal local unavailable",
			conversationID: "signal:+15551230000",
			platform:       "signal",
			message:        &db.Message{MessageID: "signal:local:abc", SourceID: "local:abc", Body: "signal body", TimestampMS: 1_900_000_000_003},
			wantErr:        ErrReplyTargetUnavailable,
		},
		{
			name:           "whatsapp missing source",
			conversationID: "whatsapp:chat",
			platform:       "whatsapp",
			message:        &db.Message{MessageID: "whatsapp:missing", Body: "wa body", TimestampMS: 1_900_000_000_004},
			wantErr:        ErrReplyTargetUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			legacy := openLegacyTestStore(t)
			v2 := openV2TestStore(t)
			seedLegacyConversation(t, legacy, test.conversationID, test.platform, false)
			test.message.ConversationID = test.conversationID
			test.message.SourcePlatform = test.platform
			if err := legacy.UpsertMessage(test.message); err != nil {
				t.Fatalf("UpsertMessage(): %v", err)
			}

			messageID, err := MirrorReplyTarget(legacy, v2, test.message.MessageID)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("MirrorReplyTarget() error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil {
				return
			}
			repository, err := sqlite.NewMessageRepository(v2, time.Now)
			if err != nil {
				t.Fatalf("NewMessageRepository(): %v", err)
			}
			got, err := repository.GetMessage(context.Background(), messageID)
			if err != nil {
				t.Fatalf("GetMessage(%q): %v", messageID, err)
			}
			if got.RemoteMessageID != test.wantRemoteID || got.Body != test.message.Body || got.SenderIdentityID != nil {
				t.Fatalf("mirrored reply target = %+v, want remote %q", got, test.wantRemoteID)
			}
		})
	}
}

func TestMirrorReplyTargetReturnsExistingNaturalKeyMessageID(t *testing.T) {
	legacy := openLegacyTestStore(t)
	v2 := openV2TestStore(t)
	seedLegacyConversation(t, legacy, "whatsapp:chat", "whatsapp", false)
	legacyMessage := &db.Message{
		MessageID: "whatsapp:remote-existing", ConversationID: "whatsapp:chat",
		SourcePlatform: "whatsapp", SourceID: "remote-existing",
		Body: "already present", TimestampMS: 1_900_000_000_010,
	}
	if err := legacy.UpsertMessage(legacyMessage); err != nil {
		t.Fatalf("legacy UpsertMessage(): %v", err)
	}
	if _, _, err := MirrorConversation(legacy, v2, legacyMessage.ConversationID); err != nil {
		t.Fatalf("MirrorConversation(): %v", err)
	}
	const existingLocalID = "existing-v2-message"
	projectV2TestMessage(t, v2, sqlite.Message{
		MessageID: existingLocalID, ConversationID: legacyMessage.ConversationID,
		AccountID: whatsappAccountID, RemoteMessageID: legacyMessage.SourceID,
		Direction: sqlite.MessageDirectionOutgoing, Body: legacyMessage.Body,
		State: sqlite.MessageStateActive, OccurredAtMS: legacyMessage.TimestampMS,
	})

	got, err := MirrorReplyTarget(legacy, v2, legacyMessage.MessageID)
	if err != nil {
		t.Fatalf("MirrorReplyTarget(): %v", err)
	}
	if got != existingLocalID {
		t.Fatalf("MirrorReplyTarget() = %q, want existing %q", got, existingLocalID)
	}
}

func openLegacyTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func openV2TestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedLegacyConversation(t *testing.T, store *db.Store, conversationID, platform string, group bool) {
	t.Helper()
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Conversation " + conversationID,
		IsGroup:        group,
		Participants:   `[]`,
		LastMessageTS:  1_900_000_000_000,
		SourcePlatform: platform,
	}); err != nil {
		t.Fatalf("UpsertConversation(%q): %v", conversationID, err)
	}
}

func projectV2TestMessage(t *testing.T, store *sqlite.Store, message sqlite.Message) {
	t.Helper()
	repository, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	inboxID := "inbox-" + message.MessageID
	if _, err := repository.AppendInbox(context.Background(), sqlite.InboxRecord{
		InboxID: inboxID, AccountID: message.AccountID, Generation: 1,
		DedupeKey: inboxID, Codec: "test", CodecVersion: 1, Payload: []byte("test"),
	}); err != nil {
		t.Fatalf("AppendInbox(): %v", err)
	}
	if err := repository.ProjectMessage(context.Background(), sqlite.MessageProjection{
		InboxID: inboxID,
		Message: message,
	}); err != nil {
		t.Fatalf("ProjectMessage(): %v", err)
	}
}
