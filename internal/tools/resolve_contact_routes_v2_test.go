package tools

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/v2read"
	"github.com/maxghenis/openmessage/internal/web"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// routeFixtureAccount is the Google Messages account a migrated install files
// its v2 threads under.
const routeFixtureAccount = "google-primary"

// routeFixturePerson is one person in a route fixture. LegacyThreadID is the
// Google thread number the legacy store files them under; RemoteThreadID is
// the number their v2 thread carries now. A phone change re-numbers Google
// threads (issue #177), so the two can differ, and either may be empty.
// Number is the E.164 form v2 stores; LegacyNumber, when set, is the form the
// legacy store keeps, as the address book writes it. NamelessInV2 leaves the
// v2 thread with the number only, as for someone missing from the phone's
// contacts, so only the legacy store knows the name. ParticipantName, when
// set, is the name the person's v2 participant row carries in place of the
// identity's name; reads show that name, and v2's search matches both. Kind
// is the kind of the v2 identity that holds Number, e164 when empty; a person
// with an email address or a user path has the kind that v2 gives that value.
type routeFixturePerson struct {
	Name            string
	Number          string
	Kind            string
	LegacyNumber    string
	LegacyThreadID  string
	RemoteThreadID  string
	InAddressBook   bool
	NamelessInV2    bool
	ParticipantName string
}

// routeFixtureOwner is the account's owner. Both stores list the owner in
// every fixture thread, as a live install's stores list them in most
// threads. ParticipantName, when set, is the name the owner's v2 participant
// rows carry in place of the identity's name, as some live rows do; reads
// show that name. InAddressBook files the owner's own card in the address
// book, as a phone's "my card" does. LegacyUnflaggedCopy lists the owner a
// second time in each legacy thread, under the same name and number but
// without is_me, as Google's participant lists do in 211 of the 1,355
// one-to-one SMS threads of one live legacy store. v2 folds that copy into
// the owner's self identity.
type routeFixtureOwner struct {
	Name                string
	ParticipantName     string
	Number              string
	InAddressBook       bool
	LegacyUnflaggedCopy bool
}

// defaultRouteFixtureOwner shares no name or number with the people the
// tests seed.
var defaultRouteFixtureOwner = routeFixtureOwner{Name: "Me", Number: "+15550009999"}

// seedRouteFixture seeds people under the default owner.
func seedRouteFixture(t *testing.T, legacy *db.Store, store *sqlite.Store, people []routeFixturePerson) map[string]string {
	t.Helper()
	return seedRouteFixtureWithOwner(t, legacy, store, defaultRouteFixtureOwner, people)
}

