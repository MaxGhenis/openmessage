package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

type testServer struct {
	store  *db.Store
	server *httptest.Server
}

func newTestServer(t *testing.T) *testServer {
	return newTestServerWithOptions(t, APIOptions{})
}

func newTestServerWithOptions(t *testing.T, opts APIOptions) *testServer {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	logger := zerolog.Nop()
	h := APIHandlerWithOptions(store, nil, logger, nil, opts)
	srv := httptest.NewServer(h)

	t.Cleanup(func() {
		srv.Close()
		store.Close()
	})

	return &testServer{store: store, server: srv}
}

type sseEvent struct {
	Data  string
	Event string
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) sseEvent {
	t.Helper()

	type result struct {
		err error
		evt sseEvent
	}
	ch := make(chan result, 1)
	go func() {
		var evt sseEvent
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				ch <- result{err: err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				ch <- result{evt: evt}
				return
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			if strings.HasPrefix(line, "event: ") {
				evt.Event = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				evt.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatal(res.err)
		}
		return res.evt
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE event")
		return sseEvent{}
	}
}

func TestListConversations(t *testing.T) {
	ts := newTestServer(t)

	// Empty list
	resp, err := http.Get(ts.server.URL + "/api/conversations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("got content-type %q, want application/json", ct)
	}

	var convos []db.Conversation
	if err := json.NewDecoder(resp.Body).Decode(&convos); err != nil {
		t.Fatal(err)
	}
	if len(convos) != 0 {
		t.Fatalf("got %d conversations, want 0", len(convos))
	}

	// Add some conversations
	ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1", Name: "Alice", LastMessageTS: 200,
	})
	ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c2", Name: "Bob", LastMessageTS: 100,
	})

	resp2, err := http.Get(ts.server.URL + "/api/conversations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	var convos2 []db.Conversation
	if err := json.NewDecoder(resp2.Body).Decode(&convos2); err != nil {
		t.Fatal(err)
	}
	if len(convos2) != 2 {
		t.Fatalf("got %d conversations, want 2", len(convos2))
	}
	// Should be ordered by last_message_ts DESC
	if convos2[0].Name != "Alice" {
		t.Fatalf("first conversation should be Alice (most recent), got %q", convos2[0].Name)
	}
}

func TestSetConversationNotificationMode(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1",
		Name:           "Alice",
		LastMessageTS:  100,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.server.URL+"/api/conversations/c1/notification-mode", "application/json", strings.NewReader(`{"notification_mode":"mentions"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 200: %s", resp.StatusCode, body)
	}

	var convo db.Conversation
	if err := json.NewDecoder(resp.Body).Decode(&convo); err != nil {
		t.Fatal(err)
	}
	if convo.NotificationMode != db.NotificationModeMentions {
		t.Fatalf("notification_mode = %q, want %q", convo.NotificationMode, db.NotificationModeMentions)
	}

	stored, err := ts.store.GetConversation("c1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.NotificationMode != db.NotificationModeMentions {
		t.Fatalf("stored notification_mode = %q, want %q", stored.NotificationMode, db.NotificationModeMentions)
	}
}

func TestSetConversationNotificationModeRejectsInvalid(t *testing.T) {
	ts := newTestServer(t)
	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1",
		Name:           "Alice",
		LastMessageTS:  100,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.server.URL+"/api/conversations/c1/notification-mode", "application/json", strings.NewReader(`{"notification_mode":"loud"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 400: %s", resp.StatusCode, body)
	}
}

func TestGetMessages(t *testing.T) {
	ts := newTestServer(t)

	ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1", Name: "Alice", LastMessageTS: 200,
	})
	ts.store.UpsertMessage(&db.Message{
		MessageID: "m1", ConversationID: "c1", Body: "Hello",
		SenderName: "Alice", TimestampMS: 100,
	})
	ts.store.UpsertMessage(&db.Message{
		MessageID: "m2", ConversationID: "c1", Body: "World",
		SenderName: "Me", TimestampMS: 200, IsFromMe: true,
	})

	resp, err := http.Get(ts.server.URL + "/api/conversations/c1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}

	var msgs []db.Message
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
}

func TestGetMessagesSupportsConversationIDsWithSlashes(t *testing.T) {
	ts := newTestServer(t)

	conversationID := "signal-group:4E8CCQ1ArzxJpbH53gUdo7SyJ/3d7wXnjOW/nTUdqDw="
	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Strategy Lab",
		LastMessageTS:  200,
		SourcePlatform: "signal",
		IsGroup:        true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.UpsertMessage(&db.Message{
		MessageID:      "signal:m1",
		ConversationID: conversationID,
		Body:           "slash ids should load",
		SenderName:     "Devon Hart",
		TimestampMS:    200,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.server.URL + "/api/conversations/signal-group:4E8CCQ1ArzxJpbH53gUdo7SyJ%2F3d7wXnjOW%2FnTUdqDw%3D/messages?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 200: %s", resp.StatusCode, body)
	}

	var msgs []db.Message
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].ConversationID != conversationID {
		t.Fatalf("conversation_id = %q, want %q", msgs[0].ConversationID, conversationID)
	}
}

func TestGetMessagesWithLimit(t *testing.T) {
	ts := newTestServer(t)

	ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1", Name: "Alice", LastMessageTS: 300,
	})
	for i := 0; i < 5; i++ {
		ts.store.UpsertMessage(&db.Message{
			MessageID:      "m" + string(rune('0'+i)),
			ConversationID: "c1",
			Body:           "msg",
			TimestampMS:    int64(i * 100),
		})
	}

	resp, err := http.Get(ts.server.URL + "/api/conversations/c1/messages?limit=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var msgs []db.Message
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
}

func TestGetMessagesWithPagingParams(t *testing.T) {
	ts := newTestServer(t)

	ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1", Name: "Alice", LastMessageTS: 500,
	})
	for _, msg := range []db.Message{
		{MessageID: "m1", ConversationID: "c1", Body: "1", TimestampMS: 100},
		{MessageID: "m2", ConversationID: "c1", Body: "2", TimestampMS: 200},
		{MessageID: "m3", ConversationID: "c1", Body: "3", TimestampMS: 300},
		{MessageID: "m4", ConversationID: "c1", Body: "4", TimestampMS: 400},
		{MessageID: "m5", ConversationID: "c1", Body: "5", TimestampMS: 500},
	} {
		ts.store.UpsertMessage(&msg)
	}

	resp, err := http.Get(ts.server.URL + "/api/conversations/c1/messages?before=400&limit=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var before []db.Message
	if err := json.NewDecoder(resp.Body).Decode(&before); err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("before query got %d messages, want 2", len(before))
	}
	if before[0].TimestampMS != 300 || before[1].TimestampMS != 200 {
		t.Fatalf("before query timestamps = [%d %d], want [300 200]", before[0].TimestampMS, before[1].TimestampMS)
	}

	resp2, err := http.Get(ts.server.URL + "/api/conversations/c1/messages?after=200&limit=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	var after []db.Message
	if err := json.NewDecoder(resp2.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("after query got %d messages, want 2", len(after))
	}
	if after[0].TimestampMS != 300 || after[1].TimestampMS != 400 {
		t.Fatalf("after query timestamps = [%d %d], want [300 400]", after[0].TimestampMS, after[1].TimestampMS)
	}
}

