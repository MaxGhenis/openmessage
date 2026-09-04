package tools

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// The person tools against a real v2 store, on the shape the fakes can only
// imitate: a titleless direct thread whose self participant sorts before the
// peer (ListParticipants orders by identity_id). The label must name the
// peer, never the account owner.
func TestPersonToolsLabelTitlelessV2DirectThreadByPeer(t *testing.T) {
	a := testApp(t)
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: "wa-account", BridgeKey: "whatsmeow", DisplayName: "WhatsApp",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: now, UpdatedAtMS: now,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID: "conv-direct", AccountID: "wa-account", RemoteConversationID: "remote-direct",
		Kind: sqlite.ConversationKindDirect, Title: "", NotificationMode: sqlite.NotificationModeAll,
		MetadataJSON: "{}", LastMessageAtMS: now, CreatedAtMS: now, UpdatedAtMS: now,
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	for _, identity := range []sqlite.Identity{
		{IdentityID: "identity-aaa-self", AccountID: "wa-account", Kind: sqlite.IdentityKind("e164"),
			CanonicalValue: "+15550009999", RawValue: "+15550009999", DisplayName: "Me", IsSelf: true,
			MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now},
		{IdentityID: "identity-zzz-peer", AccountID: "wa-account", Kind: sqlite.IdentityKind("e164"),
			CanonicalValue: "+15550000001", RawValue: "+15550000001", DisplayName: "Real Peer",
			MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now},
	} {
		if err := store.UpsertIdentity(identity); err != nil {
			t.Fatalf("UpsertIdentity(%s): %v", identity.IdentityID, err)
		}
	}
	if err := store.ReplaceConversationParticipants("conv-direct", []sqlite.ConversationParticipant{
		{AccountID: "wa-account", ConversationID: "conv-direct", IdentityID: "identity-aaa-self", Role: sqlite.ParticipantRoleMember, IsActive: true},
		{AccountID: "wa-account", ConversationID: "conv-direct", IdentityID: "identity-zzz-peer", Role: sqlite.ParticipantRoleMember, IsActive: true},
	}); err != nil {
		t.Fatalf("ReplaceConversationParticipants(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	peerID := "identity-zzz-peer"
	if err := messages.ImportMessage(context.Background(), sqlite.MessageProjection{Message: sqlite.Message{
		MessageID: "msg-1", ConversationID: "conv-direct", AccountID: "wa-account", RemoteMessageID: "remote-msg-1",
		SenderIdentityID: &peerID, Direction: sqlite.MessageDirectionIncoming, Body: "hello from the peer",
		State: sqlite.MessageStateActive, OccurredAtMS: now,
	}}); err != nil {
		t.Fatalf("ImportMessage(): %v", err)
	}

	options := Options{Reads: v2read.New(store), V2Primary: true}
	for name, handler := range map[string]func() (string, error){
		"get_person_messages": func() (string, error) {
			result, err := getPersonMessagesHandler(a, options)(context.Background(), toolRequest(map[string]any{"name": "real peer"}))
			if err != nil {
				return "", err
			}
			return resultText(t, result), nil
		},
		"get_person_messages_range": func() (string, error) {
			day := time.UnixMilli(now)
			result, err := getPersonMessagesRangeHandler(a, options)(context.Background(), toolRequest(map[string]any{
				"name":   "real peer",
				"after":  day.Add(-24 * time.Hour).Format("2006-01-02"),
				"before": day.Add(24 * time.Hour).Format("2006-01-02"),
			}))
			if err != nil {
				return "", err
			}
			return resultText(t, result), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			text, err := handler()
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(text, "hello from the peer") {
				t.Fatalf("%s did not serve the v2 message: %q", name, text)
			}
			if !strings.Contains(text, "Real Peer [whatsapp]") {
				t.Fatalf("%s did not label the titleless thread by its peer: %q", name, text)
			}
			if strings.Contains(text, "Me [whatsapp]") || strings.Contains(text, "+15550009999 [whatsapp]") {
				t.Fatalf("%s labelled the thread with the account owner: %q", name, text)
			}
		})
	}
}
