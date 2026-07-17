package whatsapplive

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	waWeb "go.mau.fi/whatsmeow/proto/waWeb"
	wastore "go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestObserveIngressIsSingleSlotAndRemovalCannotClearReplacement(t *testing.T) {
	bridge := &Bridge{}
	var firstCalls, secondCalls int
	removeFirst := bridge.ObserveIngress(func(IngressFrame) {
		firstCalls++
	})
	removeSecond := bridge.ObserveIngress(func(frame IngressFrame) {
		secondCalls++
		frame.Payload[0] = 'X'
	})

	// Removing the replaced observer must leave the current slot installed.
	removeFirst()
	payload := []byte("payload")
	bridge.emitIngress(IngressFrame{DedupeKey: "one", Payload: payload})
	if firstCalls != 0 || secondCalls != 1 {
		t.Fatalf("observer calls = first %d, second %d; want 0, 1", firstCalls, secondCalls)
	}
	if string(payload) != "payload" {
		t.Fatalf("observer mutated caller payload: %q", payload)
	}

	removeSecond()
	removeSecond()
	bridge.emitIngress(IngressFrame{DedupeKey: "two", Payload: []byte("ignored")})
	if secondCalls != 1 {
		t.Fatalf("removed observer calls = %d, want 1", secondCalls)
	}
}

func TestMessageIngressCaptureExactEnvelopeMentionsAndDedupe(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	groupJID := watypes.NewJID("120363019999999999", watypes.GroupServer)
	mentionedLID := watypes.NewJID("134149377675278", watypes.HiddenUserServer)
	mentionedPN := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: waConversationID(groupJID),
		Name:           "Alias Group",
		IsGroup:        true,
		Participants:   `[{"name":"Max Ghenis","number":"+15551230000"}]`,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	ownJID := watypes.NewJID("15550009999", watypes.DefaultUserServer)
	bridge := &Bridge{
		store: store,
		client: &whatsmeow.Client{Store: &wastore.Device{
			ID: &ownJID,
			LIDs: &testLIDStore{
				NoopStore: wastore.NoopStore{},
				lidToPN: map[string]watypes.JID{
					mentionedLID.String(): mentionedPN,
				},
			},
		}},
	}
	var frames []IngressFrame
	bridge.ObserveIngress(func(frame IngressFrame) {
		frames = append(frames, frame)
	})

	event := &waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{
				Chat:     groupJID,
				Sender:   watypes.NewJID("15551234567", watypes.DefaultUserServer),
				IsGroup:  true,
				IsFromMe: false,
			},
			ID:        "dedupe-message",
			PushName:  "Jenn",
			Timestamp: time.UnixMilli(1775002000000),
		},
		Message: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("hello @134149377675278"),
			ContextInfo: &waE2E.ContextInfo{
				MentionedJID: []string{mentionedLID.String()},
			},
		}},
	}
	bridge.handleMessage(event)
	bridge.handleMessage(event)

	changed := proto.Clone(event.Message).(*waE2E.Message)
	changed.ExtendedTextMessage.Text = proto.String("changed @134149377675278")
	changedEvent := *event
	changedEvent.Message = changed
	bridge.handleMessage(&changedEvent)

	if len(frames) != 3 {
		t.Fatalf("captured frames = %d, want 3", len(frames))
	}
	protoBytes, err := proto.Marshal(event.Message)
	if err != nil {
		t.Fatalf("proto.Marshal(): %v", err)
	}
	wantPayload := fmt.Sprintf(
		`{"kind":"message","chat_jid":"120363019999999999@g.us","sender_jid":"15551234567@s.whatsapp.net","message_id":"dedupe-message","timestamp_ms":1775002000000,"is_from_me":false,"is_group":true,"push_name":"Jenn","mentions":{"134149377675278@lid":"Max","15551230000@s.whatsapp.net":"Max"},"proto_b64":%q}`,
		base64.StdEncoding.EncodeToString(protoBytes),
	)
	if got := string(frames[0].Payload); got != wantPayload {
		t.Fatalf("message payload:\n got: %s\nwant: %s", got, wantPayload)
	}
	wantDedupe := "msg:dedupe-message:" + ingressHash8(protoBytes)
	if frames[0].DedupeKey != wantDedupe || frames[1].DedupeKey != wantDedupe {
		t.Fatalf("identical dedupe keys = %q, %q; want %q", frames[0].DedupeKey, frames[1].DedupeKey, wantDedupe)
	}
	if frames[2].DedupeKey == wantDedupe {
		t.Fatalf("changed proto reused dedupe key %q", wantDedupe)
	}
	for index, frame := range frames {
		if frame.ReceivedAt.IsZero() {
			t.Fatalf("frame %d ReceivedAt is zero", index)
		}
	}
}