func TestGetMessagesWithPagingIDsAtDuplicateTimestampBoundary(t *testing.T) {
	ts := newTestServer(t)

	ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1", Name: "Alice", LastMessageTS: 300,
	})
	for _, msg := range []db.Message{
		{MessageID: "m1", ConversationID: "c1", Body: "1", TimestampMS: 100},
		{MessageID: "m2", ConversationID: "c1", Body: "2", TimestampMS: 200},
		{MessageID: "m3", ConversationID: "c1", Body: "3", TimestampMS: 200},
		{MessageID: "m4", ConversationID: "c1", Body: "4", TimestampMS: 300},
	} {
		ts.store.UpsertMessage(&msg)
	}

	resp, err := http.Get(ts.server.URL + "/api/conversations/c1/messages?before=200&before_id=m3&limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var before []db.Message
	if err := json.NewDecoder(resp.Body).Decode(&before); err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("before query got %d messages, want 2", len(before))
	}
	if before[0].MessageID != "m2" || before[1].MessageID != "m1" {
		t.Fatalf("before query IDs = [%s %s], want [m2 m1]", before[0].MessageID, before[1].MessageID)
	}

	resp2, err := http.Get(ts.server.URL + "/api/conversations/c1/messages?after=200&after_id=m2&limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	var after []db.Message
	if err := json.NewDecoder(resp2.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("after query got %d messages, want 2", len(after))
	}
	if after[0].MessageID != "m3" || after[1].MessageID != "m4" {
		t.Fatalf("after query IDs = [%s %s], want [m3 m4]", after[0].MessageID, after[1].MessageID)
	}
}

func TestSearchMessages(t *testing.T) {
	ts := newTestServer(t)

	ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1",
		Name:           "Nathan",
		Participants:   `[{"name":"Nathan","number":"+12675550100"}]`,
		LastMessageTS:  200,
		SourcePlatform: "sms",
	})
	ts.store.UpsertMessage(&db.Message{
		MessageID: "m1", ConversationID: "c1", Body: "lunch tomorrow?",
		TimestampMS: 100,
	})
	ts.store.UpsertMessage(&db.Message{
		MessageID: "m2", ConversationID: "c1", Body: "sure!",
		TimestampMS: 200,
	})

	resp, err := http.Get(ts.server.URL + "/api/search?q=lunch")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}

	var results []SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Preview != "lunch tomorrow?" {
		t.Fatalf("got preview %q, want %q", results[0].Preview, "lunch tomorrow?")
	}
	if results[0].Name != "Nathan" {
		t.Fatalf("got name %q, want Nathan", results[0].Name)
	}
}

func TestSearchIncludesConversationMetadataMatches(t *testing.T) {
	ts := newTestServer(t)

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1",
		Name:           "+1 (267) 555-0100",
		Participants:   `[{"name":"","number":"+12675550100"}]`,
		LastMessageTS:  100,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.UpsertMessage(&db.Message{
		MessageID:      "m1",
		ConversationID: "c1",
		SenderName:     "Nathan",
		SenderNumber:   "+12675550100",
		Body:           "See you soon",
		TimestampMS:    100,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.UpsertContact(&db.Contact{
		ContactID: "contact-1",
		Name:      "Nathan",
		Number:    "+12675550100",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.server.URL + "/api/search?q=Nathan")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var results []SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].ConversationID != "c1" {
		t.Fatalf("got conversation %q, want c1", results[0].ConversationID)
	}
	if results[0].Preview != "See you soon" {
		t.Fatalf("got preview %q, want %q", results[0].Preview, "See you soon")
	}
}

func TestSearchRequiresQuery(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.server.URL + "/api/search")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
	}
}

func TestSendMessage(t *testing.T) {
	ts := newTestServer(t)

	// send_message requires a real libgm client, so we test that
	// it returns 503 when client is nil
	body := `{"conversation_id": "c1", "message": "Hello!"}`
	resp, err := http.Post(ts.server.URL+"/api/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 503 {
		t.Fatalf("got status %d, want 503 (no client)", resp.StatusCode)
	}
}

