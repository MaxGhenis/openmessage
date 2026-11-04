package db

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf16"

	"github.com/maxghenis/openmessage/internal/phoneline"
	"github.com/maxghenis/openmessage/internal/phoneline/phonelinetest"
)

// ownerRuleNumbers are values the owner rule meets in "number" and "phone":
// the owner's number in many written forms, the owner's WhatsApp JIDs,
// values that hold the owner's digits but name no line (a NUL among them),
// a LID, other people's numbers, fragments too short for a line, and blanks.
var ownerRuleNumbers = append(append(append(append(append([]string{
	"105274891234567@lid", "+15555550123", "5555550123",
	"+44 20 7946 0958", "12345", "555-0199", "", "  ", " ",
}, ownerNumberForms...), ownerNumberSeparatedForms...), ownerJIDs...), ownerDigitHolders...), ownerNULForms...)

// ownerRuleQuote writes s as a JSON string. With ascii set it escapes every
// rune outside printable ASCII, as some writers do.
func ownerRuleQuote(s string, ascii bool) string {
	if !ascii {
		quoted, _ := json.Marshal(s)
		return string(quoted)
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r > 0xffff:
			high, low := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, high, low)
		case r < 0x20 || r > 0x7e:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ownerRuleNested is JSON nested depth arrays deep.
func ownerRuleNested(depth int) string {
	return strings.Repeat("[", depth) + strings.Repeat("]", depth)
}

// ownerRuleElement is the JSON text of one random element of a participants
// list, with the traits it drew. Most elements are objects whose fields can
// repeat a key, escape a character of a key, hold another JSON type than the
// rule reads, hold a NUL, nest deeper than SQLite's JSON reader goes, or sit
// between extra whitespace. Some are strings: a name, the owner's number in
// one of its written forms, any other value a number can hold, or text that
// reads as a flagged object. The rest are neither objects nor strings.
func ownerRuleElement(random *rand.Rand) (string, []string) {
	switch random.Intn(10) {
	case 0:
		odd := []string{`17`, `null`, `true`, `[{"is_me":true,"number":"+15550009999"}]`, `{}`}
		return odd[random.Intn(len(odd))], []string{"an element that's neither an object nor a string"}
	case 1:
		traits := []string{"a string element"}
		switch random.Intn(4) {
		case 0:
			return `"{\"is_me\":true,\"number\":\"+15550009999\"}"`, append(traits, "a string that reads as a flagged object")
		case 1:
			return `"Sam Owner"`, traits
		case 2:
			return ownerRuleQuote(ownerNumberForms[random.Intn(len(ownerNumberForms))], random.Intn(3) == 0), traits
		}
		return ownerRuleQuote(ownerRuleNumbers[random.Intn(len(ownerRuleNumbers))], random.Intn(3) == 0), traits
	}
	var traits []string
	space := func() string { return []string{"", " ", "\n\t"}[random.Intn(3)] }
	key := func(name string) string {
		if random.Intn(8) == 0 {
			traits = append(traits, "an escaped key")
			at := random.Intn(len(name))
			return `"` + name[:at] + fmt.Sprintf(`\u%04x`, name[at]) + name[at+1:] + `"`
		}
		return `"` + name + `"`
	}
	value := func(field string) string {
		switch field {
		case "name", "email":
			text := []string{`"Sam Owner"`, `"Alice Example"`, `["Sam"]`, `""`, `"Zed\u0000Hidden"`, `"Ángel"`, `"sam@example.com"`}[random.Intn(7)]
			if strings.Contains(text, `\u0000`) {
				traits = append(traits, "a NUL in a text")
			}
			return text
		case "is_me", "isMe":
			if random.Intn(5) < 3 {
				return "true"
			}
			traits = append(traits, "a flag that isn't true")
			return []string{"false", `"true"`, "1", "null"}[random.Intn(4)]
		case "nested":
			traits = append(traits, "a member nested deeper than SQLite reads")
			return ownerRuleNested(1000 + random.Intn(500))
		}
		if random.Intn(5) == 0 {
			traits = append(traits, "a number of another JSON type")
			return []string{"15550009999", `["+15550009999"]`, "null", "true"}[random.Intn(4)]
		}
		return ownerRuleQuote(ownerRuleNumbers[random.Intn(len(ownerRuleNumbers))], random.Intn(3) == 0)
	}
	var fields []string
	for _, field := range []struct {
		name    string
		percent int
	}{{"name", 70}, {"number", 70}, {"phone", 30}, {"email", 20}, {"is_me", 40}, {"isMe", 15}, {"nested", 4}} {
		if random.Intn(100) >= field.percent {
			continue
		}
		fields = append(fields, key(field.name)+":"+space()+value(field.name))
		if random.Intn(10) == 0 {
			traits = append(traits, "a repeated key")
			fields = append(fields, key(field.name)+":"+space()+value(field.name))
		}
	}
	random.Shuffle(len(fields), func(i, j int) { fields[i], fields[j] = fields[j], fields[i] })
	return "{" + space() + strings.Join(fields, ","+space()) + space() + "}", traits
}

// ownerRuleList is a random participants list and the elements it holds,
// or, now and then, text that isn't a JSON array, which holds none. Some
// lists hold an element nested deeper than SQLite's JSON reader goes, or
// deeper than Go's, and some carry bytes after a NUL, where SQLite's reader
// stops and Go's doesn't.
func ownerRuleList(random *rand.Rand, exercised map[string]int) (string, []string) {
	if random.Intn(10) == 0 {
		exercised["a list that isn't a JSON array"]++
		return []string{"", "{}", "null", `[{"is_me":true,"number":"+15550009999"}`, `"[]"`}[random.Intn(5)], nil
	}
	elements := make([]string, random.Intn(7))
	for index := range elements {
		var traits []string
		elements[index], traits = ownerRuleElement(random)
		for _, trait := range traits {
			exercised[trait]++
		}
	}
	if random.Intn(8) == 0 {
		depth := 1000 + random.Intn(2000)
		if random.Intn(4) == 0 {
			depth = 10000 + random.Intn(3)
			exercised["a list nested deeper than Go reads"]++
		} else {
			exercised["a list nested deeper than SQLite reads"]++
		}
		elements = slices.Insert(elements, random.Intn(len(elements)+1), ownerRuleNested(depth))
	}
	list := "[" + strings.Join(elements, ",") + "]"
	if random.Intn(10) == 0 {
		list += "\x00" + []string{"", "garbage", `,{"is_me":true,"number":"+15550009999"}]`}[random.Intn(3)]
		exercised["a list with bytes after a NUL"]++
	}
	return list, elements
}

// ownerRuleReference reads one element apart from owner.go: through a
// generic decode, a string as its trimmed text, and an object through the
// first non-blank string of "number" and "phone", with a flag only where
// "is_me" or "isMe" decodes to true, and with the texts of its "name",
// "number", "phone" and "email" strings in a token decode's order. ok is
// false when the element is neither an object nor a string.
func ownerRuleReference(element string) (entry ParticipantEntry, ok bool) {
	var value any
	if json.Unmarshal([]byte(element), &value) != nil {
		return ParticipantEntry{}, false
	}
	if text, isText := value.(string); isText {
		return ParticipantEntry{Number: strings.TrimSpace(text), Texts: []string{text}}, true
	}
	object, ok := value.(map[string]any)
	if !ok {
		return ParticipantEntry{}, false
	}
	entry.IsMe = object["is_me"] == true || object["isMe"] == true
	for _, key := range []string{"number", "phone"} {
		if text, isText := object[key].(string); isText && strings.TrimSpace(text) != "" {
			entry.Number = strings.TrimSpace(text)
			break
		}
	}
	for _, member := range modelMembers([]byte(element)) {
		switch member.key {
		case "name", "number", "phone", "email":
			if text, isText := member.value.(string); isText {
				entry.Texts = append(entry.Texts, text)
			}
		}
	}
	return entry, true
}

// sameEntries reports whether a and b hold the same entries in order.
func sameEntries(a, b []ParticipantEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index].Number != b[index].Number || a[index].IsMe != b[index].IsMe || !slices.Equal(a[index].Texts, b[index].Texts) {
			return false
		}
	}
	return true
}

