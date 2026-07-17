package v2read

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const sourceTestTimeMS int64 = 2_000_000_000_000

var _ readsource.ReadSource = (*Source)(nil)

func TestSourceMapsConversationAndMessagesToLegacyDTOs(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "google-account", "google_messages")
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conversation-google",
		AccountID:            "google-account",
		RemoteConversationID: "remote-google",
		Kind:                 sqlite.ConversationKindGroup,
		Title:                "Byte-exact group",
		NotificationMode:     sqlite.NotificationModeMuted,
		IsFavorite:           true,
		LastMessageAtMS:      300,
	})
	identity := sqlite.Identity{
		IdentityID:     "identity-alice",
		AccountID:      "google-account",
		Kind:           sqlite.IdentityKind("e164"),
		CanonicalValue: "+15550000001",
		RawValue:       "+1 (555) 000-0001",
		DisplayName:    "Alice",
		MetadataJSON:   `{}`,
		CreatedAtMS:    sourceTestTimeMS,
		UpdatedAtMS:    sourceTestTimeMS,
	}
	if err := store.UpsertIdentity(identity); err != nil {
		t.Fatalf("UpsertIdentity(): %v", err)
	}
	if err := store.ReplaceConversationParticipants(
		"conversation-google",
		[]sqlite.ConversationParticipant{{
			AccountID:      "google-account",
			ConversationID: "conversation-google",
			IdentityID:     identity.IdentityID,
			Role:           sqlite.ParticipantRoleMember,
			DisplayName:    "Alice in group",
			IsActive:       true,
		}},
	); err != nil {
		t.Fatalf("ReplaceConversationParticipants(): %v", err)
	}

	replyRemoteID := "remote-parent"
	incoming := sqlite.Message{
		MessageID:        "message-incoming",
		ConversationID:   "conversation-google",
		AccountID:        "google-account",
		RemoteMessageID:  "remote-incoming",
		SenderIdentityID: stringPointer(identity.IdentityID),
		Direction:        sqlite.MessageDirectionIncoming,
		Body:             "body\x00kept byte-for-byte",
		ReplyToRemoteID:  &replyRemoteID,
		State:            sqlite.MessageStateEdited,
		OccurredAtMS:     200,
	}
	size := int64(123)
	importSourceMessage(t, messages, incoming, sqlite.MessageAttachment{
		Ordinal:   0,
		RemoteID:  "remote-media",
		RemoteRef: []byte("opaque"),
		Filename:  "photo.png",
		MIME:      "image/png",
		SizeBytes: &size,
	})
	outgoing := sqlite.Message{
		MessageID:       "message-outgoing",
		ConversationID:  "conversation-google",
		AccountID:       "google-account",
		RemoteMessageID: "remote-outgoing",
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            "sent by me",
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    300,
	}
	importSourceMessage(t, messages, outgoing)

	conversation, err := source.GetConversation("conversation-google")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if conversation.ConversationID != "conversation-google" ||
		conversation.Name != "Byte-exact group" ||
		!conversation.IsGroup ||
		conversation.LastMessageTS != 300 ||
		conversation.SourcePlatform != "sms" ||
		!conversation.IsFavorite ||
		conversation.NotificationMode != "muted" ||
		conversation.UnreadCount != 0 ||
		conversation.Tab != "" {
		t.Fatalf("mapped conversation = %+v", conversation)
	}
	if _, err := source.GetConversation("missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetConversation(missing) error = %v, want sql.ErrNoRows", err)
	}
	var participants []struct {
		Name   string `json:"name"`
		Number string `json:"number"`
	}
	if err := json.Unmarshal([]byte(conversation.Participants), &participants); err != nil {
		t.Fatalf("decode Participants %q: %v", conversation.Participants, err)
	}
	if got, want := participants, []struct {
		Name   string `json:"name"`
		Number string `json:"number"`
	}{{Name: "Alice in group", Number: "+15550000001"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("participants = %+v, want %+v", got, want)
	}

	gotMessages, err := source.GetMessagesByConversation("conversation-google", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(gotMessages) != 2 {
		t.Fatalf("mapped messages = %d, want 2: %+v", len(gotMessages), gotMessages)
	}
	gotOutgoing := gotMessages[0]
	if gotOutgoing.MessageID != outgoing.MessageID ||
		!gotOutgoing.IsFromMe ||
		gotOutgoing.SenderName != "" ||
		gotOutgoing.SenderNumber != "" ||
		gotOutgoing.SourcePlatform != "sms" ||
		gotOutgoing.SourceID != outgoing.RemoteMessageID {
		t.Fatalf("mapped outgoing = %+v", gotOutgoing)
	}
	gotIncoming := gotMessages[1]
	if gotIncoming.MessageID != incoming.MessageID ||
		gotIncoming.ConversationID != incoming.ConversationID ||
		gotIncoming.Body != incoming.Body ||
		gotIncoming.TimestampMS != incoming.OccurredAtMS ||
		gotIncoming.IsFromMe ||
		gotIncoming.SenderName != identity.DisplayName ||
		gotIncoming.SenderNumber != identity.CanonicalValue ||
		gotIncoming.SourcePlatform != "sms" ||
		gotIncoming.SourceID != incoming.RemoteMessageID ||
		gotIncoming.ReplyToID != replyRemoteID ||
		gotIncoming.MediaID != "v2msg:message-incoming:0" ||
		gotIncoming.MimeType != "image/png" ||
		gotIncoming.Status != "" ||
		gotIncoming.Reactions != "" {
		t.Fatalf("mapped incoming = %+v", gotIncoming)
	}
}

func TestSourceCrossAccountRecencyPaginationAndLIKEFilters(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "google-account", "google_messages")
	seedSourceAccount(t, store, "whatsapp-account", "whatsmeow")
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conversation-google",
		AccountID:            "google-account",
		RemoteConversationID: "remote-google",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Google",
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      500,
	})
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conversation-whatsapp",
		AccountID:            "whatsapp-account",
		RemoteConversationID: "remote-whatsapp",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "WhatsApp",
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      500,
	})
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conversation-old",
		AccountID:            "google-account",
		RemoteConversationID: "remote-old",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Old",
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      100,
	})
	for _, identity := range []sqlite.Identity{
		{
			IdentityID:     "alice-google",
			AccountID:      "google-account",
			Kind:           sqlite.IdentityKind("e164"),
			CanonicalValue: "+15550000001",
			RawValue:       "+15550000001",
			DisplayName:    "Alice",
			MetadataJSON:   `{}`,
			CreatedAtMS:    sourceTestTimeMS,
			UpdatedAtMS:    sourceTestTimeMS,
		},
		{
			IdentityID:     "bob-google",
			AccountID:      "google-account",
			Kind:           sqlite.IdentityKind("e164"),
			CanonicalValue: "+15550000002",
			RawValue:       "+15550000002",
			DisplayName:    "Bob",
			MetadataJSON:   `{}`,
			CreatedAtMS:    sourceTestTimeMS,
			UpdatedAtMS:    sourceTestTimeMS,
		},
	} {
		if err := store.UpsertIdentity(identity); err != nil {
			t.Fatalf("UpsertIdentity(%q): %v", identity.IdentityID, err)
		}
	}

	for _, item := range []struct {
		id       string
		ts       int64
		body     string
		senderID string
	}{
		{id: "message-old", ts: 100, body: "Needle oldest", senderID: "alice-google"},
		{id: "message-tie-a", ts: 200, body: "needle from Bob", senderID: "bob-google"},
		{id: "message-tie-b", ts: 200, body: "NEEDLE from Alice", senderID: "alice-google"},
		{id: "message-new", ts: 300, body: "needle newest", senderID: "alice-google"},
	} {
		importSourceMessage(t, messages, sqlite.Message{
			MessageID:        item.id,
			ConversationID:   "conversation-google",
			AccountID:        "google-account",
			RemoteMessageID:  "remote-" + item.id,
			SenderIdentityID: stringPointer(item.senderID),
			Direction:        sqlite.MessageDirectionIncoming,
			Body:             item.body,
			State:            sqlite.MessageStateActive,
			OccurredAtMS:     item.ts,
		})
	}

	conversations, err := source.ListConversations(3)
	if err != nil {
		t.Fatalf("ListConversations(): %v", err)
	}
	assertConversationIDs(
		t,
		conversations,
		"conversation-google",
		"conversation-whatsapp",
		"conversation-old",
	)

	latest, err := source.GetMessagesByConversation("conversation-google", 3)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	assertMessageIDs(t, latest, "message-new", "message-tie-b", "message-tie-a")
	before, err := source.GetMessagesByConversationBefore(
		"conversation-google", 200, "message-tie-b", 10,
	)
	if err != nil {
		t.Fatalf("GetMessagesByConversationBefore(): %v", err)
	}
	assertMessageIDs(t, before, "message-tie-a", "message-old")
	after, err := source.GetMessagesByConversationAfter(
		"conversation-google", 200, "message-tie-a", 10,
	)
	if err != nil {
		t.Fatalf("GetMessagesByConversationAfter(): %v", err)
	}
	assertMessageIDs(t, after, "message-tie-b", "message-new")
	around, err := source.GetMessagesAroundMessage(
		"conversation-google", "message-tie-b", 2, 1,
	)
	if err != nil {
		t.Fatalf("GetMessagesAroundMessage(): %v", err)
	}
	assertMessageIDs(t, around, "message-old", "message-tie-a", "message-tie-b", "message-new")
	if _, err := source.GetMessagesAroundMessage(
		"conversation-whatsapp", "message-tie-b", 1, 1,
	); !errors.Is(err, db.ErrMessageNotFound) {
		t.Fatalf("wrong-conversation around error = %v, want db.ErrMessageNotFound", err)
	}

	searchResults, err := source.SearchMessagesFiltered("needle", db.SearchFilter{
		Phone:          "+15550000001",
		ConversationID: "conversation-google",
		SinceMS:        150,
		UntilMS:        350,
		Limit:          10,
	})
	if err != nil {
		t.Fatalf("SearchMessagesFiltered(): %v", err)
	}
	assertMessageIDs(t, searchResults, "message-new", "message-tie-b")
}