// seedRouteFixtureWithOwner writes people into the legacy store and the v2
// store and returns each v2 thread's conversation ID by name. It writes
// legacy rows in order, so a later person under the same thread number
// overwrites an earlier one, as the 2026-09-03 re-numbering overwrote legacy
// rows.
func seedRouteFixtureWithOwner(t *testing.T, legacy *db.Store, store *sqlite.Store, owner routeFixtureOwner, people []routeFixturePerson) map[string]string {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: routeFixtureAccount, BridgeKey: "google_messages", DisplayName: "Google Messages",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: now, UpdatedAtMS: now,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	self := sqlite.Identity{
		IdentityID: "identity-self", AccountID: routeFixtureAccount, Kind: sqlite.IdentityKind("e164"),
		CanonicalValue: owner.Number, RawValue: owner.Number, DisplayName: owner.Name, IsSelf: true,
		MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
	}
	if err := store.UpsertIdentity(self); err != nil {
		t.Fatalf("UpsertIdentity(self): %v", err)
	}
	if owner.InAddressBook {
		if err := legacy.UpsertContact(&db.Contact{ContactID: "contact-owner", Name: owner.Name, Number: owner.Number}); err != nil {
			t.Fatalf("seed the owner's contact card: %v", err)
		}
	}

	threads := map[string]string{}
	for index, person := range people {
		lastMS := now - int64(index)
		legacyNumber := firstNonEmpty(person.LegacyNumber, person.Number)
		if person.LegacyThreadID != "" {
			entries := []map[string]any{
				{"name": person.Name, "number": legacyNumber},
				{"name": owner.Name, "number": owner.Number, "is_me": true},
			}
			if owner.LegacyUnflaggedCopy {
				entries = append(entries, map[string]any{
					"name": owner.Name, "number": owner.Number,
					"id": "owner-participant-" + person.LegacyThreadID, "contact_id": "owner-contact",
				})
			}
			participants, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			if err := legacy.UpsertConversation(&db.Conversation{
				ConversationID: person.LegacyThreadID,
				Name:           person.Name,
				Participants:   string(participants),
				LastMessageTS:  lastMS,
				SourcePlatform: "sms",
			}); err != nil {
				t.Fatalf("seed legacy conversation %q: %v", person.LegacyThreadID, err)
			}
		}
		if person.InAddressBook {
			if err := legacy.UpsertContact(&db.Contact{
				ContactID: "contact-" + person.Name, Name: person.Name, Number: legacyNumber,
			}); err != nil {
				t.Fatalf("seed contact %q: %v", person.Name, err)
			}
		}
		if person.RemoteThreadID == "" {
			continue
		}
		identityID := "identity-" + strings.ReplaceAll(strings.ToLower(person.Name), " ", "-")
		displayName := person.Name
		if person.NamelessInV2 {
			displayName = ""
		}
		if err := store.UpsertIdentity(sqlite.Identity{
			IdentityID: identityID, AccountID: routeFixtureAccount, Kind: sqlite.IdentityKind(firstNonEmpty(person.Kind, "e164")),
			CanonicalValue: person.Number, RawValue: person.Number, DisplayName: displayName,
			MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("UpsertIdentity(%s): %v", identityID, err)
		}
		// v2 conversation IDs outlive a re-numbering; only the remote ID
		// moves. Derive the ID from the person, not the thread number.
		conversationID := v2keys.DeriveID("conversation", routeFixtureAccount, "fixture:"+person.Name)
		if err := store.UpsertConversation(sqlite.Conversation{
			ConversationID: conversationID, AccountID: routeFixtureAccount,
			RemoteConversationID: person.RemoteThreadID, Kind: sqlite.ConversationKindDirect,
			NotificationMode: sqlite.NotificationModeAll, MetadataJSON: "{}",
			LastMessageAtMS: lastMS, CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("UpsertConversation(%s): %v", person.Name, err)
		}
		if err := store.ReplaceConversationParticipants(conversationID, []sqlite.ConversationParticipant{
			{AccountID: routeFixtureAccount, ConversationID: conversationID, IdentityID: self.IdentityID, Role: sqlite.ParticipantRoleMember, DisplayName: owner.ParticipantName, IsActive: true},
			{AccountID: routeFixtureAccount, ConversationID: conversationID, IdentityID: identityID, Role: sqlite.ParticipantRoleMember, DisplayName: person.ParticipantName, IsActive: true},
		}); err != nil {
			t.Fatalf("ReplaceConversationParticipants(%s): %v", person.Name, err)
		}
		threads[person.Name] = conversationID
	}
	return threads
}

// seedTitledRouteGroup writes a group thread with title as its name, under id,
// into both stores, newer than any fixture thread. Its roster lists the owner
// and a guest whom no test query names, so a query reaches the group only
// through its title. Seed the fixture first: the v2 group lists the fixture's
// owner.
func seedTitledRouteGroup(t *testing.T, legacy *db.Store, store *sqlite.Store, owner routeFixtureOwner, id, title string) {
	t.Helper()
	seedTitledRouteGroupWithGuest(t, legacy, store, owner, id, title, "+15555550999")
}

// seedTitledRouteGroupWithGuest is seedTitledRouteGroup with the guest's
// number. v2 holds one identity for each number, so each group in a store
// needs its own guest.
func seedTitledRouteGroupWithGuest(t *testing.T, legacy *db.Store, store *sqlite.Store, owner routeFixtureOwner, id, title, guestNumber string) {
	t.Helper()
	now := time.Now().UnixMilli() + 1000
	participants, err := json.Marshal([]map[string]any{
		{"name": "Guest", "number": guestNumber},
		{"name": owner.Name, "number": owner.Number, "is_me": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.UpsertConversation(&db.Conversation{
		ConversationID: id, Name: title, IsGroup: true, Participants: string(participants),
		LastMessageTS: now, SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed legacy group %q: %v", id, err)
	}
	guest := sqlite.Identity{
		IdentityID: "identity-" + id + "-guest", AccountID: routeFixtureAccount, Kind: sqlite.IdentityKind("e164"),
		CanonicalValue: guestNumber, RawValue: guestNumber, MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
	}
	if err := store.UpsertIdentity(guest); err != nil {
		t.Fatalf("UpsertIdentity(%s): %v", guest.IdentityID, err)
	}
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID: id, AccountID: routeFixtureAccount, RemoteConversationID: "remote-" + id,
		Kind: sqlite.ConversationKindGroup, Title: title, NotificationMode: sqlite.NotificationModeAll,
		MetadataJSON: "{}", LastMessageAtMS: now, CreatedAtMS: now, UpdatedAtMS: now,
	}); err != nil {
		t.Fatalf("UpsertConversation(%s): %v", id, err)
	}
	if err := store.ReplaceConversationParticipants(id, []sqlite.ConversationParticipant{
		{AccountID: routeFixtureAccount, ConversationID: id, IdentityID: "identity-self", Role: sqlite.ParticipantRoleMember, IsActive: true},
		{AccountID: routeFixtureAccount, ConversationID: id, IdentityID: guest.IdentityID, Role: sqlite.ParticipantRoleMember, IsActive: true},
	}); err != nil {
		t.Fatalf("ReplaceConversationParticipants(%s): %v", id, err)
	}
}

// v2PrimaryClientOptions wires the tools the way `serve --mcp-stdio` does on a
// v2-primary install: reads from the v2 store, and every send through the
// running app's real local API, backed here by the harness's outbox and fake
// transport.
func v2PrimaryClientOptions(t *testing.T, h *v2ToolHarness) Options {
	t.Helper()
	handler := web.APIHandlerWithOptions(h.app.Store, nil, zerolog.Nop(), nil, web.APIOptions{
		V2Primary: true,
		V2: &web.V2Options{
			Service:  h.deps.Service,
			V2Store:  h.deps.V2Store,
			Registry: h.deps.Registry,
		},
	})
	daemon := httptest.NewServer(handler)
	t.Cleanup(daemon.Close)
	return Options{
		Reads:     v2read.New(h.deps.V2Store),
		V2Primary: true,
		Daemon:    localapi.NewClient(daemon.URL, ""),
	}
}

func callRouteTool(t *testing.T, handler server.ToolHandlerFunc, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := handler(context.Background(), toolRequest(arguments))
	if err != nil {
		t.Fatalf("tool handler: %v", err)
	}
	return result
}

func routeMatches(t *testing.T, result *mcp.CallToolResult) []resolvedRouteMatch {
	t.Helper()
	if result.IsError {
		t.Fatalf("resolve_contact_routes failed: %s", resultText(t, result))
	}
	matches, ok := structuredMap(t, result)["matches"].([]resolvedRouteMatch)
	if !ok {
		t.Fatalf("matches = %T, want []resolvedRouteMatch", structuredMap(t, result)["matches"])
	}
	return matches
}

func onlyRouteMatch(t *testing.T, result *mcp.CallToolResult) resolvedRouteMatch {
	t.Helper()
	matches := routeMatches(t, result)
	if len(matches) != 1 {
		t.Fatalf("route matches = %+v, want exactly one", matches)
	}
	return matches[0]
}

// Replays the 2026-11-03 failure in client mode against the real outbox. An
// agent asked resolve_contact_routes for a contact and sent to the preferred
// route. The tool read the legacy store and handed out Google thread number
// 2957, which a v2-primary app's outbox does not know as a conversation ID
// (HTTP 404 "not found"), while send_message by phone number, which reads the
// v2 store, worked. The route must carry the v2 conversation ID, and the send
// must reach thread 2957.
func TestClientModeSendsToTheRouteResolveContactRoutesPrefers(t *testing.T) {
	h := newV2ToolHarness(t)
	threads := seedRouteFixture(t, h.app.Store, h.deps.V2Store, []routeFixturePerson{
		{Name: "Alice Example", Number: "+15555550123", LegacyThreadID: "2957", RemoteThreadID: "2957", InAddressBook: true},
	})
	options := v2PrimaryClientOptions(t, h)

	match := onlyRouteMatch(t, callRouteTool(t, resolveContactRoutesHandler(h.app, options), map[string]any{"query": "Alice"}))
	if match.PreferredReplyConversationID != threads["Alice Example"] {
		t.Fatalf("preferred_reply_conversation_id = %q, want the v2 conversation %q", match.PreferredReplyConversationID, threads["Alice Example"])
	}
	for _, route := range match.Routes {
		if route.Conversation.ConversationID != threads["Alice Example"] {
			t.Fatalf("route conversation_id = %q, want only %q; routes=%+v", route.Conversation.ConversationID, threads["Alice Example"], match.Routes)
		}
	}

	sent := callRouteTool(t, daemonSendToConversationHandler(options), map[string]any{
		"conversation_id": match.PreferredReplyConversationID,
		"message":         "Here is the link",
		"idempotency_key": "route-send-1",
	})
	if sent.IsError {
		t.Fatalf("send_to_conversation(preferred route) failed: %s", resultText(t, sent))
	}
	payload := structuredMap(t, sent)
	if payload["ok"] != true || payload["settled"] != true || payload["state"] != "confirmed" {
		t.Fatalf("send payload = %v, want a confirmed delivery", payload)
	}
	requests := h.sender.snapshotRequests()
	if len(requests) != 1 || requests[0].Conversation.RemoteID != "2957" || requests[0].Body != "Here is the link" {
		t.Fatalf("transport requests = %+v, want one text to Google thread 2957", requests)
	}
}

// A Google thread number is a transport address, not a conversation ID: it
// is device-local, and a phone change re-numbers threads (issue #177). The
// outbox refuses it, says what to send instead, and sends nothing.
func TestClientModeSendRefusesAGoogleThreadNumberAndSaysWhatToUse(t *testing.T) {
	h := newV2ToolHarness(t)
	seedRouteFixture(t, h.app.Store, h.deps.V2Store, []routeFixturePerson{
		{Name: "Alice Example", Number: "+15555550123", LegacyThreadID: "2957", RemoteThreadID: "2957", InAddressBook: true},
	})
	options := v2PrimaryClientOptions(t, h)
	file := filepath.Join(t.TempDir(), "link.png")
	if err := os.WriteFile(file, []byte("\x89PNG\r\n\x1a\nroute fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, call := range map[string]func() *mcp.CallToolResult{
		"send_to_conversation": func() *mcp.CallToolResult {
			return callRouteTool(t, daemonSendToConversationHandler(options), map[string]any{
				"conversation_id": "2957", "message": "Here is the link",
			})
		},
		"send_media_to_conversation": func() *mcp.CallToolResult {
			return callRouteTool(t, daemonSendMediaToConversationHandler(options), map[string]any{
				"conversation_id": "2957", "file_path": file,
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			result := call()
			if !result.IsError {
				t.Fatalf("%s(2957) = %v, want a rejection", name, structuredMap(t, result))
			}
			message, _ := structuredMap(t, result)["error"].(string)
			for _, want := range []string{"rejected by the app", "HTTP 404", "unknown conversation_id", "resolve_contact_routes"} {
				if !strings.Contains(message, want) {
					t.Fatalf("%s(2957) error = %q, want it to contain %q", name, message, want)
				}
			}
		})
	}
	if count := h.sender.requestCount(); count != 0 {
		t.Fatalf("transport saw %d text sends, want none", count)
	}
	pending, err := h.deps.Service.ListPending(context.Background(), messaging.ListPendingQuery{Limit: 10})
	if err != nil || len(pending) != 0 {
		t.Fatalf("refused sends left outbox rows: %+v, %v", pending, err)
	}
}

// After a phone change re-numbers Google threads (2026-09-03, issue #177),
// the legacy store still files Alice under thread 2957, while 2957 now names
// Bob's thread and Alice's thread is 3100. Routing Alice by her legacy thread
// number would text Bob, so routes must come from the serving store's own
// participants.
func TestResolveContactRoutesNeverRoutesByAReusedGoogleThreadNumber(t *testing.T) {
	h := newV2ToolHarness(t)
	threads := seedRouteFixture(t, h.app.Store, h.deps.V2Store, []routeFixturePerson{
		{Name: "Alice Example", Number: "+15555550123", LegacyThreadID: "2957", RemoteThreadID: "3100", InAddressBook: true},
		{Name: "Bob Example", Number: "+15555550199", RemoteThreadID: "2957"},
	})
	options := v2PrimaryClientOptions(t, h)

	for _, query := range []string{"Alice", "+15555550123", "5555550123"} {
		t.Run(query, func(t *testing.T) {
			match := onlyRouteMatch(t, callRouteTool(t, resolveContactRoutesHandler(h.app, options), map[string]any{"query": query}))
			if match.PreferredReplyConversationID != threads["Alice Example"] {
				t.Fatalf("preferred_reply_conversation_id = %q, want Alice's thread %q (Bob's is %q)",
					match.PreferredReplyConversationID, threads["Alice Example"], threads["Bob Example"])
			}
			for _, route := range match.Routes {
				if route.Conversation.ConversationID != threads["Alice Example"] {
					t.Fatalf("routes = %+v, want only Alice's thread", match.Routes)
				}
			}
		})
	}

	// A thread number names no route either. 2957, Alice's stale legacy ID,
	// names Bob's thread now, so an agent recovering from a refused 2957 must
	// not get Bob's thread back; 3100, Alice's thread number today, is just
	// as device-local.
	for _, query := range []string{"2957", "3100"} {
		t.Run("thread number "+query, func(t *testing.T) {
			matches := routeMatches(t, callRouteTool(t, resolveContactRoutesHandler(h.app, options), map[string]any{"query": query}))
			if len(matches) != 0 {
				t.Fatalf("routes for thread number %s = %+v, want none (Alice's thread is %q, Bob's %q)",
					query, matches, threads["Alice Example"], threads["Bob Example"])
			}
		})
	}

	match := onlyRouteMatch(t, callRouteTool(t, resolveContactRoutesHandler(h.app, options), map[string]any{"query": "Alice"}))
	sent := callRouteTool(t, daemonSendToConversationHandler(options), map[string]any{
		"conversation_id": match.PreferredReplyConversationID,
		"message":         "for Alice",
		"idempotency_key": "reused-thread-1",
	})
	if sent.IsError {
		t.Fatalf("send_to_conversation failed: %s", resultText(t, sent))
	}
	requests := h.sender.snapshotRequests()
	if len(requests) != 1 || requests[0].Conversation.RemoteID != "3100" {
		t.Fatalf("transport requests = %+v, want one text to Alice's thread 3100", requests)
	}
}

// routeNumberForms lists ways people and address books write a +1 number.
func routeNumberForms(number string) []string {
	digits := strings.TrimPrefix(number, "+1")
	area, exchange, line := digits[:3], digits[3:6], digits[6:]
	return []string{
		number,
		"+1 (" + area + ") " + exchange + "-" + line,
		"(" + area + ") " + exchange + "-" + line,
		area + "-" + exchange + "-" + line,
		area + "." + exchange + "." + line,
		"1-" + area + "-" + exchange + "-" + line,
		"+1 " + area + " " + exchange + " " + line,
		area + exchange + line,
	}
}

// v2 stores numbers in E.164 form, while the legacy store keeps them as the
// address book writes them. A number in any form, or a name that only the
// legacy store knows, must still find the person's v2 thread: the one
// send_message picks for that number.
func TestResolveContactRoutesFindsPeopleByAnyFormOfTheirNumber(t *testing.T) {
	h := newV2ToolHarness(t)
	people := []routeFixturePerson{
		// Only the address book knows Alice's name.
		{Name: "Alice Example", Number: "+15555550123", LegacyNumber: "+1 (555) 555-0123", LegacyThreadID: "2957", RemoteThreadID: "3100", InAddressBook: true, NamelessInV2: true},
		// Only a legacy thread knows Carol's name.
		{Name: "Carol Example", Number: "+15555550177", LegacyNumber: "(555) 555-0177", LegacyThreadID: "3101", RemoteThreadID: "3102", NamelessInV2: true},
		{Name: "Bob Example", Number: "+15555550199", RemoteThreadID: "2957"},
	}
	threads := seedRouteFixture(t, h.app.Store, h.deps.V2Store, people)
	options := v2PrimaryClientOptions(t, h)

	want := map[string]string{"Alice": "Alice Example", "Carol": "Carol Example", "Bob": "Bob Example"}
	for _, person := range people {
		for _, form := range routeNumberForms(person.Number) {
			want[form] = person.Name
		}
	}
	for query, name := range want {
		t.Run(query, func(t *testing.T) {
			match := onlyRouteMatch(t, callRouteTool(t, resolveContactRoutesHandler(h.app, options), map[string]any{"query": query}))
			if match.PreferredReplyConversationID != threads[name] {
				t.Fatalf("preferred_reply_conversation_id = %q, want %s's thread %q", match.PreferredReplyConversationID, name, threads[name])
			}
			for _, route := range match.Routes {
				if route.Conversation.ConversationID != threads[name] {
					t.Fatalf("routes = %+v, want only %s's thread", match.Routes, name)
				}
			}
			if !looksLikePhoneNumber(query) {
				return
			}
			direct, err := findDirectSMSConversation(options.Reads, query)
			if err != nil || direct == nil || direct.ConversationID != threads[name] {
				t.Fatalf("send_message's thread for %q = %+v (%v), want %s's thread %q", query, direct, err, name, threads[name])
			}
		})
	}
}

// Both stores list the account's owner in most threads, and a phone's own
// contact card puts the owner in the address book. The legacy store's Google
// threads can list the owner twice, once without is_me. A thread that matches
// a query only through the owner says nothing about whom to send to. Here,
// where no title holds the owner's name, the owner's names and number route
// nowhere, and a search for "Sam" finds only Sam Example's thread, not the
// other threads that list the owner, Sam Owner, or hold the owner's
// messages. This holds whichever store serves reads.
func TestResolveContactRoutesIgnoresTheAccountOwner(t *testing.T) {
	owner := routeFixtureOwner{Name: "Sam Owner", ParticipantName: "Sammy", Number: "+15550009999", InAddressBook: true, LegacyUnflaggedCopy: true}
	people := []routeFixturePerson{
		{Name: "Sam Example", Number: "+15555550123", LegacyThreadID: "3100", RemoteThreadID: "3100"},
		{Name: "Bob Example", Number: "+15555550199", LegacyThreadID: "3101", RemoteThreadID: "3101", InAddressBook: true},
	}
	status := http.NewServeMux()
	status.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"connected": true})
	})
	daemon := daemonClientFor(t, status)

	for _, mode := range []string{"v2-primary", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
			if err != nil {
				t.Fatalf("sqlite.Open(): %v", err)
			}
			defer store.Close()
			threads := seedRouteFixtureWithOwner(t, a.Store, store, owner, people)
			// A legacy thread that doesn't list the owner still holds a
			// message the owner sent, under the owner's name and number.
			if err := a.Store.UpsertConversation(&db.Conversation{
				ConversationID: "3102", Name: "Dee Example", SourcePlatform: "sms",
				Participants: `[{"name":"Dee Example","number":"+15555550188"}]`, LastMessageTS: 1,
			}); err != nil {
				t.Fatalf("seed Dee's legacy thread: %v", err)
			}
			if err := a.Store.UpsertMessage(&db.Message{
				MessageID: "owner-wrote", ConversationID: "3102", SenderName: owner.Name, SenderNumber: owner.Number,
				IsFromMe: true, Body: "on my way", TimestampMS: 1, SourcePlatform: "sms",
			}); err != nil {
				t.Fatalf("seed the owner's message to Dee: %v", err)
			}
			options := Options{Reads: v2read.New(store), V2Primary: true, Daemon: daemon}
			if mode == "legacy" {
				// A legacy install routes by the legacy store's thread IDs.
				options = Options{Reads: a.Store, Daemon: daemon}
				threads = map[string]string{"Sam Example": "3100", "Bob Example": "3101"}
			}
			handler := resolveContactRoutesHandler(a, options)

			for _, query := range append([]string{"Sam Owner", "Owner", "Sammy"}, routeNumberForms(owner.Number)...) {
				if matches := routeMatches(t, callRouteTool(t, handler, map[string]any{"query": query})); len(matches) != 0 {
					t.Errorf("routes for the owner's %q = %+v, want none", query, matches)
				}
			}
			for query, name := range map[string]string{"Sam": "Sam Example", "Bob": "Bob Example"} {
				matches := routeMatches(t, callRouteTool(t, handler, map[string]any{"query": query}))
				if len(matches) != 1 || matches[0].PreferredReplyConversationID != threads[name] || matches[0].RouteCount != 1 {
					t.Errorf("routes for %q = %+v, want one match with one route, %s's thread %q", query, matches, name, threads[name])
				}
			}
		})
	}
}

