package tools

import (
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// sendRecorder is a running app that takes every send. It records each
// request it gets, status checks included, and the conversation ID each send
// names. v2Send picks the v2 outbox over the legacy /api/send.
type sendRecorder struct {
	mu       sync.Mutex
	requests []string
	sentTo   []string
}

func newSendRecorder(t *testing.T, v2Send bool) (*sendRecorder, *localapi.Client) {
	t.Helper()
	recorder := &sendRecorder{}
	recordSend := func(r *http.Request) {
		var request struct {
			ConversationID string `json:"conversation_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode the send: %v", err)
		}
		recorder.mu.Lock()
		recorder.sentTo = append(recorder.sentTo, request.ConversationID)
		recorder.mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		recorder.mu.Lock()
		recorder.requests = append(recorder.requests, r.Method+" "+r.URL.Path)
		recorder.mu.Unlock()
		switch {
		case r.URL.Path == "/api/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"connected": true, "v2_primary": v2Send, "v2_send": v2Send})
		case v2Send && r.Method == http.MethodPost && r.URL.Path == "/api/v1/outbox/messages":
			recordSend(r)
			_ = json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out-1", "state": "queued"})
		case v2Send && r.URL.Path == "/api/v1/outbox/out-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"outbox_id": "out-1", "state": "confirmed", "remote_message_id": "remote-1"})
		case !v2Send && r.Method == http.MethodPost && r.URL.Path == "/api/send":
			recordSend(r)
			_ = json.NewEncoder(w).Encode(map[string]any{"message_id": "msg-1", "status": "SUCCESS", "success": true})
		default:
			t.Errorf("the app got an unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	return recorder, daemonClientFor(t, mux)
}

func (r *sendRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests, r.sentTo = nil, nil
}

// snapshot returns every request the app got since the last reset, and the
// conversation ID each send named.
func (r *sendRecorder) snapshot() (requests, sentTo []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...), append([]string(nil), r.sentTo...)
}

// sendOwnerRefusal reports whether result is send_message's refusal of the
// account owner's own number.
func sendOwnerRefusal(t *testing.T, result *mcp.CallToolResult) bool {
	t.Helper()
	text := resultText(t, result)
	return result.IsError && strings.Contains(text, "own number") && strings.Contains(text, "sent nothing")
}

// The account's owner appears in most threads: v2 lists the owner's self
// identity, and the legacy store lists an is_me entry and, in many Google
// threads, an unflagged copy of it. send_message matched any participant, so
// a send to the owner's own number went to the most recent one-to-one SMS
// thread, which belongs to someone else. It must find no thread, say why,
// and ask the app nothing, whichever store serves reads. Another person's
// number, in any written form, still reaches that person's thread.
func TestClientModeSendMessageRefusesTheOwnersOwnNumber(t *testing.T) {
	owner := routeFixtureOwner{Name: "Sam Owner", Number: "+15550009999", LegacyUnflaggedCopy: true}
	people := []routeFixturePerson{
		{Name: "Alice Example", Number: "+15555550123", LegacyNumber: "+1 (555) 555-0123", LegacyThreadID: "3100", RemoteThreadID: "3100"},
		{Name: "Bob Example", Number: "+15555550199", LegacyThreadID: "3101", RemoteThreadID: "3101"},
	}
	for _, mode := range []string{"v2-primary", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
			if err != nil {
				t.Fatalf("sqlite.Open(): %v", err)
			}
			defer store.Close()
			threads := seedRouteFixtureWithOwner(t, a.Store, store, owner, people)
			recorder, daemon := newSendRecorder(t, mode == "v2-primary")
			options := Options{Reads: v2read.New(store), V2Primary: true, Daemon: daemon}
			if mode == "legacy" {
				// A legacy install sends to the legacy store's thread IDs.
				options = Options{Reads: a.Store, Daemon: daemon}
				threads = map[string]string{"Alice Example": "3100", "Bob Example": "3101"}
			}
			send := daemonSendMessageHandler(options)

			for _, form := range routeNumberForms(owner.Number) {
				conversation, err := findDirectSMSConversation(options.Reads, form)
				if conversation != nil || !errors.Is(err, errOwnNumber) {
					t.Errorf("findDirectSMSConversation(the owner's %q) = %+v, %v; want no thread and errOwnNumber", form, conversation, err)
				}
				result := callRouteTool(t, send, map[string]any{"recipient": form, "message": "note to self"})
				if !sendOwnerRefusal(t, result) {
					t.Errorf("send_message(the owner's %q) = %q; want a refusal that names the owner's own number", form, resultText(t, result))
				}
			}
			if requests, _ := recorder.snapshot(); len(requests) != 0 {
				t.Fatalf("sends to the owner's own number asked the app %v; want nothing", requests)
			}

			for _, person := range people {
				for _, form := range routeNumberForms(person.Number) {
					recorder.reset()
					conversation, err := findDirectSMSConversation(options.Reads, form)
					if err != nil || conversation == nil || conversation.ConversationID != threads[person.Name] {
						t.Errorf("findDirectSMSConversation(%s's %q) = %+v, %v; want %s's thread %q", person.Name, form, conversation, err, person.Name, threads[person.Name])
					}
					result := callRouteTool(t, send, map[string]any{"recipient": form, "message": "hi"})
					if result.IsError {
						t.Errorf("send_message(%s's %q) failed: %s", person.Name, form, resultText(t, result))
						continue
					}
					if _, sentTo := recorder.snapshot(); len(sentTo) != 1 || sentTo[0] != threads[person.Name] {
						t.Errorf("send_message(%s's %q) sent to %v; want only %s's thread %q", person.Name, form, sentTo, person.Name, threads[person.Name])
					}
				}
			}
		})
	}
}

// ownerPresence is how a legacy thread lists the account's owner.
type ownerPresence int

const (
	ownerFlagged        ownerPresence = iota // one is_me entry
	ownerFlaggedAndCopy                      // an is_me entry and an unflagged copy
	ownerUnflaggedCopy                       // only the unflagged copy
	ownerPresenceCount
)

// sendFixtureThread is one thread of a send_message property case. Members
// index the case's people; every thread lists the owner.
type sendFixtureThread struct {
	Key      string
	Platform string // "sms" or "whatsapp"
	Group    bool
	Members  []int
	Owner    ownerPresence
	LastMS   int64
	Odd      string // raw JSON of an element the legacy list starts with, or ""
	// CopyNumber is the number the unflagged copy of the owner holds, or ""
	// for one that ownerForm writes.
	CopyNumber string
}

// oddParticipantElements are list elements that aren't a person: an element
// that isn't an object, or one with a field of another JSON type than the
// owner rule reads. None names anyone in a send property case.
var oddParticipantElements = []string{
	`{"name":"Odd Example","number":"+15555550188","is_me":"false"}`,
	`{"name":"Odd Example","number":"+15555550188","isMe":1}`,
	`{"name":"Odd Example","number":15555550188}`,
	`{"name":["Odd Example"],"phone":["+15555550188"],"is_me":null}`,
	`"Odd Example"`,
	`["Odd Example"]`,
	`17`,
	`null`,
}

// seedSendFixture writes threads into the legacy store and the v2 store,
// and returns each thread's conversation ID in each store by Key. personForm
// and ownerForm give the written form of each legacy entry's number; v2
// stores E.164.
func seedSendFixture(
	t *testing.T,
	legacy *db.Store,
	store *sqlite.Store,
	owner routeFixtureOwner,
	names, numbers []string,
	threads []sendFixtureThread,
	personForm func(member int) string,
	ownerForm func() string,
) (legacyIDs, v2IDs map[string]string) {
	t.Helper()
	now := time.Now().UnixMilli()
	accounts := map[string]string{"sms": "google-primary", "whatsapp": "whatsapp-primary"}
	bridges := map[string]string{"sms": "google_messages", "whatsapp": "whatsmeow"}
	for platform, account := range accounts {
		if err := store.UpsertAccount(sqlite.Account{
			AccountID: account, BridgeKey: bridges[platform], DisplayName: platform,
			Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
			CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("UpsertAccount(%s): %v", account, err)
		}
		identities := []sqlite.Identity{{
			IdentityID: account + "-self", AccountID: account, Kind: sqlite.IdentityKind("e164"),
			CanonicalValue: owner.Number, RawValue: owner.Number, DisplayName: owner.Name, IsSelf: true,
			MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
		}}
		for index, number := range numbers {
			identities = append(identities, sqlite.Identity{
				IdentityID: account + "-person-" + strconv.Itoa(index), AccountID: account, Kind: sqlite.IdentityKind("e164"),
				CanonicalValue: number, RawValue: number, DisplayName: names[index],
				MetadataJSON: "{}", CreatedAtMS: now, UpdatedAtMS: now,
			})
		}
		for _, identity := range identities {
			if err := store.UpsertIdentity(identity); err != nil {
				t.Fatalf("UpsertIdentity(%s): %v", identity.IdentityID, err)
			}
		}
	}

	legacyIDs, v2IDs = map[string]string{}, map[string]string{}
	for _, thread := range threads {
		entries := make([]map[string]any, 0, len(thread.Members)+2)
		for _, member := range thread.Members {
			entries = append(entries, map[string]any{"name": names[member], "number": personForm(member)})
		}
		if thread.Owner != ownerUnflaggedCopy {
			entries = append(entries, map[string]any{"name": owner.Name, "number": ownerForm(), "is_me": true})
		}
		if thread.Owner != ownerFlagged {
			copyNumber := thread.CopyNumber
			if copyNumber == "" {
				copyNumber = ownerForm()
			}
			entries = append(entries, map[string]any{
				"name": owner.Name, "number": copyNumber,
				"id": "owner-participant-" + thread.Key, "contact_id": "owner-contact",
			})
		}
		participants, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		if thread.Odd != "" {
			participants = append([]byte("["+thread.Odd+","), participants[1:]...)
		}
		legacyID := "legacy-" + thread.Key
		title := names[thread.Members[0]]
		if thread.Group {
			title = "Group " + thread.Key
		}
		if err := legacy.UpsertConversation(&db.Conversation{
			ConversationID: legacyID, Name: title, IsGroup: thread.Group,
			Participants: string(participants), LastMessageTS: thread.LastMS, SourcePlatform: thread.Platform,
		}); err != nil {
			t.Fatalf("seed legacy thread %s: %v", thread.Key, err)
		}
		legacyIDs[thread.Key] = legacyID

		account := accounts[thread.Platform]
		kind := sqlite.ConversationKindDirect
		v2Title := ""
		if thread.Group {
			kind, v2Title = sqlite.ConversationKindGroup, title
		}
		conversationID := v2keys.DeriveID("conversation", account, "fixture:"+thread.Key)
		if err := store.UpsertConversation(sqlite.Conversation{
			ConversationID: conversationID, AccountID: account, RemoteConversationID: "remote-" + thread.Key,
			Kind: kind, Title: v2Title, NotificationMode: sqlite.NotificationModeAll, MetadataJSON: "{}",
			LastMessageAtMS: thread.LastMS, CreatedAtMS: now, UpdatedAtMS: now,
		}); err != nil {
			t.Fatalf("seed v2 thread %s: %v", thread.Key, err)
		}
		members := []sqlite.ConversationParticipant{{
			AccountID: account, ConversationID: conversationID, IdentityID: account + "-self",
			Role: sqlite.ParticipantRoleMember, IsActive: true,
		}}
		for _, member := range thread.Members {
			members = append(members, sqlite.ConversationParticipant{
				AccountID: account, ConversationID: conversationID, IdentityID: account + "-person-" + strconv.Itoa(member),
				Role: sqlite.ParticipantRoleMember, IsActive: true,
			})
		}
		if err := store.ReplaceConversationParticipants(conversationID, members); err != nil {
			t.Fatalf("seed v2 participants of %s: %v", thread.Key, err)
		}
		v2IDs[thread.Key] = conversationID
	}
	return legacyIDs, v2IDs
}

// Invariants of client-mode send_message for SMS, over random stores in which
// every thread lists the owner: as a self identity in v2, and in legacy
// threads as an is_me entry, an unflagged copy of it, or both, with an is_me
// entry in at least one thread. People have one-to-one SMS threads, SMS
// groups and WhatsApp threads. The legacy store writes people's numbers as
// Google does, in E.164 form or as bare digits, and each of the owner's
// entries in any written form. An unflagged copy sometimes holds only
// 000-9999, the seven digits of the owner's local number, which also end
// Erin's number. The owner rule counts that copy as the owner's, so Erin's
// messages must never reach a thread through it. Some legacy lists start
// with an odd element (oddParticipantElements), which must hide no one
// beside it.
//
//	I1. The owner's number, in any written form, finds no thread, and
//	    send_message refuses it before it asks the app anything.
//	I2. A person's number, in any written form, finds that person's
//	    one-to-one SMS thread, and send_message sends there and nowhere else.
//	    A person without one gets no thread and no send, even when an SMS
//	    group or a WhatsApp thread lists them.
//	I3. A number no one in the store has finds no thread and sends nothing.
//
// Where a person has one thread, a one-to-one SMS thread,
// resolve_contact_routes prefers the thread send_message sends to, as #267
// tests for its fixtures.
func TestClientModeSendMessageSendsOnlyToThePersonsOwnThread(t *testing.T) {
	names := []string{"Alice Example", "Bob Example", "Carol Example", "Dave Example", "Erin Example"}
	numbers := []string{"+15555550100", "+15555550101", "+15555550102", "+15555550103", "+14440009999"}
	erin := len(names) - 1
	owner := routeFixtureOwner{Name: "Sam Owner", Number: "+15550009999"}
	v2Recorder, v2Daemon := newSendRecorder(t, true)
	legacyRecorder, legacyDaemon := newSendRecorder(t, false)
	exercised := map[string]int{}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		a := testApp(t)
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
		if err != nil {
			t.Fatalf("sqlite.Open(): %v", err)
		}
		defer store.Close()
		mode := "v2-primary"
		if random.Intn(2) == 0 {
			mode = "legacy"
		}

		var threads []sendFixtureThread
		threadsOf := make([][]string, len(names))
		addThread := func(thread sendFixtureThread) {
			thread.Owner = ownerPresence(random.Intn(int(ownerPresenceCount)))
			if random.Intn(3) == 0 {
				thread.Odd = oddParticipantElements[random.Intn(len(oddParticipantElements))]
			}
			if random.Intn(3) == 0 {
				thread.CopyNumber = "000-9999"
			}
			threads = append(threads, thread)
			for _, member := range thread.Members {
				threadsOf[member] = append(threadsOf[member], thread.Key)
			}
		}
		direct := map[int]string{}
		for index := range names {
			if random.Intn(4) != 0 {
				key := "sms-" + strconv.Itoa(index)
				addThread(sendFixtureThread{Key: key, Platform: "sms", Members: []int{index}})
				direct[index] = key
			}
			if random.Intn(3) == 0 {
				addThread(sendFixtureThread{Key: "whatsapp-" + strconv.Itoa(index), Platform: "whatsapp", Members: []int{index}})
			}
		}
		groups := random.Intn(3)
		for group := 0; group < groups; group++ {
			members := random.Perm(len(names))[:2]
			addThread(sendFixtureThread{Key: "group-" + strconv.Itoa(group), Platform: "sms", Group: true, Members: members})
		}
		if len(threads) == 0 {
			addThread(sendFixtureThread{Key: "sms-0", Platform: "sms", Members: []int{0}})
			direct[0] = "sms-0"
		}
		flagged := false
		for index, order := range random.Perm(len(threads)) {
			threads[index].LastMS = int64(1_000_000 + order)
			if threads[index].Owner != ownerUnflaggedCopy {
				flagged = true
			}
		}
		if !flagged {
			// The rule learns the owner's number only from a flagged entry.
			threads[random.Intn(len(threads))].Owner = ownerFlaggedAndCopy
		}
		erinsLastMS := int64(-1)
		for _, thread := range threads {
			if thread.Key == direct[erin] {
				erinsLastMS = thread.LastMS
			}
		}
		for _, thread := range threads {
			if mode == "legacy" && thread.Owner == ownerUnflaggedCopy && thread.Platform == "sms" && !thread.Group {
				exercised["a legacy one-to-one SMS thread that lists the owner only without is_me"]++
			}
			if mode == "legacy" && thread.Odd != "" && thread.Odd != "null" && thread.Owner != ownerUnflaggedCopy {
				exercised["a legacy owner flag beside an odd element"]++
			}
			if mode == "legacy" && thread.CopyNumber != "" && thread.Owner != ownerFlagged && thread.Platform == "sms" && !thread.Group &&
				thread.Key != direct[erin] && thread.LastMS > erinsLastMS {
				exercised["a seven-digit copy of the owner in a one-to-one SMS thread newer than Erin's own"]++
			}
		}
		// Google writes the legacy store's SMS numbers in E.164 form or as
		// bare digits; each of the owner's entries takes any written form.
		personForms := map[int]string{}
		personForm := func(member int) string {
			if form, ok := personForms[member]; ok {
				return form
			}
			digits := digitsOnly(numbers[member])
			forms := []string{numbers[member], digits, digits[1:]}
			personForms[member] = forms[random.Intn(len(forms))]
			return personForms[member]
		}
		ownerForm := func() string {
			forms := routeNumberForms(owner.Number)
			return forms[random.Intn(len(forms))]
		}
		legacyIDs, v2IDs := seedSendFixture(t, a.Store, store, owner, names, numbers, threads, personForm, ownerForm)

		recorder, options, ids := v2Recorder, Options{Reads: v2read.New(store), V2Primary: true, Daemon: v2Daemon}, v2IDs
		if mode == "legacy" {
			recorder, options, ids = legacyRecorder, Options{Reads: a.Store, Daemon: legacyDaemon}, legacyIDs
		}
		send := daemonSendMessageHandler(options)
		failed := func(format string, args ...any) bool {
			t.Logf("seed %d (%s): "+format+"; threads=%+v", append(append([]any{seed, mode}, args...), threads)...)
			return false
		}

		// I1.
		for _, form := range routeNumberForms(owner.Number) {
			recorder.reset()
			conversation, err := findDirectSMSConversation(options.Reads, form)
			if conversation != nil || !errors.Is(err, errOwnNumber) {
				return failed("findDirectSMSConversation(the owner's %q) = %+v, %v; want no thread and errOwnNumber", form, conversation, err)
			}
			result := callRouteTool(t, send, map[string]any{"recipient": form, "message": "note to self"})
			if !sendOwnerRefusal(t, result) {
				return failed("send_message(the owner's %q) = %q; want the owner refusal", form, resultText(t, result))
			}
			if requests, _ := recorder.snapshot(); len(requests) != 0 {
				return failed("send_message(the owner's %q) asked the app %v; want nothing", form, requests)
			}
		}
		exercised["the owner's number, "+mode]++

		// I2 and the differential.
		for index, name := range names {
			personForms := routeNumberForms(numbers[index])
			form := personForms[random.Intn(len(personForms))]
			want := ""
			if key, ok := direct[index]; ok {
				want = ids[key]
			}
			recorder.reset()
			conversation, err := findDirectSMSConversation(options.Reads, form)
			got := ""
			if conversation != nil {
				got = conversation.ConversationID
			}
			if err != nil || got != want {
				return failed("findDirectSMSConversation(%s's %q) = %q, %v; want %q", name, form, got, err, want)
			}
			result := callRouteTool(t, send, map[string]any{"recipient": form, "message": "hi"})
			_, sentTo := recorder.snapshot()
			if want == "" {
				if !result.IsError || len(sentTo) != 0 {
					return failed("send_message(%s's %q) without a one-to-one thread sent to %v (%s); want no send", name, form, sentTo, resultText(t, result))
				}
				if len(threadsOf[index]) > 0 {
					exercised["a person only in a group or on WhatsApp"]++
				}
				continue
			}
			if result.IsError || len(sentTo) != 1 || sentTo[0] != want {
				return failed("send_message(%s's %q) sent to %v (%s); want only %q", name, form, sentTo, resultText(t, result), want)
			}
			exercised["a person's one-to-one thread, "+mode]++
			if len(threadsOf[index]) != 1 {
				continue
			}
			match := routeMatches(t, callRouteTool(t, resolveContactRoutesHandler(a, options), map[string]any{"query": form}))
			if len(match) != 1 || match[0].PreferredReplyConversationID != want {
				return failed("resolve_contact_routes(%s's %q) = %+v; want one match preferring send_message's thread %q", name, form, match, want)
			}
			exercised["the differential, "+mode]++
		}

		// I3.
		unknown := "+1555555019" + strconv.Itoa(random.Intn(10))
		recorder.reset()
		if conversation, err := findDirectSMSConversation(options.Reads, unknown); conversation != nil || err != nil {
			return failed("findDirectSMSConversation(unknown %q) = %+v, %v; want nothing", unknown, conversation, err)
		}
		if result := callRouteTool(t, send, map[string]any{"recipient": unknown, "message": "hi"}); !result.IsError {
			return failed("send_message(unknown %q) succeeded; want no send", unknown)
		}
		if _, sentTo := recorder.snapshot(); len(sentTo) != 0 {
			return failed("send_message(unknown %q) sent to %v; want no send", unknown, sentTo)
		}
		exercised["an unknown number"]++
		return true
	}
	cases := 120
	if raceDetectorEnabled {
		cases = 40
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"the owner's number, v2-primary", "the owner's number, legacy",
		"a person's one-to-one thread, v2-primary", "a person's one-to-one thread, legacy",
		"the differential, v2-primary", "the differential, legacy",
		"a person only in a group or on WhatsApp", "a legacy one-to-one SMS thread that lists the owner only without is_me",
		"a legacy owner flag beside an odd element", "an unknown number",
		"a seven-digit copy of the owner in a one-to-one SMS thread newer than Erin's own",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}

// Only a value that names a phone line, as phoneline reads one, marks a line
// as the owner's: a phone number, or a WhatsApp JID of one. A flagged entry
// can hold something else, such as a WhatsApp LID, whose digits can end like
// another person's number; that person must still get their thread, not a
// refusal.
func TestFindDirectSMSConversationTakesTheOwnersLinesOnlyFromPhoneNumbers(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	for _, conversation := range []*db.Conversation{
		{
			ConversationID: "sms-dana", Name: "Dana Example", SourcePlatform: "sms", LastMessageTS: 2000,
			Participants: `[{"name":"Dana Example","number":"+14891234567"},{"name":"Sam Owner","number":"+15550009999","is_me":true}]`,
		},
		{
			ConversationID: "whatsapp-picnic", Name: "Picnic", IsGroup: true, SourcePlatform: "whatsapp", LastMessageTS: 1000,
			Participants: `[{"name":"Sam Owner","number":"105274891234567@lid","is_me":true},{"name":"Eve Example","number":"+15555550123"}]`,
		},
	} {
		if err := store.UpsertConversation(conversation); err != nil {
			t.Fatalf("seed %s: %v", conversation.ConversationID, err)
		}
	}
	if got, err := findDirectSMSConversation(store, "+1 (489) 123-4567"); err != nil || got == nil || got.ConversationID != "sms-dana" {
		t.Fatalf("findDirectSMSConversation(Dana's number) = %+v, %v; want Dana's thread", got, err)
	}
	if got, err := findDirectSMSConversation(store, "+15550009999"); got != nil || !errors.Is(err, errOwnNumber) {
		t.Fatalf("findDirectSMSConversation(the owner's number) = %+v, %v; want errOwnNumber", got, err)
	}
}

// legacySendFixture is a legacy store with a running app that records sends,
// for findDirectSMSConversation cases that start from raw participants JSON.
func legacySendFixture(t *testing.T, conversations ...*db.Conversation) (*db.Store, *sendRecorder, server.ToolHandlerFunc) {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	for _, conversation := range conversations {
		if err := store.UpsertConversation(conversation); err != nil {
			t.Fatalf("seed %s: %v", conversation.ConversationID, err)
		}
	}
	recorder, daemon := newSendRecorder(t, false)
	return store, recorder, daemonSendMessageHandler(Options{Reads: store, Daemon: daemon})
}

// checkOwnerRefusedAndPersonReached asserts that the owner's number finds no
// thread and sends nothing, and that the person's number reaches the
// person's thread alone.
func checkOwnerRefusedAndPersonReached(t *testing.T, store *db.Store, recorder *sendRecorder, send server.ToolHandlerFunc, ownerNumber, personNumber, personThread string) {
	t.Helper()
	if got, err := findDirectSMSConversation(store, ownerNumber); got != nil || !errors.Is(err, errOwnNumber) {
		t.Errorf("findDirectSMSConversation(the owner's number) = %+v, %v; want no thread and errOwnNumber", got, err)
	}
	recorder.reset()
	if result := callRouteTool(t, send, map[string]any{"recipient": ownerNumber, "message": "note to self"}); !sendOwnerRefusal(t, result) {
		t.Errorf("send_message(the owner's number) = %q; want the owner refusal", resultText(t, result))
	}
	if requests, _ := recorder.snapshot(); len(requests) != 0 {
		t.Errorf("send_message(the owner's number) asked the app %v; want nothing", requests)
	}
	if got, err := findDirectSMSConversation(store, personNumber); err != nil || got == nil || got.ConversationID != personThread {
		t.Errorf("findDirectSMSConversation(%q) = %+v, %v; want thread %q", personNumber, got, err, personThread)
	}
	recorder.reset()
	result := callRouteTool(t, send, map[string]any{"recipient": personNumber, "message": "hi"})
	if _, sentTo := recorder.snapshot(); result.IsError || len(sentTo) != 1 || sentTo[0] != personThread {
		t.Errorf("send_message(%q) sent to %v (%s); want only %q", personNumber, sentTo, resultText(t, result), personThread)
	}
}

// One odd element of a participants list must cost only itself.
// findDirectSMSConversation decoded each list whole into typed fields, so an
// element that isn't an object, or a field of another JSON type, failed the
// decode and dropped the list. The finder then missed the owner's flag beside
// it, answered the owner's number with the newest one-to-one thread that
// lists the owner without the flag, and found no thread for the person in the
// list. Only a JSON true flags the owner, and only a string counts as a
// number, as in the legacy search.
func TestFindDirectSMSConversationReadsEachParticipantOnItsOwn(t *testing.T) {
	for _, odd := range []string{
		`{"name":"Odd Example","number":"+15555550188","is_me":"false"}`,
		`{"name":"Odd Example","number":"+15555550188","isMe":1}`,
		`{"name":"Odd Example","number":15555550188}`,
		`{"name":"Odd Example","phone":["+15555550188"]}`,
		`{"name":["Odd Example"],"number":"+15555550188","is_me":null}`,
		`"Odd Example"`,
		`["Odd Example"]`,
		`17`,
		`null`,
	} {
		t.Run(odd, func(t *testing.T) {
			store, recorder, send := legacySendFixture(t,
				&db.Conversation{
					ConversationID: "sms-alice", Name: "Alice Example", SourcePlatform: "sms", LastMessageTS: 1000,
					Participants: `[{"name":"Alice Example","number":"+15555550123"},` + odd + `,{"name":"Sam Owner","number":"+15550009999","is_me":true}]`,
				},
				&db.Conversation{
					ConversationID: "sms-bob", Name: "Bob Example", SourcePlatform: "sms", LastMessageTS: 2000,
					Participants: `[{"name":"Bob Example","number":"+15555550199"},{"name":"Sam Owner","number":"+15550009999"}]`,
				},
			)
			checkOwnerRefusedAndPersonReached(t, store, recorder, send, "+15550009999", "+15555550123", "sms-alice")
		})
	}
}

// The owner's flag can sit in one old thread alone. findDirectSMSConversation
// read only the 2,000 most recent conversations, so when the newer ones list
// the owner only without the flag, the owner's number went to the newest
// one-to-one thread, and a person whose one-to-one thread is older than those
// 2,000 got none.
func TestFindDirectSMSConversationReadsEveryConversation(t *testing.T) {
	conversations := []*db.Conversation{
		{
			ConversationID: "sms-flag", Name: "Old group", IsGroup: true, SourcePlatform: "sms", LastMessageTS: 1,
			Participants: `[{"name":"Dee Example","number":"+15555550144"},{"name":"Sam Owner","number":"+15550009999","is_me":true}]`,
		},
		{
			ConversationID: "sms-carol", Name: "Carol Example", SourcePlatform: "sms", LastMessageTS: 2,
			Participants: `[{"name":"Carol Example","number":"+15555550177"},{"name":"Sam Owner","number":"+15550009999"}]`,
		},
	}
	for index := 0; index < 2000; index++ {
		conversations = append(conversations, &db.Conversation{
			ConversationID: "whatsapp-group-" + strconv.Itoa(index), Name: "Group " + strconv.Itoa(index), IsGroup: true,
			SourcePlatform: "whatsapp", LastMessageTS: int64(10 + index),
			Participants: `[{"name":"Erin Example","number":"+15555550155"},{"name":"Sam Owner","number":"+15550009999"}]`,
		})
	}
	conversations = append(conversations, &db.Conversation{
		ConversationID: "sms-bob", Name: "Bob Example", SourcePlatform: "sms", LastMessageTS: 5000,
		Participants: `[{"name":"Bob Example","number":"+15555550199"},{"name":"Sam Owner","number":"+15550009999"}]`,
	})
	store, recorder, send := legacySendFixture(t, conversations...)
	checkOwnerRefusedAndPersonReached(t, store, recorder, send, "+1 (555) 000-9999", "+15555550177", "sms-carol")
}

// The owner rule counts an unflagged entry as the owner's when its number
// sits on the owner's line, and a number shorter than a line sits on every
// line it ends: the seven digits 000-9999 end both the owner's 555-000-9999
// and Erin's 444-000-9999. When findDirectSMSConversation picks a thread, it
// must skip such a copy, or Erin's message goes to Bob's newer thread, which
// lists the owner as 000-9999.
func TestFindDirectSMSConversationSkipsTheOwnersEntriesWhenItPicksAThread(t *testing.T) {
	store, recorder, send := legacySendFixture(t,
		&db.Conversation{
			ConversationID: "sms-erin", Name: "Erin Example", SourcePlatform: "sms", LastMessageTS: 1000,
			Participants: `[{"name":"Erin Example","number":"+14440009999"},{"name":"Sam Owner","number":"+15550009999","is_me":true}]`,
		},
		&db.Conversation{
			ConversationID: "sms-bob", Name: "Bob Example", SourcePlatform: "sms", LastMessageTS: 2000,
			Participants: `[{"name":"Bob Example","number":"+15555550199"},{"name":"Sam Owner","number":"+15550009999","is_me":true},{"name":"Sam Owner","number":"000-9999"}]`,
		},
	)
	checkOwnerRefusedAndPersonReached(t, store, recorder, send, "+15550009999", "+1 (444) 000-9999", "sms-erin")
}