func TestMessageIngressCanonicalizesConversationJIDAtCapture(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	chatLID := watypes.NewJID("134149377675279", watypes.HiddenUserServer)
	chatPN := watypes.NewJID("15551230001", watypes.DefaultUserServer)
	ownJID := watypes.NewJID("15550009999", watypes.DefaultUserServer)
	bridge := &Bridge{
		store: store,
		client: &whatsmeow.Client{Store: &wastore.Device{
			ID: &ownJID,
			LIDs: &testLIDStore{
				NoopStore: wastore.NoopStore{},
				lidToPN: map[string]watypes.JID{
					chatLID.String(): chatPN,
				},
			},
		}},
	}
	var frame IngressFrame
	bridge.ObserveIngress(func(captured IngressFrame) { frame = captured })
	bridge.handleMessage(&waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{Chat: chatLID, Sender: chatLID},
			ID:            "canonical-chat",
			Timestamp:     time.UnixMilli(1775002001000),
		},
		Message: &waE2E.Message{Conversation: proto.String("hello")},
	})

	var envelope whatsappMessageIngressEnvelope
	if err := json.Unmarshal(frame.Payload, &envelope); err != nil {
		t.Fatalf("json.Unmarshal(): %v", err)
	}
	if envelope.ChatJID != chatPN.String() {
		t.Fatalf("captured chat_jid = %q, want canonical %q", envelope.ChatJID, chatPN.String())
	}
	if envelope.SenderJID != chatPN.String() {
		t.Fatalf("captured sender_jid = %q, want canonical %q", envelope.SenderJID, chatPN.String())
	}
}

func TestMessageIngressResolvesMentionsFromEditedPayload(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	group := watypes.NewJID("120363019999999999", watypes.GroupServer)
	mentioned := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: waConversationID(group),
		Name:           "Edit Group",
		IsGroup:        true,
		Participants:   `[{"name":"Max Ghenis","number":"+15551230000"}]`,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	bridge := &Bridge{store: store}
	var frame IngressFrame
	bridge.ObserveIngress(func(captured IngressFrame) { frame = captured })
	bridge.handleMessage(&waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{
				Chat:    group,
				Sender:  watypes.NewJID("15551234567", watypes.DefaultUserServer),
				IsGroup: true,
			},
			ID:        "edit-envelope",
			Timestamp: time.UnixMilli(1775002500000),
		},
		Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
			Key:  &waCommon.MessageKey{ID: proto.String("edit-target")},
			EditedMessage: &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String("hello @15551230000"),
				ContextInfo: &waE2E.ContextInfo{
					MentionedJID: []string{mentioned.String()},
				},
			}},
		}},
	})

	var envelope whatsappMessageIngressEnvelope
	if err := json.Unmarshal(frame.Payload, &envelope); err != nil {
		t.Fatalf("json.Unmarshal(): %v", err)
	}
	if got := envelope.Mentions[mentioned.String()]; got != "Max" {
		t.Fatalf("edit mention label = %q, want Max (all labels: %v)", got, envelope.Mentions)
	}
}

