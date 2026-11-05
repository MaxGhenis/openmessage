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
// the phone) of a participant entry without the owner's flag, where only
// text counts, in a bare string entry, or in the sender number of an
// incoming message. An entry carries the owner's flag when its is_me or isMe
// holds JSON true. Hits come newest first, then in conversation ID order, up
// to the limit.
type phoneLineModel struct {
	conversations []*Conversation
	messages      []*Message
}

// onLine reports how conversation is on line: at all, through an incoming
// sender alone, or only through entries with the owner's flag.
func (m phoneLineModel) onLine(conversation *Conversation, line string) (hit, senderOnly, ownerOnly bool) {
	byEntry, byOwner := false, false
	var elements []json.RawMessage
	if json.Unmarshal([]byte(conversation.Participants), &elements) == nil {
		for _, element := range elements {
			var object map[string]any
			if json.Unmarshal(element, &object) == nil && object != nil {
				number, _ := object["number"].(string)
				phone, _ := object["phone"].(string)
				if !phonelinetest.MatchFirst(line, number, phone) {
					continue
				}
				if object["is_me"] == true || object["isMe"] == true {
					byOwner = true
				} else {
					byEntry = true
				}
				continue
			}
			var text string
			if json.Unmarshal(element, &text) == nil && phonelinetest.MatchFirst(line, text) {
				byEntry = true
			}
		}
	}
	bySender := false
	for _, message := range m.messages {
		if message.ConversationID == conversation.ConversationID && !message.IsFromMe && phonelinetest.MatchFirst(line, message.SenderNumber) {
			bySender = true
		}
	}
	hit = byEntry || bySender
	return hit, bySender && !byEntry, byOwner && !hit
}

func (m phoneLineModel) search(line string, limit int) []string {
	var hits []*Conversation
	for _, conversation := range m.conversations {
		if hit, _, _ := m.onLine(conversation, line); hit {
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

// heldOnlyByIdentifiers reports whether conversation stays off the line
// although a value in it that reads as no phone number, such as an email
// address, holds digits that end with the line's: a thread that matching
// digits alone would put on the line.
func (m phoneLineModel) heldOnlyByIdentifiers(conversation *Conversation, line string) bool {
	if hit, _, _ := m.onLine(conversation, line); hit {
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
// tel: URI, which reads as no written number.
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

// Differential, over random stores: SearchConversationsByPhoneLine returns
// exactly what phoneLineModel returns, in the same order. Numbers come in
// many written forms. Decoys hold a target's last ten digits inside a longer
// number, share only its last seven, or are too short to name a line; some
// entries carry the number under phone, a blank number, or a number that
// isn't text; identifiers that are no phone number, such as an email address,
// a Signal UUID, a WhatsApp LID or a number that a NUL ends early, hold
// targets' digits; the owner's entries carry the owner's number flagged,
// unflagged or with a flag that isn't JSON true, and the owner sends messages
// too; and threads often share a time. So, as invariants, a thread joins a
// line only through someone other than the flagged owner whose number reads
// as one and ends with the line's digits (or the line ends with theirs), an
// incoming sender whom the roster lacks counts, the limit applies after
// matching, so threads whose numbers only contain the digits never crowd out
// one on the line, and threads that share a time come in conversation ID
// order.
func TestSearchConversationsByPhoneLineAgreesWithTheLineModel(t *testing.T) {
	targets := []string{"12025550123", "13035550147", "15555550199"}
	owner := "15550009999"
	decoys := []string{"447555012399", "4412025550123", "13035550123", "5550123", "550123", "12025550124"}
	// identifiers hold a target's digits but are no phone number, except a
	// WhatsApp JID on the phone-number server, which names its user's line.
	identifiers := []string{"alice12025550123@example.com", "abcdefab-abcd-abcd-abcd-a13035550147", "15555550199@lid", "users/12025550123", "+99999912025550123", "13035550147@s.whatsapp.net", "+12025550123\x00x"}
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
			switch random.Intn(12) {
			case 0:
				participants = []byte("")
			case 1:
				participants = []byte(`{"number":"+` + targets[0] + `"}`)
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
			if fmt.Sprint(gotIDs) != fmt.Sprint(want) {
				for _, conversation := range stored {
					t.Logf("  %s ts=%d: %s", conversation.ConversationID, conversation.LastMessageTS, conversation.Participants)
				}
				for _, message := range model.messages {
					t.Logf("  message %s in %s from_me=%v sender=%q", message.MessageID, message.ConversationID, message.IsFromMe, message.SenderNumber)
				}
				t.Logf("seed %d: line %q (limit %d) = %v, model says %v", seed, line, limit, gotIDs, want)
				return false
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
				hit, senderOnly, ownerOnly := model.onLine(conversation, line)
				if senderOnly {
					exercised["a hit through an incoming sender alone"]++
				}
				if ownerOnly {
					exercised["a thread on the line only through the flagged owner"]++
				}
				if model.heldOnlyByIdentifiers(conversation, line) {
					exercised["a thread that only identifiers holding the line's digits tie to it"]++
				}
				if !hit && conversation.LastMessageTS > oldest && strings.Contains(conversation.Participants, line) {
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
		"a hit behind a newer thread whose numbers only contain the line's digits",
		"a tie at the limit",
		"a thread that only identifiers holding the line's digits tie to it",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
