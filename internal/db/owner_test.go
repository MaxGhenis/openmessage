package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf16"

	"github.com/maxghenis/openmessage/internal/phoneline/phonelinetest"
)

// ownerRuleNumbers are values the owner rule meets in "number" and "phone":
// the owner's number in many written forms, the owner's WhatsApp JIDs,
// values that hold the owner's digits but name no line, a LID, other
// people's numbers, fragments too short for a line, and blanks.
var ownerRuleNumbers = append(append(append(append([]string{
	"105274891234567@lid", "+15555550123", "5555550123",
	"+44 20 7946 0958", "12345", "555-0199", "", "  ", " ",
}, ownerNumberForms...), ownerNumberSeparatedForms...), ownerJIDs...), ownerDigitHolders...)

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

// ownerRuleElement is the JSON text of one random element of a participants
// list, with the traits it drew. Most elements are objects whose fields can
// repeat a key, escape a character of a key, hold another JSON type than the
// rule reads, or sit between extra whitespace. Some are strings: a name, a
// number in any written form, or text that reads as a flagged object. The
// rest are neither objects nor strings.
func ownerRuleElement(random *rand.Rand) (string, []string) {
	switch random.Intn(10) {
	case 0:
		odd := []string{`17`, `null`, `true`, `[{"is_me":true,"number":"+15550009999"}]`, `{}`}
		return odd[random.Intn(len(odd))], []string{"an element that's neither an object nor a string"}
	case 1:
		traits := []string{"a string element"}
		switch random.Intn(3) {
		case 0:
			return `"{\"is_me\":true,\"number\":\"+15550009999\"}"`, append(traits, "a string that reads as a flagged object")
		case 1:
			return `"Sam Owner"`, traits
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
		case "name":
			return []string{`"Sam Owner"`, `"Alice Example"`, `["Sam"]`, `""`}[random.Intn(4)]
		case "is_me", "isMe":
			if random.Intn(5) < 3 {
				return "true"
			}
			traits = append(traits, "a flag that isn't true")
			return []string{"false", `"true"`, "1", "null"}[random.Intn(4)]
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
	}{{"name", 70}, {"number", 70}, {"phone", 30}, {"is_me", 40}, {"isMe", 15}} {
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
// or, now and then, text that isn't a JSON array, which holds none.
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
	return "[" + strings.Join(elements, ",") + "]", elements
}

// ownerRuleReference reads one element apart from owner.go: through a
// generic decode, a string as its trimmed text, and an object through the
// first non-blank string of "number" and "phone", with a flag only where
// "is_me" or "isMe" decodes to true. ok is false when the element is neither
// an object nor a string.
func ownerRuleReference(element string) (entry ParticipantEntry, ok bool) {
	var value any
	if json.Unmarshal([]byte(element), &value) != nil {
		return ParticipantEntry{}, false
	}
	if text, isText := value.(string); isText {
		return ParticipantEntry{Number: strings.TrimSpace(text)}, true
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
	return entry, true
}

// Invariants of the owner rule's readers, over random participants lists
// whose elements can repeat or escape keys, hold fields of other JSON types,
// carry extra whitespace, be strings, or be neither objects nor strings:
//
//	R1. ParticipantEntries gives, in order, one entry per object or string
//	    element, each as a reference decode of that element alone reads it,
//	    so an odd element changes no other entry. Text that isn't a JSON
//	    array holds no entries.
//	R2. Over json_each, as the legacy searches read a list,
//	    openmessage_owner_line gives each element the line OwnerLine gives
//	    it, and openmessage_owner_entry agrees with OwnerLines.Owner, for
//	    the list's own lines and for a line from another conversation. A
//	    string never gives a line but counts as the owner's on an owner line,
//	    even when its text reads as a flagged object. An element that is
//	    neither an object nor a string gives no line and never counts as the
//	    owner's.
//
// R2 is the differential that ties the searches to send_message: SQLite
// hands the functions its own rendering of each element, and the Go rule
// must read it as it reads the stored text.
func TestOwnerRuleReadsEachElementAlikeInGoAndSQL(t *testing.T) {
	store := newTestStore(t)
	exercised := map[string]int{}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		list, elements := ownerRuleList(random, exercised)
		failed := func(format string, args ...any) bool {
			t.Logf("seed %d, list %s: "+format, append([]any{seed, list}, args...)...)
			return false
		}

		// R1.
		var want []ParticipantEntry
		for _, element := range elements {
			if entry, ok := ownerRuleReference(element); ok {
				want = append(want, entry)
			}
		}
		got := ParticipantEntries(list)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			return failed("ParticipantEntries = %+v, want %+v", got, want)
		}

		// R2.
		lines := OwnerLines{}
		for _, entry := range got {
			lines.Add(entry)
		}
		if len(lines) > 0 {
			exercised["an owner line"]++
		}
		if random.Intn(2) == 0 {
			lines["5550009999"] = true
		}
		var joined any
		if len(lines) > 0 {
			sorted := make([]string, 0, len(lines))
			for line := range lines {
				sorted = append(sorted, line)
			}
			sort.Strings(sorted)
			joined = strings.Join(sorted, ",")
		}
		rows, err := store.db.Query(`
			WITH input(list) AS (SELECT ?)
			SELECT p.key, openmessage_owner_line(p.type, p.value), openmessage_owner_entry(p.type, p.value, ?)
			FROM input, `+sqlParticipantEntries("input.list")+` AS p
			ORDER BY p.key
		`, list, joined)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var key int
			var line sql.NullString
			var owner bool
			if err := rows.Scan(&key, &line, &owner); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if key != seen || key >= len(elements) {
				return failed("json_each gave element %d after %d of %d elements", key, seen, len(elements))
			}
			seen++
			entry, isEntry := ownerRuleReference(elements[key])
			wantLine, wantLineOK := OwnerLine(entry)
			if !isEntry {
				wantLine, wantLineOK = "", false
			}
			if line.Valid != wantLineOK || line.String != wantLine {
				return failed("openmessage_owner_line(element %d %s) = %v, want %q (%v)", key, elements[key], line, wantLine, wantLineOK)
			}
			wantOwner := isEntry && lines.Owner(entry)
			if owner != wantOwner {
				return failed("openmessage_owner_entry(element %d %s, %v) = %v, want %v", key, elements[key], joined, owner, wantOwner)
			}
			if isEntry && !entry.IsMe && wantOwner {
				exercised["an unflagged entry on an owner line"]++
				if strings.HasPrefix(elements[key], `"`) {
					exercised["a string on an owner line"]++
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if seen != len(elements) {
			return failed("json_each gave %d rows, want %d", seen, len(elements))
		}
		return true
	}
	cases := 400
	if raceDetectorEnabled {
		cases = 80
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"an element that's neither an object nor a string", "a string element",
		"a string that reads as a flagged object", "an escaped key", "a repeated key",
		"a flag that isn't true", "a number of another JSON type", "a list that isn't a JSON array",
		"an owner line", "an unflagged entry on an owner line", "a string on an owner line",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s; raise MaxCount (drew %v)", path, exercised)
		}
	}
}

// ownerRuleSeparators are runes that phoneline lets sit around and between
// a written number's digits: white space of several kinds, dashes, dots,
// slashes and parentheses.
var ownerRuleSeparators = []rune{' ', '\u00a0', '\u202f', '\t', '/', '-', '\u2013', '(', ')', '.'}

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
	for _, value := range append([]string{"105274891234567@lid", "Sam Owner"}, ownerDigitHolders...) {
		if got, ok := OwnerLine(ParticipantEntry{Number: value, IsMe: true}); ok {
			t.Errorf("OwnerLine(flagged %q) = %q; want no line", value, got)
		}
		if (OwnerLines{"5550009999": true}).Owner(ParticipantEntry{Number: value}) {
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
		lines := OwnerLines{}
		for count := random.Intn(3); count > 0; count-- {
			lines.Add(ParticipantEntry{Number: ownerRuleNumbers[random.Intn(len(ownerRuleNumbers))], IsMe: true})
		}
		failed := func(format string, args ...any) bool {
			t.Logf("seed %d, number %q, separated %q, lines %v: "+format, append([]any{seed, number, separated, lines}, args...)...)
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
		for ownerLine := range lines {
			wantOwner = wantOwner || phonelinetest.Match(ownerLine, separated)
		}
		if lines.Owner(entry) != wantOwner {
			return failed("Owner(%+v) = %v, want %v", entry, lines.Owner(entry), wantOwner)
		}
		more := OwnerLines{"5550009999": true}
		for ownerLine := range lines {
			more[ownerLine] = true
		}
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