func TestReceiptIngressCaptureUsesWireNamesAndPayloadHash(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()
	bridge := &Bridge{store: store}
	var frames []IngressFrame
	bridge.ObserveIngress(func(frame IngressFrame) { frames = append(frames, frame) })

	chat := watypes.NewJID("15551234567", watypes.DefaultUserServer)
	sender := watypes.NewJID("15557654321", watypes.DefaultUserServer)
	timestamp := time.UnixMilli(1775003000000)
	for _, receiptType := range []watypes.ReceiptType{
		watypes.ReceiptTypeDelivered,
		watypes.ReceiptTypeReadSelf,
	} {
		bridge.handleReceipt(&waevents.Receipt{
			MessageSource: watypes.MessageSource{Chat: chat, Sender: sender},
			MessageIDs:    []watypes.MessageID{"one", "two"},
			Timestamp:     timestamp,
			Type:          receiptType,
		})
	}
	if len(frames) != 2 {
		t.Fatalf("captured receipt frames = %d, want 2", len(frames))
	}
	for index, receiptType := range []string{"delivered", "read_self"} {
		want := fmt.Sprintf(
			`{"kind":"receipt","chat_jid":"15551234567@s.whatsapp.net","sender_jid":"15557654321@s.whatsapp.net","receipt_type":%q,"message_ids":["one","two"],"timestamp_ms":1775003000000}`,
			receiptType,
		)
		if got := string(frames[index].Payload); got != want {
			t.Fatalf("receipt %d payload:\n got: %s\nwant: %s", index, got, want)
		}
		if got, wantKey := frames[index].DedupeKey, "rcpt:"+ingressHash8(frames[index].Payload); got != wantKey {
			t.Fatalf("receipt %d dedupe = %q, want %q", index, got, wantKey)
		}
	}
}

func TestHistorySyncIngressCapturesOneFrameWithoutNestedMessageFrames(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	chat := watypes.NewJID("15551234567", watypes.DefaultUserServer)
	own := watypes.NewJID("15550009999", watypes.DefaultUserServer)
	data := &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{{
			ID: proto.String(chat.String()),
			Messages: []*waHistorySync.HistorySyncMsg{{Message: &waWeb.WebMessageInfo{
				Key: &waCommon.MessageKey{
					RemoteJID: proto.String(chat.String()),
					FromMe:    proto.Bool(false),
					ID:        proto.String("history-message"),
				},
				Message:          &waE2E.Message{Conversation: proto.String("from history")},
				MessageTimestamp: proto.Uint64(1775004000),
				PushName:         proto.String("Jenn"),
			}}},
		}},
	}
	protoBytes, err := proto.Marshal(data)
	if err != nil {
		t.Fatalf("proto.Marshal(): %v", err)
	}
	bridge := &Bridge{
		store:  store,
		client: &whatsmeow.Client{Store: &wastore.Device{ID: &own}},
	}
	var frames []IngressFrame
	bridge.ObserveIngress(func(frame IngressFrame) { frames = append(frames, frame) })
	bridge.handleEvent(&waevents.HistorySync{Data: data})

	if len(frames) != 1 {
		t.Fatalf("captured frames = %d, want exactly one history_sync frame", len(frames))
	}
	wantPayload := fmt.Sprintf(
		`{"kind":"history_sync","proto_b64":%q}`,
		base64.StdEncoding.EncodeToString(protoBytes),
	)
	if got := string(frames[0].Payload); got != wantPayload {
		t.Fatalf("history payload:\n got: %s\nwant: %s", got, wantPayload)
	}
	if got, want := frames[0].DedupeKey, "hist:"+ingressHash8(frames[0].Payload); got != want {
		t.Fatalf("history dedupe = %q, want %q", got, want)
	}
	legacy, err := store.GetMessageByID("whatsapp:history-message")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if legacy == nil || legacy.Body != "from history" {
		t.Fatalf("legacy history message = %#v, want retained import", legacy)
	}
}

