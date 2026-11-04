package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/phoneline"
	"github.com/maxghenis/openmessage/internal/phoneline/phonelinetest"
)

// ownerNumberForms lists ways people and address books write the owner's
// number, +1 555 000 9999.
var ownerNumberForms = []string{
	"+15550009999", "+1 (555) 000-9999", "(555) 000-9999", "555-000-9999",
	"555.000.9999", "1-555-000-9999", "+1 555 000 9999", "5550009999",
}

// ownerNumberSeparatedForms writes the owner's number with separators beyond
// ASCII spaces and + - ( ) .: no-break and narrow no-break spaces, tabs,
// slashes and en dashes.
var ownerNumberSeparatedForms = []string{
	"+1\u00a0555\u00a0000\u00a09999", "+1\u202f555\u202f000\u202f9999", "+1\t555\t000\t9999",
	"+1/555/000/9999", "+1 555\u2013000\u20139999",
}

// ownerJIDs are the owner's WhatsApp JIDs on the phone-number server, which
// name the owner's line as the number does.
var ownerJIDs = []string{"15550009999@s.whatsapp.net", "15550009999@C.US"}

// ownerDigitHolders hold the owner's digits but name no phone line, as
// phoneline reads a number: directional marks around a number, a tel: URI,
// a WhatsApp LID, a user path, an email address, and a name.
var ownerDigitHolders = []string{
	"\u202a+1 (555) 000-9999\u202c", "tel:+15550009999", "15550009999@lid",
	"users/15550009999", "sam.15550009999@example.com", "Sam 15550009999",
}

// ownerNULForms hold the owner's number and a NUL, so they name no line, as
// phoneline reads a number. SQLite cuts a TEXT value at its first NUL when it
// hands the value to Go, and the first two, cut there, read as the owner's
// number.
var ownerNULForms = []string{"15550009999\x00", "+1 555 000 9999\x00x", "\x00+15550009999"}

func seedConversations(t *testing.T, store *Store, conversations ...*Conversation) {
	t.Helper()
	for _, conversation := range conversations {
		if err := store.UpsertConversation(conversation); err != nil {
			t.Fatalf("seed conversation %s: %v", conversation.ConversationID, err)
		}
	}
}