// v2's search matches a participant by the name on their participant row or
// by their identity's name, and reads show the row's name when it has one.
// Here the owner is Sam Owner and Sam Example's row reads "Bobby", so a search
// for "Sam" finds Sam's thread through a name that reads hide, while the only
// "Sam" that reads show is the owner's. The thread still routes: v2's search
// skips the owner, so none of its hits matches only through the owner.
func TestResolveContactRoutesKeepsV2MatchesOnANameReadsHide(t *testing.T) {
	a := testApp(t)
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	defer store.Close()
	owner := routeFixtureOwner{Name: "Sam Owner", Number: "+15550009999"}
	threads := seedRouteFixtureWithOwner(t, a.Store, store, owner, []routeFixturePerson{
		{Name: "Sam Example", ParticipantName: "Bobby", Number: "+15555550177", RemoteThreadID: "3100"},
	})
	status := http.NewServeMux()
	status.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"connected": true})
	})
	options := Options{Reads: v2read.New(store), V2Primary: true, Daemon: daemonClientFor(t, status)}
	handler := resolveContactRoutesHandler(a, options)

	for _, query := range []string{"Sam", "Sam Example", "Bobby"} {
		matches := routeMatches(t, callRouteTool(t, handler, map[string]any{"query": query}))
		if len(matches) != 1 || matches[0].PreferredReplyConversationID != threads["Sam Example"] {
			t.Errorf("routes for %q = %+v, want one whose preferred route is Sam Example's thread %q", query, matches, threads["Sam Example"])
		}
	}
	if matches := routeMatches(t, callRouteTool(t, handler, map[string]any{"query": owner.Name})); len(matches) != 0 {
		t.Errorf("routes for the owner's %q = %+v, want none", owner.Name, matches)
	}
}

