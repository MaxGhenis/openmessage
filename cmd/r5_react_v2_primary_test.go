package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/bridgeadapters/scripted"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/v2read"
	"github.com/maxghenis/openmessage/internal/web"
)

// r5ReactRouteAdapter gives a scripted account, which has no reaction sender
// of its own, one that records each request and confirms it.
type r5ReactRouteAdapter struct {
	*scripted.Adapter

	mu       sync.Mutex
	requests []bridge.ReactionRequest
}

func (a *r5ReactRouteAdapter) SendReaction(_ context.Context, req bridge.ReactionRequest) (bridge.SendResult, error) {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()
	return bridge.SendResult{}, nil
}

func (a *r5ReactRouteAdapter) reactionRequests() []bridge.ReactionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

// r5LegacyReactionCall is one call into a legacy per-platform reaction
// callback (App.SendSignalReaction / App.SendWhatsAppReaction in production).
type r5LegacyReactionCall struct {
	Platform       string
	ConversationID string
	MessageID      string
}

// TestR5ReactRoutesThroughTheOutboxOnV2Primary posts what the web UI posts to
// /api/react on a migrated v2-primary daemon — the v2 conversation ID and the
// v2 message ID that the read API handed it — for a Signal, a WhatsApp and a
// Google conversation, and requires each reaction to reach that platform's
// transport naming the right conversation and target.
func TestR5ReactRoutesThroughTheOutboxOnV2Primary(t *testing.T) {
	fixture := buildR5LegacyFixture(t)
	now := time.Date(2026, 7, 17, 18, 0, 0, 0, time.UTC)
	runR5Migrate(t, fixture.DataDir, filepath.Join(fixture.DataDir, "v2"), false, now)

	t.Setenv("OPENMESSAGES_DATA_DIR", fixture.DataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	t.Setenv("OPENMESSAGES_V2_PRIMARY", "1")
	t.Setenv("OPENMESSAGES_V2_SEND", "")
	t.Setenv("OPENMESSAGES_V2_INGEST", "")

	stack, err := newV2Stack(v2StackDeps{DataDir: fixture.DataDir, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("open migrated v2 stack: %v", err)
	}
	t.Cleanup(func() { _ = stack.Store.Close() })
	adapters := map[string]*r5ReactRouteAdapter{
		"sms":      {Adapter: scripted.New(r5GoogleAccountID, bridge.PlatformGoogle)},
		"whatsapp": {Adapter: scripted.New(r5WhatsAppAccountID, bridge.PlatformWhatsApp)},
		"signal":   {Adapter: scripted.New(r5SignalAccountID, bridge.PlatformSignal)},
	}
	for platform, adapter := range adapters {
		if err := stack.RegisterAdapter(adapter); err != nil {
			t.Fatalf("register scripted %s adapter: %v", platform, err)
		}
	}
	inertLegacy, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create inert legacy store: %v", err)
	}
	t.Cleanup(func() { _ = inertLegacy.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	stopStack := stack.Start(ctx, inertLegacy, nil, true)
	t.Cleanup(func() {
		stopStack()
		cancel()
	})

	var (
		legacyMu    sync.Mutex
		legacyCalls []r5LegacyReactionCall
	)
	legacyReaction := func(platform string) func(conversationID, messageID, emoji, action string) error {
		return func(conversationID, messageID, _, _ string) error {
			legacyMu.Lock()
			legacyCalls = append(legacyCalls, r5LegacyReactionCall{platform, conversationID, messageID})
			legacyMu.Unlock()
			return nil
		}
	}
	reads := v2read.New(stack.Store)
	// The daemon's wiring (cmd/serve.go), with the legacy per-platform reaction
	// callbacks recorded and no live Google client.
	handler := web.APIHandlerWithOptions(inertLegacy, nil, zerolog.Nop(), nil, web.APIOptions{
		Reads:     reads,
		V2Primary: true,
		V2: &web.V2Options{
			Service: stack.Service, Media: stack.Media, V2Store: stack.Store,
			Blobs: stack.Blobs, Registry: stack.Registry,
		},
		SendSignalReaction:   legacyReaction("signal"),
		SendWhatsAppReaction: legacyReaction("whatsapp"),
	})

	for _, conversation := range fixture.Conversations {
		adapter := adapters[conversation.Platform]
		if adapter == nil || conversation.LegacyID == r5SignalGroup {
			continue
		}
		t.Run(conversation.Platform, func(t *testing.T) {
			// The UI reads the thread, then reacts to a message it rendered.
			listing := httptest.NewRecorder()
			handler.ServeHTTP(listing, httptest.NewRequest(
				http.MethodGet, "http://127.0.0.1/api/conversations/"+conversation.V2ID()+"/messages?limit=100", nil,
			))
			var listed []*db.Message
			if err := json.Unmarshal(listing.Body.Bytes(), &listed); err != nil || listing.Code != http.StatusOK {
				t.Fatalf("list messages = %d, %v: %s", listing.Code, err, listing.Body.String())
			}
			incoming := conversation.Messages[0]
			var target *db.Message
			for _, message := range listed {
				if message.Body == incoming.Body {
					target = message
				}
			}
			if target == nil || target.ConversationID != conversation.V2ID() {
				t.Fatalf("listed messages %s lack the incoming fixture message under the v2 conversation ID", listing.Body.String())
			}

			body, err := json.Marshal(map[string]string{
				"conversation_id": target.ConversationID,
				"message_id":      target.MessageID,
				"emoji":           "👍",
				"action":          "add",
			})
			if err != nil {
				t.Fatalf("encode reaction request: %v", err)
			}
			legacyMu.Lock()
			legacyBefore := len(legacyCalls)
			legacyMu.Unlock()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(
				http.MethodPost, "http://127.0.0.1/api/react", bytes.NewReader(body),
			))
			t.Logf("POST /api/react %s -> %d %s", body, response.Code, bytes.TrimSpace(response.Body.Bytes()))

			legacyMu.Lock()
			reachedLegacy := slices.Clone(legacyCalls[legacyBefore:])
			legacyMu.Unlock()
			if len(reachedLegacy) != 0 {
				t.Errorf("the reaction went to the legacy transport callback, which reads the frozen legacy store: %+v", reachedLegacy)
			}
			if response.Code < 200 || response.Code > 299 {
				t.Errorf("POST /api/react = %d %s, want a 2xx", response.Code, bytes.TrimSpace(response.Body.Bytes()))
			}

			deadline := time.Now().Add(3 * time.Second)
			for len(adapter.reactionRequests()) == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			requests := adapter.reactionRequests()
			if len(requests) != 1 {
				t.Fatalf("the %s transport received %d reaction requests, want 1", conversation.Platform, len(requests))
			}
			request := requests[0]
			if request.Conversation.RemoteID != conversation.RemoteID || request.Target.RemoteID != incoming.RemoteID ||
				request.Emoji != "👍" || request.Action != bridge.ReactionAdd {
				t.Fatalf("reaction request = %+v, want 👍 add on %q in %q", request, incoming.RemoteID, conversation.RemoteID)
			}

			// The UI reloads the thread after the answer. Nothing echoes the
			// reaction back here, so what it shows is the own reaction the
			// outbox recorded when the transport accepted it.
			reloaded := httptest.NewRecorder()
			handler.ServeHTTP(reloaded, httptest.NewRequest(
				http.MethodGet, "http://127.0.0.1/api/conversations/"+conversation.V2ID()+"/messages?limit=100", nil,
			))
			var after []*db.Message
			if err := json.Unmarshal(reloaded.Body.Bytes(), &after); err != nil {
				t.Fatalf("reload messages: %v: %s", err, reloaded.Body.String())
			}
			for _, message := range after {
				if message.MessageID != target.MessageID {
					continue
				}
				// The Google fixture message carries a migrated 👍 with no
				// reactor, so compare with what the target showed before.
				before, now := r5ThumbsUp(t, target.Reactions), r5ThumbsUp(t, message.Reactions)
				if now.Count != before.Count+1 || slices.Contains(before.Actors, "me") || !slices.Contains(now.Actors, "me") {
					t.Fatalf("target reactions = %q before, %q after; want one more 👍, by me", target.Reactions, message.Reactions)
				}
				return
			}
			t.Fatalf("reloaded thread lacks the target %q", target.MessageID)
		})
	}
}

type r5ReactionGroup struct {
	Emoji  string   `json:"emoji"`
	Count  int      `json:"count"`
	Actors []string `json:"actors"`
}

// r5ThumbsUp returns the 👍 group of a message DTO's Reactions JSON, empty
// when there is none.
func r5ThumbsUp(t *testing.T, raw string) r5ReactionGroup {
	t.Helper()
	if raw == "" {
		return r5ReactionGroup{}
	}
	var groups []r5ReactionGroup
	if err := json.Unmarshal([]byte(raw), &groups); err != nil {
		t.Fatalf("decode reactions %q: %v", raw, err)
	}
	for _, group := range groups {
		if group.Emoji == "👍" {
			return group
		}
	}
	return r5ReactionGroup{}
}
