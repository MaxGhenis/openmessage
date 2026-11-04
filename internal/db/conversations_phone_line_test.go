package db

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/phoneline"
	"github.com/maxghenis/openmessage/internal/phoneline/phonelinetest"
)

// phoneLineModel is a reference for SearchConversationsByPhoneLine. A thread
// is on a line when phonelinetest.MatchFirst, which states phoneline's rule
// with regular expressions, finds the line in the number (or, without one,
// the phone) of a participant object, where only text counts, in a bare
// string entry, or in the sender number of an incoming message. Entries
// that the owner rule counts as the owner's don't put a thread on a line:
// objects whose is_me or isMe holds JSON true, and any entry whose number
// sits on one of the owner's lines, which modelOwnerLines takes from every
// thread. Hits come newest first, then in conversation ID order, up to the
// limit.
type phoneLineModel struct {
	conversations []*Conversation
	messages      []*Message
}

// lineReach says how a thread is on a line.
type lineReach struct {
	// hit holds when the search returns the thread, and senderOnly when an
	// incoming sender alone puts it there.
	hit, senderOnly bool
	// flaggedOnly holds for a thread on the line only through the owner's
	// entries, one of them flagged, and copyOnly for a thread on it only
	// through unflagged copies of the owner.
	flaggedOnly, copyOnly bool
	// stringCopy holds when a bare string on the line counts as the owner's.
	stringCopy bool
}

// onLine says how conversation is on line, given the owner's lines.
func (m phoneLineModel) onLine(conversation *Conversation, line string, ownerLines []string) lineReach {
	byEntry, byFlagged, byCopy, stringCopy := false, false, false, false
	for _, entry := range modelEntries(conversation.Participants) {
		on := false
		if entry.isString {
			on = phonelinetest.MatchFirst(line, entry.text)
		} else {
			number, _ := entry.object["number"].(string)
			phone, _ := entry.object["phone"].(string)
			on = phonelinetest.MatchFirst(line, number, phone)
		}
		switch {
		case !on:
		case modelFlagged(entry):
			byFlagged = true
		case modelOwners(entry, ownerLines):
			byCopy = true
			stringCopy = stringCopy || entry.isString
		default:
			byEntry = true
		}
	}
	bySender := false
	for _, message := range m.messages {
		if message.ConversationID == conversation.ConversationID && !message.IsFromMe && phonelinetest.MatchFirst(line, message.SenderNumber) {
			bySender = true
		}
	}
	hit := byEntry || bySender
	return lineReach{
		hit:         hit,
		senderOnly:  bySender && !byEntry,
		flaggedOnly: byFlagged && !hit,
		copyOnly:    byCopy && !byFlagged && !hit,
		stringCopy:  stringCopy,
	}
}

func (m phoneLineModel) search(line string, limit int) []string {
	ownerLines := modelOwnerLines(m.conversations)
	var hits []*Conversation
	for _, conversation := range m.conversations {
		if m.onLine(conversation, line, ownerLines).hit {
			hits = append(hits, conversation)
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].LastMessageTS != hits[j].LastMessageTS {
			return hits[i].LastMessageTS > hits[j].LastMessageTS
		}
		return hits[i].ConversationID < hits[j].ConversationID
	})
	ids := []string{}
	for _, conversation := range hits {
		if len(ids) == limit {
			break
		}
		ids = append(ids, conversation.ConversationID)
	}
	return ids
}

// heldOnlyByIdentifiers reports whether conversation is off line although a
// value in it, one that is no phone number such as an email address, holds
// digits that end with the line's: a thread that matching digits alone
// would put on the line.
func (m phoneLineModel) heldOnlyByIdentifiers(conversation *Conversation, line string, ownerLines []string) bool {
	if m.onLine(conversation, line, ownerLines).hit {
		return false
	}
	values := []string{}
	var elements []any
	if json.Unmarshal([]byte(conversation.Participants), &elements) == nil {
		for _, element := range elements {
			switch element := element.(type) {
			case string:
				values = append(values, element)
			case map[string]any:
				for _, key := range []string{"number", "phone"} {
					if value, ok := element[key].(string); ok {
						values = append(values, value)
					}
				}
			}
		}
	}
	for _, message := range m.messages {
		if message.ConversationID == conversation.ConversationID {
			values = append(values, message.SenderNumber)
		}
	}
	for _, value := range values {
		digits := phoneline.Digits(value)
		if _, ok := phonelinetest.Digits(value); !ok && len(digits) >= 7 && strings.HasSuffix(digits, line) {
			return true
		}
	}
	return false
}

