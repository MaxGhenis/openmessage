package web

import (
	"net/http"
	"strings"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

type fixtureGoogleTextClient func() (*gmproto.SendMessageResponse, error)

func (fixtureGoogleTextClient) GetConversation(string) (*gmproto.Conversation, error) {
	return &gmproto.Conversation{}, nil
}

func (send fixtureGoogleTextClient) SendMessage(*gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
	return send()
}

func TestLegacyGoogleTextSendStoresOnlySyncedMessage(t *testing.T) {
	for _, tc := range []struct {
		name, route, body string
		echoBeforeAck     bool
	}{
		{"echo before acknowledgement", "/api/send", `{"conversation_id":"thread","message":"fixture","idempotency_key":"fixture-key"}`, true},
		{"echo after acknowledgement", "/api/send", `{"conversation_id":"thread","message":"fixture","idempotency_key":"fixture-key"}`, false},
		{"draft send", "/api/drafts/send", `{"draft_id":"draft","body":"fixture"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var store *db.Store
			echo := &db.Message{MessageID: "123", ConversationID: "thread", Body: "fixture",
				IsFromMe: true, Status: "OUTGOING_DELIVERED", TimestampMS: 1000}
			calls := 0
			ts := newTestServerWithOptions(t, APIOptions{googleText: fixtureGoogleTextClient(func() (*gmproto.SendMessageResponse, error) {
				calls++
				if tc.echoBeforeAck {
					if err := store.UpsertMessage(echo); err != nil {
						return nil, err
					}
				}
				return &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS}, nil
			})})
			store = ts.store
			if err := store.UpsertConversation(&db.Conversation{ConversationID: "thread"}); err != nil {
				t.Fatal(err)
			}
			if tc.route == "/api/drafts/send" {
				if err := store.UpsertDraft(&db.Draft{DraftID: "draft", ConversationID: "thread", Body: "fixture"}); err != nil {
					t.Fatal(err)
				}
			}
			post := func() {
				t.Helper()
				resp, err := http.Post(ts.server.URL+tc.route, "application/json", strings.NewReader(tc.body))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("send HTTP %d", resp.StatusCode)
				}
			}
			post()
			if !tc.echoBeforeAck {
				rows, err := store.GetMessagesByConversation("thread", 10)
				if err != nil || len(rows) != 0 {
					t.Fatalf("stored %d rows before echo, err=%v", len(rows), err)
				}
				if err := store.UpsertMessage(echo); err != nil {
					t.Fatal(err)
				}
			}
			if tc.route == "/api/send" {
				post()
			} else if draft, err := store.GetDraft("draft"); err != nil || draft != nil {
				t.Fatalf("draft was not removed, err=%v", err)
			}
			rows, err := store.GetMessagesByConversation("thread", 10)
			if err != nil || len(rows) != 1 || rows[0].MessageID != echo.MessageID || calls != 1 {
				t.Fatalf("want one canonical row and one transport call, got %d rows, %d calls, err=%v", len(rows), calls, err)
			}
		})
	}
}