func TestSourceStatsCountsLatestAndPreviewsPageAllMessages(t *testing.T) {
	store, messages, source := openSourceTestStore(t)
	seedSourceAccount(t, store, "google-account", "google_messages")
	seedSourceAccount(t, store, "whatsapp-account", "whatsmeow")
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conversation-google",
		AccountID:            "google-account",
		RemoteConversationID: "remote-google",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Google",
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      1_000,
	})
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conversation-google-empty",
		AccountID:            "google-account",
		RemoteConversationID: "remote-google-empty",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "Empty",
		NotificationMode:     sqlite.NotificationModeAll,
	})
	seedSourceConversation(t, store, sqlite.Conversation{
		ConversationID:       "conversation-whatsapp",
		AccountID:            "whatsapp-account",
		RemoteConversationID: "remote-whatsapp",
		Kind:                 sqlite.ConversationKindDirect,
		Title:                "WhatsApp",
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      2_000,
	})

	for index := 0; index < sourceMessagePageSize+1; index++ {
		direction := sqlite.MessageDirectionIncoming
		if index == sourceMessagePageSize {
			direction = sqlite.MessageDirectionOutgoing
		}
		body := fmt.Sprintf("message %03d", index)
		if index == sourceMessagePageSize {
			body = "  Latest\n  outgoing   preview  "
		}
		importSourceMessage(t, messages, sqlite.Message{
			MessageID:       fmt.Sprintf("google-message-%03d", index),
			ConversationID:  "conversation-google",
			AccountID:       "google-account",
			RemoteMessageID: fmt.Sprintf("remote-google-%03d", index),
			Direction:       direction,
			Body:            body,
			State:           sqlite.MessageStateActive,
			OccurredAtMS:    int64(index + 1),
		})
	}
	whatsapp := sqlite.Message{
		MessageID:       "whatsapp-message",
		ConversationID:  "conversation-whatsapp",
		AccountID:       "whatsapp-account",
		RemoteMessageID: "remote-whatsapp-message",
		Direction:       sqlite.MessageDirectionIncoming,
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    2_000,
	}
	importSourceMessage(t, messages, whatsapp, sqlite.MessageAttachment{
		Ordinal:   0,
		RemoteID:  "remote-video",
		RemoteRef: []byte("video-ref"),
		Filename:  "clip.mp4",
		MIME:      "video/mp4",
	})

	wantSMSMessages := sourceMessagePageSize + 1
	for _, test := range []struct {
		platform string
		want     int
	}{
		{platform: "", want: wantSMSMessages + 1},
		{platform: "sms", want: wantSMSMessages},
		{platform: "whatsapp", want: 1},
		{platform: "signal", want: 0},
	} {
		got, err := source.MessageCount(test.platform)
		if err != nil {
			t.Fatalf("MessageCount(%q): %v", test.platform, err)
		}
		if got != test.want {
			t.Fatalf("MessageCount(%q) = %d, want %d", test.platform, got, test.want)
		}
	}
	for _, test := range []struct {
		platform string
		want     int
	}{
		{platform: "", want: 3},
		{platform: "sms", want: 2},
		{platform: "whatsapp", want: 1},
		{platform: "signal", want: 0},
	} {
		got, err := source.ConversationCount(test.platform)
		if err != nil {
			t.Fatalf("ConversationCount(%q): %v", test.platform, err)
		}
		if got != test.want {
			t.Fatalf("ConversationCount(%q) = %d, want %d", test.platform, got, test.want)
		}
	}
	latestSMS, err := source.LatestTimestamp("sms")
	if err != nil {
		t.Fatalf("LatestTimestamp(sms): %v", err)
	}
	if latestSMS != int64(sourceMessagePageSize+1) {
		t.Fatalf("LatestTimestamp(sms) = %d, want %d", latestSMS, sourceMessagePageSize+1)
	}
	latestMissing, err := source.LatestTimestamp("signal")
	if err != nil {
		t.Fatalf("LatestTimestamp(signal): %v", err)
	}
	if latestMissing != 0 {
		t.Fatalf("LatestTimestamp(signal) = %d, want 0", latestMissing)
	}

	stats, err := source.PlatformStats()
	if err != nil {
		t.Fatalf("PlatformStats(): %v", err)
	}
	wantStats := []db.PlatformStat{
		{Platform: "whatsapp", Count: 1, LatestMS: 2_000, LatestRecvMS: 2_000},
		{
			Platform:     "sms",
			Count:        wantSMSMessages,
			LatestMS:     int64(sourceMessagePageSize + 1),
			LatestRecvMS: int64(sourceMessagePageSize),
		},
	}
	if !reflect.DeepEqual(stats, wantStats) {
		t.Fatalf("PlatformStats() = %+v, want %+v", stats, wantStats)
	}

	previews, err := source.LatestConversationPreviews([]string{
		" conversation-google ",
		"conversation-whatsapp",
		"conversation-google-empty",
		"conversation-google",
		"",
	})
	if err != nil {
		t.Fatalf("LatestConversationPreviews(): %v", err)
	}
	wantPreviews := map[string]string{
		"conversation-google":   "You: Latest outgoing preview",
		"conversation-whatsapp": "Video",
	}
	if !reflect.DeepEqual(previews, wantPreviews) {
		t.Fatalf("LatestConversationPreviews() = %#v, want %#v", previews, wantPreviews)
	}
}