// ownerLinesOf is the set of the owner's lines that flagged entries with
// these numbers give.
func ownerLinesOf(numbers ...string) OwnerLines {
	lines := OwnerLines{}
	for _, number := range numbers {
		lines.Add(ParticipantEntry{Number: number, IsMe: true})
	}
	return lines
}

// sqlEntryRow is an entry as openmessage_participant_entries writes it.
type sqlEntryRow struct {
	IsMe       bool     `json:"is_me"`
	Number     string   `json:"number"`
	NumberKeys []string `json:"number_keys"`
	Texts      []string `json:"texts"`
}

// Invariants of the owner rule's readers, over random participants lists
// whose elements can repeat or escape keys, hold fields of other JSON types,
// hold NULs, carry extra whitespace, be strings, or be neither objects nor
// strings, and whose text can nest deeper than SQLite's JSON reader goes or
// carry bytes after a NUL:
//
//	R1. ParticipantEntries gives, in order, one entry per object or string
//	    element, each as a reference decode of that element alone reads it,
//	    so an odd element changes no other entry. Text that isn't a JSON
//	    array, as encoding/json reads JSON, holds no entries.
//	R2. The legacy searches read a list through Go, never through SQLite's
//	    JSON reader: openmessage_participant_entries gives each entry
//	    ParticipantEntries gives, in order, with its number's
//	    phoneline.ValueKeys, and openmessage_owner_line_keys gives the
//	    phoneline.LineKeys of each line OwnerLine gives one of them.
//	R3. Over those rows, sqlEntryIsOwners agrees with OwnerLines.Owner, for
//	    the list's own lines and for a line from another conversation. A
//	    string never gives a line but counts as the owner's on an owner line,
//	    even when its text reads as a flagged object.
//
// R2 and R3 are the differential that ties the searches to send_message:
// SQLite hands Go each list whole, as a BLOB, and Go's reading comes back
// through SQLite's JSON functions unchanged, NULs included.
func TestOwnerRuleReadsEachElementAlikeInGoAndSQL(t *testing.T) {
	store := newTestStore(t)
	exercised := map[string]int{}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		list, elements := ownerRuleList(random, exercised)
		failed := func(format string, args ...any) bool {
			t.Logf("seed %d, list %q: "+format, append([]any{seed, list}, args...)...)
			return false
		}

		// R1.
		var want []ParticipantEntry
		var strs []bool
		if json.Valid([]byte(list)) {
			for _, element := range elements {
				if entry, ok := ownerRuleReference(element); ok {
					want = append(want, entry)
					strs = append(strs, strings.HasPrefix(element, `"`))
				}
			}
		}
		got := ParticipantEntries(list)
		if !sameEntries(got, want) {
			return failed("ParticipantEntries = %#v, want %#v", got, want)
		}

		// R2: the owner's line keys.
		lines := OwnerLines{}
		lineKeys := map[string]bool{}
		addLine := func(entry ParticipantEntry) {
			lines.Add(entry)
			if line, ok := OwnerLine(entry); ok {
				for _, key := range phoneline.LineKeys(line) {
					lineKeys[key] = true
				}
			}
		}
		for _, entry := range got {
			if _, ok := OwnerLine(entry); ok {
				exercised["an owner line"]++
			}
			addLine(entry)
		}
		keyRows, err := store.db.Query(`
			WITH input(list) AS (SELECT ?)
			SELECT line_key.value FROM input, `+sqlOwnerLineKeys("input.list")+` AS line_key
		`, list)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		gotKeys := map[string]bool{}
		for keyRows.Next() {
			var key string
			if err := keyRows.Scan(&key); err != nil {
				t.Fatalf("scan: %v", err)
			}
			gotKeys[key] = true
		}
		keyRows.Close()
		if fmt.Sprint(gotKeys) != fmt.Sprint(lineKeys) {
			return failed("openmessage_owner_line_keys = %v, want %v", gotKeys, lineKeys)
		}

		// R2: the entries, and R3.
		if random.Intn(2) == 0 {
			addLine(ParticipantEntry{Number: "5550009999", IsMe: true})
		}
		keys := make([]string, 0, len(lineKeys))
		for key := range lineKeys {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		keysJSON, _ := json.Marshal(keys)
		rows, err := store.db.Query(`
			WITH input(list) AS (SELECT ?),
			owner_keys(owner_key) AS (SELECT value FROM json_each(?))
			SELECT p.key, p.value, `+sqlEntryIsOwners+`
			FROM input, `+sqlParticipantEntries("input.list")+` AS p
			ORDER BY p.key
		`, list, string(keysJSON))
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var key int
			var value string
			var owner bool
			if err := rows.Scan(&key, &value, &owner); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if key != seen || key >= len(got) {
				return failed("SQL gave entry %d after %d of %d entries", key, seen, len(got))
			}
			seen++
			var row sqlEntryRow
			if err := json.Unmarshal([]byte(value), &row); err != nil {
				return failed("entry %d: %q isn't an entry object: %v", key, value, err)
			}
			entry := got[key]
			if row.Number != entry.Number || row.IsMe != entry.IsMe || !slices.Equal(row.Texts, entry.Texts) ||
				!slices.Equal(row.NumberKeys, phoneline.ValueKeys(entry.Number)) {
				return failed("SQL entry %d = %#v, want %#v with keys %q", key, row, entry, phoneline.ValueKeys(entry.Number))
			}
			if owner != lines.Owner(entry) {
				return failed("sqlEntryIsOwners(entry %d %#v, keys %s) = %v, want %v", key, entry, keysJSON, owner, !owner)
			}
			if strings.ContainsRune(entry.Number, 0) {
				exercised["a number with a NUL"]++
			}
			if !entry.IsMe && owner {
				exercised["an unflagged entry on an owner line"]++
				if strs[key] {
					exercised["a string on an owner line"]++
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if seen != len(got) {
			return failed("SQL gave %d entries, want %d", seen, len(got))
		}
		return true
	}
	// The race job runs every case as well: a case costs a few milliseconds
	// under -race, and fewer cases can miss the rarest paths, such as a list
	// nested deeper than Go reads.
	if err := quick.Check(property, &quick.Config{MaxCount: 400, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"an element that's neither an object nor a string", "a string element",
		"a string that reads as a flagged object", "an escaped key", "a repeated key",
		"a flag that isn't true", "a number of another JSON type", "a list that isn't a JSON array",
		"a NUL in a text", "a number with a NUL", "a member nested deeper than SQLite reads",
		"a list nested deeper than SQLite reads", "a list nested deeper than Go reads", "a list with bytes after a NUL",
		"an owner line", "an unflagged entry on an owner line", "a string on an owner line",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}

// Invariant: over random stores, sqlEntryIsOwners counts an entry as the
// owner's, given the owner_keys that both legacy searches gather from every
// conversation, exactly when OwnerLines, gathered as send_message gathers it
// from every conversation ListConversations returns, says it is. The stores'
// lists hold everything TestOwnerRuleReadsEachElementAlikeInGoAndSQL draws,
// so some flag the owner beside an element nested deeper than SQLite reads,
// and some carry a flag after a NUL, where SQLite's reader stops and Go's
// reads the whole text as no JSON.
func TestOwnerKeysHoldTheLinesSendMessageTakes(t *testing.T) {
	exercised := map[string]int{}
	probes := append(append([]string{"0009999", "000-9999", "+16665550000"}, ownerRuleNumbers...), "Sam Owner")
	probeList, _ := json.Marshal(probes)
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store := newTestStore(t)
		count := 1 + random.Intn(5)
		for index := 0; index < count; index++ {
			list, elements := ownerRuleList(random, exercised)
			seedConversations(t, store, &Conversation{
				ConversationID: "c" + strconv.Itoa(index), Name: "Thread", SourcePlatform: "sms", LastMessageTS: int64(index), Participants: list,
			})
			if cut := strings.IndexByte(list, 0); cut >= 0 && len(ParticipantEntries(list)) == 0 {
				for _, entry := range ParticipantEntries(list[:cut]) {
					if _, ok := OwnerLine(entry); ok {
						exercised["a flag after which SQLite's reader stops at a NUL"]++
					}
				}
			}
			for _, element := range elements {
				if strings.HasPrefix(element, "[[[") && json.Valid([]byte(list)) {
					for _, entry := range ParticipantEntries(list) {
						if _, ok := OwnerLine(entry); ok {
							exercised["a flag beside an element nested deeper than SQLite reads"]++
						}
					}
				}
			}
		}
		conversations, err := store.ListConversations(1 << 30)
		if err != nil {
			t.Fatalf("list conversations: %v", err)
		}
		lines := OwnerLines{}
		for _, conversation := range conversations {
			for _, entry := range ParticipantEntries(conversation.Participants) {
				lines.Add(entry)
			}
		}
		rows, err := store.db.Query(`
			WITH `+sqlOwnerKeys()+`, probe(list) AS (SELECT ?)
			SELECT p.key, `+sqlEntryIsOwners+`
			FROM probe, `+sqlParticipantEntries("probe.list")+` AS p
			ORDER BY p.key
		`, string(probeList))
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var key int
			var owner bool
			if err := rows.Scan(&key, &owner); err != nil {
				t.Fatalf("scan: %v", err)
			}
			seen++
			want := lines.OnLine(probes[key])
			if owner != want {
				for _, conversation := range conversations {
					t.Logf("  %s: %q", conversation.ConversationID, conversation.Participants)
				}
				t.Logf("seed %d: SQL counts %q as the owner's: %v; OwnerLines says %v", seed, probes[key], owner, want)
				return false
			}
			if want {
				exercised["a probe on an owner line"]++
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if seen != len(probes) {
			t.Fatalf("SQL read %d probes, want %d", seen, len(probes))
		}
		return true
	}
	cases := 150
	if raceDetectorEnabled {
		cases = 30
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261104))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"a probe on an owner line", "a flag after which SQLite's reader stops at a NUL",
		"a flag beside an element nested deeper than SQLite reads",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}

// OwnerLines.OnLine costs the same however many lines the set holds: a store
// can flag the owner on many numbers, and a send asks about every entry of
// every conversation.
func TestOwnerLinesOnLineCostsTheSameForAnyNumberOfLines(t *testing.T) {
	one := ownerLinesOf("+15550009999")
	many := OwnerLines{}
	for index := 0; index < 2000; index++ {
		many.Add(ParticipantEntry{Number: fmt.Sprintf("+1666%07d", index), IsMe: true})
	}
	for number, onMany := range map[string]bool{
		"+44 20 7946 0958": false, "+1 666 000 1234": true, "0001234": true, "+1 777 000 1234": false,
	} {
		if got := many.OnLine(number); got != onMany {
			t.Errorf("OnLine(%q) over 2,000 lines = %v, want %v", number, got, onMany)
		}
		oneAllocs := testing.AllocsPerRun(20, func() { one.OnLine(number) })
		manyAllocs := testing.AllocsPerRun(20, func() { many.OnLine(number) })
		if manyAllocs > oneAllocs {
			t.Errorf("OnLine(%q) made %.0f allocations over 2,000 lines and %.0f over one", number, manyAllocs, oneAllocs)
		}
	}
}

// ownerRuleSeparators are runes that phoneline lets sit around and between
// a written number's digits: white space of several kinds, dashes, dots,
// slashes and parentheses.
var ownerRuleSeparators = []rune{' ', ' ', ' ', '\t', '/', '-', '–', '(', ')', '.'}

// Invariants of the owner's phone lines, over the values the owner rule
// meets, each written number with random separators added:
//
//	P1. OwnerLine gives a line exactly for a flagged entry whose number names
//	    one, as phonelinetest restates phoneline's rule: a number as people
//	    write one, or a WhatsApp JID of one. The line is the last 10 of those
//	    digits, and the number sits on it. Every written form of the owner's
//	    number, and each of the owner's JIDs, gives the same line; a value
//	    that only holds the owner's digits gives none.
//	P2. Separators that phoneline allows change nothing: a written number
//	    gives the line it gives without them, and counts as the owner's on
//	    the same lines.
//	P3. The rule counts a flagged entry as the owner's whatever the lines,
//	    and an unflagged entry exactly when its number sits on one of the
//	    lines, as phonelinetest.Match says, so more lines never make an
//	    owner's entry someone else's.
func TestOwnerLinesFollowPhoneline(t *testing.T) {
	for _, form := range append(append(append([]string{}, ownerNumberForms...), ownerNumberSeparatedForms...), ownerJIDs...) {
		if got, ok := OwnerLine(ParticipantEntry{Number: form, IsMe: true}); !ok || got != "5550009999" {
			t.Errorf("OwnerLine(the owner's %q) = %q, %v; want 5550009999", form, got, ok)
		}
	}
	for _, value := range append(append([]string{"105274891234567@lid", "Sam Owner"}, ownerDigitHolders...), ownerNULForms...) {
		if got, ok := OwnerLine(ParticipantEntry{Number: value, IsMe: true}); ok {
			t.Errorf("OwnerLine(flagged %q) = %q; want no line", value, got)
		}
		if ownerLinesOf("5550009999").Owner(ParticipantEntry{Number: value}) {
			t.Errorf("an unflagged %q counts as the owner's on the owner's line", value)
		}
	}
	exercised := map[string]int{}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		number := ownerRuleNumbers[random.Intn(len(ownerRuleNumbers))]
		separated := number
		if _, written := phonelinetest.Number(number); written {
			runes := []rune(number)
			for count := 1 + random.Intn(3); count > 0; count-- {
				at := random.Intn(len(runes) + 1)
				separator := ownerRuleSeparators[random.Intn(len(ownerRuleSeparators))]
				runes = append(runes[:at], append([]rune{separator}, runes[at:]...)...)
			}
			separated = string(runes)
			exercised["a written number with separators added"]++
		}
		var flagged []string
		for count := random.Intn(3); count > 0; count-- {
			flagged = append(flagged, ownerRuleNumbers[random.Intn(len(ownerRuleNumbers))])
		}
		lines := ownerLinesOf(flagged...)
		var ownerLines []string
		for _, value := range flagged {
			if line, ok := OwnerLine(ParticipantEntry{Number: value, IsMe: true}); ok {
				ownerLines = append(ownerLines, line)
			}
		}
		failed := func(format string, args ...any) bool {
			t.Logf("seed %d, number %q, separated %q, lines %v: "+format, append([]any{seed, number, separated, ownerLines}, args...)...)
			return false
		}

		// P1.
		entry := ParticipantEntry{Number: separated, IsMe: random.Intn(2) == 0}
		line, ok := OwnerLine(entry)
		digits, names := phonelinetest.Digits(separated)
		if ok != (entry.IsMe && names) {
			return failed("OwnerLine(%+v) ok = %v", entry, ok)
		}
		if ok {
			exercised["an owner line"]++
			if line != modelLastTen(digits) || !phonelinetest.Match(line, separated) {
				return failed("OwnerLine(%+v) = %q", entry, line)
			}
		}

		// P2.
		plain := ParticipantEntry{Number: number, IsMe: entry.IsMe}
		if plainLine, plainOK := OwnerLine(plain); plainLine != line || plainOK != ok {
			return failed("OwnerLine = %q, %v with separators and %q, %v without", line, ok, plainLine, plainOK)
		}
		if lines.Owner(entry) != lines.Owner(plain) {
			return failed("separators changed whether the entry is the owner's")
		}

		// P3.
		wantOwner := entry.IsMe
		for _, ownerLine := range ownerLines {
			wantOwner = wantOwner || phonelinetest.Match(ownerLine, separated)
		}
		if lines.Owner(entry) != wantOwner {
			return failed("Owner(%+v) = %v, want %v", entry, lines.Owner(entry), wantOwner)
		}
		more := ownerLinesOf(append(flagged, "5550009999")...)
		if lines.Owner(entry) && !more.Owner(entry) {
			return failed("more lines made the owner's entry someone else's")
		}
		if !entry.IsMe && lines.Owner(entry) {
			exercised["an unflagged entry on an owner line"]++
		}
		if !entry.IsMe && !lines.Owner(entry) && more.Owner(entry) {
			exercised["an entry that a further line makes the owner's"]++
		}
		return true
	}
	cases := 1000
	if raceDetectorEnabled {
		cases = 200
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"a written number with separators added", "an owner line",
		"an unflagged entry on an owner line", "an entry that a further line makes the owner's",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}
