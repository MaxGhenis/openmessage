package client

import (
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestHandleMessage_RemovesOnlyMatchingTmpPlaceholder(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.UpsertMessage(&db.Message{
		MessageID:      "tmp_match",
		ConversationID: "c1",
		Body:           "pending 1",
		IsFromMe:       true,
		TimestampMS:    1000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "tmp_other",
		ConversationID: "c1",
		Body:           "pending 2",
		IsFromMe:       true,
		TimestampMS:    1001,
	}); err != nil {
		t.Fatal(err)
	}

	var (
		conversationChanges int
		messagesChangedFor  string
	)
	handler := &EventHandler{
		Store:  store,
		Logger: zerolog.Nop(),
		OnConversationsChange: func() {
			conversationChanges++
		},
		OnMessagesChange: func(conversationID string) {
			messagesChangedFor = conversationID
		},
	}

	handler.handleMessage(&libgm.WrappedMessage{
		Message: &gmproto.Message{
			MessageID:      "real_msg_1",
			ConversationID: "c1",
			Timestamp:      2000 * 1000,
			TmpID:          "tmp_match",
			SenderParticipant: &gmproto.Participant{
				IsMe:     true,
				FullName: "Me",
				ID:       &gmproto.SmallInfo{Number: "+15551234567"},
			},
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{
					MessageContent: &gmproto.MessageContent{Content: "delivered"},
				},
			}},
		},
	})

	if got, err := store.GetMessageByID("tmp_match"); err != nil {
		t.Fatalf("lookup tmp_match: %v", err)
	} else if got != nil {
		t.Fatalf("tmp_match should have been removed, got %+v", got)
	}

	if got, err := store.GetMessageByID("tmp_other"); err != nil {
		t.Fatalf("lookup tmp_other: %v", err)
	} else if got == nil {
		t.Fatal("tmp_other should remain in the store")
	}

	if got, err := store.GetMessageByID("real_msg_1"); err != nil {
		t.Fatalf("lookup real message: %v", err)
	} else if got == nil {
		t.Fatal("real echoed message should be stored")
	}
	if messagesChangedFor != "c1" {
		t.Fatalf("messages change callback conversation = %q, want c1", messagesChangedFor)
	}
	if conversationChanges != 1 {
		t.Fatalf("conversation change callback count = %d, want 1", conversationChanges)
	}
}

func TestHandleMessage_BumpsConversationTimestamp(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: "c1",
		Name:           "Alice",
		LastMessageTS:  1000,
	}); err != nil {
		t.Fatal(err)
	}

	handler := &EventHandler{
		Store:  store,
		Logger: zerolog.Nop(),
	}

	handler.handleMessage(&libgm.WrappedMessage{
		Message: &gmproto.Message{
			MessageID:      "m1",
			ConversationID: "c1",
			Timestamp:      3000 * 1000,
			SenderParticipant: &gmproto.Participant{
				IsMe:     false,
				FullName: "Alice",
				ID:       &gmproto.SmallInfo{Number: "+15551234567"},
			},
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{
					MessageContent: &gmproto.MessageContent{Content: "latest"},
				},
			}},
		},
	})

	convo, err := store.GetConversation("c1")
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if convo.LastMessageTS != 3000 {
		t.Fatalf("conversation last_message_ts = %d, want 3000", convo.LastMessageTS)
	}
}

func TestHandleRecoveryEvents_TriggerRealtimeGapCallback(t *testing.T) {
	var reasons []string
	handler := &EventHandler{
		Logger: zerolog.Nop(),
		OnRealtimeGapRecovered: func(reason string) {
			reasons = append(reasons, reason)
		},
	}

	handler.Handle(&events.ListenRecovered{})
	handler.Handle(&events.PhoneRespondingAgain{})

	if len(reasons) != 2 {
		t.Fatalf("recovery callback count = %d, want 2", len(reasons))
	}
	if reasons[0] != "listen_recovered" {
		t.Fatalf("first recovery reason = %q, want %q", reasons[0], "listen_recovered")
	}
	if reasons[1] != "phone_responding_again" {
		t.Fatalf("second recovery reason = %q, want %q", reasons[1], "phone_responding_again")
	}
}