// phoneLineForm writes a number's digits as an address book or a transport
// might: E.164, national with separators, or with a stray prefix, or as a
// tel: URI, which is no written number.
func phoneLineForm(random *rand.Rand, digits string) string {
	switch random.Intn(5) {
	case 0:
		return "+" + digits
	case 1:
		if len(digits) == 11 {
			return "+" + digits[:1] + " (" + digits[1:4] + ") " + digits[4:7] + "-" + digits[7:]
		}
	case 2:
		var b strings.Builder
		for index, r := range digits {
			if index > 0 && index%3 == 0 {
				b.WriteString([]string{" ", "-", "."}[random.Intn(3)])
			}
			b.WriteRune(r)
		}
		return b.String()
	case 3:
		return "tel:" + digits
	}
	return digits
}

// An object that repeats "number" names the line of the last one, as Go
// reads the object for send_message and for the owner rule. SQLite's
// json_extract takes the first, so the line search found Bob's thread under
// a number that send_message never reads there.
func TestSearchConversationsByPhoneLineReadsARepeatedNumberAsSendMessageDoes(t *testing.T) {
	store := newTestStore(t)
	seedConversations(t, store, &Conversation{
		ConversationID: "bob", Name: "Bob Example", SourcePlatform: "sms", LastMessageTS: 1000,
		Participants: `[{"name":"Bob Example","number":"+15555550199","number":"+15555550188"}]`,
	})
	if got := lineSearchIDs(t, store, "+15555550188", 10); fmt.Sprint(got) != "[bob]" {
		t.Errorf("the line of the last number finds %v, want [bob]", got)
	}
	if got := lineSearchIDs(t, store, "+15555550199", 10); len(got) != 0 {
		t.Errorf("the line of the first number finds %v, want none", got)
	}
}

