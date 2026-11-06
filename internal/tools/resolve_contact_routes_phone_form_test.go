package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// routeIdentityThread is a v2 direct thread whose one peer has an identity
// of the given kind and canonical value, as a bridge stores it, and no name.
type routeIdentityThread struct {
	ID, Kind, Value string
}

// seedRouteIdentityThreads writes threads newest first.
func seedRouteIdentityThreads(t *testing.T, store *sqlite.Store, threads []routeIdentityThread) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: routeFixtureAccount, BridgeKey: "google_messages", DisplayName: "Google Messages",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	for index, thread := range threads {
		identityID := "identity-" + thread.ID
		if err := store.UpsertIdentity(sqlite.Identity{
			IdentityID: identityID, AccountID: routeFixtureAccount, Kind: sqlite.IdentityKind(thread.Kind),
			CanonicalValue: thread.Value, RawValue: thread.Value, MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("UpsertIdentity(%s): %v", thread.ID, err)
		}
		if err := store.UpsertConversation(sqlite.Conversation{
			ConversationID: thread.ID, AccountID: routeFixtureAccount, RemoteConversationID: "remote-" + thread.ID,
			Kind: sqlite.ConversationKindDirect, NotificationMode: sqlite.NotificationModeAll, MetadataJSON: "{}",
			LastMessageAtMS: now - int64(index), CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("UpsertConversation(%s): %v", thread.ID, err)
		}
		if err := store.ReplaceConversationParticipants(thread.ID, []sqlite.ConversationParticipant{
			{AccountID: routeFixtureAccount, ConversationID: thread.ID, IdentityID: identityID, Role: sqlite.ParticipantRoleMember, IsActive: true},
		}); err != nil {
			t.Fatalf("ReplaceConversationParticipants(%s): %v", thread.ID, err)
		}
	}
}

// A number names a phone line only in a value that reads as a number, or in
// a WhatsApp JID whose user is one. An email address, a Signal UUID, a
// WhatsApp LID or group, a Google Chat user, a legacy placeholder and a
// number too long for E.164 can each hold Alice's ten digits, but none of
// them is Alice's line. So Alice's number in any written form routes to
// Alice's threads only, and send_message picks Alice's SMS thread over a newer
// one with an SMS gateway's email address.
func TestResolveContactRoutesFindsANumberOnlyWhereItReadsAsOne(t *testing.T) {
	t.Run("v2", func(t *testing.T) {
		a := testApp(t)
		store := openRouteV2Store(t)
		seedRouteIdentityThreads(t, store, []routeIdentityThread{
			{"mail", "email", "alice2025550123@example.com"},
			{"signal", "signal_aci", "abcdefab-abcd-abcd-abcd-ab2025550123"},
			{"lid", "jid", "12025550123@lid"},
			{"group", "jid", "120363012025550123@g.us"},
			{"chat", "username", "users/12025550123"},
			{"placeholder", "legacy_participant", "legacy-participant:12025550123"},
			{"long", "e164", "+123456782025550123"},
			{"alice", "e164", "+12025550123"},
			{"alice-whatsapp", "jid", "12025550123@s.whatsapp.net"},
			{"alice-national", "username", "202-555-0123"},
		})
		for _, query := range routeNumberForms("+12025550123") {
			got, err := findRouteConversations(v2read.New(store), a.Store, query, 80)
			if err != nil {
				t.Fatalf("findRouteConversations(%q): %v", query, err)
			}
			if fmt.Sprint(routeIDs(got)) != "[alice alice-whatsapp alice-national]" {
				t.Errorf("routes for %q = %v, want Alice's three threads only", query, routeIDs(got))
			}
		}
	})
	t.Run("legacy", func(t *testing.T) {
		a := testApp(t)
		for index, thread := range []struct{ id, platform, participants string }{
			{"mail", "gchat", `[{"name":"Mail","number":"alice2025550123@example.com"}]`},
			{"gateway", "sms", `[{"name":"Gateway","number":"2025550123@vtext.com"}]`},
			{"signal", "signal", `[{"name":"Signal","number":"abcdefab-abcd-abcd-abcd-ab2025550123"}]`},
			{"lid", "whatsapp", `[{"name":"Lid","number":"12025550123@lid"}]`},
			{"long", "whatsapp", `[{"name":"Long","number":"+123456782025550123"}]`},
			{"chat", "gchat", `["users/12025550123"]`},
			{"senders", "gchat", `[{"name":"Dee Example","number":"dee@example.com"}]`},
			{"alice", "sms", `[{"name":"Alice Example","number":"+12025550123"}]`},
			{"alice-whatsapp", "whatsapp", `[{"name":"Alice Example","number":"12025550123@s.whatsapp.net"}]`},
			{"club", "sms", `[{"name":"Bob Example","number":"+15555550199"}]`},
		} {
			if err := a.Store.UpsertConversation(&db.Conversation{
				ConversationID: thread.id, Name: "Thread " + fmt.Sprint(index), Participants: thread.participants,
				SourcePlatform: thread.platform, LastMessageTS: int64(100 - index),
			}); err != nil {
				t.Fatalf("seed %s: %v", thread.id, err)
			}
		}
		for index, sender := range []struct{ conversation, number string }{
			{"senders", "alice2025550123@example.com"},
			{"senders", "abcdefab-abcd-abcd-abcd-ab2025550123"},
			{"club", "+1 (202) 555-0123"},
		} {
			if err := a.Store.UpsertMessage(&db.Message{
				MessageID: fmt.Sprintf("m%d", index), ConversationID: sender.conversation, SenderNumber: sender.number,
				Body: "hello", TimestampMS: int64(index + 1), SourcePlatform: "sms",
			}); err != nil {
				t.Fatalf("seed message %d: %v", index, err)
			}
		}
		for _, query := range routeNumberForms("+12025550123") {
			got, err := findRouteConversations(a.Store, a.Store, query, 80)
			if err != nil {
				t.Fatalf("findRouteConversations(%q): %v", query, err)
			}
			if fmt.Sprint(routeIDs(got)) != "[alice alice-whatsapp club]" {
				t.Errorf("routes for %q = %v, want Alice's two threads and Club, where they wrote", query, routeIDs(got))
			}
			direct, err := findDirectSMSConversation(a.Store, query)
			if err != nil || direct == nil || direct.ConversationID != "alice" {
				t.Errorf("send_message's thread for %q = %+v (%v), want Alice's", query, direct, err)
			}
		}
	})
}

// A query is a number only when someone wrote it as one: letters in any script
// make it a name. 不存在2025550123 finds no one, though Alice's number ends
// with its digits, because no title, participant or card holds it, and
// send_message refuses it as an SMS recipient. A number with no-break
// spaces or en dashes in it is still a number.
func TestResolveContactRoutesTakesOnlyAWrittenNumberAsANumber(t *testing.T) {
	names := []string{"不存在2025550123", "Мама 2025550123", "Ελένη +1 202 555 0123", "Alice 2025550123", "2025550123 ext 5", "tel:+12025550123", "2025550123@vtext.com"}
	numbers := []string{"+1\u00a0202\u00a0555\u00a00123", "+1 202\u2013555\u20130123", "(202) 555-0123", "+44 20 7946 0958"}
	for _, query := range names {
		if looksLikePhoneNumber(query) {
			t.Errorf("looksLikePhoneNumber(%q) = true, want false", query)
		}
	}
	for _, query := range numbers {
		if !looksLikePhoneNumber(query) {
			t.Errorf("looksLikePhoneNumber(%q) = false, want true", query)
		}
	}
	for _, mode := range routeStoreModes {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store := openRouteV2Store(t)
			threads := seedRouteFixture(t, a.Store, store, []routeFixturePerson{
				{Name: "Alice Example", Number: "+12025550123", LegacyThreadID: "alice", RemoteThreadID: "alice"},
			})
			reads, _ := routeReadsFor(t, a, store, mode)
			alice := threads["Alice Example"]
			if mode == "legacy" {
				alice = "alice"
			}
			for _, query := range names[:3] {
				got, err := findRouteConversations(reads, a.Store, query, 80)
				if err != nil {
					t.Fatalf("findRouteConversations(%q): %v", query, err)
				}
				if len(got) != 0 {
					t.Errorf("routes for %q = %v, want none", query, routeIDs(got))
				}
			}
			for _, query := range numbers[:2] {
				got, err := findRouteConversations(reads, a.Store, query, 80)
				if err != nil {
					t.Fatalf("findRouteConversations(%q): %v", query, err)
				}
				if len(got) != 1 || got[0].ConversationID != alice {
					t.Errorf("routes for %q = %v, want Alice's thread %q only", query, routeIDs(got), alice)
				}
			}
		})
	}

	submitted := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"v2_send": true, "v2_primary": true, "connected": true})
	})
	mux.HandleFunc("/api/v1/outbox/messages", func(w http.ResponseWriter, r *http.Request) {
		submitted++
		http.Error(w, "the test daemon takes no sends", http.StatusTeapot)
	})
	a := testApp(t)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "alice", Name: "Alice Example", SourcePlatform: "sms", LastMessageTS: 1,
		Participants: `[{"name":"Alice Example","number":"+12025550123"}]`,
	}); err != nil {
		t.Fatalf("seed Alice's thread: %v", err)
	}
	handler := daemonSendMessageHandler(Options{Daemon: daemonClientFor(t, mux), Reads: a.Store})
	request := mcp.CallToolRequest{}
	request.Params.Arguments = map[string]any{"recipient": "不存在2025550123", "message": "hi"}
	result, err := handler(context.Background(), request)
	if err != nil {
		t.Fatalf("send_message: %v", err)
	}
	if !result.IsError || !strings.Contains(resultText(t, result), "must be a phone number") || submitted != 0 {
		t.Errorf("send_message to 不存在2025550123 = %s (%d sends), want a refusal and no send", resultText(t, result), submitted)
	}
}