func TestPlatformForBridgeKey(t *testing.T) {
	for _, test := range []struct {
		bridgeKey string
		want      string
	}{
		{bridgeKey: "google_messages", want: "sms"},
		{bridgeKey: "whatsmeow", want: "whatsapp"},
		{bridgeKey: "signal_cli", want: "signal"},
		{bridgeKey: "gchat", want: "gchat"},
		{bridgeKey: "imessage", want: "imessage"},
		{bridgeKey: "custom_bridge", want: "custom_bridge"},
	} {
		t.Run(test.bridgeKey, func(t *testing.T) {
			if got := platformForBridgeKey(test.bridgeKey); got != test.want {
				t.Fatalf("platformForBridgeKey(%q) = %q, want %q", test.bridgeKey, got, test.want)
			}
		})
	}
}

func openSourceTestStore(t *testing.T) (*sqlite.Store, *sqlite.MessageRepository, *Source) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close(): %v", err)
		}
	})
	messages, err := sqlite.NewMessageRepository(
		store,
		func() time.Time { return time.UnixMilli(sourceTestTimeMS) },
	)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	return store, messages, New(store)
}

func seedSourceAccount(t *testing.T, store *sqlite.Store, accountID, bridgeKey string) {
	t.Helper()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   accountID,
		BridgeKey:   bridgeKey,
		DisplayName: accountID,
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: sourceTestTimeMS,
		UpdatedAtMS: sourceTestTimeMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(%q): %v", accountID, err)
	}
}

