package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
)

// TestMarkReadUsesLiveClientGetter pins the wiring that broke in production.
//
// /api/mark-read originally resolved the Google client from the bare `cli`
// parameter. EVERY caller of APIHandlerWithOptions passes nil for that
// parameter — cmd/serve.go, cmd/e2e-server, and every test — and supplies the
// live client through opts.Client instead (serve.go sets it to a.GetClient).
// So the remote read cursor was skipped on every request while the endpoint
// still returned 200: the local unread flag cleared and the phone stayed
// unread, with nothing logged above Debug to say so.
//
// The assertion is deliberately on "did the handler consult opts.Client",
// rather than on a real MarkRead call, because faking *libgm.Client is not
// worth the coupling. Consulting the accessor is the exact thing the bug got
// wrong, and this fails against the old code.
func TestMarkReadUsesLiveClientGetter(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: "conv-wiring",
		Name:           "Wiring Test",
		SourcePlatform: "sms",
		UnreadCount:    1,
		LastMessageTS:  1000,
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	// An incoming message so the helper has a cursor target and runs the full
	// path rather than short-circuiting on "no incoming message".
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "m-in",
		ConversationID: "conv-wiring",
		Body:           "hi",
		TimestampMS:    1000,
		IsFromMe:       false,
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	clientAccessorCalls := 0
	// nil positionally, exactly as every real caller does.
	h := APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, APIOptions{
		Client: func() *client.Client {
			clientAccessorCalls++
			return nil
		},
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Post(
		srv.URL+"/api/mark-read",
		"application/json",
		strings.NewReader(`{"conversation_id":"conv-wiring"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if clientAccessorCalls == 0 {
		t.Fatal("handler never consulted opts.Client: it is resolving the Google " +
			"client from the always-nil cli parameter, so the remote read cursor " +
			"is silently skipped and the phone is never updated")
	}

	// The local clear must still happen regardless of the remote outcome.
	conv, err := store.GetConversation("conv-wiring")
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if conv == nil {
		t.Fatal("conversation missing after mark-read")
	}
	if conv.UnreadCount != 0 {
		t.Fatalf("unread_count = %d, want 0", conv.UnreadCount)
	}
}