// In-process serving (the app's own MCP endpoint, or `serve --transports`)
// takes the same IDs as client mode. send_media_to_conversation looked the ID
// up in the legacy store before it reached the v2 outbox, so on a v2-primary
// install it refused every ID the read tools return ("failed to load
// conversation"), while send_to_conversation took them.
func TestInProcessSendsTakeTheIDsResolveContactRoutesReturns(t *testing.T) {
	h := newV2ToolHarness(t)
	h.deps.V2Primary = true
	threads := seedRouteFixture(t, h.app.Store, h.deps.V2Store, []routeFixturePerson{
		{Name: "Alice Example", Number: "+15555550123", LegacyThreadID: "2957", RemoteThreadID: "3100", InAddressBook: true},
		{Name: "Bob Example", Number: "+15555550199", RemoteThreadID: "2957"},
	})
	options := Options{Reads: v2read.New(h.deps.V2Store), V2Primary: true, V2: &h.deps}
	match := onlyRouteMatch(t, callRouteTool(t, resolveContactRoutesHandler(h.app, options), map[string]any{"query": "Alice"}))
	if match.PreferredReplyConversationID != threads["Alice Example"] {
		t.Fatalf("preferred_reply_conversation_id = %q, want Alice's thread %q", match.PreferredReplyConversationID, threads["Alice Example"])
	}
	file := filepath.Join(t.TempDir(), "link.png")
	if err := os.WriteFile(file, []byte("\x89PNG\r\n\x1a\nroute fixture"), 0o600); err != nil {
		t.Fatal(err)
	}

	sends := map[string]func(conversationID, key string) *mcp.CallToolResult{
		"send_to_conversation": func(conversationID, key string) *mcp.CallToolResult {
			return callRouteTool(t, sendToConversationHandler(h.app, &h.deps), map[string]any{
				"conversation_id": conversationID, "message": "for Alice", "idempotency_key": key,
			})
		},
		"send_media_to_conversation": func(conversationID, key string) *mcp.CallToolResult {
			return callRouteTool(t, sendMediaToConversationHandler(h.app, &h.deps), map[string]any{
				"conversation_id": conversationID, "file_path": file, "idempotency_key": key,
			})
		},
	}
	for name, send := range sends {
		t.Run(name, func(t *testing.T) {
			sent := send(match.PreferredReplyConversationID, "in-process-"+name)
			if sent.IsError {
				t.Fatalf("%s(preferred route) failed: %s", name, resultText(t, sent))
			}
			if payload := v2ToolPayload(t, sent); payload["ok"] != true || payload["state"] != "confirmed" {
				t.Fatalf("%s payload = %v, want a confirmed delivery", name, payload)
			}
			// 2957 is Alice's legacy thread number, and Bob's thread number now.
			refused := send("2957", "in-process-2957-"+name)
			if !refused.IsError || !strings.Contains(resultText(t, refused), "unknown conversation") {
				t.Fatalf("%s(2957) = %s, want an unknown-conversation refusal", name, resultText(t, refused))
			}
		})
	}
	if requests := h.sender.snapshotRequests(); len(requests) != 1 || requests[0].Conversation.RemoteID != "3100" {
		t.Fatalf("text requests = %+v, want one to Alice's thread 3100", requests)
	}
	if requests := h.media.snapshotRequests(); len(requests) != 1 || requests[0].ConversationID != "3100" {
		t.Fatalf("media requests = %+v, want one to Alice's thread 3100", requests)
	}
}