func TestHandleMessage_NotifiesOnlyFreshIncomingMessages(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	handler := &EventHandler{
		Store:  store,
		Logger: zerolog.Nop(),
	}

	var notified []*db.Message
	handler.OnIncomingMessage = func(message *db.Message) {
		notified = append(notified, message)
	}

	handler.handleMessage(&libgm.WrappedMessage{
		Message: &gmproto.Message{
			MessageID:      "incoming-live",
			ConversationID: "c1",
			Timestamp:      1000 * 1000,
			SenderParticipant: &gmproto.Participant{
				IsMe:     false,
				FullName: "Alice",
				ID:       &gmproto.SmallInfo{Number: "+15551234567"},
			},
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{
					MessageContent: &gmproto.MessageContent{Content: "live"},
				},
			}},
		},
	})

	handler.handleMessage(&libgm.WrappedMessage{
		IsOld: true,
		Message: &gmproto.Message{
			MessageID:      "incoming-old",
			ConversationID: "c1",
			Timestamp:      2000 * 1000,
			SenderParticipant: &gmproto.Participant{
				IsMe:     false,
				FullName: "Alice",
				ID:       &gmproto.SmallInfo{Number: "+15551234567"},
			},
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{
					MessageContent: &gmproto.MessageContent{Content: "old"},
				},
			}},
		},
	})

	handler.handleMessage(&libgm.WrappedMessage{
		Message: &gmproto.Message{
			MessageID:      "outgoing-live",
			ConversationID: "c1",
			Timestamp:      3000 * 1000,
			SenderParticipant: &gmproto.Participant{
				IsMe:     true,
				FullName: "Me",
				ID:       &gmproto.SmallInfo{Number: "+15550001111"},
			},
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{
					MessageContent: &gmproto.MessageContent{Content: "outgoing"},
				},
			}},
		},
	})

	if len(notified) != 1 {
		t.Fatalf("incoming notification count = %d, want 1", len(notified))
	}
	if notified[0].MessageID != "incoming-live" {
		t.Fatalf("incoming notification message_id = %q, want %q", notified[0].MessageID, "incoming-live")
	}
}

func TestHandleMessage_SchedulesPendingMediaRefreshForUndownloadedMMS(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	handler := &EventHandler{
		Store:  store,
		Logger: zerolog.Nop(),
	}

	var got struct {
		conversationID string
		messageID      string
	}
	handler.OnPendingMedia = func(conversationID, messageID string) {
		got.conversationID = conversationID
		got.messageID = messageID
	}

	handler.handleMessage(&libgm.WrappedMessage{
		Message: &gmproto.Message{
			MessageID:      "incoming-mms",
			ConversationID: "c1",
			Timestamp:      1000 * 1000,
			Type:           3,
			SenderParticipant: &gmproto.Participant{
				IsMe:     false,
				FullName: "Alice",
				ID:       &gmproto.SmallInfo{Number: "+15551234567"},
			},
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{
					MessageContent: &gmproto.MessageContent{Content: "Image from phone"},
				},
			}},
		},
	})

	if got.conversationID != "c1" {
		t.Fatalf("conversationID = %q, want c1", got.conversationID)
	}
	if got.messageID != "incoming-mms" {
		t.Fatalf("messageID = %q, want incoming-mms", got.messageID)
	}
}

func TestHandleTyping_ResolvesParticipantName(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: "c1",
		Name:           "Weekend Hiking Group",
		IsGroup:        true,
		Participants:   `[{"name":"Alice","number":"+15551234567"},{"name":"Me","number":"+15550001111","is_me":true}]`,
		LastMessageTS:  1000,
	}); err != nil {
		t.Fatal(err)
	}

	handler := &EventHandler{
		Store:  store,
		Logger: zerolog.Nop(),
	}

	var got struct {
		conversationID string
		senderName     string
		senderNumber   string
		typing         bool
	}
	handler.OnTypingChange = func(conversationID, senderName, senderNumber string, typing bool) {
		got.conversationID = conversationID
		got.senderName = senderName
		got.senderNumber = senderNumber
		got.typing = typing
	}

	handler.Handle(&gmproto.TypingData{
		ConversationID: "c1",
		User:           &gmproto.User{Number: "+15551234567"},
		Type:           gmproto.TypingTypes_STARTED_TYPING,
	})

	if got.conversationID != "c1" {
		t.Fatalf("conversationID = %q, want c1", got.conversationID)
	}
	if got.senderName != "Alice" {
		t.Fatalf("senderName = %q, want Alice", got.senderName)
	}
	if got.senderNumber != "+15551234567" {
		t.Fatalf("senderNumber = %q, want +15551234567", got.senderNumber)
	}
	if !got.typing {
		t.Fatal("typing = false, want true")
	}
}