// Differential, over random stores: SearchConversationsByPhoneLine returns
// exactly what phoneLineModel returns, in the same order. Numbers come in
// many written forms. Decoys hold a target's last ten digits inside a longer
// number, share only its last seven, or are too short to name a line, and one
// holds the owner's last seven digits, which also end a target's number; some
// entries carry the number under phone, a blank number, or a number that
// isn't text, and some repeat "number"; identifiers that are no phone
// number, such as an email address, a Signal UUID, a WhatsApp LID or a
// number followed by a NUL, hold targets' digits; some lists nest deeper
// than SQLite's JSON reader goes or carry bytes after a NUL, where SQLite's
// reader stops and Go's doesn't, so only Go's reading counts; the owner's entries
// carry the owner's number flagged, unflagged or with a flag that isn't JSON
// true, other objects and bare strings carry it too, and the owner sends
// messages too; and threads often share a time. So, as invariants, a thread
// is on a line only through an incoming sender or an entry that the owner
// rule doesn't count as the owner's (neither flagged nor on a flagged
// entry's line, which any thread may list) whose number, written as one,
// ends with the line's digits (or the line ends with theirs); an incoming
// sender whom the roster lacks counts; once a thread lists the owner
// flagged, the owner's own line finds a thread only through an incoming
// sender; the limit applies after matching, so threads whose numbers only
// contain the digits never crowd out one on the line; and threads that share
// a time come in conversation ID order.
func TestSearchConversationsByPhoneLineAgreesWithTheLineModel(t *testing.T) {
	// neighbor's number ends in the owner's last seven digits, which the last
	// decoy holds.
	neighbor := "14440009999"
	targets := []string{"12025550123", "13035550147", "15555550199", neighbor}
	owner := "15550009999"
	decoys := []string{"447555012399", "4412025550123", "13035550123", "5550123", "550123", "12025550124", "0009999"}
	// identifiers hold a target's digits but are no phone number, except a
	// WhatsApp JID on the phone-number server, which names its user's line.
	identifiers := []string{"alice12025550123@example.com", "abcdefab-abcd-abcd-abcd-a13035550147", "15555550199@lid", "users/12025550123", "+99999912025550123", "13035550147@s.whatsapp.net", "+12025550123\x00", "+1 303 555 0147\x00x"}
	exercised := map[string]int{}

	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store := newTestStore(t)
		model := phoneLineModel{}
		numbers := append(append([]string{owner}, targets...), decoys...)
		pick := func() string { return numbers[random.Intn(len(numbers))] }
		value := func() string {
			if random.Intn(5) == 0 {
				return identifiers[random.Intn(len(identifiers))]
			}
			return phoneLineForm(random, pick())
		}

		count := 4 + random.Intn(10)
		for index := 0; index < count; index++ {
			id := "c" + strconv.Itoa(index)
			var entries []any
			for member := 0; member < 1+random.Intn(3); member++ {
				form := value()
				switch random.Intn(8) {
				case 0:
					entries = append(entries, form)
				case 1:
					entries = append(entries, map[string]any{"name": "P" + strconv.Itoa(member), "phone": form})
				case 2:
					entries = append(entries, map[string]any{"name": "P" + strconv.Itoa(member), "number": " ", "phone": form})
				case 3:
					entries = append(entries, map[string]any{"name": "P" + strconv.Itoa(member), "number": form, "phone": value()})
				case 4:
					number, _ := strconv.ParseInt(phoneline.Digits(form), 10, 64)
					entries = append(entries, map[string]any{"name": "P" + strconv.Itoa(member), "number": number})
				case 5:
					first, _ := json.Marshal(value())
					last, _ := json.Marshal(form)
					entries = append(entries, json.RawMessage(`{"name":"P`+strconv.Itoa(member)+`","number":`+string(first)+`,"number":`+string(last)+`}`))
					exercised["an object that repeats number"]++
				default:
					entries = append(entries, map[string]any{"name": "P" + strconv.Itoa(member), "number": form})
				}
			}
			switch random.Intn(5) {
			case 0: // the owner isn't listed
			case 1:
				entries = append(entries, map[string]any{"name": "Me", "number": phoneLineForm(random, owner), "is_me": "true"})
			case 2:
				entries = append(entries, map[string]any{"name": "Me", "phone": phoneLineForm(random, owner), "isMe": true})
			default:
				entries = append(entries, map[string]any{"name": "Me", "number": phoneLineForm(random, owner), "is_me": true})
			}
			random.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
			participants, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			switch random.Intn(14) {
			case 0:
				participants = []byte("")
			case 1:
				participants = []byte(`{"number":"+` + targets[0] + `"}`)
			case 2:
				participants = append([]byte("["+ownerRuleNested(1000+random.Intn(500))+","), participants[1:]...)
				exercised["a list nested deeper than SQLite reads"]++
			case 3:
				participants = append(participants, "\x00garbage"...)
				exercised["a list with bytes after a NUL"]++
			}
			if err := store.UpsertConversation(&Conversation{
				ConversationID: id, Name: "Thread " + id, Participants: string(participants),
				LastMessageTS: int64(1_000_000 + 1000*random.Intn(6)), SourcePlatform: "sms",
			}); err != nil {
				t.Fatalf("seed conversation: %v", err)
			}
			for number := 0; number < random.Intn(3); number++ {
				message := &Message{
					MessageID: id + "-m" + strconv.Itoa(number), ConversationID: id, Body: "hello",
					TimestampMS: int64(1000 + number), SourcePlatform: "sms",
					SenderNumber: value(),
				}
				if random.Intn(3) == 0 {
					message.IsFromMe = true
				}
				if err := store.UpsertMessage(message); err != nil {
					t.Fatalf("seed message: %v", err)
				}
				model.messages = append(model.messages, message)
			}
		}
		stored, err := store.ListConversations(1000)
		if err != nil {
			t.Fatalf("list conversations: %v", err)
		}
		model.conversations = stored
		ownerLines := modelOwnerLines(stored)
		incomingOn := func(conversationID, line string) bool {
			for _, message := range model.messages {
				if message.ConversationID == conversationID && !message.IsFromMe && phonelinetest.MatchFirst(line, message.SenderNumber) {
					return true
				}
			}
			return false
		}

		for _, number := range append(append([]string{owner, "5550123", "550123"}, targets...), decoys[:3]...) {
			line := phoneline.Line(number)
			limit := 1 + random.Intn(6)
			want := model.search(line, limit)
			got, err := store.SearchConversationsByPhoneLine(line, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByPhoneLine(%q): %v", line, err)
			}
			gotIDs := []string{}
			for _, conversation := range got {
				gotIDs = append(gotIDs, conversation.ConversationID)
			}
			failed := func(format string, args ...any) bool {
				for _, conversation := range stored {
					t.Logf("  %s ts=%d: %s", conversation.ConversationID, conversation.LastMessageTS, conversation.Participants)
				}
				for _, message := range model.messages {
					t.Logf("  message %s in %s from_me=%v sender=%q", message.MessageID, message.ConversationID, message.IsFromMe, message.SenderNumber)
				}
				t.Logf("seed %d: line %q (limit %d) = %v, owner lines %v: "+format, append([]any{seed, line, limit, gotIDs, ownerLines}, args...)...)
				return false
			}
			if fmt.Sprint(gotIDs) != fmt.Sprint(want) {
				return failed("model says %v", want)
			}
			if line == phoneline.Line(owner) && len(ownerLines) > 0 {
				exercised["the owner's own line once a thread lists the owner flagged"]++
				for _, id := range gotIDs {
					if !incomingOn(id, line) {
						return failed("%s is on the owner's own line through no incoming sender", id)
					}
				}
			}
			for _, conversation := range stored {
				reach := model.onLine(conversation, line, ownerLines)
				if reach.flaggedOnly {
					exercised["a thread on the line only through the flagged owner"]++
				}
				if reach.copyOnly {
					exercised["a thread on the line only through unflagged copies of the owner"]++
					if line == phoneline.Line(neighbor) {
						exercised["a thread on another person's line only through a short copy of the owner"]++
					}
				}
				if reach.stringCopy {
					exercised["a bare string that counts as the owner's"]++
				}
			}
			if len(want) == 0 {
				continue
			}
			exercised["a line with hits"]++
			if all := model.search(line, len(stored)); len(all) > len(want) {
				at := map[string]int64{}
				for _, conversation := range stored {
					at[conversation.ConversationID] = conversation.LastMessageTS
				}
				if at[all[len(want)]] == at[want[len(want)-1]] {
					exercised["a tie at the limit"]++
				}
			}
			oldest := int64(-1)
			for _, conversation := range stored {
				if conversation.ConversationID == want[len(want)-1] {
					oldest = conversation.LastMessageTS
				}
			}
			for _, conversation := range stored {
				reach := model.onLine(conversation, line, ownerLines)
				if reach.senderOnly {
					exercised["a hit through an incoming sender alone"]++
				}
				if model.heldOnlyByIdentifiers(conversation, line, ownerLines) {
					exercised["a thread that only identifiers holding the line's digits tie to it"]++
				}
				if !reach.hit && conversation.LastMessageTS > oldest && strings.Contains(conversation.Participants, line) {
					exercised["a hit behind a newer thread whose numbers only contain the line's digits"]++
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
		"a line with hits",
		"a hit through an incoming sender alone",
		"a thread on the line only through the flagged owner",
		"a thread on the line only through unflagged copies of the owner",
		"a thread on another person's line only through a short copy of the owner",
		"a bare string that counts as the owner's",
		"the owner's own line once a thread lists the owner flagged",
		"a hit behind a newer thread whose numbers only contain the line's digits",
		"a tie at the limit",
		"a thread that only identifiers holding the line's digits tie to it",
		"an object that repeats number", "a list nested deeper than SQLite reads", "a list with bytes after a NUL",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