// In client mode this process holds no transports, so whether WhatsApp or
// Signal can send is the running app's answer. This process's own bridges
// always answer "not connected", and building them here opens the app's
// WhatsApp session store and starts Signal temp-dir sweeps.
func TestResolveContactRoutesClientModeAsksTheAppWhichPlatformsCanSend(t *testing.T) {
	a := testApp(t)
	now := time.Now().UnixMilli()
	for _, conversation := range []*db.Conversation{
		{ConversationID: "whatsapp:15551230000@s.whatsapp.net", Name: "Leigh Gibson", Participants: `[{"name":"Leigh Gibson","number":"+15551230000"}]`, LastMessageTS: now, SourcePlatform: "whatsapp"},
		{ConversationID: "signal:+15551230000", Name: "Leigh Gibson", Participants: `[{"name":"Leigh Gibson","number":"+15551230000"}]`, LastMessageTS: now + 1, SourcePlatform: "signal"},
	} {
		if err := a.Store.UpsertConversation(conversation); err != nil {
			t.Fatalf("seed %s: %v", conversation.ConversationID, err)
		}
	}
	originalWhatsAppStatus, originalSignalStatus := whatsAppStatus, signalStatus
	whatsAppStatus = func(*app.App) whatsapplive.StatusSnapshot {
		t.Error("client mode asked this process's WhatsApp bridge for its status")
		return whatsapplive.StatusSnapshot{}
	}
	signalStatus = func(*app.App) signallive.StatusSnapshot {
		t.Error("client mode asked this process's Signal bridge for its status")
		return signallive.StatusSnapshot{}
	}
	t.Cleanup(func() { whatsAppStatus, signalStatus = originalWhatsAppStatus, originalSignalStatus })

	sendable := func(match resolvedRouteMatch) map[string]bool {
		byPlatform := map[string]bool{}
		for _, route := range match.Routes {
			byPlatform[route.Conversation.SourcePlatform] = route.Sendable
		}
		return byPlatform
	}

	status := http.NewServeMux()
	status.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connected": true,
			"whatsapp":  map[string]any{"connected": true, "paired": true},
			"signal":    map[string]any{"connected": false, "paired": true},
		})
	})
	options := Options{Reads: a.Store, Daemon: daemonClientFor(t, status)}
	match := onlyRouteMatch(t, callRouteTool(t, resolveContactRoutesHandler(a, options), map[string]any{"query": "Leigh"}))
	if got := sendable(match); !got["whatsapp"] || got["signal"] {
		t.Fatalf("sendable by platform = %v, want the app's answer (whatsapp yes, signal no)", got)
	}
	if match.PreferredReplyConversationID != "whatsapp:15551230000@s.whatsapp.net" {
		t.Fatalf("preferred_reply_conversation_id = %q, want the connected WhatsApp thread", match.PreferredReplyConversationID)
	}

	// With the app down nothing can send through it.
	options = Options{Reads: a.Store, Daemon: deadDaemonClient()}
	match = onlyRouteMatch(t, callRouteTool(t, resolveContactRoutesHandler(a, options), map[string]any{"query": "Leigh"}))
	if got := sendable(match); got["whatsapp"] || got["signal"] || match.PreferredReplyConversationID != "" {
		t.Fatalf("app down: sendable = %v, preferred = %q, want no sendable WhatsApp or Signal route", got, match.PreferredReplyConversationID)
	}
}

