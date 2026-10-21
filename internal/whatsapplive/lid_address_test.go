package whatsapplive

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"

	"github.com/maxghenis/openmessage/internal/db"
)

// A LID ("<digits>@lid") has an all-digit user, like a phone number, a
// new-style group or a newsletter. Only the phone-number server names a
// phone number; storing a LID's digits as "+<digits>" made LID senders look
// like phone numbers in v2.

const (
	lidAddressTestLID   = "98765432109876"
	lidAddressTestPhone = "15557654321"
	lidAddressTestGroup = "120363019999999999"
)

func TestJIDToPhoneNamesOnlyPhoneNumberJIDs(t *testing.T) {
	cases := []struct {
		jid  watypes.JID
		want string
	}{
		{watypes.NewJID(lidAddressTestPhone, watypes.DefaultUserServer), "+" + lidAddressTestPhone},
		{watypes.NewADJID(lidAddressTestPhone, 0, 12), "+" + lidAddressTestPhone},
		{watypes.NewJID(lidAddressTestLID, watypes.HiddenUserServer), ""},
		{watypes.NewJID(lidAddressTestGroup, watypes.GroupServer), ""},
		{watypes.NewJID("120363111111111111", watypes.NewsletterServer), ""},
		{watypes.NewJID(lidAddressTestPhone, watypes.HostedServer), ""},
		{watypes.NewJID(lidAddressTestLID, watypes.HostedLIDServer), ""},
		{watypes.NewJID("status", watypes.BroadcastServer), ""},
		{watypes.EmptyJID, ""},
	}
	for _, tc := range cases {
		if got := jidToPhone(tc.jid); got != tc.want {
			t.Errorf("jidToPhone(%q) = %q, want %q", tc.jid.String(), got, tc.want)
		}
	}
}

// An unnamed chat or person is named by something a real name replaces later
// (looksLikeRawIdentifier). A LID's or an all-digit group's user part alone
// would pass for a name and stick.
func TestFallbackChatNameStaysReplaceableForLIDsAndGroups(t *testing.T) {
	cases := []struct {
		jid  watypes.JID
		want string
	}{
		{watypes.NewJID(lidAddressTestPhone, watypes.DefaultUserServer), "+" + lidAddressTestPhone},
		{watypes.NewJID(lidAddressTestLID, watypes.HiddenUserServer), lidAddressTestLID + "@lid"},
		{watypes.NewJID(lidAddressTestGroup, watypes.GroupServer), lidAddressTestGroup + "@g.us"},
		{watypes.NewJID("15551234567-1585405251", watypes.GroupServer), "15551234567-1585405251@g.us"},
	}
	for _, tc := range cases {
		got := fallbackChatName(tc.jid)
		if got != tc.want {
			t.Errorf("fallbackChatName(%q) = %q, want %q", tc.jid.String(), got, tc.want)
		}
		if !looksLikeRawIdentifier(got) {
			t.Errorf("fallbackChatName(%q) = %q, which a real name would never replace", tc.jid.String(), got)
		}
		if !shouldReplaceConversationName(got, "Book club") {
			t.Errorf("a real name does not replace fallback name %q", got)
		}
	}
}

func TestAddressForJIDKeepsUnmappedLIDsAsLIDs(t *testing.T) {
	lid := watypes.NewJID(lidAddressTestLID, watypes.HiddenUserServer)
	unmapped := reactionTestBridge(reactionTestLIDs{})
	mapped := reactionTestBridge(reactionTestLIDs{lidAddressTestLID: lidAddressTestPhone})
	cases := []struct {
		name   string
		bridge *Bridge
		jid    watypes.JID
		want   string
	}{
		{"unmapped LID", unmapped, lid, lidAddressTestLID + "@lid"},
		{"unmapped LID device", unmapped, watypes.NewADJID(lidAddressTestLID, watypes.LIDDomain, 3), lidAddressTestLID + "@lid"},
		{"mapped LID", mapped, lid, "+" + lidAddressTestPhone},
		{"phone number", unmapped, watypes.NewJID(lidAddressTestPhone, watypes.DefaultUserServer), "+" + lidAddressTestPhone},
		{"hosted LID", unmapped, watypes.NewJID(lidAddressTestLID, watypes.HostedLIDServer), lidAddressTestLID + "@hosted.lid"},
		{"bot", unmapped, watypes.NewJID("867051314767696", watypes.BotServer), "867051314767696@bot"},
		{"group", unmapped, watypes.NewJID(lidAddressTestGroup, watypes.GroupServer), ""},
		{"newsletter", unmapped, watypes.NewJID("120363111111111111", watypes.NewsletterServer), ""},
		{"broadcast", unmapped, watypes.NewJID("status", watypes.BroadcastServer), ""},
		{"empty", unmapped, watypes.EmptyJID, ""},
	}
	for _, tc := range cases {
		if got := tc.bridge.addressForJID(tc.jid); got != tc.want {
			t.Errorf("%s: addressForJID(%q) = %q, want %q", tc.name, tc.jid.String(), got, tc.want)
		}
	}
}