func searchIDs(t *testing.T, store *Store, query string, limit int) []string {
	t.Helper()
	got, err := store.SearchConversationsByMetadata(query, limit)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	ids := make([]string, 0, len(got))
	for _, conversation := range got {
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}

func lineSearchIDs(t *testing.T, store *Store, number string, limit int) []string {
	t.Helper()
	got, err := store.SearchConversationsByPhoneLine(phoneline.Line(number), limit)
	if err != nil {
		t.Fatalf("search the line of %q: %v", number, err)
	}
	ids := []string{}
	for _, conversation := range got {
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}

// Most threads list the account's owner: an is_me entry and, in many of
// Google's threads, a second entry with the owner's name and number but no
// is_me. Signal and WhatsApp threads can list the owner only that way. A
// search must find no thread through the owner's entries, flagged or not,
// while titles and every other participant still match, a Sam who isn't the
// owner included.
func TestSearchConversationsByMetadataSkipsTheOwnersParticipantEntries(t *testing.T) {
	store := newTestStore(t)
	seedConversations(t, store,
		&Conversation{
			ConversationID: "sms-nathan", Name: "Nathan Example", SourcePlatform: "sms", LastMessageTS: 4000,
			Participants: `[{"name":"Nathan Example","number":"+12675550100","id":"7","contact_id":"21"},` +
				`{"name":"Sam Owner","number":"+15550009999","is_me":true},` +
				`{"name":"Sam Owner","number":"+15550009999","id":"1","contact_id":"9"}]`,
		},
		&Conversation{
			ConversationID: "whatsapp-picnic", Name: "Picnic", IsGroup: true, SourcePlatform: "whatsapp", LastMessageTS: 3000,
			Participants: `[{"name":"Sam Example","number":"+15555550123"},{"name":"Sammy","number":"+15550009999"}]`,
		},
		&Conversation{
			ConversationID: "sms-titled", Name: "Sam Owner, Nathan", IsGroup: true, SourcePlatform: "sms", LastMessageTS: 2000,
			Participants: `[{"name":"Nathan Example","number":"+12675550100"},{"name":"Sam Owner","number":"+15550009999","is_me":true}]`,
		},
		&Conversation{
			ConversationID: "signal-bob", Name: "Bob Example", SourcePlatform: "signal", LastMessageTS: 1000,
			Participants: `[{"name":"Bob Example","number":"+15555550199"},{"name":"Sam O.","number":"(555) 000-9999"}]`,
		},
	)

	for query, want := range map[string][]string{
		"Sam Owner":  {"sms-titled"},
		"Owner":      {"sms-titled"},
		"Sammy":      {},
		"Sam O.":     {},
		"is_me":      {},
		"Sam":        {"whatsapp-picnic", "sms-titled"},
		"Nathan":     {"sms-nathan", "sms-titled"},
		"Bob":        {"signal-bob"},
		"+1555555":   {"whatsapp-picnic", "signal-bob"},
		"contact_id": {},
	} {
		if got := searchIDs(t, store, query, 10); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("search %q = %v, want %v", query, got, want)
		}
	}
	for _, form := range append(ownerNumberForms, "0009999", "000-9999") {
		if got := searchIDs(t, store, form, 10); len(got) != 0 {
			t.Errorf("search for the owner's %q = %v, want none", form, got)
		}
	}
}

// The line search skips the owner's entries as the metadata search does:
// flagged ones, and unflagged copies on a flagged entry's line, objects, IDs
// and bare strings alike, in any thread. Google's threads often list the
// owner a second time without the flag, so a search for the owner's own
// number found those threads, each someone else's. Every other participant's
// number still finds their thread.
func TestSearchConversationsByPhoneLineSkipsTheOwnersParticipantEntries(t *testing.T) {
	store := newTestStore(t)
	seedConversations(t, store,
		&Conversation{
			ConversationID: "sms-nathan", Name: "Nathan Example", SourcePlatform: "sms", LastMessageTS: 4000,
			Participants: `[{"name":"Nathan Example","number":"+12675550100"},{"name":"Sam Owner","number":"+1 (555) 000-9999","is_me":true}]`,
		},
		&Conversation{
			ConversationID: "sms-alice", Name: "Alice Example", SourcePlatform: "sms", LastMessageTS: 3000,
			Participants: `[{"name":"Alice Example","number":"+15555550123"},{"name":"Sam Owner","number":"15550009999"}]`,
		},
		&Conversation{
			ConversationID: "whatsapp-picnic", Name: "Picnic", IsGroup: true, SourcePlatform: "whatsapp", LastMessageTS: 2000,
			Participants: `[{"name":"Eve Example","number":"+15555550188"},{"name":"Sam","phone":"15550009999@s.whatsapp.net"}]`,
		},
		&Conversation{
			ConversationID: "sms-bob", Name: "Bob Example", SourcePlatform: "sms", LastMessageTS: 1000,
			Participants: `["+1 555 000 9999",{"name":"Bob Example","number":"+15555550199"}]`,
		},
	)
	lineIDs := func(number string) []string {
		t.Helper()
		got, err := store.SearchConversationsByPhoneLine(phoneline.Line(number), 10)
		if err != nil {
			t.Fatalf("search the line of %q: %v", number, err)
		}
		ids := []string{}
		for _, conversation := range got {
			ids = append(ids, conversation.ConversationID)
		}
		return ids
	}
	for _, form := range append(append([]string{"0009999"}, ownerNumberForms...), ownerNumberSeparatedForms...) {
		if got := lineIDs(form); len(got) != 0 {
			t.Errorf("the line of the owner's %q finds %v, want none", form, got)
		}
	}
	for number, want := range map[string]string{
		"+12675550100": "[sms-nathan]", "+15555550123": "[sms-alice]",
		"+15555550188": "[whatsapp-picnic]", "+15555550199": "[sms-bob]",
	} {
		if got := lineIDs(number); fmt.Sprint(got) != want {
			t.Errorf("the line of %q finds %v, want %v", number, got, want)
		}
	}
}

// The owner rule reads a number as phoneline reads one, as send_message
// reads a recipient, whatever separators it uses. The search stripped only
// + - ( ) . and spaces before it looked for digits. When the owner's flagged
// number used no-break spaces, tabs or slashes, it marked no phone line, and
// the owner's unflagged copies matched the owner's name again; a copy that
// used them escaped the line too. The owner's WhatsApp JID names the owner's
// line, flagged or not. A value that names no line, such as a WhatsApp LID,
// marks none even in a flagged entry.
func TestSearchConversationsByMetadataReadsTheOwnersNumberWithAnySeparators(t *testing.T) {
	for _, form := range append(append([]string{}, ownerNumberSeparatedForms...), ownerJIDs...) {
		for _, pair := range [][2]string{{form, "+15550009999"}, {"+15550009999", form}, {form, "15550009999@s.whatsapp.net"}} {
			flagged, copied := pair[0], pair[1]
			t.Run(fmt.Sprintf("%q flagged, %q copied", flagged, copied), func(t *testing.T) {
				store := newTestStore(t)
				seedConversations(t, store,
					&Conversation{
						ConversationID: "flag", Name: "Alice Example", SourcePlatform: "sms", LastMessageTS: 2000,
						Participants: fmt.Sprintf(`[{"name":"Alice Example","number":"+15555550123"},{"name":"Myself","number":%q,"is_me":true}]`, flagged),
					},
					&Conversation{
						ConversationID: "copy", Name: "Bob Example", SourcePlatform: "sms", LastMessageTS: 1000,
						Participants: fmt.Sprintf(`[{"name":"Bob Example","number":"+15555550199"},{"name":"Sam Owner","number":%q}]`, copied),
					},
				)
				for query, want := range map[string]string{"Sam Owner": "[]", "Owner": "[]", "Myself": "[]", "Bob": "[copy]", "Alice": "[flag]"} {
					if got := searchIDs(t, store, query, 10); fmt.Sprint(got) != want {
						t.Errorf("search %q = %v, want %v", query, got, want)
					}
				}
			})
		}
	}
	store := newTestStore(t)
	seedConversations(t, store,
		&Conversation{
			ConversationID: "lid", Name: "Picnic", IsGroup: true, SourcePlatform: "whatsapp", LastMessageTS: 2000,
			Participants: `[{"name":"Me","number":"105274891234567@lid","is_me":true},{"name":"Eve Example","number":"+15555550123"}]`,
		},
		&Conversation{
			ConversationID: "dana", Name: "Dana Example", SourcePlatform: "sms", LastMessageTS: 1000,
			Participants: `[{"name":"Dana Example","number":"+14891234567"}]`,
		},
	)
	if got := searchIDs(t, store, "Dana", 10); fmt.Sprint(got) != "[dana]" {
		t.Errorf("search %q = %v, want [dana]: a flagged LID marks no phone line", "Dana", got)
	}
}

// A value with a NUL in it names no phone line, as send_message reads it, so
// the owner rule doesn't count "15550009999\u0000" as the owner's copy, and
// the line search finds no one through "+15555550199\u0000x". SQLite cut each
// value at its NUL before Go read it, so the metadata search dropped the
// thread that lists the first, and the line search put Erin's thread on the
// line of what came before the NUL.
func TestLegacySearchesReadAValueWithANULAsSendMessageDoes(t *testing.T) {
	store := newTestStore(t)
	seedConversations(t, store,
		&Conversation{ConversationID: "owner", Name: "Owner thread", SourcePlatform: "sms", LastMessageTS: 1000,
			Participants: `[{"name":"Sam Owner","number":"15550009999","is_me":true}]`},
		&Conversation{ConversationID: "nul", Name: "Generic", SourcePlatform: "sms", LastMessageTS: 2000,
			Participants: `["15550009999\u0000"]`},
		&Conversation{ConversationID: "erin", Name: "Erin Example", SourcePlatform: "sms", LastMessageTS: 3000,
			Participants: `[{"name":"Erin Example","number":"+15555550199\u0000x"},"+15555550188\u0000"]`},
	)
	if got := searchIDs(t, store, "15550009999", 10); fmt.Sprint(got) != "[nul]" {
		t.Errorf("search 15550009999 = %v, want [nul]: its value names no line, so it isn't the owner's", got)
	}
	for _, number := range []string{"+15555550199", "+15555550188", "+15550009999"} {
		if got := lineSearchIDs(t, store, number, 10); len(got) != 0 {
			t.Errorf("the line of %s finds %v, want none: no value names it", number, got)
		}
	}
}

// SQLite's JSON reader stops at 1,000 levels of nesting, and Go's reads ten
// times as deep. A list that flags the owner beside an element nested 1,001
// deep gave send_message the owner's line and gave the searches none, so
// they found another thread through the owner's unflagged copy, and they
// read no one in that list. Both searches read lists through Go now.
func TestLegacySearchesReadAListNestedDeeperThanSQLiteReads(t *testing.T) {
	store := newTestStore(t)
	seedConversations(t, store,
		&Conversation{ConversationID: "owner", Name: "Owner thread", SourcePlatform: "sms", LastMessageTS: 1000,
			Participants: `[` + ownerRuleNested(1001) + `,{"name":"Sam Owner","number":"15550009999","is_me":true},{"name":"Bob Example","number":"+15555550199"}]`},
		&Conversation{ConversationID: "copy", Name: "Generic", SourcePlatform: "sms", LastMessageTS: 2000,
			Participants: `[{"name":"Owner Copy","number":"15550009999"}]`},
	)
	for _, query := range []string{"15550009999", "Owner Copy"} {
		if got := searchIDs(t, store, query, 10); len(got) != 0 {
			t.Errorf("search %q = %v, want none: the copy is the owner's", query, got)
		}
	}
	if got := lineSearchIDs(t, store, "+15550009999", 10); len(got) != 0 {
		t.Errorf("the owner's line finds %v, want none", got)
	}
	if got := searchIDs(t, store, "Bob", 10); fmt.Sprint(got) != "[owner]" {
		t.Errorf("search Bob = %v, want [owner]", got)
	}
	if got := lineSearchIDs(t, store, "+15555550199", 10); fmt.Sprint(got) != "[owner]" {
		t.Errorf("Bob's line finds %v, want [owner]", got)
	}
}

// SQLite's JSON reader stops at a NUL, and Go's reads the whole text, so a
// list with bytes after a NUL is a JSON array to SQLite and no JSON to Go. Go
// reads no entry from it, so it gives send_message no owner line, and Sam,
// whose number is on that line, is just another person in Bob's thread.
// Through the flag before the NUL, SQLite gave the searches the line, so they
// skipped Sam. Both searches read lists through Go now.
func TestLegacySearchesReadNoEntryFromAListWithBytesAfterANUL(t *testing.T) {
	store := newTestStore(t)
	seedConversations(t, store,
		&Conversation{ConversationID: "owner", Name: "Owner thread", SourcePlatform: "sms", LastMessageTS: 1000,
			Participants: "[{\"name\":\"Sam Owner\",\"number\":\"15550009999\",\"is_me\":true}]\x00garbage"},
		&Conversation{ConversationID: "bob", Name: "Bob Example", SourcePlatform: "sms", LastMessageTS: 2000,
			Participants: `[{"name":"Bob Example","number":"+15555550199"},{"name":"Sam","number":"+15550009999"}]`},
	)
	if got := lineSearchIDs(t, store, "+15550009999", 10); fmt.Sprint(got) != "[bob]" {
		t.Errorf("Sam's line finds %v, want [bob]", got)
	}
	if got := searchIDs(t, store, "Sam", 10); fmt.Sprint(got) != "[bob]" {
		t.Errorf("search Sam = %v, want [bob]", got)
	}
}

// Both legacy searches allocate about as much however many phone lines
// flagged entries give the owner: an entry's owner test looks its number's
// keys up in owner_keys, which SQLite gathers once for each query. They
// passed every line to Go for every entry, so each search's work and
// allocations grew with entries times lines. Allocations can't tell lookups
// from a scan that allocates nothing, so this test bounds only allocations;
// the owner test looks up at most ten keys, in an index that SQLite builds
// once for each query.
func TestLegacySearchesAllocateAboutTheSameForAnyNumberOfOwnerLines(t *testing.T) {
	const threads = 300
	seed := func(distinct bool) *Store {
		store := newTestStore(t)
		for index := 0; index < threads; index++ {
			own := "+15550009999"
			if distinct {
				own = fmt.Sprintf("+1666%07d", index)
			}
			seedConversations(t, store, &Conversation{
				ConversationID: fmt.Sprintf("c%03d", index), Name: "Thread", SourcePlatform: "sms", LastMessageTS: int64(index),
				Participants: fmt.Sprintf(`[{"name":"Me","number":%q,"is_me":true},{"name":"Person %d","number":"+1444%07d"}]`, own, index, index),
			})
		}
		return store
	}
	one, many := seed(false), seed(true)
	allocated := func(search func()) uint64 {
		search()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		search()
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	for _, search := range []struct {
		name string
		run  func(*Store)
	}{
		{"metadata", func(store *Store) { searchIDs(t, store, "Person 7", 10) }},
		{"line", func(store *Store) { lineSearchIDs(t, store, "+14440000007", 10) }},
	} {
		oneBytes := allocated(func() { search.run(one) })
		manyBytes := allocated(func() { search.run(many) })
		if manyBytes > 2*oneBytes {
			t.Errorf("the %s search allocated %d bytes with %d owner lines and %d with one", search.name, manyBytes, threads, oneBytes)
		}
	}
}

// searchModel is a reference for SearchConversationsByMetadata on the
// legacy store: a conversation matches when the query sits in its title; in
// a participant entry that isn't the owner's, which for a string element
// means its text and for an object the text of any "name", "number", "phone"
// or "email" member; or in the name or number of an incoming message's
// sender, or of the contact with that sender's number. A text holds the
// query as SQLite's LIKE finds it: up to the text's first NUL, where LIKE
// stops reading, and with only ASCII letters' case aside. Queries hold no
// NUL. Elements that are neither strings nor objects match nothing, and so
// does a list that isn't JSON as encoding/json reads it, however SQLite's
// reader would take it. It counts as the owner's every object that carries
// is_me or isMe, and every entry on the phone line of a flagged entry whose
// number names one, as phonelinetest restates phoneline's rule. Threads that
// share a time come in conversation ID order. It states the rule with
// regular expressions and a token decode, apart from the helpers that
// implement it.
type searchModel struct {
	conversations []*Conversation
	messages      []*Message
	contacts      []*Contact
}

func modelLastTen(digits string) string {
	if len(digits) > 10 {
		return digits[len(digits)-10:]
	}
	return digits
}

// modelEntry is one string or object element of a participants list.
type modelEntry struct {
	// text is a string element's text.
	text     string
	isString bool
	// object is an object element as a map, the last of a repeated key
	// winning, and members lists every member in order.
	object  map[string]any
	members []modelMember
}

type modelMember struct {
	key   string
	value any
}

// modelDecode decodes data, which must hold one JSON value and nothing but
// whitespace after it, as json.Unmarshal does, except that numbers decode
// into json.Number: a number too large for a float64 fails json.Unmarshal
// into an interface, and costs a list nothing where owner.go reads it.
func modelDecode(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("bytes after the JSON value: %v", err)
	}
	return nil
}

// modelEntries reads a participants list's string and object elements;
// anything else, a list that isn't a JSON array included, holds none.
func modelEntries(participants string) []modelEntry {
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(participants), &raw); err != nil {
		return nil
	}
	entries := make([]modelEntry, 0, len(raw))
	for _, element := range raw {
		switch bytes.TrimSpace(element)[0] {
		case '"':
			var text string
			if json.Unmarshal(element, &text) == nil {
				entries = append(entries, modelEntry{text: text, isString: true})
			}
		case '{':
			var object map[string]any
			if modelDecode(element, &object) == nil {
				entries = append(entries, modelEntry{object: object, members: modelMembers(element)})
			}
		}
	}
	return entries
}