// The legacy store can hold the only name for a person: R&D Desk, <Ops>
// Team and Bob "Builder" have v2 identities with no name, in threads with no
// title. Go writes &, < and > into JSON as \u0026, \u003c and \u003e, and a
// quote as \", so ListContactsFromConversations matches each name by
// what its entry says, not by its JSON text, and the name routes to the v2
// thread on its line.
func TestResolveContactRoutesFindsALegacyNameThatJSONEscapes(t *testing.T) {
	a := testApp(t)
	store := openRouteV2Store(t)
	people := []struct{ name, number, thread string }{
		{"R&D Desk", "+15555550188", "lab"},
		{"<Ops> Team", "+15555550177", "ops"},
		{`Bob "Builder"`, "+15555550166", "bob"},
	}
	var threads []routeIdentityThread
	for index, person := range people {
		participants, err := json.Marshal([]map[string]any{
			{"name": person.name, "number": person.number},
			{"name": "Me", "number": "+15550009999", "is_me": true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Store.UpsertConversation(&db.Conversation{
			ConversationID: "legacy-" + person.thread, Name: fmt.Sprintf("Thread %d", index), Participants: string(participants),
			SourcePlatform: "sms", LastMessageTS: int64(10 - index),
		}); err != nil {
			t.Fatalf("seed legacy %s: %v", person.thread, err)
		}
		threads = append(threads, routeIdentityThread{person.thread, "e164", person.number})
	}
	seedRouteIdentityThreads(t, store, threads)
	for query, want := range map[string]string{"R&D": "lab", "r&d desk": "lab", "<ops>": "ops", `"Builder"`: "bob"} {
		got, err := findRouteConversations(v2read.New(store), a.Store, query, 80)
		if err != nil {
			t.Fatalf("findRouteConversations(%q): %v", query, err)
		}
		if len(got) != 1 || got[0].ConversationID != want {
			t.Errorf("routes for %q = %v, want the v2 thread %q only", query, routeIDs(got), want)
		}
	}
}