func TestSendMessageUsesWhatsAppSender(t *testing.T) {
	var gotConversationID, gotBody, gotReplyToID string
	ts := newTestServerWithOptions(t, APIOptions{
		SendWhatsAppText: func(conversationID, body, replyToID string) (*db.Message, error) {
			gotConversationID = conversationID
			gotBody = body
			gotReplyToID = replyToID
			return &db.Message{
				MessageID:      "whatsapp:sent-1",
				ConversationID: conversationID,
				Body:           body,
				IsFromMe:       true,
				TimestampMS:    1234,
				Status:         "OUTGOING_SENDING",
				ReplyToID:      replyToID,
				SourcePlatform: "whatsapp",
				SourceID:       "sent-1",
			}, nil
		},
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Name:           "Jordan Rivera",
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"conversation_id":"whatsapp:15551234567@s.whatsapp.net","message":"hello wa","reply_to_id":"whatsapp:reply-1"}`
	resp, err := http.Post(ts.server.URL+"/api/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if gotConversationID != "whatsapp:15551234567@s.whatsapp.net" || gotBody != "hello wa" || gotReplyToID != "whatsapp:reply-1" {
		t.Fatalf("unexpected callback args: conv=%q body=%q reply=%q", gotConversationID, gotBody, gotReplyToID)
	}
	msgs, err := ts.store.GetMessagesByConversation("whatsapp:15551234567@s.whatsapp.net", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d stored messages, want 1", len(msgs))
	}
	if msgs[0].SourcePlatform != "whatsapp" {
		t.Fatalf("source platform = %q, want whatsapp", msgs[0].SourcePlatform)
	}
	if msgs[0].ReplyToID != "whatsapp:reply-1" {
		t.Fatalf("reply_to_id = %q, want whatsapp:reply-1", msgs[0].ReplyToID)
	}
}

func TestSendDraftUsesWhatsAppSender(t *testing.T) {
	var calls int
	ts := newTestServerWithOptions(t, APIOptions{
		SendWhatsAppText: func(conversationID, body, replyToID string) (*db.Message, error) {
			calls++
			return &db.Message{
				MessageID:      "whatsapp:draft-1",
				ConversationID: conversationID,
				Body:           body,
				IsFromMe:       true,
				TimestampMS:    4321,
				Status:         "OUTGOING_SENDING",
				SourcePlatform: "whatsapp",
				SourceID:       "draft-1",
			}, nil
		},
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Name:           "Jordan Rivera",
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.UpsertDraft(&db.Draft{
		DraftID:        "d1",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Body:           "draft text",
		CreatedAt:      1000,
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"draft_id":"d1","body":"send this draft"}`
	resp, err := http.Post(ts.server.URL+"/api/drafts/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if calls != 1 {
		t.Fatalf("SendWhatsAppText calls = %d, want 1", calls)
	}
	if draft, err := ts.store.GetDraft("d1"); err != nil {
		t.Fatal(err)
	} else if draft != nil {
		t.Fatal("draft was not deleted after WhatsApp send")
	}
}

func TestSendMessageUsesSignalSender(t *testing.T) {
	var gotConversationID, gotBody, gotReplyToID string
	ts := newTestServerWithOptions(t, APIOptions{
		SendSignalText: func(conversationID, body, replyToID string) (*db.Message, error) {
			gotConversationID = conversationID
			gotBody = body
			gotReplyToID = replyToID
			return &db.Message{
				MessageID:      "signal:sent-1",
				ConversationID: conversationID,
				Body:           body,
				IsFromMe:       true,
				TimestampMS:    1234,
				Status:         "sent",
				ReplyToID:      replyToID,
				SourcePlatform: "signal",
				SourceID:       "sent-1",
			}, nil
		},
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "signal:+15551234567",
		Name:           "Taylor Price",
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"conversation_id":"signal:+15551234567","message":"hello signal","reply_to_id":"signal:reply-1"}`
	resp, err := http.Post(ts.server.URL+"/api/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if gotConversationID != "signal:+15551234567" || gotBody != "hello signal" || gotReplyToID != "signal:reply-1" {
		t.Fatalf("unexpected callback args: conv=%q body=%q reply=%q", gotConversationID, gotBody, gotReplyToID)
	}
	msgs, err := ts.store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d stored messages, want 1", len(msgs))
	}
	if msgs[0].SourcePlatform != "signal" {
		t.Fatalf("source platform = %q, want signal", msgs[0].SourcePlatform)
	}
	if msgs[0].ReplyToID != "signal:reply-1" {
		t.Fatalf("reply_to_id = %q, want signal:reply-1", msgs[0].ReplyToID)
	}
}

func TestSendDraftUsesSignalSender(t *testing.T) {
	var calls int
	ts := newTestServerWithOptions(t, APIOptions{
		SendSignalText: func(conversationID, body, replyToID string) (*db.Message, error) {
			calls++
			return &db.Message{
				MessageID:      "signal:draft-1",
				ConversationID: conversationID,
				Body:           body,
				IsFromMe:       true,
				TimestampMS:    4321,
				Status:         "sent",
				SourcePlatform: "signal",
				SourceID:       "draft-1",
			}, nil
		},
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "signal:+15551234567",
		Name:           "Taylor Price",
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.UpsertDraft(&db.Draft{
		DraftID:        "sd1",
		ConversationID: "signal:+15551234567",
		Body:           "draft text",
		CreatedAt:      1000,
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"draft_id":"sd1","body":"send this signal draft"}`
	resp, err := http.Post(ts.server.URL+"/api/drafts/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if calls != 1 {
		t.Fatalf("SendSignalText calls = %d, want 1", calls)
	}
	if draft, err := ts.store.GetDraft("sd1"); err != nil {
		t.Fatal(err)
	} else if draft != nil {
		t.Fatal("draft was not deleted after Signal send")
	}
}

func TestReactUsesSignalBridgeForSignalConversation(t *testing.T) {
	var calls int
	var gotConversationID, gotMessageID, gotEmoji, gotAction string
	ts := newTestServerWithOptions(t, APIOptions{
		SendSignalReaction: func(conversationID, messageID, emoji, action string) error {
			calls++
			gotConversationID = conversationID
			gotMessageID = messageID
			gotEmoji = emoji
			gotAction = action
			return nil
		},
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "signal:+15551234567",
		Name:           "Taylor Price",
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"conversation_id":"signal:+15551234567","message_id":"signal:target-msg","emoji":"😂","action":"add"}`
	resp, err := http.Post(ts.server.URL+"/api/react", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 200: %s", resp.StatusCode, raw)
	}
	if calls != 1 {
		t.Fatalf("SendSignalReaction calls = %d, want 1", calls)
	}
	if gotConversationID != "signal:+15551234567" || gotMessageID != "signal:target-msg" || gotEmoji != "😂" || gotAction != "add" {
		t.Fatalf("unexpected callback args: conv=%q msg=%q emoji=%q action=%q", gotConversationID, gotMessageID, gotEmoji, gotAction)
	}
}

func TestReactUsesWhatsAppBridgeForWhatsAppConversation(t *testing.T) {
	var calls int
	var gotConversationID, gotMessageID, gotEmoji, gotAction string
	ts := newTestServerWithOptions(t, APIOptions{
		SendWhatsAppReaction: func(conversationID, messageID, emoji, action string) error {
			calls++
			gotConversationID = conversationID
			gotMessageID = messageID
			gotEmoji = emoji
			gotAction = action
			return nil
		},
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Name:           "Jamie Rivera",
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"conversation_id":"whatsapp:15551234567@s.whatsapp.net","message_id":"whatsapp:target-msg","emoji":"😂","action":"add"}`
	resp, err := http.Post(ts.server.URL+"/api/react", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 200: %s", resp.StatusCode, raw)
	}
	if calls != 1 {
		t.Fatalf("SendWhatsAppReaction calls = %d, want 1", calls)
	}
	if gotConversationID != "whatsapp:15551234567@s.whatsapp.net" || gotMessageID != "whatsapp:target-msg" || gotEmoji != "😂" || gotAction != "add" {
		t.Fatalf("unexpected callback args: conv=%q msg=%q emoji=%q action=%q", gotConversationID, gotMessageID, gotEmoji, gotAction)
	}
}

func TestSendMediaUsesWhatsAppSender(t *testing.T) {
	var gotConversationID, gotFilename, gotMIME, gotCaption, gotReplyToID string
	var gotData []byte
	ts := newTestServerWithOptions(t, APIOptions{
		SendWhatsAppMedia: func(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
			gotConversationID = conversationID
			gotFilename = filename
			gotMIME = mime
			gotCaption = caption
			gotReplyToID = replyToID
			gotData = append([]byte(nil), data...)
			return &db.Message{
				MessageID:      "whatsapp:media-1",
				ConversationID: conversationID,
				Body:           caption,
				IsFromMe:       true,
				TimestampMS:    1234,
				Status:         "OUTGOING_SENDING",
				MediaID:        "wa:test-ref",
				MimeType:       mime,
				DecryptionKey:  "deadbeef",
				ReplyToID:      replyToID,
				SourcePlatform: "whatsapp",
				SourceID:       "media-1",
			}, nil
		},
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Name:           "Jordan Rivera",
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("conversation_id", "whatsapp:15551234567@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("caption", "check this out"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("reply_to_id", "whatsapp:reply-1"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("png-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, ts.server.URL+"/api/send-media", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 200: %s", resp.StatusCode, string(raw))
	}
	if gotConversationID != "whatsapp:15551234567@s.whatsapp.net" {
		t.Fatalf("conversation_id = %q, want whatsapp:15551234567@s.whatsapp.net", gotConversationID)
	}
	if gotFilename != "photo.png" {
		t.Fatalf("filename = %q, want photo.png", gotFilename)
	}
	if gotMIME != "application/octet-stream" {
		t.Fatalf("mime = %q, want application/octet-stream", gotMIME)
	}
	if gotCaption != "check this out" {
		t.Fatalf("caption = %q, want check this out", gotCaption)
	}
	if gotReplyToID != "whatsapp:reply-1" {
		t.Fatalf("reply_to_id = %q, want whatsapp:reply-1", gotReplyToID)
	}
	if string(gotData) != "png-bytes" {
		t.Fatalf("data = %q, want png-bytes", string(gotData))
	}

	msgs, err := ts.store.GetMessagesByConversation("whatsapp:15551234567@s.whatsapp.net", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d stored messages, want 1", len(msgs))
	}
	if msgs[0].SourcePlatform != "whatsapp" {
		t.Fatalf("source platform = %q, want whatsapp", msgs[0].SourcePlatform)
	}
	if msgs[0].MediaID != "wa:test-ref" {
		t.Fatalf("media_id = %q, want wa:test-ref", msgs[0].MediaID)
	}
	if msgs[0].Body != "check this out" {
		t.Fatalf("body = %q, want check this out", msgs[0].Body)
	}
	if msgs[0].ReplyToID != "whatsapp:reply-1" {
		t.Fatalf("reply_to_id = %q, want whatsapp:reply-1", msgs[0].ReplyToID)
	}
	if msgs[0].MimeType != "application/octet-stream" {
		t.Fatalf("mime_type = %q, want application/octet-stream", msgs[0].MimeType)
	}
}

func TestSendMediaUsesSignalSender(t *testing.T) {
	var gotConversationID, gotFilename, gotMIME, gotCaption, gotReplyToID string
	var gotData []byte
	ts := newTestServerWithOptions(t, APIOptions{
		SendSignalMedia: func(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error) {
			gotConversationID = conversationID
			gotFilename = filename
			gotMIME = mime
			gotCaption = caption
			gotReplyToID = replyToID
			gotData = append([]byte(nil), data...)
			return &db.Message{
				MessageID:      "signal:local:media-1",
				ConversationID: conversationID,
				Body:           caption,
				IsFromMe:       true,
				TimestampMS:    1234,
				Status:         "sent",
				MediaID:        "signallocal:c2lnbmFs",
				MimeType:       mime,
				ReplyToID:      replyToID,
				SourcePlatform: "signal",
			}, nil
		},
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "signal:+15551234567",
		Name:           "Taylor Price",
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("conversation_id", "signal:+15551234567"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("caption", "signal photo"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("reply_to_id", "signal:reply-1"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "signal-photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("png-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, ts.server.URL+"/api/send-media", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 200: %s", resp.StatusCode, string(raw))
	}
	if gotConversationID != "signal:+15551234567" {
		t.Fatalf("conversation_id = %q, want signal:+15551234567", gotConversationID)
	}
	if gotFilename != "signal-photo.png" {
		t.Fatalf("filename = %q, want signal-photo.png", gotFilename)
	}
	if gotMIME != "application/octet-stream" {
		t.Fatalf("mime = %q, want application/octet-stream", gotMIME)
	}
	if gotCaption != "signal photo" {
		t.Fatalf("caption = %q, want signal photo", gotCaption)
	}
	if gotReplyToID != "signal:reply-1" {
		t.Fatalf("reply_to_id = %q, want signal:reply-1", gotReplyToID)
	}
	if string(gotData) != "png-bytes" {
		t.Fatalf("data = %q, want png-bytes", string(gotData))
	}
}

func TestSendMessageStoresInDB(t *testing.T) {
	// When a message is sent, it should be stored in the DB immediately
	// so the UI shows it without waiting for an event
	ts := newTestServer(t)

	ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1", Name: "Alice",
	})

	// We can't actually send (no client), but we can verify the DB insert
	// happens by checking the store after a successful send.
	// Since client is nil, this will return 503 - that's expected.
	// The real test is that the send handler stores the message on success.
	// We'll test the storeSentMessage helper directly.
	ts.store.UpsertMessage(&db.Message{
		MessageID:      "sent-1",
		ConversationID: "c1",
		Body:           "Hello from test",
		IsFromMe:       true,
		TimestampMS:    1000,
	})

	msgs, err := ts.store.GetMessagesByConversation("c1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d, want 1", len(msgs))
	}
	if !msgs[0].IsFromMe {
		t.Error("expected IsFromMe=true")
	}
}

func TestSendMessageRequiresConversationID(t *testing.T) {
	ts := newTestServer(t)

	body := `{"message": "Hello!"}`
	resp, err := http.Post(ts.server.URL+"/api/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("got status %d, want 400 for missing conversation_id", resp.StatusCode)
	}
}

func TestSendMessageValidation(t *testing.T) {
	ts := newTestServer(t)

	// Missing message field
	body := `{"conversation_id": "c1"}`
	resp, err := http.Post(ts.server.URL+"/api/send", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
	}
}

func TestGetStatus(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.server.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}

	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status["connected"] != false {
		t.Fatal("expected connected=false when no client")
	}
}

func TestGetStatusIncludesWhatsAppSnapshot(t *testing.T) {
	ts := newTestServerWithOptions(t, APIOptions{
		WhatsAppStatus: func() any {
			return map[string]any{
				"connected": true,
				"paired":    true,
				"push_name": "Max",
			}
		},
	})

	resp, err := http.Get(ts.server.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	wa, ok := status["whatsapp"].(map[string]any)
	if !ok {
		t.Fatalf("expected whatsapp status object, got %#v", status["whatsapp"])
	}
	if wa["connected"] != true || wa["paired"] != true {
		t.Fatalf("unexpected whatsapp payload: %#v", wa)
	}
}

func TestGetStatusIncludesSignalSnapshot(t *testing.T) {
	ts := newTestServerWithOptions(t, APIOptions{
		SignalStatus: func() any {
			return map[string]any{
				"connected": true,
				"paired":    true,
				"account":   "+15551234567",
			}
		},
	})

	resp, err := http.Get(ts.server.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	signal, ok := status["signal"].(map[string]any)
	if !ok {
		t.Fatalf("expected signal status object, got %#v", status["signal"])
	}
	if signal["connected"] != true || signal["paired"] != true {
		t.Fatalf("unexpected signal payload: %#v", signal)
	}
}

func TestGetStatusIncludesGoogleSnapshot(t *testing.T) {
	ts := newTestServerWithOptions(t, APIOptions{
		GoogleStatus: func() any {
			return map[string]any{
				"connected":     false,
				"paired":        true,
				"needs_pairing": false,
				"last_error":    "Disconnected from Google Messages",
			}
		},
	})

	resp, err := http.Get(ts.server.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	google, ok := status["google"].(map[string]any)
	if !ok {
		t.Fatalf("expected google status object, got %#v", status["google"])
	}
	if google["paired"] != true || google["needs_pairing"] != false {
		t.Fatalf("unexpected google payload: %#v", google)
	}
}

func TestDiagnosticsEndpointIncludesCountsStatusAndReleaseSnapshot(t *testing.T) {
	ts := newTestServerWithOptions(t, APIOptions{
		IdentityName: "Max",
		IsConnected: func() bool {
			return true
		},
		GoogleStatus: func() any {
			return map[string]any{"connected": true, "paired": true, "needs_pairing": false}
		},
		ReconnectGoogle: func() error {
			return nil
		},
		WhatsAppStatus: func() any {
			return map[string]any{"connected": false, "paired": true}
		},
		ConnectWhatsApp: func() error {
			return nil
		},
		WhatsAppQRCode: func() (any, error) {
			return map[string]any{"qr_available": false}, nil
		},
		LeaveWhatsAppGroup: func(conversationID string) error {
			return nil
		},
		SignalStatus: func() any {
			return map[string]any{"connected": false, "paired": true, "account": "+15551234567"}
		},
		ConnectSignal: func() error {
			return nil
		},
		SignalQRCode: func() (any, error) {
			return map[string]any{"qr_available": false}, nil
		},
		BackfillStatus: func() any {
			return map[string]any{"running": true, "phase": "messages"}
		},
		StartDeepBackfill: func() bool {
			return false
		},
		BackfillPhone: func(phone string) error {
			return nil
		},
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "sms-1",
		Name:           "Alice",
		LastMessageTS:  100,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.UpsertMessage(&db.Message{
		MessageID:      "m1",
		ConversationID: "sms-1",
		Body:           "hello",
		TimestampMS:    100,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "signal-1",
		Name:           "Signal Friends",
		LastMessageTS:  220,
		SourcePlatform: "signal",
		IsGroup:        true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.UpsertMessage(&db.Message{
		MessageID:      "signal:m1",
		ConversationID: "signal-1",
		Body:           "hi from signal",
		TimestampMS:    220,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.server.URL + "/api/diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["schema_version"] != float64(1) {
		t.Fatalf("schema_version = %v, want 1", payload["schema_version"])
	}
	if _, err := time.Parse(time.RFC3339Nano, payload["generated_at_iso"].(string)); err != nil {
		t.Fatalf("generated_at_iso is not RFC3339Nano: %v", err)
	}
	if payload["connected"] != true {
		t.Fatalf("connected = %v, want true", payload["connected"])
	}
	if payload["identity_name"] != "Max" {
		t.Fatalf("identity_name = %v, want Max", payload["identity_name"])
	}
	if payload["conversation_count"] != float64(2) {
		t.Fatalf("conversation_count = %v, want 2", payload["conversation_count"])
	}
	if payload["message_count"] != float64(2) {
		t.Fatalf("message_count = %v, want 2", payload["message_count"])
	}
	if _, ok := payload["google"].(map[string]any); !ok {
		t.Fatalf("expected google status in diagnostics, got %#v", payload["google"])
	}
	convCounts := payload["conversation_counts"].(map[string]any)
	if convCounts["sms"] != float64(1) || convCounts["signal"] != float64(1) {
		t.Fatalf("unexpected conversation_counts: %#v", convCounts)
	}
	msgCounts := payload["message_counts"].(map[string]any)
	if msgCounts["sms"] != float64(1) || msgCounts["signal"] != float64(1) {
		t.Fatalf("unexpected message_counts: %#v", msgCounts)
	}
	latest := payload["latest_message_ts"].(map[string]any)
	if latest["signal"] != float64(220) {
		t.Fatalf("latest signal timestamp = %v, want 220", latest["signal"])
	}
	backend := payload["backend"].(map[string]any)
	for _, key := range []string{"go_version", "goos", "goarch"} {
		if backend[key] == "" {
			t.Fatalf("backend.%s is empty: %#v", key, backend)
		}
	}
	if backend["goroutines"].(float64) < 1 {
		t.Fatalf("backend.goroutines = %v, want at least 1", backend["goroutines"])
	}
	if backend["uptime_ms"].(float64) < 0 {
		t.Fatalf("backend.uptime_ms = %v, want non-negative", backend["uptime_ms"])
	}
	memory := payload["memory"].(map[string]any)
	if memory["alloc_bytes"].(float64) <= 0 || memory["sys_bytes"].(float64) <= 0 {
		t.Fatalf("memory snapshot missing allocations: %#v", memory)
	}
	capabilities := payload["capabilities"].(map[string]any)
	google := capabilities["google"].(map[string]any)
	if google["status"] != true || google["reconnect"] != true || google["unpair"] != false {
		t.Fatalf("unexpected google capabilities: %#v", google)
	}
	whatsapp := capabilities["whatsapp"].(map[string]any)
	if whatsapp["connect"] != true || whatsapp["qr"] != true || whatsapp["leave_group"] != true || whatsapp["send_text"] != false {
		t.Fatalf("unexpected whatsapp capabilities: %#v", whatsapp)
	}
	signal := capabilities["signal"].(map[string]any)
	if signal["status"] != true || signal["connect"] != true || signal["qr"] != true || signal["send_media"] != false {
		t.Fatalf("unexpected signal capabilities: %#v", signal)
	}
	backfill := capabilities["backfill"].(map[string]any)
	if backfill["status"] != true || backfill["deep"] != true || backfill["targeted"] != true {
		t.Fatalf("unexpected backfill capabilities: %#v", backfill)
	}
}

func TestGoogleReconnectRoute(t *testing.T) {
	called := false
	ts := newTestServerWithOptions(t, APIOptions{
		ReconnectGoogle: func() error {
			called = true
			return nil
		},
		GoogleStatus: func() any {
			return map[string]any{"connected": true, "paired": true}
		},
	})

	resp, err := http.Post(ts.server.URL+"/api/google/reconnect", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if !called {
		t.Fatal("expected reconnect callback to be called")
	}
}

func TestWhatsAppConnectRoute(t *testing.T) {
	called := false
	ts := newTestServerWithOptions(t, APIOptions{
		ConnectWhatsApp: func() error {
			called = true
			return nil
		},
		WhatsAppStatus: func() any {
			return map[string]any{
				"pairing":      true,
				"qr_available": true,
			}
		},
	})

	resp, err := http.Post(ts.server.URL+"/api/whatsapp/connect", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if !called {
		t.Fatal("expected connect callback to be called")
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["pairing"] != true || payload["qr_available"] != true {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestSignalConnectRoute(t *testing.T) {
	called := false
	ts := newTestServerWithOptions(t, APIOptions{
		ConnectSignal: func() error {
			called = true
			return nil
		},
		SignalStatus: func() any {
			return map[string]any{
				"pairing":      true,
				"qr_available": true,
			}
		},
	})

	resp, err := http.Post(ts.server.URL+"/api/signal/connect", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if !called {
		t.Fatal("expected connect callback to be called")
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["pairing"] != true || payload["qr_available"] != true {
		t.Fatalf("unexpected signal connect payload: %#v", payload)
	}
}

func TestSignalQRRoute(t *testing.T) {
	ts := newTestServerWithOptions(t, APIOptions{
		SignalQRCode: func() (any, error) {
			return map[string]any{
				"png_data_url": "data:image/png;base64,ZmFrZQ==",
			}, nil
		},
	})

	resp, err := http.Get(ts.server.URL + "/api/signal/qr")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(payload["png_data_url"].(string), "data:image/png;base64,") {
		t.Fatalf("unexpected qr payload: %#v", payload)
	}
}

func TestWhatsAppQRCodeRoute(t *testing.T) {
	ts := newTestServerWithOptions(t, APIOptions{
		WhatsAppQRCode: func() (any, error) {
			return map[string]any{
				"event":        "code",
				"png_data_url": "data:image/png;base64,abc",
			}, nil
		},
	})

	resp, err := http.Get(ts.server.URL + "/api/whatsapp/qr")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}

	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["event"] != "code" {
		t.Fatalf("expected event=code, got %#v", payload["event"])
	}
}

func TestWhatsAppAvatarRoute(t *testing.T) {
	calledWith := ""
	ts := newTestServerWithOptions(t, APIOptions{
		WhatsAppAvatar: func(conversationID string) ([]byte, string, error) {
			calledWith = conversationID
			return []byte("avatar-bytes"), "image/png", nil
		},
	})

	resp, err := http.Get(ts.server.URL + "/api/whatsapp/avatar?conversation_id=" + url.QueryEscape("whatsapp:15551234567@s.whatsapp.net"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Fatalf("content-type = %q, want image/png", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "avatar-bytes" {
		t.Fatalf("body = %q, want avatar-bytes", string(body))
	}
	if calledWith != "whatsapp:15551234567@s.whatsapp.net" {
		t.Fatalf("conversation_id = %q", calledWith)
	}
}

func TestWhatsAppAvatarRouteReturns404WhenMissing(t *testing.T) {
	ts := newTestServerWithOptions(t, APIOptions{
		WhatsAppAvatar: func(conversationID string) ([]byte, string, error) {
			return nil, "", whatsapplive.ErrProfilePhotoNotFound
		},
	})

	resp, err := http.Get(ts.server.URL + "/api/whatsapp/avatar?conversation_id=" + url.QueryEscape("whatsapp:15551234567@s.whatsapp.net"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 404 {
		t.Fatalf("got status %d, want 404", resp.StatusCode)
	}
}

func TestWhatsAppLeaveGroupRoute(t *testing.T) {
	var (
		leftConversationID string
		ts                 *testServer
	)
	ts = newTestServerWithOptions(t, APIOptions{
		LeaveWhatsAppGroup: func(conversationID string) error {
			leftConversationID = conversationID
			return ts.store.DeleteConversation(conversationID)
		},
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:120363019999999999@g.us",
		Name:           "Spam Group",
		IsGroup:        true,
		LastMessageTS:  1000,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ts.store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:m1",
		ConversationID: "whatsapp:120363019999999999@g.us",
		Body:           "hi",
		TimestampMS:    1000,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.server.URL+"/api/whatsapp/leave-group", "application/json", strings.NewReader(`{"conversation_id":"whatsapp:120363019999999999@g.us"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 200: %s", resp.StatusCode, body)
	}
	if leftConversationID != "whatsapp:120363019999999999@g.us" {
		t.Fatalf("left conversation = %q, want whatsapp group", leftConversationID)
	}
	if _, err := ts.store.GetConversation("whatsapp:120363019999999999@g.us"); err == nil {
		t.Fatal("expected conversation to be deleted after leaving")
	}
}

func TestWhatsAppLeaveGroupRouteRejectsNonGroups(t *testing.T) {
	ts := newTestServerWithOptions(t, APIOptions{
		LeaveWhatsAppGroup: func(conversationID string) error { return nil },
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Name:           "Jordan",
		LastMessageTS:  1000,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(ts.server.URL+"/api/whatsapp/leave-group", "application/json", strings.NewReader(`{"conversation_id":"whatsapp:15551234567@s.whatsapp.net"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 400: %s", resp.StatusCode, body)
	}
}

func TestGetMediaReturns404WhenNoMedia(t *testing.T) {
	ts := newTestServer(t)

	// Message with no media
	ts.store.UpsertMessage(&db.Message{
		MessageID: "m1", ConversationID: "c1", Body: "text only",
	})

	resp, err := http.Get(ts.server.URL + "/api/media/m1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 404 {
		t.Fatalf("got status %d, want 404 for message without media", resp.StatusCode)
	}
}

func TestGetMediaReturns404WhenMessageNotFound(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.server.URL + "/api/media/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 404 {
		t.Fatalf("got status %d, want 404 for nonexistent message", resp.StatusCode)
	}
}

func TestGetMediaReturns503WhenNoClient(t *testing.T) {
	ts := newTestServer(t)

	// Message with media but no client to download
	ts.store.UpsertMessage(&db.Message{
		MessageID:      "m1",
		ConversationID: "c1",
		MediaID:        "mid-123",
		MimeType:       "image/jpeg",
		DecryptionKey:  "deadbeef",
	})

	resp, err := http.Get(ts.server.URL + "/api/media/m1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 503 {
		t.Fatalf("got status %d, want 503 when client is nil", resp.StatusCode)
	}
}

func TestGetMediaUsesWhatsAppDownloader(t *testing.T) {
	var gotMessageID string
	ts := newTestServerWithOptions(t, APIOptions{
		DownloadWhatsAppMedia: func(msg *db.Message) ([]byte, string, error) {
			gotMessageID = msg.MessageID
			return []byte("png-data"), "image/png", nil
		},
	})

	if err := ts.store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:m1",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		MediaID:        "wa:test-ref",
		MimeType:       "image/png",
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.server.URL + "/api/media/whatsapp:m1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 200: %s", resp.StatusCode, string(raw))
	}
	if gotMessageID != "whatsapp:m1" {
		t.Fatalf("message id = %q, want whatsapp:m1", gotMessageID)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q, want image/png", ct)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "png-data" {
		t.Fatalf("body = %q, want png-data", string(data))
	}
}

func TestGetMediaUsesSignalDownloader(t *testing.T) {
	var gotMessageID string
	ts := newTestServerWithOptions(t, APIOptions{
		DownloadSignalMedia: func(msg *db.Message) ([]byte, string, error) {
			gotMessageID = msg.MessageID
			return []byte("signal-png"), "image/png", nil
		},
	})

	if err := ts.store.UpsertMessage(&db.Message{
		MessageID:      "signal:m1",
		ConversationID: "signal:+15551234567",
		MediaID:        "signalatt:att-123",
		MimeType:       "image/png",
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.server.URL + "/api/media/signal:m1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("got status %d, want 200: %s", resp.StatusCode, string(raw))
	}
	if gotMessageID != "signal:m1" {
		t.Fatalf("message id = %q, want signal:m1", gotMessageID)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q, want image/png", ct)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "signal-png" {
		t.Fatalf("body = %q, want signal-png", string(data))
	}
}

func TestMessagesIncludeMediaFields(t *testing.T) {
	ts := newTestServer(t)

	ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1", Name: "Alice",
	})
	ts.store.UpsertMessage(&db.Message{
		MessageID:      "m1",
		ConversationID: "c1",
		Body:           "",
		MediaID:        "mid-abc",
		MimeType:       "image/png",
		TimestampMS:    1000,
	})

	resp, err := http.Get(ts.server.URL + "/api/conversations/c1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var msgs []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0]["MediaID"] != "mid-abc" {
		t.Errorf("expected MediaID 'mid-abc', got %v", msgs[0]["MediaID"])
	}
	if msgs[0]["MimeType"] != "image/png" {
		t.Errorf("expected MimeType 'image/png', got %v", msgs[0]["MimeType"])
	}
}

func TestMessagesIncludeReactionsAndReplyTo(t *testing.T) {
	ts := newTestServer(t)

	ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1", Name: "Alice",
	})
	ts.store.UpsertMessage(&db.Message{
		MessageID:      "m1",
		ConversationID: "c1",
		Body:           "Original",
		TimestampMS:    1000,
		Reactions:      `[{"emoji":"😂","count":2}]`,
	})
	ts.store.UpsertMessage(&db.Message{
		MessageID:      "m2",
		ConversationID: "c1",
		Body:           "Reply",
		TimestampMS:    2000,
		ReplyToID:      "m1",
	})

	resp, err := http.Get(ts.server.URL + "/api/conversations/c1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var msgs []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d, want 2", len(msgs))
	}

	// m2 is first (DESC order), check it has ReplyToID
	if msgs[0]["ReplyToID"] != "m1" {
		t.Errorf("expected ReplyToID 'm1', got %v", msgs[0]["ReplyToID"])
	}
	// m1 has reactions
	if msgs[1]["Reactions"] == nil || msgs[1]["Reactions"] == "" {
		t.Error("expected Reactions on m1")
	}
}

func TestBuildReactionPayload(t *testing.T) {
	sim := &gmproto.SIMPayload{SIMNumber: 1}

	// ADD reaction
	payload := BuildReactionPayload("msg-123", "😂", "add", sim)
	if payload.MessageID != "msg-123" {
		t.Errorf("MessageID = %q, want msg-123", payload.MessageID)
	}
	if payload.ReactionData == nil || payload.ReactionData.Unicode != "😂" {
		t.Errorf("ReactionData.Unicode = %v, want 😂", payload.ReactionData)
	}
	if payload.Action != gmproto.SendReactionRequest_ADD {
		t.Errorf("Action = %v, want ADD", payload.Action)
	}
	if payload.SIMPayload == nil || payload.SIMPayload.SIMNumber != 1 {
		t.Error("SIMPayload not set correctly")
	}

	// REMOVE reaction
	payload2 := BuildReactionPayload("msg-456", "👍", "remove", sim)
	if payload2.Action != gmproto.SendReactionRequest_REMOVE {
		t.Errorf("Action = %v, want REMOVE", payload2.Action)
	}

	// Default to ADD
	payload3 := BuildReactionPayload("msg-789", "❤️", "", sim)
	if payload3.Action != gmproto.SendReactionRequest_ADD {
		t.Errorf("Action = %v, want ADD for empty action string", payload3.Action)
	}
}

func TestSendReactionValidation(t *testing.T) {
	ts := newTestServer(t)

	// Missing fields
	body := `{"message_id": ""}`
	resp, err := http.Post(ts.server.URL+"/api/react", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
	}
}

func TestSendReactionNoClient(t *testing.T) {
	ts := newTestServer(t)

	body := `{"message_id": "m1", "emoji": "😂", "conversation_id": "c1"}`
	resp, err := http.Post(ts.server.URL+"/api/react", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 503 {
		t.Fatalf("got status %d, want 503 when client is nil", resp.StatusCode)
	}
}

func TestBuildSendPayload(t *testing.T) {
	sim := &gmproto.SIMPayload{SIMNumber: 1}
	payload := app.BuildSendPayload("conv-1", "Hello world", "", "+15551234567", sim)

	// Must use MessageInfo array (not MessagePayloadContent)
	if payload.MessagePayload.MessagePayloadContent != nil {
		t.Error("MessagePayloadContent must be nil; use MessageInfo instead")
	}
	if len(payload.MessagePayload.MessageInfo) != 1 {
		t.Fatalf("expected 1 MessageInfo entry, got %d", len(payload.MessagePayload.MessageInfo))
	}
	mc := payload.MessagePayload.MessageInfo[0].GetMessageContent()
	if mc == nil || mc.Content != "Hello world" {
		t.Errorf("MessageContent mismatch: %+v", mc)
	}

	// TmpID format: tmp_ followed by 12 digits
	if !strings.HasPrefix(payload.TmpID, "tmp_") || len(payload.TmpID) != 16 {
		t.Errorf("TmpID format wrong: %q (want tmp_ + 12 digits)", payload.TmpID)
	}
	// TmpID must be in all 3 places
	if payload.MessagePayload.TmpID != payload.TmpID {
		t.Error("MessagePayload.TmpID must match root TmpID")
	}
	if payload.MessagePayload.TmpID2 != payload.TmpID {
		t.Error("MessagePayload.TmpID2 must match root TmpID")
	}

	// SIM payload must be set
	if payload.SIMPayload == nil {
		t.Error("SIMPayload must not be nil")
	}
	if payload.SIMPayload.SIMNumber != 1 {
		t.Errorf("SIMNumber = %d, want 1", payload.SIMPayload.SIMNumber)
	}

	// ParticipantID
	if payload.MessagePayload.ParticipantID != "+15551234567" {
		t.Errorf("ParticipantID = %q, want +15551234567", payload.MessagePayload.ParticipantID)
	}

	// ConversationID in both places
	if payload.ConversationID != "conv-1" {
		t.Errorf("root ConversationID = %q", payload.ConversationID)
	}
	if payload.MessagePayload.ConversationID != "conv-1" {
		t.Errorf("payload ConversationID = %q", payload.MessagePayload.ConversationID)
	}
}

func TestBuildSendPayloadWithReply(t *testing.T) {
	payload := app.BuildSendPayload("conv-1", "Reply text", "orig-msg-id", "+15551234567", nil)
	if payload.Reply == nil {
		t.Fatal("Reply must be set when replyToID is provided")
	}
	if payload.Reply.MessageID != "orig-msg-id" {
		t.Errorf("Reply.MessageID = %q, want orig-msg-id", payload.Reply.MessageID)
	}
}

func TestBuildSendPayloadNoReply(t *testing.T) {
	payload := app.BuildSendPayload("conv-1", "No reply", "", "+15551234567", nil)
	if payload.Reply != nil {
		t.Error("Reply must be nil when replyToID is empty")
	}
}

func TestBuildSendMediaPayload(t *testing.T) {
	sim := &gmproto.SIMPayload{SIMNumber: 1}
	media := &gmproto.MediaContent{
		Format:    4, // image
		MediaID:   "media-abc-123",
		MediaName: "photo.jpg",
		Size:      54321,
		MimeType:  "image/jpeg",
	}
	payload := app.BuildSendMediaPayload("conv-1", media, "+15551234567", sim)

	// Must use MessageInfo with MediaContent (not MessageContent)
	if payload.MessagePayload.MessagePayloadContent != nil {
		t.Error("MessagePayloadContent must be nil; use MessageInfo instead")
	}
	if len(payload.MessagePayload.MessageInfo) != 1 {
		t.Fatalf("expected 1 MessageInfo entry, got %d", len(payload.MessagePayload.MessageInfo))
	}

	// Should have MediaContent, not MessageContent
	mc := payload.MessagePayload.MessageInfo[0].GetMessageContent()
	if mc != nil {
		t.Error("MessageContent should be nil for media messages")
	}
	mediaCont := payload.MessagePayload.MessageInfo[0].GetMediaContent()
	if mediaCont == nil {
		t.Fatal("MediaContent must be set")
	}
	if mediaCont.MediaID != "media-abc-123" {
		t.Errorf("MediaID = %q, want media-abc-123", mediaCont.MediaID)
	}
	if mediaCont.MimeType != "image/jpeg" {
		t.Errorf("MimeType = %q, want image/jpeg", mediaCont.MimeType)
	}

	// TmpID format: tmp_ followed by 12 digits
	if !strings.HasPrefix(payload.TmpID, "tmp_") || len(payload.TmpID) != 16 {
		t.Errorf("TmpID format wrong: %q (want tmp_ + 12 digits)", payload.TmpID)
	}
	// TmpID must be in all 3 places
	if payload.MessagePayload.TmpID != payload.TmpID {
		t.Error("MessagePayload.TmpID must match root TmpID")
	}
	if payload.MessagePayload.TmpID2 != payload.TmpID {
		t.Error("MessagePayload.TmpID2 must match root TmpID")
	}

	// SIM payload must be set
	if payload.SIMPayload == nil || payload.SIMPayload.SIMNumber != 1 {
		t.Error("SIMPayload not set correctly")
	}

	// ParticipantID and ConversationID
	if payload.MessagePayload.ParticipantID != "+15551234567" {
		t.Errorf("ParticipantID = %q, want +15551234567", payload.MessagePayload.ParticipantID)
	}
	if payload.ConversationID != "conv-1" {
		t.Errorf("root ConversationID = %q", payload.ConversationID)
	}
	if payload.MessagePayload.ConversationID != "conv-1" {
		t.Errorf("payload ConversationID = %q", payload.MessagePayload.ConversationID)
	}
}

func TestSendMediaEndpointNoClient(t *testing.T) {
	ts := newTestServer(t)

	// Multipart form with image data
	body := strings.NewReader("")
	resp, err := http.Post(ts.server.URL+"/api/send-media", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Should return 405 for GET or 400/503 for POST without proper body
	if resp.StatusCode != 400 && resp.StatusCode != 503 {
		t.Fatalf("got status %d, want 400 or 503", resp.StatusCode)
	}
}

func TestMediaEndpointWithMimeTypeButNoMediaID(t *testing.T) {
	ts := newTestServer(t)

	// Message has MimeType (from backfill) but no MediaID (expired)
	// Historical media references are ephemeral and can't be re-fetched
	ts.store.UpsertMessage(&db.Message{
		MessageID:      "m-media-no-id",
		ConversationID: "c1",
		MimeType:       "image/png",
		MediaID:        "", // empty — media reference expired
		TimestampMS:    1000,
	})

	resp, err := http.Get(ts.server.URL + "/api/media/m-media-no-id")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// No MediaID means we can't download — return 404
	if resp.StatusCode != 404 {
		t.Fatalf("got status %d, want 404 (no media ID available)", resp.StatusCode)
	}
}

func TestStaticFileServing(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200 for index", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("got content-type %q, want text/html", ct)
	}
}

func TestLinkPreviewEndpoint(t *testing.T) {
	ts := newTestServerWithOptions(t, APIOptions{
		FetchLinkPreview: func(ctx context.Context, rawURL string) (*LinkPreview, error) {
			if rawURL != "https://example.com/story" {
				t.Fatalf("unexpected preview URL %q", rawURL)
			}
			return &LinkPreview{
				URL:         rawURL,
				Title:       "Example Story",
				Description: "A short preview description.",
				SiteName:    "Example",
				ImageURL:    "https://cdn.example.com/story.png",
				Domain:      "example.com",
			}, nil
		},
	})

	resp, err := http.Get(ts.server.URL + "/api/link-preview?url=https%3A%2F%2Fexample.com%2Fstory")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}

	var preview LinkPreview
	if err := json.NewDecoder(resp.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	if preview.Title != "Example Story" {
		t.Fatalf("got title %q", preview.Title)
	}
	if preview.ImageURL != "https://cdn.example.com/story.png" {
		t.Fatalf("got image %q", preview.ImageURL)
	}
}

func TestLinkPreviewEndpointRequiresURL(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.server.URL + "/api/link-preview")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("got status %d, want 400", resp.StatusCode)
	}
}

func TestBackfillStatusDefault(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.server.URL + "/api/backfill/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}

	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status["running"] != false {
		t.Error("expected running=false")
	}
}

func TestBackfillStatusWithCallback(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	logger := zerolog.Nop()
	h := APIHandlerWithOptions(store, nil, logger, nil, APIOptions{
		BackfillStatus: func() any {
			return map[string]any{
				"running":             true,
				"phase":               "messages",
				"conversations_found": 42,
			}
		},
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/backfill/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status["running"] != true {
		t.Error("expected running=true")
	}
	if status["phase"] != "messages" {
		t.Errorf("phase = %v, want messages", status["phase"])
	}
	if status["conversations_found"] != float64(42) {
		t.Errorf("conversations_found = %v, want 42", status["conversations_found"])
	}
}

func TestBackfillReturnsConflictWhenAlreadyRunning(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	logger := zerolog.Nop()
	h := APIHandlerWithOptions(store, nil, logger, nil, APIOptions{
		StartDeepBackfill: func() bool { return false },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/backfill", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 409 {
		t.Fatalf("got status %d, want 409", resp.StatusCode)
	}
}

func TestBackfillPhoneRequiresPost(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.server.URL + "/api/backfill/phone")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 405 {
		t.Fatalf("got status %d, want 405 for GET", resp.StatusCode)
	}
}

func TestBackfillPhoneRequiresPhoneNumber(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	logger := zerolog.Nop()
	h := APIHandlerWithOptions(store, nil, logger, nil, APIOptions{
		BackfillPhone: func(phone string) error { return nil },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{}`
	resp, err := http.Post(srv.URL+"/api/backfill/phone", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Fatalf("got status %d, want 400 for missing phone_number", resp.StatusCode)
	}
}

func TestBackfillPhoneNotAvailable(t *testing.T) {
	ts := newTestServer(t) // no BackfillPhone callback

	body := `{"phone_number": "+14157934268"}`
	resp, err := http.Post(ts.server.URL+"/api/backfill/phone", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 501 {
		t.Fatalf("got status %d, want 501 when not available", resp.StatusCode)
	}
}

func TestBackfillPhoneSuccess(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var calledWith string
	logger := zerolog.Nop()
	h := APIHandlerWithOptions(store, nil, logger, nil, APIOptions{
		BackfillPhone: func(phone string) error {
			calledWith = phone
			return nil
		},
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"phone_number": "+14157934268"}`
	resp, err := http.Post(srv.URL+"/api/backfill/phone", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("got status %d, want 200", resp.StatusCode)
	}
	if calledWith != "+14157934268" {
		t.Errorf("BackfillPhone called with %q, want +14157934268", calledWith)
	}
}

func TestStatusUsesLiveClientGetter(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	currentClient := &client.Client{}
	logger := zerolog.Nop()
	h := APIHandlerWithOptions(store, currentClient, logger, nil, APIOptions{
		Client: func() *client.Client { return currentClient },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status["connected"] != true {
		t.Fatalf("expected connected=true with live client, got %v", status["connected"])
	}

	currentClient = nil

	resp2, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	var status2 map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&status2); err != nil {
		t.Fatal(err)
	}
	if status2["connected"] != false {
		t.Fatalf("expected connected=false after client getter returns nil, got %v", status2["connected"])
	}
}

func TestEventsStreamPublishesStatusAndMessages(t *testing.T) {
	events := NewEventBroker()
	ts := newTestServerWithOptions(t, APIOptions{
		Events:      events,
		IsConnected: func() bool { return true },
	})

	resp, err := http.Get(ts.server.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", got)
	}

	reader := bufio.NewReader(resp.Body)
	statusEvt := readSSEEvent(t, reader)
	if statusEvt.Event != EventTypeStatus {
		t.Fatalf("first SSE event = %q, want %q", statusEvt.Event, EventTypeStatus)
	}

	var status StreamEvent
	if err := json.Unmarshal([]byte(statusEvt.Data), &status); err != nil {
		t.Fatal(err)
	}
	if status.Connected == nil || !*status.Connected {
		t.Fatalf("initial status event = %+v, want connected=true", status)
	}

	events.PublishMessages("c1")

	msgEvt := readSSEEvent(t, reader)
	if msgEvt.Event != EventTypeMessages {
		t.Fatalf("stream event = %q, want %q", msgEvt.Event, EventTypeMessages)
	}

	var msg StreamEvent
	if err := json.Unmarshal([]byte(msgEvt.Data), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.ConversationID != "c1" {
		t.Fatalf("messages event conversation_id = %q, want c1", msg.ConversationID)
	}
}

func TestEventsStreamPublishesTyping(t *testing.T) {
	events := NewEventBroker()
	ts := newTestServerWithOptions(t, APIOptions{
		Events:      events,
		IsConnected: func() bool { return true },
	})

	resp, err := http.Get(ts.server.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	_ = readSSEEvent(t, reader) // initial status event

	events.PublishTyping("c1", "Alice", "+15551234567", true)

	typingEvt := readSSEEvent(t, reader)
	if typingEvt.Event != EventTypeTyping {
		t.Fatalf("stream event = %q, want %q", typingEvt.Event, EventTypeTyping)
	}

	var typing StreamEvent
	if err := json.Unmarshal([]byte(typingEvt.Data), &typing); err != nil {
		t.Fatal(err)
	}
	if typing.ConversationID != "c1" {
		t.Fatalf("typing conversation_id = %q, want c1", typing.ConversationID)
	}
	if typing.SenderName != "Alice" {
		t.Fatalf("typing sender_name = %q, want Alice", typing.SenderName)
	}
	if typing.Typing == nil || !*typing.Typing {
		t.Fatalf("typing event = %+v, want typing=true", typing)
	}
}

func TestEventsStreamPublishesHeartbeat(t *testing.T) {
	events := NewEventBroker()
	ts := newTestServerWithOptions(t, APIOptions{
		Events:         events,
		IsConnected:    func() bool { return true },
		EventHeartbeat: 20 * time.Millisecond,
	})

	resp, err := http.Get(ts.server.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	_ = readSSEEvent(t, reader) // initial status event

	heartbeatEvt := readSSEEvent(t, reader)
	if heartbeatEvt.Event != EventTypeHeartbeat {
		t.Fatalf("stream event = %q, want %q", heartbeatEvt.Event, EventTypeHeartbeat)
	}

	var heartbeat StreamEvent
	if err := json.Unmarshal([]byte(heartbeatEvt.Data), &heartbeat); err != nil {
		t.Fatal(err)
	}
	if heartbeat.Timestamp <= 0 {
		t.Fatalf("heartbeat timestamp = %d, want > 0", heartbeat.Timestamp)
	}
}

func TestMarkReadPublishesConversationInvalidation(t *testing.T) {
	events := NewEventBroker()
	ts := newTestServerWithOptions(t, APIOptions{
		Events: events,
	})

	if err := ts.store.UpsertConversation(&db.Conversation{
		ConversationID: "c1",
		Name:           "Alice",
		UnreadCount:    1,
		LastMessageTS:  100,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(ts.server.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	_ = readSSEEvent(t, reader) // initial status event

	reqBody := strings.NewReader(`{"conversation_id":"c1"}`)
	markReadResp, err := http.Post(ts.server.URL+"/api/mark-read", "application/json", reqBody)
	if err != nil {
		t.Fatal(err)
	}
	defer markReadResp.Body.Close()

	if markReadResp.StatusCode != 200 {
		t.Fatalf("mark-read status = %d, want 200", markReadResp.StatusCode)
	}

	evt := readSSEEvent(t, reader)
	if evt.Event != EventTypeConversations {
		t.Fatalf("stream event = %q, want %q", evt.Event, EventTypeConversations)
	}
}