// redactedPhoneContacts returns a contact whose only identifier is the masked
// number a LID group shows a non-contact by.
type redactedPhoneContacts struct {
	wastore.NoopStore
	redacted string
}

func (c *redactedPhoneContacts) GetContact(context.Context, watypes.JID) (watypes.ContactInfo, error) {
	return watypes.ContactInfo{Found: true, RedactedPhone: c.redacted}, nil
}

// A RedactedPhone ("+1 ••• ••21") is a display name, not a number: stored as
// a number its digits ("+121") minted a fake phone identity in v2.
func TestRedactedPhoneIsANameNotANumber(t *testing.T) {
	bridge := reactionTestBridge(reactionTestLIDs{})
	bridge.client.Store.Contacts = &redactedPhoneContacts{redacted: "+1 ••• ••21"}
	lid := watypes.NewJID(lidAddressTestLID, watypes.HiddenUserServer)

	if got := bridge.phoneForJID(lid); got != "" {
		t.Fatalf("phoneForJID(LID) = %q, want none: a redacted phone is no number", got)
	}
	if got := bridge.addressForJID(lid); got != lidAddressTestLID+"@lid" {
		t.Fatalf("addressForJID(LID) = %q, want the LID", got)
	}
	if got := bridge.contactDisplayName(lid, ""); got != "+1 ••• ••21" {
		t.Fatalf("contactDisplayName(LID) = %q, want the redacted phone as a name", got)
	}
}

func lidAddressTestMessage(chat, sender watypes.JID, id, pushName string, isGroup bool) *waevents.Message {
	return &waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{Chat: chat, Sender: sender, IsGroup: isGroup},
			ID:            watypes.MessageID(id),
			PushName:      pushName,
			Timestamp:     time.UnixMilli(1700000000000),
		},
		Message: &waE2E.Message{Conversation: strPtr("hello")},
	}
}

func lidAddressTestStoreBridge(t *testing.T, lids reactionTestLIDs) (*Bridge, *db.Store) {
	t.Helper()
	store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { store.Close() })
	bridge := reactionTestBridge(lids)
	bridge.store = store
	// The bridge is not connected, so the group enrichment it starts for a
	// new group returns without a network call.
	bridge.connected = false
	return bridge, store
}

func TestHandleMessageStoresAGroupLIDSenderByItsJIDOrNumber(t *testing.T) {
	group := watypes.NewJID(lidAddressTestGroup, watypes.GroupServer)
	lid := watypes.NewJID(lidAddressTestLID, watypes.HiddenUserServer)
	cases := []struct {
		name       string
		lids       reactionTestLIDs
		pushName   string
		wantNumber string
		wantName   string
	}{
		{"unmapped, named", reactionTestLIDs{}, "Ana", lidAddressTestLID + "@lid", "Ana"},
		{"unmapped, unnamed", reactionTestLIDs{}, "", lidAddressTestLID + "@lid", lidAddressTestLID + "@lid"},
		{"mapped", reactionTestLIDs{lidAddressTestLID: lidAddressTestPhone}, "Ana", "+" + lidAddressTestPhone, "Ana"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bridge, store := lidAddressTestStoreBridge(t, tc.lids)
			bridge.handleMessage(lidAddressTestMessage(group, lid, "group-msg", tc.pushName, true))

			msg, err := store.GetMessageByID("whatsapp:group-msg")
			if err != nil || msg == nil {
				t.Fatalf("GetMessageByID(): %v, %v", msg, err)
			}
			if msg.SenderNumber != tc.wantNumber {
				t.Fatalf("sender number = %q, want %q", msg.SenderNumber, tc.wantNumber)
			}
			if msg.SenderName != tc.wantName {
				t.Fatalf("sender name = %q, want %q", msg.SenderName, tc.wantName)
			}
			convo, err := store.GetConversation(waConversationID(group))
			if err != nil || convo == nil {
				t.Fatalf("GetConversation(): %v, %v", convo, err)
			}
			if convo.Name != lidAddressTestGroup+"@g.us" {
				t.Fatalf("group fallback name = %q, want the group JID", convo.Name)
			}
		})
	}
}