func seedSourceConversation(t *testing.T, store *sqlite.Store, conversation sqlite.Conversation) {
	t.Helper()
	if conversation.MetadataJSON == "" {
		conversation.MetadataJSON = `{}`
	}
	if conversation.CreatedAtMS == 0 {
		conversation.CreatedAtMS = sourceTestTimeMS
	}
	if conversation.UpdatedAtMS == 0 {
		conversation.UpdatedAtMS = sourceTestTimeMS
	}
	if err := store.UpsertConversation(conversation); err != nil {
		t.Fatalf("UpsertConversation(%q): %v", conversation.ConversationID, err)
	}
}

func importSourceMessage(
	t *testing.T,
	repository *sqlite.MessageRepository,
	message sqlite.Message,
	attachments ...sqlite.MessageAttachment,
) {
	t.Helper()
	if err := repository.ImportMessage(context.Background(), sqlite.MessageProjection{
		Message:     message,
		Attachments: attachments,
	}); err != nil {
		t.Fatalf("ImportMessage(%q): %v", message.MessageID, err)
	}
}

func assertConversationIDs(t *testing.T, conversations []*db.Conversation, want ...string) {
	t.Helper()
	if len(conversations) != len(want) {
		t.Fatalf("conversation IDs = %d rows %+v, want %d %v", len(conversations), conversations, len(want), want)
	}
	for i, wantID := range want {
		if conversations[i].ConversationID != wantID {
			t.Fatalf("conversation ID %d = %q, want %q", i, conversations[i].ConversationID, wantID)
		}
	}
}

func assertMessageIDs(t *testing.T, messages []*db.Message, want ...string) {
	t.Helper()
	if len(messages) != len(want) {
		t.Fatalf("message IDs = %d rows %+v, want %d %v", len(messages), messages, len(want), want)
	}
	for i, wantID := range want {
		if messages[i].MessageID != wantID {
			t.Fatalf("message ID %d = %q, want %q", i, messages[i].MessageID, wantID)
		}
	}
}

func stringPointer(value string) *string {
	return &value
}

func TestFormatLastMessagePreviewMatchesLegacyRules(t *testing.T) {
	long := strings.Repeat("界", 130)
	got := formatLastMessagePreview(long, "", "", false)
	if runes := []rune(got); len(runes) != 120 || runes[len(runes)-1] != '…' {
		t.Fatalf("long preview = %q (%d runes), want 120 runes ending ellipsis", got, len([]rune(got)))
	}
	if got := formatLastMessagePreview("", "v2msg:m:0", "audio/ogg", true); got != "You: Audio" {
		t.Fatalf("audio preview = %q, want %q", got, "You: Audio")
	}
}