// modelMembers lists an object's members in order, a repeated key each
// time it appears, with numbers as json.Number.
func modelMembers(object []byte) []modelMember {
	decoder := json.NewDecoder(bytes.NewReader(object))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	var members []modelMember
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return members
		}
		key, _ := token.(string)
		var value any
		if decoder.Decode(&value) != nil {
			return members
		}
		members = append(members, modelMember{key: key, value: value})
	}
	return members
}

func modelFlagged(entry modelEntry) bool {
	return entry.object != nil && (entry.object["is_me"] == true || entry.object["isMe"] == true)
}

// modelNumber is an entry's number: a string's trimmed text, or an object's
// first non-blank "number" or "phone" text.
func modelNumber(entry modelEntry) string {
	if entry.isString {
		return strings.TrimSpace(entry.text)
	}
	for _, key := range []string{"number", "phone"} {
		if value, ok := entry.object[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// modelOwnerLines lists the owner's phone lines in conversations: the last
// ten digits of the number of each flagged object whose number names a line,
// as phonelinetest.Digits reads it.
func modelOwnerLines(conversations []*Conversation) []string {
	var lines []string
	for _, conversation := range conversations {
		for _, entry := range modelEntries(conversation.Participants) {
			if !modelFlagged(entry) {
				continue
			}
			if digits, ok := phonelinetest.Digits(modelNumber(entry)); ok {
				lines = append(lines, modelLastTen(digits))
			}
		}
	}
	return lines
}

// modelOwners reports whether the owner rule counts entry as the owner's:
// a flagged object, or any entry whose number sits on one of lines, as
// phonelinetest.Match says.
func modelOwners(entry modelEntry, lines []string) bool {
	if modelFlagged(entry) {
		return true
	}
	for _, line := range lines {
		if phonelinetest.Match(line, modelNumber(entry)) {
			return true
		}
	}
	return false
}

// modelEntryMatches reports whether entry's searchable text holds query: a
// string's text, or the text of an object's "name", "number", "phone" or
// "email" member.
func modelEntryMatches(entry modelEntry, query string) bool {
	if entry.isString {
		return modelLike(entry.text, query)
	}
	for _, member := range entry.members {
		switch member.key {
		case "name", "number", "phone", "email":
			if text, ok := member.value.(string); ok && modelLike(text, query) {
				return true
			}
		}
	}
	return false
}

// modelLike reports whether text holds query as SQLite's LIKE '%query%'
// finds it: up to text's first NUL, and with only ASCII letters' case aside.
func modelLike(text, query string) bool {
	if cut := strings.IndexByte(text, 0); cut >= 0 {
		text = text[:cut]
	}
	return strings.Contains(modelFoldASCII(text), modelFoldASCII(query))
}

// modelFoldASCII lowers ASCII letters and leaves every other byte alone.
func modelFoldASCII(text string) string {
	folded := []byte(text)
	for index, c := range folded {
		if c >= 'A' && c <= 'Z' {
			folded[index] = c + 'a' - 'A'
		}
	}
	return string(folded)
}

func (m searchModel) search(query string, limit int) []string {
	lines := modelOwnerLines(m.conversations)
	contactNames := map[string][]string{}
	for _, contact := range m.contacts {
		contactNames[contact.Number] = append(contactNames[contact.Number], contact.Name, contact.Number)
	}
	var hits []*Conversation
	for _, conversation := range m.conversations {
		hit := modelLike(conversation.Name, query)
		for _, entry := range modelEntries(conversation.Participants) {
			if !hit && modelEntryMatches(entry, query) && !modelOwners(entry, lines) {
				hit = true
			}
		}
		for _, message := range m.messages {
			if hit || message.ConversationID != conversation.ConversationID || message.IsFromMe {
				continue
			}
			hit = modelLike(message.SenderName, query) || modelLike(message.SenderNumber, query)
			for _, value := range contactNames[message.SenderNumber] {
				hit = hit || modelLike(value, query)
			}
		}
		if hit {
			hits = append(hits, conversation)
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].LastMessageTS != hits[j].LastMessageTS {
			return hits[i].LastMessageTS > hits[j].LastMessageTS
		}
		return hits[i].ConversationID < hits[j].ConversationID
	})
	ids := make([]string, 0, len(hits))
	for _, conversation := range hits {
		if len(ids) == limit {
			break
		}
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}

// Over random stores, SearchConversationsByMetadata returns what searchModel
// returns, in the same order. The stores list the owner flagged (is_me or
// isMe), as an unflagged copy in any written form, both, or not at all, on
// SMS, Signal and WhatsApp threads; the forms include no-break spaces, tabs,
// slashes and dashes, some copies hold WhatsApp JIDs, and some values hold
// the owner's digits but name no line, flagged or not; other people share
// words of the owner's name; some participants columns hold nothing,
// malformed JSON or a list of bare names, and lists can hold odd elements
// (an element that isn't an object, or a field of another JSON type) and
// entries, flagged or not, with a number too large for a float64, nest
// deeper than SQLite's JSON reader goes, or carry bytes after a NUL, where
// SQLite's reader stops and Go's doesn't; names and the owner's values can
// hold a NUL, and some names hold letters beyond ASCII; the owner's own
// messages and contact card carry the owner's name and number; and threads
// often share a time. So no query finds a thread through an entry of the
// owner's, every title, other participant and incoming sender still
// matches, the searches read every list as send_message does, and threads
// that share a time come in conversation ID order.
func TestSearchConversationsByMetadataAgreesWithTheOwnerSkippingModel(t *testing.T) {
	people := []struct{ name, number string }{
		{"Sam Example", "+15555550100"},
		{"Bob Owner", "+15555550101"},
		{"Carol Sammy", "15555550102"},
		{"Dee Example", "+44 20 7946 0103"},
		{"Ed Short", "56789"},
		{"\u00c1ngel N\u00fa\u00f1ez", "+15555550104"},
		{"Zed\x00Hidden", "+15555550105"},
	}
	owner := struct{ name, number string }{"Sam Owner", "+15550009999"}
	exercised := map[string]int{}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store := newTestStore(t)
		model := searchModel{}
		flaggedSomewhere := false

		count := 3 + random.Intn(6)
		for index := 0; index < count; index++ {
			id := "c" + strconv.Itoa(index)
			platform := []string{"sms", "sms", "whatsapp", "signal"}[random.Intn(4)]
			var entries []map[string]any
			for _, member := range random.Perm(len(people))[:1+random.Intn(3)] {
				entry := map[string]any{"name": people[member].name, "number": people[member].number}
				switch random.Intn(8) {
				case 0, 1:
					entry["id"] = strconv.Itoa(member)
				case 2:
					entry["id"] = json.RawMessage("1e400")
					exercised["a person's entry with a number too large for a float64"]++
				}
				entries = append(entries, entry)
			}
			ownerForms := append(append(append([]string{}, ownerNumberForms...), ownerNumberSeparatedForms...), ownerJIDs...)
			ownerForm := func() string {
				if random.Intn(8) == 0 {
					exercised["an owner's value that names no line"]++
					return ownerDigitHolders[random.Intn(len(ownerDigitHolders))]
				}
				if random.Intn(12) == 0 {
					exercised["an owner's value with a NUL"]++
					return ownerNULForms[random.Intn(len(ownerNULForms))]
				}
				return ownerForms[random.Intn(len(ownerForms))]
			}
			switch random.Intn(5) {
			case 0: // not listed
			case 1:
				flagged := map[string]any{"name": owner.name, "number": ownerForm(), "is_me": true}
				if random.Intn(3) == 0 {
					flagged["contact_id"] = json.RawMessage("-1e400")
					exercised["a flagged entry with a number too large for a float64"]++
				}
				entries = append(entries, flagged)
				flaggedSomewhere = true
			case 2:
				entries = append(entries, map[string]any{"name": "Me", "phone": ownerForm(), "isMe": true})
				flaggedSomewhere = true
			case 3:
				entries = append(entries,
					map[string]any{"name": owner.name, "number": ownerForm(), "is_me": true},
					map[string]any{"name": owner.name, "number": ownerForm(), "id": "1", "contact_id": "9"},
				)
				flaggedSomewhere = true
			default:
				copied := ownerForm()
				if random.Intn(4) == 0 {
					copied = ownerJIDs[random.Intn(len(ownerJIDs))]
				}
				entries = append(entries, map[string]any{"name": owner.name + " (phone)", "number": copied})
				exercised["an unflagged copy of the owner"]++
			}
			random.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
			elements := make([]any, 0, len(entries)+1)
			for _, entry := range entries {
				elements = append(elements, entry)
			}
			if random.Intn(3) == 0 {
				odd := []any{
					map[string]any{"name": owner.name, "number": owner.number, "is_me": "true"},
					map[string]any{"name": "Odd Owner", "number": 15550009999, "isMe": true},
					map[string]any{"name": []string{owner.name}, "phone": owner.number, "is_me": 1},
					owner.name, []string{"Sam"}, 17, nil,
				}[random.Intn(7)]
				elements = append(elements, odd)
				exercised["an odd element"]++
			}
			random.Shuffle(len(elements), func(i, j int) { elements[i], elements[j] = elements[j], elements[i] })
			participants, err := json.Marshal(elements)
			if err != nil {
				t.Fatal(err)
			}
			switch random.Intn(16) {
			case 0:
				participants = []byte("")
			case 1:
				participants = []byte("not json " + owner.name)
			case 2:
				participants = []byte(`["` + owner.name + `","Bob Owner"]`)
			case 3:
				participants = append([]byte("["+ownerRuleNested(1000+random.Intn(500))+","), participants[1:]...)
				exercised["a list nested deeper than SQLite reads"]++
			case 4:
				participants = append(participants, "\x00garbage"...)
				exercised["a list with bytes after a NUL"]++
			case 5:
				if at := bytes.IndexByte(participants, '{'); at >= 0 {
					participants = append(append(append([]byte{}, participants[:at+1]...), `"nested":`+ownerRuleNested(1000+random.Intn(500))+`,`...), participants[at+1:]...)
					exercised["a member nested deeper than SQLite reads"]++
				}
			}
			title := entries[0]["name"].(string)
			if random.Intn(3) == 0 {
				title = []string{"Group " + id, owner.name + " and friends", "Sam's people"}[random.Intn(3)]
			}
			conversation := &Conversation{
				ConversationID: id, Name: title, IsGroup: len(entries) > 2, Participants: string(participants),
				LastMessageTS: int64(1_000_000 + 1000*random.Intn(4)), SourcePlatform: platform,
			}
			seedConversations(t, store, conversation)
			for number := 0; number < random.Intn(3); number++ {
				message := &Message{
					MessageID: id + "-m" + strconv.Itoa(number), ConversationID: id, Body: "hello",
					TimestampMS: int64(1000 + number), SourcePlatform: platform,
				}
				if random.Intn(2) == 0 {
					message.IsFromMe, message.SenderName, message.SenderNumber = true, owner.name, owner.number
				} else {
					sender := people[random.Intn(len(people))]
					message.SenderName, message.SenderNumber = sender.name, sender.number
				}
				if err := store.UpsertMessage(message); err != nil {
					t.Fatalf("seed message: %v", err)
				}
				model.messages = append(model.messages, message)
			}
		}
		contacts := []*Contact{{ContactID: "card-owner", Name: owner.name, Number: owner.number}}
		if random.Intn(2) == 0 {
			contacts = append(contacts, &Contact{ContactID: "card-sam", Name: "Sammy E", Number: people[0].number})
		}
		for _, contact := range contacts {
			if err := store.UpsertContact(contact); err != nil {
				t.Fatalf("seed contact: %v", err)
			}
		}
		model.contacts = contacts
		stored, err := store.ListConversations(1000)
		if err != nil {
			t.Fatalf("list conversations: %v", err)
		}
		model.conversations = stored
		if !flaggedSomewhere {
			exercised["a store that never flags the owner"]++
		}

		queries := append(append([]string{
			owner.name, "Sam", "Owner", "Me", "is_me", "isMe", "name", "phone", "contact_id", "Odd", "whatsapp",
			"Sammy", "Bob", "Carol", "Dee", "+1555555", "0103", "56789", "friends", "Group", "not json", "(phone)",
			"\u00e1ngel", "\u00c1NGEL", "\u00c1ngel", "N\u00fa\u00f1ez", "N\u00da\u00d1EZ", "Zed", "zed", "Hidden",
		}, ownerNumberForms...), ownerNumberSeparatedForms...)
		queries = append(append(queries, ownerJIDs...), ownerDigitHolders...)
		for _, query := range queries {
			limit := 1 + random.Intn(10)
			want := model.search(query, limit)
			got := searchIDs(t, store, query, limit)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				for _, conversation := range stored {
					t.Logf("  %s %q %s ts=%d: %s", conversation.ConversationID, conversation.Name, conversation.SourcePlatform, conversation.LastMessageTS, conversation.Participants)
				}
				t.Logf("seed %d: search %q (limit %d) = %v, model says %v", seed, query, limit, got, want)
				return false
			}
			if len(want) > 0 {
				exercised["a query with hits"]++
			}
			if query == "\u00e1ngel" || query == "\u00c1NGEL" || query == "N\u00da\u00d1EZ" {
				for _, conversation := range stored {
					if strings.Contains(conversation.Name+conversation.Participants, "\u00c1ngel N\u00fa\u00f1ez") {
						exercised["a query that only Unicode case folding would match"]++
					}
				}
			}
			if all := model.search(query, len(stored)); len(all) > len(want) && len(want) > 0 {
				at := map[string]int64{}
				for _, conversation := range stored {
					at[conversation.ConversationID] = conversation.LastMessageTS
				}
				if at[all[len(want)]] == at[want[len(want)-1]] {
					exercised["a tie at the limit"]++
				}
			}
		}
		return true
	}
	cases := 200
	if raceDetectorEnabled {
		cases = 40
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"an unflagged copy of the owner", "an owner's value that names no line", "a store that never flags the owner",
		"an odd element", "a query with hits", "a tie at the limit", "an owner's value with a NUL",
		"a list nested deeper than SQLite reads", "a list with bytes after a NUL", "a member nested deeper than SQLite reads",
		"a query that only Unicode case folding would match", "a person's entry with a number too large for a float64",
		"a flagged entry with a number too large for a float64",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
