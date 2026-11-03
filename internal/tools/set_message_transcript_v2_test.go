package tools

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
	"github.com/maxghenis/openmessage/internal/web"
)

// transcriptV2MessageID is a voice note's ID in the v2 store: a 32-hex derived
// key, which is what every read tool hands an agent on a v2-primary install.
const transcriptV2MessageID = "5c0ffee0d15ea5e5b0a7f00dfeedface"

// v2RefusalText is the part of the tool's v2 refusal the tests require.
const v2RefusalText = "not available while v2 is the serving store"

// registeredTranscriptTool registers the tools the way serve does and returns
// set_message_transcript's handler, so a test exercises the registration.
func registeredTranscriptTool(t *testing.T, a *app.App, options Options) server.ToolHandlerFunc {
	t.Helper()
	s := server.NewMCPServer("transcript-test", "test")
	RegisterWithOptions(s, a, options)
	registered := s.GetTool("set_message_transcript")
	if registered == nil {
		t.Fatal("set_message_transcript is not registered")
	}
	return registered.Handler
}

func callTranscriptTool(t *testing.T, handler server.ToolHandlerFunc, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := handler(context.Background(), toolRequest(arguments))
	if err != nil {
		t.Fatalf("set_message_transcript handler: %v", err)
	}
	return result
}

// seedLegacyVoiceNote stores a voice note with no transcript in a legacy store.
func seedLegacyVoiceNote(t *testing.T, store *db.Store, messageID string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: "conversation-voice",
		Name:           "Sam",
		Participants:   `[]`,
		LastMessageTS:  now,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed legacy conversation: %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      messageID,
		ConversationID: "conversation-voice",
		MediaID:        "media-voice",
		MimeType:       "audio/ogg",
		TimestampMS:    now,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed legacy message: %v", err)
	}
}