func TestPanickingIngressObserverDoesNotInterruptLegacyMessageHandling(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	messageChanges := 0
	bridge := &Bridge{
		store: store,
		callbacks: Callbacks{OnMessagesChange: func(string) {
			messageChanges++
		}},
	}
	observerCalls := 0
	bridge.ObserveIngress(func(IngressFrame) {
		observerCalls++
		panic("sink panic")
	})

	chat := watypes.NewJID("15551234567", watypes.DefaultUserServer)
	for index := 1; index <= 2; index++ {
		bridge.handleEvent(&waevents.Message{
			Info: watypes.MessageInfo{
				MessageSource: watypes.MessageSource{Chat: chat, Sender: chat},
				ID:            watypes.MessageID(fmt.Sprintf("panic-message-%d", index)),
				Timestamp:     time.UnixMilli(1775005000000 + int64(index)),
			},
			Message: &waE2E.Message{Conversation: proto.String(fmt.Sprintf("body %d", index))},
		})
	}

	if observerCalls != 2 {
		t.Fatalf("observer calls = %d, want 2", observerCalls)
	}
	if messageChanges != 2 {
		t.Fatalf("legacy message callbacks = %d, want 2", messageChanges)
	}
	for index := 1; index <= 2; index++ {
		message, err := store.GetMessageByID(fmt.Sprintf("whatsapp:panic-message-%d", index))
		if err != nil {
			t.Fatalf("GetMessageByID(%d): %v", index, err)
		}
		if message == nil || message.Body != fmt.Sprintf("body %d", index) {
			t.Fatalf("legacy message %d = %#v", index, message)
		}
	}
}

func TestExtractMentionedJIDsReturnsIndependentSlice(t *testing.T) {
	original := []string{"15551230000@s.whatsapp.net"}
	message := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		ContextInfo: &waE2E.ContextInfo{MentionedJID: original},
	}}
	got := ExtractMentionedJIDs(message)
	if !bytes.Equal([]byte(got[0]), []byte(original[0])) {
		t.Fatalf("ExtractMentionedJIDs() = %v, want %v", got, original)
	}
	got[0] = "mutated"
	if original[0] != "15551230000@s.whatsapp.net" {
		t.Fatalf("returned mentions alias proto storage: %v", original)
	}
}

// Receipt capture must observe the canonical chat JID without mutating the
// legacy store: normalizeConversationJID merges conversation aliases (deleting
// the @lid row) as a side effect, and the legacy receipt path never touched
// conversation JIDs. A receipt for an @lid chat therefore must leave the
// legacy @lid conversation row exactly where it was.
func TestReceiptIngressCaptureDoesNotMergeLegacyConversations(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	chatLID := watypes.NewJID("134149377675280", watypes.HiddenUserServer)
	chatPN := watypes.NewJID("15551230002", watypes.DefaultUserServer)
	ownJID := watypes.NewJID("15550009999", watypes.DefaultUserServer)
	lidConversationID := waConversationID(chatLID)
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: lidConversationID,
		Name:           "LID alias thread",
		Participants:   `[]`,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed @lid conversation: %v", err)
	}

	bridge := &Bridge{
		store: store,
		client: &whatsmeow.Client{Store: &wastore.Device{
			ID: &ownJID,
			LIDs: &testLIDStore{
				NoopStore: wastore.NoopStore{},
				lidToPN: map[string]watypes.JID{
					chatLID.String(): chatPN,
				},
			},
		}},
	}
	var frame IngressFrame
	bridge.ObserveIngress(func(captured IngressFrame) { frame = captured })
	bridge.handleReceipt(&waevents.Receipt{
		MessageSource: watypes.MessageSource{Chat: chatLID, Sender: chatLID},
		MessageIDs:    []watypes.MessageID{"receipt-target"},
		Timestamp:     time.UnixMilli(1775003001000),
		Type:          watypes.ReceiptTypeRead,
	})

	var envelope whatsappReceiptIngressEnvelope
	if err := json.Unmarshal(frame.Payload, &envelope); err != nil {
		t.Fatalf("json.Unmarshal(): %v", err)
	}
	if envelope.ChatJID != chatPN.String() {
		t.Fatalf("captured chat_jid = %q, want canonical %q", envelope.ChatJID, chatPN.String())
	}

	survived, err := store.GetConversation(lidConversationID)
	if err != nil {
		t.Fatalf("GetConversation(@lid): %v", err)
	}
	if survived == nil {
		t.Fatal("legacy @lid conversation row was merged away by receipt capture")
	}
	if survived.Name != "LID alias thread" {
		t.Fatalf("legacy @lid conversation mutated: %+v", survived)
	}
}