// Invariants, over random re-numberings of Google threads: every conversation
// ID resolve_contact_routes returns names a conversation in the serving store,
// so the send tools accept it; a person's routes are threads that person is
// in, however the legacy store files them and even when the account's owner,
// whom every thread lists, shares their first name, beside a newer group whose
// title holds the name searched for, when one exists; a thread number, or the
// owner's name or number, yields no route; and the tool finds everyone the
// stores can identify, by their number in any form or by a name either store
// still holds, even where v2 reads show another name or a title also matches.
// Each person's legacy thread number and current v2 thread number come from
// one small pool, so legacy numbers collide, overwrite each other and name
// other people's v2 threads. Some people have an email address, which the
// legacy Google Chat importer files as a number, or a Google Chat user path in
// place of a phone number, and for some of them a stranger's address or a
// group's title holds theirs: a name query that finds the address on a legacy
// thread or a card routes to neither. Differential: send_message's thread
// lookup scans the newest 2,000 threads and skips groups. These stores hold
// far fewer threads, and no group sits on a line the test searches, so for a
// person's number in any form, that lookup and resolve_contact_routes pick the
// same thread.
func TestResolveContactRoutesReturnsOnlyServingStoreThreadsOfThePerson(t *testing.T) {
	names := []string{"Alice Example", "Bob Example", "Carol Example", "Dave Example"}
	pool := []string{"2957", "3100", "3101", "3102", "3103"}
	status := http.NewServeMux()
	status.HandleFunc("/api/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"connected": true, "v2_primary": true, "v2_send": true})
	})
	daemon := daemonClientFor(t, status)
	exercised := map[string]int{}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		a := testApp(t)
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
		if err != nil {
			t.Fatalf("sqlite.Open(): %v", err)
		}
		defer store.Close()

		remote := random.Perm(len(pool))
		people := make([]routeFixturePerson, 0, len(names))
		// traps are strangers whose addresses hold a person's, and
		// trapTitles are groups whose titles hold it, by the person's index.
		var traps []routeFixturePerson
		trapped, trapTitles := map[int]bool{}, map[int]string{}
		for index, name := range names {
			person := routeFixturePerson{
				Name:          name,
				InAddressBook: random.Intn(2) == 0,
				NamelessInV2:  random.Intn(3) == 0,
			}
			switch random.Intn(6) {
			case 0:
				// The legacy store may write the address in other letter case.
				address := "p" + strconv.Itoa(index) + ".mail@example.com"
				person.Number, person.Kind = address, "email"
				person.LegacyNumber = []string{address, "P" + address[1:]}[random.Intn(2)]
			case 1:
				address := "users/10" + strconv.Itoa(index)
				person.Number, person.Kind = address, "username"
			default:
				number := "+1555555010" + strconv.Itoa(index)
				forms := routeNumberForms(number)
				person.Number, person.LegacyNumber = number, forms[random.Intn(len(forms))]
			}
			if person.Kind != "" {
				if random.Intn(2) == 0 {
					// x then the address, or the user path then a digit.
					trap := "x" + person.Number
					if person.Kind == "username" {
						trap = person.Number + "7"
					}
					traps = append(traps, routeFixturePerson{
						Name: "Zed Trap " + strconv.Itoa(index), Number: trap, Kind: person.Kind,
						LegacyThreadID: "trap-" + strconv.Itoa(index), RemoteThreadID: "trap-remote-" + strconv.Itoa(index),
					})
					trapped[index] = true
				}
				if random.Intn(2) == 0 {
					trapTitles[index] = "Ask " + person.Number
				}
			}
			if random.Intn(4) != 0 {
				person.LegacyThreadID = pool[random.Intn(len(pool))]
			}
			if random.Intn(5) != 0 {
				person.RemoteThreadID = pool[remote[index]]
			}
			if random.Intn(2) == 0 {
				person.ParticipantName = "Contact " + strconv.Itoa(index)
			}
			people = append(people, person)
		}
		ownerFirstName := strings.Fields(names[random.Intn(len(names))])[0]
		owner := routeFixtureOwner{Name: ownerFirstName + " Owner", Number: "+15550009999", InAddressBook: random.Intn(2) == 0}
		if random.Intn(2) == 0 {
			owner.ParticipantName = ownerFirstName + " O."
		}
		// The strangers come after everyone else, under thread numbers of
		// their own.
		threads := seedRouteFixtureWithOwner(t, a.Store, store, owner, append(slices.Clone(people), traps...))
		for index, trapTitle := range trapTitles {
			seedTitledRouteGroupWithGuest(t, a.Store, store, owner, "ask-"+strconv.Itoa(index), trapTitle, "+1555555090"+strconv.Itoa(index))
		}

		// legacyNameSurvives reports whether people[index]'s legacy thread
		// still carries their name: a later person under the same thread
		// number overwrites it.
		legacyNameSurvives := func(index int) bool {
			if people[index].LegacyThreadID == "" {
				return false
			}
			for _, later := range people[index+1:] {
				if later.LegacyThreadID == people[index].LegacyThreadID {
					return false
				}
			}
			return true
		}
		// findableByName reports whether a name query can reach people[index]'s
		// v2 thread: by the name v2 holds, through the address book, or
		// through a legacy thread that still carries the name.
		findableByName := func(index int) bool {
			person := people[index]
			if threads[person.Name] == "" {
				return false
			}
			return !person.NamelessInV2 || person.InAddressBook || legacyNameSurvives(index)
		}

		var query, person, number, title string
		found, legacyOnlyName := false, false
		switch random.Intn(7) {
		case 0, 1:
			index := random.Intn(len(names))
			person, query = names[index], strings.Fields(names[index])[0]
			found = findableByName(index)
			title = names[index] + " dinner plans"
			if query == ownerFirstName {
				exercised["a first name the owner shares"]++
			}
			// The query finds the person's address on their card or legacy
			// thread, whatever v2 holds.
			if people[index].Kind != "" && (people[index].InAddressBook || legacyNameSurvives(index)) {
				exercised["an address that a name finds"]++
				if trapped[index] {
					exercised["a stranger whose address holds one that a name finds"]++
				}
				if trapTitles[index] != "" {
					exercised["a title that holds an address that a name finds"]++
				}
			}
			switch {
			case !found:
				exercised["unfindable name"]++
			case !people[index].NamelessInV2:
				exercised["name in v2"]++
				if people[index].ParticipantName != "" {
					exercised["a name in v2 that reads hide"]++
				}
			case people[index].InAddressBook:
				exercised["name in the address book"]++
			default:
				exercised["name on a legacy thread"]++
				legacyOnlyName = true
			}
		case 2, 3:
			var phones []int
			for index := range people {
				if people[index].Kind == "" {
					phones = append(phones, index)
				}
			}
			if len(phones) == 0 {
				query = "nobody-" + strconv.Itoa(random.Intn(100))
				exercised["unknown text"]++
				break
			}
			index := phones[random.Intn(len(phones))]
			forms := routeNumberForms(people[index].Number)
			person, query = names[index], forms[random.Intn(len(forms))]
			number = query
			found = threads[person] != ""
			if query == people[index].Number {
				exercised["E.164 number"]++
			} else {
				exercised["number in another form"]++
			}
		case 4:
			query = pool[random.Intn(len(pool))]
			exercised["thread number"]++
		case 5:
			forms := append([]string{owner.Name, "Owner", firstNonEmpty(owner.ParticipantName, owner.Name)}, routeNumberForms(owner.Number)...)
			query = forms[random.Intn(len(forms))]
			exercised["the owner's name or number"]++
		default:
			query = "nobody-" + strconv.Itoa(random.Intn(100))
			exercised["unknown text"]++
		}

		options := Options{Reads: v2read.New(store), V2Primary: true, Daemon: daemon}
		// check runs the query and tests the invariants. group is the ID of a
		// group whose title holds the query, when the store holds one.
		check := func(group string) bool {
			matches := routeMatches(t, callRouteTool(t, resolveContactRoutesHandler(a, options), map[string]any{"query": query}))
			for _, match := range matches {
				ids := []string{match.PreferredReplyConversationID}
				for _, route := range match.Routes {
					ids = append(ids, route.Conversation.ConversationID)
				}
				for _, id := range ids {
					if id == "" {
						continue
					}
					if conversation, err := store.GetConversation(id); err != nil || conversation.ConversationID != id {
						t.Logf("seed %d, query %q: route ID %q is not a serving-store conversation ID (%v); people=%+v", seed, query, id, err, people)
						return false
					}
					if person != "" && id != threads[person] && id != group {
						t.Logf("seed %d, query %q: route %q is neither %s's thread %q nor a group whose title holds the name; people=%+v", seed, query, id, person, threads[person], people)
						return false
					}
				}
			}
			// The group is newer than every fixture thread, so its match comes
			// first.
			var want []string
			if group != "" {
				want = append(want, group)
			}
			if found {
				want = append(want, threads[person])
			}
			preferred := make([]string, 0, len(matches))
			for _, match := range matches {
				preferred = append(preferred, match.PreferredReplyConversationID)
			}
			if !slices.Equal(preferred, want) {
				t.Logf("seed %d, query %q, group %q: preferred routes %v, want %v; matches=%+v; people=%+v", seed, query, group, preferred, want, matches, people)
				return false
			}
			return true
		}
		if !check("") {
			return false
		}
		// Every name query runs again beside a newer group whose title holds
		// the name. That title says nothing about where the person's own
		// thread is.
		if title != "" {
			seedTitledRouteGroup(t, a.Store, store, owner, "dinner", title)
			exercised["a title that holds the name"]++
			if legacyOnlyName {
				exercised["a name on a legacy thread, beside a title that holds it"]++
			}
			if !check("dinner") {
				return false
			}
		}
		if number != "" {
			direct, err := findDirectSMSConversation(options.Reads, number)
			if err != nil {
				t.Fatalf("findDirectSMSConversation(%q): %v", number, err)
			}
			viaSendMessage := ""
			if direct != nil {
				viaSendMessage = direct.ConversationID
			}
			if viaSendMessage != threads[person] {
				t.Logf("seed %d, number %q: send_message picks %q, resolve_contact_routes picks %q; people=%+v", seed, number, viaSendMessage, threads[person], people)
				return false
			}
		}
		return true
	}
	cases := 160
	if raceDetectorEnabled {
		cases = 80
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"name in v2", "name in the address book", "name on a legacy thread", "unfindable name",
		"E.164 number", "number in another form", "thread number", "unknown text",
		"a first name the owner shares", "the owner's name or number", "a name in v2 that reads hide",
		"a title that holds the name", "a name on a legacy thread, beside a title that holds it",
		"an address that a name finds", "a stranger whose address holds one that a name finds",
		"a title that holds an address that a name finds",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew a query by %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