// newLegacyAppStore opens the store a test's app process serves.
func newLegacyAppStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("open the app's store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// seedV2VoiceNote stores one voice note in a fresh v2 store, as a migrated
// install keeps it.
func seedV2VoiceNote(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: "google-primary", BridgeKey: "google_messages", DisplayName: "Google Messages",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: now, UpdatedAtMS: now,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID: "conversation-voice", AccountID: "google-primary", RemoteConversationID: "remote-voice",
		Kind: sqlite.ConversationKindDirect, Title: "Sam", NotificationMode: sqlite.NotificationModeAll,
		MetadataJSON: "{}", LastMessageAtMS: now, CreatedAtMS: now, UpdatedAtMS: now,
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	messages, err := sqlite.NewMessageRepository(store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageRepository(): %v", err)
	}
	if err := messages.ImportMessage(context.Background(), sqlite.MessageProjection{
		Message: sqlite.Message{
			MessageID: transcriptV2MessageID, ConversationID: "conversation-voice", AccountID: "google-primary",
			RemoteMessageID: "remote-voice-note", Direction: sqlite.MessageDirectionIncoming,
			State: sqlite.MessageStateActive, OccurredAtMS: now,
		},
		Attachments: []sqlite.MessageAttachment{{Ordinal: 0, RemoteID: "remote-audio", Filename: "voice.ogg", MIME: "audio/ogg"}},
	}); err != nil {
		t.Fatalf("ImportMessage(): %v", err)
	}
	return store
}

func assertNoTranscript(t *testing.T, store *db.Store, messageID, where string) {
	t.Helper()
	message, err := store.GetMessageByID(messageID)
	if err != nil {
		t.Fatalf("reload %s from %s: %v", messageID, where, err)
	}
	if message.Transcript != "" || message.TranscribedAtMS != 0 || message.TranscriptModel != "" {
		t.Fatalf("%s holds a transcript for %s: %q (model %q, at %d)",
			where, messageID, message.Transcript, message.TranscriptModel, message.TranscribedAtMS)
	}
}

// countingApp answers every request with 500 and counts them, standing in for
// an app the tool must not ask.
func countingApp(t *testing.T) (*localapi.Client, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, `{"error":"unexpected request"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return localapi.NewClient(server.URL, ""), &requests
}

// appAPI serves the app's real local API over store.
func appAPI(t *testing.T, store *db.Store, options web.APIOptions) *localapi.Client {
	t.Helper()
	server := httptest.NewServer(web.APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, options))
	t.Cleanup(server.Close)
	return localapi.NewClient(server.URL, "")
}

// Replays set_message_transcript in process on a migrated (v2-primary)
// install. The read tools hand out v2 message IDs, which the legacy store
// doesn't hold, and they read v2, which keeps no transcripts, so no read would
// show a transcript the tool saved to legacy. The tool must refuse and say
// why, for the ID an agent got from get_messages and for a legacy ID alike,
// and leave the legacy store as it was.
func TestSetMessageTranscriptRefusesWhileV2IsTheServingStore(t *testing.T) {
	a := testApp(t)
	seedLegacyVoiceNote(t, a.Store, "legacy-voice-note")
	options := Options{Reads: v2read.New(seedV2VoiceNote(t)), V2Primary: true}

	read, err := getMessagesHandler(a, options)(context.Background(), toolRequest(map[string]any{}))
	if err != nil {
		t.Fatalf("get_messages: %v", err)
	}
	if text := resultText(t, read); !strings.Contains(text, transcriptV2MessageID) {
		t.Fatalf("get_messages did not hand out the v2 message ID %s: %s", transcriptV2MessageID, text)
	}

	handler := registeredTranscriptTool(t, a, options)
	for _, messageID := range []string{transcriptV2MessageID, "legacy-voice-note"} {
		result := callTranscriptTool(t, handler, map[string]any{
			"message_id": messageID,
			"transcript": "hello",
			"model":      "whisper",
		})
		text := resultText(t, result)
		if !result.IsError {
			t.Fatalf("set_message_transcript(%s) reported a save no read shows: %s", messageID, text)
		}
		if !strings.Contains(text, v2RefusalText) {
			t.Fatalf("set_message_transcript(%s) = %q, want the v2 refusal", messageID, text)
		}
	}
	assertNoTranscript(t, a.Store, "legacy-voice-note", "the legacy store")
}

// In client mode on a v2-primary install the tool refuses before asking the
// app, so the refusal holds with the app closed, and nothing reaches the
// app's legacy store.
func TestSetMessageTranscriptClientModeRefusesWhileV2IsTheServingStore(t *testing.T) {
	a := testApp(t)
	seedLegacyVoiceNote(t, a.Store, "legacy-voice-note")
	daemon, requests := countingApp(t)
	options := Options{Reads: v2read.New(seedV2VoiceNote(t)), V2Primary: true, Daemon: daemon}

	handler := registeredTranscriptTool(t, a, options)
	for _, messageID := range []string{transcriptV2MessageID, "legacy-voice-note"} {
		result := callTranscriptTool(t, handler, map[string]any{"message_id": messageID, "transcript": "hello"})
		text := resultText(t, result)
		if !result.IsError || !strings.Contains(text, v2RefusalText) {
			t.Fatalf("set_message_transcript(%s) = %q (error %v), want the v2 refusal", messageID, text, result.IsError)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("the tool sent %d requests to the app, want none", got)
	}
	assertNoTranscript(t, a.Store, "legacy-voice-note", "the legacy store")
}

// In client mode the tool saves a transcript through the app's local API, as
// send_message does, and never writes the store this process opened. The test
// gives the two processes separate stores, in place of the one file they share
// on an install, to tell which process wrote. The legacy semantics survive the
// trip: an omitted model keeps the stored one, an empty transcript clears
// the transcript and its model, and an unknown ID gets "message not found".
func TestSetMessageTranscriptClientModeSavesThroughTheApp(t *testing.T) {
	a := testApp(t)
	seedLegacyVoiceNote(t, a.Store, "voice-note")
	appStore := newLegacyAppStore(t)
	seedLegacyVoiceNote(t, appStore, "voice-note")
	handler := registeredTranscriptTool(t, a, Options{Daemon: appAPI(t, appStore, web.APIOptions{})})

	saved := callTranscriptTool(t, handler, map[string]any{
		"message_id": "voice-note",
		"transcript": "hé🙂",
		"model":      "whisper",
	})
	if saved.IsError {
		t.Fatalf("set_message_transcript: %s", resultText(t, saved))
	}
	if text := resultText(t, saved); !strings.Contains(text, `(3 chars, model="whisper")`) {
		t.Fatalf("result = %q, want the rune count and the stored model", text)
	}
	message, err := appStore.GetMessageByID("voice-note")
	if err != nil {
		t.Fatal(err)
	}
	if message.Transcript != "hé🙂" || message.TranscriptModel != "whisper" || message.TranscribedAtMS == 0 {
		t.Fatalf("the app's store holds %q (model %q, at %d), want the saved transcript",
			message.Transcript, message.TranscriptModel, message.TranscribedAtMS)
	}
	assertNoTranscript(t, a.Store, "voice-note", "the MCP process's store")

	refined := callTranscriptTool(t, handler, map[string]any{"message_id": "voice-note", "transcript": "refined"})
	if refined.IsError {
		t.Fatalf("set_message_transcript(refined): %s", resultText(t, refined))
	}
	if text := resultText(t, refined); !strings.Contains(text, `model="whisper"`) {
		t.Fatalf("result = %q, want the model the app kept", text)
	}
	if message, _ := appStore.GetMessageByID("voice-note"); message.Transcript != "refined" || message.TranscriptModel != "whisper" {
		t.Fatalf("the app's store holds %q (model %q), want the refined transcript under the kept model",
			message.Transcript, message.TranscriptModel)
	}

	cleared := callTranscriptTool(t, handler, map[string]any{"message_id": "voice-note", "transcript": ""})
	if cleared.IsError {
		t.Fatalf("set_message_transcript(clear): %s", resultText(t, cleared))
	}
	assertNoTranscript(t, appStore, "voice-note", "the app's store after clearing")

	missing := callTranscriptTool(t, handler, map[string]any{"message_id": "no-such-message", "transcript": "x"})
	if text := resultText(t, missing); !missing.IsError || !strings.Contains(text, "message not found") {
		t.Fatalf("set_message_transcript(unknown) = %q (error %v), want message not found", text, missing.IsError)
	}
	assertNoTranscript(t, a.Store, "voice-note", "the MCP process's store")
}

// With the app closed, client mode can't save a transcript: it says to start
// the app, as the send tools do, and doesn't write the store itself.
func TestSetMessageTranscriptClientModeWithTheAppClosed(t *testing.T) {
	a := testApp(t)
	seedLegacyVoiceNote(t, a.Store, "voice-note")
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	handler := registeredTranscriptTool(t, a, Options{Daemon: localapi.NewClient(closedURL, "")})
	result := callTranscriptTool(t, handler, map[string]any{"message_id": "voice-note", "transcript": "hello"})
	if text := resultText(t, result); !result.IsError || !strings.Contains(text, daemonDownText) {
		t.Fatalf("set_message_transcript = %q (error %v), want the app-closed error", text, result.IsError)
	}
	assertNoTranscript(t, a.Store, "voice-note", "the MCP process's store")
}

// transcriptScript is a random sequence of set_message_transcript calls.
// Arguments come from small pools, so repeats (which leave the message as it
// was), clears, kept models, unknown and missing IDs, and every validation
// failure all turn up.
type transcriptScript struct {
	Calls []map[string]any
}

func (transcriptScript) Generate(r *rand.Rand, _ int) reflect.Value {
	messageIDs := []string{"voice-note", "voice-note", "voice-note", "no-such-message", ""}
	// "<" at the limit: JSON escaping makes the app's request six times its length.
	transcripts := []any{"", "hello", "hé🙂", "hello again", strings.Repeat("<", db.MaxTranscriptBytes), strings.Repeat("x", db.MaxTranscriptBytes+1), 42}
	models := []any{"", "whisper", "faster-whisper:base.en", strings.Repeat("m", db.MaxTranscriptModelBytes+1), 7}
	calls := make([]map[string]any, 1+r.Intn(12))
	for i := range calls {
		call := map[string]any{"message_id": messageIDs[r.Intn(len(messageIDs))]}
		if r.Intn(10) > 0 {
			call["transcript"] = transcripts[r.Intn(len(transcripts))]
		}
		if r.Intn(3) > 0 {
			call["model"] = models[r.Intn(len(models))]
		}
		calls[i] = call
	}
	return reflect.ValueOf(transcriptScript{Calls: calls})
}

type transcriptState struct {
	Transcript string
	Model      string
	Stamped    bool
}

func (s transcriptState) String() string {
	transcript := s.Transcript
	if len(transcript) > 24 {
		transcript = fmt.Sprintf("%.12s… (%d bytes)", transcript, len(transcript))
	}
	return fmt.Sprintf("{transcript %q, model %q, stamped %v}", transcript, s.Model, s.Stamped)
}

func voiceNoteTranscript(t *testing.T, store *db.Store) transcriptState {
	t.Helper()
	message, err := store.GetMessageByID("voice-note")
	if err != nil {
		t.Fatalf("reload voice-note: %v", err)
	}
	return transcriptState{Transcript: message.Transcript, Model: message.TranscriptModel, Stamped: message.TranscribedAtMS != 0}
}

// describeCall prints a call's arguments with long strings cut short, so a
// failure log stays readable.
func describeCall(call map[string]any) string {
	short := make(map[string]any, len(call))
	for key, value := range call {
		if text, ok := value.(string); ok && len(text) > 24 {
			value = fmt.Sprintf("%.12s… (%d bytes)", text, len(text))
		}
		short[key] = value
	}
	return fmt.Sprint(short)
}

// savedSummary is the "(N chars, model=...)." tail both serving modes put on
// a successful save.
func savedSummary(text string) string {
	if i := strings.LastIndex(text, "("); i >= 0 {
		return text[i:]
	}
	return text
}

// Invariants of set_message_transcript across serving modes, for random call
// sequences:
//   - client mode on a legacy-primary install agrees with the in-process tool
//     call by call: the same error text, or the same saved length and model,
//     and the store that took the save holds the same transcript, model and
//     stamp (set or cleared);
//   - client mode never writes the store its own process opened;
//   - on a v2-primary install every call gets the v2 refusal, in process and
//     in client mode, whatever its arguments; neither legacy store changes,
//     and client mode never asks the app.
func TestSetMessageTranscriptServingModesAgreeProperty(t *testing.T) {
	property := func(script transcriptScript) bool {
		inProcess := testApp(t)
		seedLegacyVoiceNote(t, inProcess.Store, "voice-note")
		client := testApp(t)
		seedLegacyVoiceNote(t, client.Store, "voice-note")
		appStore := newLegacyAppStore(t)
		seedLegacyVoiceNote(t, appStore, "voice-note")
		v2InProcess := testApp(t)
		seedLegacyVoiceNote(t, v2InProcess.Store, "voice-note")
		v2Client := testApp(t)
		seedLegacyVoiceNote(t, v2Client.Store, "voice-note")
		v2Daemon, v2Requests := countingApp(t)

		inProcessTool := registeredTranscriptTool(t, inProcess, Options{})
		clientTool := registeredTranscriptTool(t, client, Options{Daemon: appAPI(t, appStore, web.APIOptions{})})
		v2Tools := []server.ToolHandlerFunc{
			registeredTranscriptTool(t, v2InProcess, Options{V2Primary: true}),
			registeredTranscriptTool(t, v2Client, Options{V2Primary: true, Daemon: v2Daemon}),
		}
		copyOf := func(call map[string]any) map[string]any {
			copied := make(map[string]any, len(call))
			for key, value := range call {
				copied[key] = value
			}
			return copied
		}

		for i, call := range script.Calls {
			want := callTranscriptTool(t, inProcessTool, copyOf(call))
			got := callTranscriptTool(t, clientTool, copyOf(call))
			wantText, gotText := resultText(t, want), resultText(t, got)
			switch {
			case want.IsError != got.IsError,
				want.IsError && wantText != gotText,
				!want.IsError && savedSummary(wantText) != savedSummary(gotText):
				t.Logf("call %d %v: in process %q (error %v), client mode %q (error %v)", i, describeCall(call), wantText, want.IsError, gotText, got.IsError)
				return false
			}
			if wantState, gotState := voiceNoteTranscript(t, inProcess.Store), voiceNoteTranscript(t, appStore); wantState != gotState {
				t.Logf("call %d %v: in-process store %+v, app's store %+v", i, describeCall(call), wantState, gotState)
				return false
			}
			if state := voiceNoteTranscript(t, client.Store); state != (transcriptState{}) {
				t.Logf("call %d %v: client mode wrote its own store: %+v", i, describeCall(call), state)
				return false
			}
			for _, tool := range v2Tools {
				refusal := callTranscriptTool(t, tool, copyOf(call))
				if text := resultText(t, refusal); !refusal.IsError || !strings.Contains(text, v2RefusalText) {
					t.Logf("call %d %v on v2: %q (error %v), want the v2 refusal", i, describeCall(call), text, refusal.IsError)
					return false
				}
			}
		}
		for _, store := range []*db.Store{v2InProcess.Store, v2Client.Store} {
			if state := voiceNoteTranscript(t, store); state != (transcriptState{}) {
				t.Logf("a v2-primary call wrote the legacy store: %+v", state)
				return false
			}
		}
		if got := v2Requests.Load(); got != 0 {
			t.Logf("client mode on v2 sent %d requests to the app", got)
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 60, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
}

// A client that started before the app, with no v2 hint in its environment,
// serves the legacy store, and the app it later reaches runs v2-primary. The
// app refuses the transcript, and the tool passes on why.
func TestSetMessageTranscriptClientModeReportsAV2PrimaryAppsRefusal(t *testing.T) {
	a := testApp(t)
	seedLegacyVoiceNote(t, a.Store, "voice-note")
	appStore := newLegacyAppStore(t)
	seedLegacyVoiceNote(t, appStore, "voice-note")
	handler := registeredTranscriptTool(t, a, Options{Daemon: appAPI(t, appStore, web.APIOptions{V2Primary: true})})

	result := callTranscriptTool(t, handler, map[string]any{"message_id": "voice-note", "transcript": "hello"})
	if text := resultText(t, result); !result.IsError || !strings.Contains(text, v2RefusalText) {
		t.Fatalf("set_message_transcript = %q (error %v), want the app's v2 refusal", text, result.IsError)
	}
	assertNoTranscript(t, appStore, "voice-note", "the app's legacy store")
	assertNoTranscript(t, a.Store, "voice-note", "the MCP process's store")
}