func TestHandleMessageStoresAnUnmappedLIDPeerByItsJID(t *testing.T) {
	bridge, store := lidAddressTestStoreBridge(t, reactionTestLIDs{})
	lid := watypes.NewJID(lidAddressTestLID, watypes.HiddenUserServer)
	bridge.handleMessage(lidAddressTestMessage(lid, lid, "direct-msg", "", false))

	msg, err := store.GetMessageByID("whatsapp:direct-msg")
	if err != nil || msg == nil {
		t.Fatalf("GetMessageByID(): %v, %v", msg, err)
	}
	if msg.SenderNumber != lidAddressTestLID+"@lid" {
		t.Fatalf("sender number = %q, want the LID", msg.SenderNumber)
	}
	convo, err := store.GetConversation("whatsapp:" + lidAddressTestLID + "@lid")
	if err != nil || convo == nil {
		t.Fatalf("GetConversation(): %v, %v", convo, err)
	}
	var participants []participantJSON
	if err := json.Unmarshal([]byte(convo.Participants), &participants); err != nil {
		t.Fatalf("participants %q: %v", convo.Participants, err)
	}
	if len(participants) != 1 || participants[0].Number != lidAddressTestLID+"@lid" {
		t.Fatalf("participants = %#v, want the LID as the peer's number", participants)
	}
	if strings.Contains(convo.Participants+msg.SenderNumber+msg.SenderName, "+"+lidAddressTestLID) {
		t.Fatalf("the LID was stored as a phone number: %q / %q / %q", convo.Participants, msg.SenderNumber, msg.SenderName)
	}
}

func TestGroupParticipantsJSONStoresAnUnmappedLIDMemberByItsJID(t *testing.T) {
	bridge := reactionTestBridge(reactionTestLIDs{})
	serialized, err := bridge.groupParticipantsJSON([]watypes.GroupParticipant{
		{JID: watypes.NewJID(lidAddressTestLID, watypes.HiddenUserServer), DisplayName: "+1 ••• ••21"},
		{JID: watypes.NewJID("11111111111111", watypes.HiddenUserServer), PhoneNumber: watypes.NewJID(lidAddressTestPhone, watypes.DefaultUserServer)},
	})
	if err != nil {
		t.Fatalf("groupParticipantsJSON(): %v", err)
	}
	var participants []participantJSON
	if err := json.Unmarshal([]byte(serialized), &participants); err != nil {
		t.Fatalf("participants %q: %v", serialized, err)
	}
	if len(participants) != 2 {
		t.Fatalf("participants = %#v, want 2", participants)
	}
	if participants[0].Number != lidAddressTestLID+"@lid" || participants[0].Name != "+1 ••• ••21" {
		t.Fatalf("LID member = %#v, want its LID as number and its display name as name", participants[0])
	}
	if participants[1].Number != "+"+lidAddressTestPhone {
		t.Fatalf("member with a number = %#v, want its number", participants[1])
	}
}

// The legacy reaction path reads the stored sender back. A LID stored as its
// JID must go out as that LID, not "<digits>@lid@s.whatsapp.net".
func TestLegacySendReactionNamesAStoredLIDSenderByItsLID(t *testing.T) {
	bridge, store := lidAddressTestStoreBridge(t, reactionTestLIDs{})
	bridge.connected = true
	group := "whatsapp:" + lidAddressTestGroup + "@g.us"
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:lid-target",
		ConversationID: group,
		SenderName:     "Ana",
		SenderNumber:   lidAddressTestLID + "@lid",
		Body:           "group hello",
		TimestampMS:    1700000000001,
		SourcePlatform: "whatsapp",
		SourceID:       "lid-target",
	}); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	originalSend := sendTextMessage
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }
	var captured *waE2E.Message
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, _ watypes.JID, message *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		captured = message
		return whatsmeow.SendResponse{}, nil
	}

	if err := bridge.SendReaction(group, "whatsapp:lid-target", "🔥", "add"); err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}
	reaction := extractReactionMessage(captured)
	if reaction == nil {
		t.Fatal("expected an outgoing reaction")
	}
	if got := reaction.GetKey().GetParticipant(); got != lidAddressTestLID+"@lid" {
		t.Fatalf("participant = %q, want the stored LID", got)
	}
}
