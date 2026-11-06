package phoneline

import (
	"database/sql"
	"encoding/json"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/phoneline/phonelinetest"
)

func TestMatch(t *testing.T) {
	for _, test := range []struct {
		line, stored string
		want         bool
	}{
		{"2025550123", "+12025550123", true},
		{"2025550123", "+1 (202) 555-0123", true},
		{"2025550123", "202.555.0123", true},
		{"2025550123", "202/555-0123", true},
		{"2025550123", "+1\u00a0202\u00a0555\u00a00123", true},
		{"2025550123", "+1 202\u2013555\u20130123", true},
		{"2025550123", "5550123", true},
		{"5550123", "+12025550123", true},
		{"5550123", "+447555012399", false},
		{"2025550123", "+12025550124", false},
		{"2025550123", "550123", false},
		{"550123", "550123", false},
		{"2025550123", "", false},
		{"", "+12025550123", false},
		// A WhatsApp JID on the phone-number server names its user's number.
		{"2025550123", "12025550123@s.whatsapp.net", true},
		{"2025550123", "12025550123@c.us", true},
		// Other identifiers can hold a line's digits but name no phone line.
		{"2025550123", "alice2025550123@example.com", false},
		{"2025550123", "2025550123@vtext.com", false},
		{"2025550123", "abcdefab-abcd-abcd-abcd-ab2025550123", false},
		{"2025550123", "12025550123@lid", false},
		{"2025550123", "120363012025550123@g.us", false},
		{"2025550123", "users/12025550123", false},
		{"2025550123", "legacy-participant:12025550123", false},
		{"2025550123", "tel:+12025550123", false},
		{"2025550123", "+1 202 555 0123 ext 4", false},
		{"2025550123", "Alice 2025550123", false},
		{"2025550123", "不存在2025550123", false},
		{"2025550123", "+12+025550123", false},
		{"2025550123", "+12345678902025550123", false},
	} {
		if got := Match(test.line, test.stored); got != test.want {
			t.Errorf("Match(%q, %q) = %v, want %v", test.line, test.stored, got, test.want)
		}
	}
	if got := Line("+44 7555 012399"); got != "7555012399" {
		t.Errorf("Line() = %q, want the last ten digits", got)
	}
	if !MatchFirst("2025550123", " ", "", "+12025550123") || MatchFirst("2025550123", "+15555550199", "+12025550123") {
		t.Error("MatchFirst must test the first value that isn't blank, and only it")
	}
	if MatchFirst("2025550123", "alice2025550123@example.com", "+12025550123") {
		t.Error("MatchFirst must stop at the first value that isn't blank, even one that is no phone number")
	}
}

// Number takes a value as a phone number only when someone wrote it as one,
// and agrees with phonelinetest on every draw. A WhatsApp JID, which a stored
// value may be, is no number a person types.
func TestNumber(t *testing.T) {
	for value, want := range map[string]string{
		"+1 (202) 555-0123": "12025550123", "202.555.0123": "2025550123", "5550123": "5550123",
		"+44 20 7946 0958": "442079460958", "123456789012345": "123456789012345", "(+1) 202/555 0123 ": "12025550123",
		"555012": "", "1234567890123456": "", "+": "", "": "", "+1 202 555 0123 ext 4": "", "+1 202 555 0123#": "",
		"12025550123@s.whatsapp.net": "", "tel:+12025550123": "", "+1+2025550123": "", "1+2025550123": "",
		"２０２５５５０１２３": "",
	} {
		got, ok := Number(value)
		if got != want || ok != (want != "") {
			t.Errorf("Number(%q) = %q, %v, want %q", value, got, ok, want)
		}
	}
	exercised := map[string]int{}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		digits := randomDigits(random, 1+random.Intn(17))
		for _, value := range []string{numberForm(random, digits), identifierForm(random, digits), digits + "@s.whatsapp.net", noiseForm(random), noiseForm(random) + digits} {
			got, ok := Number(value)
			want, wantOK := phonelinetest.Number(value)
			if got != want || ok != wantOK {
				t.Logf("seed %d: Number(%q) = %q, %v; phonelinetest says %q, %v", seed, value, got, ok, want, wantOK)
				return false
			}
			if ok {
				exercised["a number"]++
			} else {
				exercised["something else"]++
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 400, Rand: rand.New(rand.NewSource(20261104))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"a number", "something else"} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s (drew %v)", path, exercised)
		}
	}
}

// numberForm writes digits as people write a phone number: separators of
// every kind between them, white space around them, and an optional +
// before them.
func numberForm(random *rand.Rand, digits string) string {
	var b strings.Builder
	b.WriteString([]string{"", "", "+", " +", "(+", "\u00a0"}[random.Intn(6)])
	for index, r := range digits {
		if index > 0 && random.Intn(4) == 0 {
			b.WriteString([]string{" ", "-", ".", ") ", "/", "\u00a0", "\u2013", "\u2011"}[random.Intn(8)])
		}
		b.WriteRune(r)
	}
	if random.Intn(5) == 0 {
		b.WriteString([]string{" ", ")", "\t"}[random.Intn(3)])
	}
	return b.String()
}

// identifierForm hides digits in an identifier that reads as no phone
// number: an email address, a Signal UUID, a WhatsApp LID or group, a
// Google Chat user, a legacy placeholder, a URI, a number with an
// extension, or a name with a number in it.
func identifierForm(random *rand.Rand, digits string) string {
	switch random.Intn(10) {
	case 0:
		return "alice" + digits + "@example.com"
	case 1:
		return digits + "@vtext.com"
	case 2:
		tail := digits
		if len(tail) > 12 {
			tail = tail[len(tail)-12:]
		}
		return "abcdefab-abcd-abcd-abcd-" + strings.Repeat("a", 12-len(tail)) + tail
	case 3:
		return digits + "@lid"
	case 4:
		return "120363" + digits + "@g.us"
	case 5:
		return "users/" + digits
	case 6:
		return "legacy-participant:" + digits
	case 7:
		return "tel:+" + digits
	case 8:
		return numberForm(random, digits) + " ext " + strconv.Itoa(random.Intn(100))
	}
	return []string{"Mom ", "不存在", "Мама "}[random.Intn(3)] + digits
}

// noiseForm draws a short string from the characters that decide whether a
// value is a written number, so the grammar's edges come up: a + after a
// digit, separators alone, letters, @ and other punctuation.
func noiseForm(random *rand.Rand) string {
	alphabet := []string{"0", "1", "2", "5", "9", "+", " ", "-", ".", "/", "(", ")", "@", "a", "x", ":", "#", "\u00a0", "\u2013", "\u00b7", "s.whatsapp.net", "lid"}
	var b strings.Builder
	for count := random.Intn(20); count > 0; count-- {
		b.WriteString(alphabet[random.Intn(len(alphabet))])
	}
	return b.String()
}

func randomDigits(random *rand.Rand, length int) string {
	var b strings.Builder
	for index := 0; index < length; index++ {
		b.WriteByte(byte('0' + random.Intn(10)))
	}
	return b.String()
}

// LineKeys and ValueKeys index Match: for any line and any value, the two
// share a key exactly when Match(line, value) holds, as phonelinetest also
// says, and neither gives more than ten keys. Lines run from no digits to 16,
// a few with a stray character; values are written forms of numbers that end
// with the line, that the line ends with, or that share only some of its
// last digits, WhatsApp JIDs, identifiers that hold the line's digits, and
// noise.
func TestLineKeysIndexMatch(t *testing.T) {
	exercised := map[string]int{}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		line := randomDigits(random, random.Intn(17))
		if random.Intn(8) == 0 {
			at := random.Intn(len(line) + 1)
			line = line[:at] + []string{"a", "+", " ", "٣"}[random.Intn(4)] + line[at:]
			exercised["a line with a stray character"]++
		}
		tail := line
		if cut := 7 + random.Intn(4); len(tail) > cut {
			tail = tail[len(tail)-cut:]
		}
		values := []string{
			numberForm(random, randomDigits(random, random.Intn(6))+line),
			numberForm(random, tail),
			numberForm(random, randomDigits(random, 1+random.Intn(4))+tail),
			randomDigits(random, random.Intn(4)) + line + []string{"@s.whatsapp.net", "@C.US"}[random.Intn(2)],
			identifierForm(random, line),
			noiseForm(random),
			numberForm(random, randomDigits(random, random.Intn(18))),
		}
		lineKeys := LineKeys(line)
		if len(lineKeys) > 10 {
			t.Logf("seed %d: LineKeys(%q) gives %d keys", seed, line, len(lineKeys))
			return false
		}
		for _, value := range values {
			valueKeys := ValueKeys(value)
			if len(valueKeys) > 10 {
				t.Logf("seed %d: ValueKeys(%q) gives %d keys", seed, value, len(valueKeys))
				return false
			}
			shared := false
			for _, lineKey := range lineKeys {
				for _, valueKey := range valueKeys {
					shared = shared || lineKey == valueKey
				}
			}
			if match, oracle := Match(line, value), phonelinetest.Match(line, value); shared != match || shared != oracle {
				t.Logf("seed %d: LineKeys(%q) = %q and ValueKeys(%q) = %q share a key: %v; Match says %v, phonelinetest %v",
					seed, line, lineKeys, value, valueKeys, shared, match, oracle)
				return false
			}
			if !shared {
				exercised["a value off the line"]++
				continue
			}
			if digits, _ := phonelinetest.Digits(value); strings.HasSuffix(digits, line) {
				exercised["a value whose digits end with the line"]++
			} else {
				exercised["a line that ends with a value's digits"]++
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261104))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"a line with a stray character", "a value off the line",
		"a value whose digits end with the line", "a line that ends with a value's digits",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s (drew %v)", path, exercised)
		}
	}
}

// Properties, for any digits d: a written form of d, with or without a
// country prefix, matches the line of d whenever d has 7 digits or more, and
// so does a WhatsApp JID of d; an identifier that only holds d's digits, such
// as an email address or a Signal UUID, never matches; and the line's digits
// matter only where they end a number, never where they sit inside a longer
// one. Match agrees on every draw with phonelinetest, which states the rule
// with regular expressions, and the SQLite function that both stores call
// agrees with MatchFirst, so a search and a send match the same numbers.
func TestMatchProperties(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	defer conn.Close()
	exercised := map[string]int{}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		national := randomDigits(random, 4+random.Intn(8))
		line := Line(national)
		prefix := []string{"", "1", "44", "353"}[random.Intn(4)]
		written := numberForm(random, prefix+national)
		if got, want := Match(line, written), len(national) >= 7; got != want {
			t.Logf("seed %d: Match(%q, %q) = %v, want %v", seed, line, written, got, want)
			return false
		}
		jid := prefix + national + []string{"@s.whatsapp.net", "@c.us", "@S.WhatsApp.Net"}[random.Intn(3)]
		if got, want := Match(line, jid), len(national) >= 7; got != want {
			t.Logf("seed %d: Match(%q, %q) = %v, want %v", seed, line, jid, got, want)
			return false
		}
		hidden := identifierForm(random, prefix+national)
		if Match(line, hidden) {
			t.Logf("seed %d: Match(%q, %q) holds for an identifier that is no phone number", seed, line, hidden)
			return false
		}
		if len(national) >= 7 {
			exercised["a written form of a line"]++
			exercised["an identifier that holds a line's digits"]++
		}
		inside := numberForm(random, randomDigits(random, 1+random.Intn(3))+national+randomDigits(random, 1+random.Intn(3)))
		if Match(line, inside) && !strings.HasSuffix(Digits(inside), line) && !strings.HasSuffix(line, Digits(inside)) {
			t.Logf("seed %d: Match(%q, %q) holds for digits inside the number", seed, line, inside)
			return false
		}
		// A NUL ends a value early only for code that reads it as a C string.
		cut := written + "\x00" + noiseForm(random)
		for _, value := range []string{written, jid, hidden, inside, cut, numberForm(random, randomDigits(random, random.Intn(18))), noiseForm(random), noiseForm(random) + national} {
			if got, want := Match(line, value), phonelinetest.Match(line, value); got != want {
				t.Logf("seed %d: Match(%q, %q) = %v, phonelinetest says %v", seed, line, value, got, want)
				return false
			}
		}
		values := []any{line}
		strs := []string{}
		for count := 1 + random.Intn(3); count > 0; count-- {
			value := []string{"", "  ", written, jid, hidden, inside, cut, noiseForm(random), numberForm(random, randomDigits(random, random.Intn(14)))}[random.Intn(9)]
			if value == cut {
				exercised["a number that a NUL ends early"]++
			}
			values = append(values, value)
			strs = append(strs, value)
		}
		placeholders := strings.Split(strings.Repeat("?", len(values)), "")
		var fromSQL int64
		if err := conn.QueryRow("SELECT "+SQL(placeholders...), values...).Scan(&fromSQL); err != nil {
			t.Fatalf("query %s: %v", SQLMatch, err)
		}
		if fromGo := MatchFirst(line, strs...); fromGo != (fromSQL == 1) {
			t.Logf("seed %d: %s%v = %d, MatchFirst says %v", seed, SQLMatch, values, fromSQL, fromGo)
			return false
		}
		if fromGo := phonelinetest.MatchFirst(line, strs...); fromGo != (fromSQL == 1) {
			t.Logf("seed %d: %s%v = %d, phonelinetest.MatchFirst says %v", seed, SQLMatch, values, fromSQL, fromGo)
			return false
		}
		if fromSQL == 1 {
			exercised["an SQL match"]++
		} else {
			exercised["an SQL mismatch"]++
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 400, Rand: rand.New(rand.NewSource(20261103))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"a written form of a line", "an identifier that holds a line's digits", "a number that a NUL ends early", "an SQL match", "an SQL mismatch"} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s (drew %v)", path, exercised)
		}
	}
	var none int64
	if err := conn.QueryRow("SELECT "+SQLMatch+"(?, NULL, 15555550123, ?)", "5555550123", "+15555550123").Scan(&none); err != nil || none != 1 {
		t.Errorf("NULL and INTEGER values must count as blank: got %d, %v", none, err)
	}
	var blob int64
	if err := conn.QueryRow("SELECT "+SQLMatch+"(?, CAST(? AS BLOB))", "2025550123", "+12025550123").Scan(&blob); err != nil || blob != 1 {
		t.Errorf("a BLOB must count as its text: got %d, %v", blob, err)
	}
	// The driver hands a Go function a TEXT argument only up to its first
	// NUL, so a bare call reads this value as a number. SQL casts each
	// argument to BLOB, and the matcher then sees every byte.
	var bare, cast int64
	if err := conn.QueryRow("SELECT "+SQLMatch+"(?, ?), "+SQL("?", "?"), "2025550123", "+12025550123\x00x", "2025550123", "+12025550123\x00x").Scan(&bare, &cast); err != nil || bare != 1 || cast != 0 {
		t.Errorf("a number that a NUL ends early: a bare call = %d and SQL = %d, want 1 and 0 (%v)", bare, cast, err)
	}
}

// SQLKeys answers SQLMatch for a whole set of lines at once, through SQLite
// and the SQL each function's helper writes: a tuple of values is on one of the
// set's lines, as SQLMatch says for each line in turn, exactly when a key that
// SQLKeys gives for the tuple is among the keys that KeysOfLines gives for the
// set. Tuples mix written numbers, WhatsApp JIDs, identifiers that hold a
// line's digits, noise, blanks, NULLs, integers and BLOBs, and sets run from no
// lines to six, so the first value that isn't blank decides, as it does for
// SQLMatch, and a value off every line or a set with no lines matches nothing.
// Some lines and values hold a NUL, which the helpers' casts keep, so a value
// or a line never matches through the part before it. SQLKeys gives ValueKeys
// of that value, at most ten, and NULL where it names no line.
func TestSQLKeysAgreesWithSQLMatchOverSetsOfLines(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	defer conn.Close()
	exercised := map[string]int{}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		var lines, digits []string
		for count := random.Intn(7); count > 0; count-- {
			line := Line(randomDigits(random, 5+random.Intn(9)))
			digits = append(digits, line)
			switch random.Intn(8) {
			case 0:
				line += "\x00" + randomDigits(random, 7)
			case 1:
				line = "\x00" + line
			case 2:
				at := 1 + random.Intn(len(line)-1)
				line = line[:at] + "\x00" + line[at:]
			}
			lines = append(lines, line)
		}
		set := KeysOfLines(lines)
		base := randomDigits(random, 7+random.Intn(6))
		if len(digits) > 0 && random.Intn(3) != 0 {
			base = digits[random.Intn(len(digits))]
			if random.Intn(2) == 0 && len(base) > 7 {
				base = base[random.Intn(len(base)-6):]
			}
		}
		prefix := []string{"", "1", "44", "353"}[random.Intn(4)]
		var values []any
		var strs []string
		for count := 1 + random.Intn(3); count > 0; count-- {
			var value any
			switch random.Intn(10) {
			case 0:
				value = nil
				strs = append(strs, "")
				exercised["a NULL"]++
			case 1:
				number := int64(random.Intn(1_000_000_000))
				value = number
				strs = append(strs, strconv.FormatInt(number, 10))
				exercised["an INTEGER"]++
			case 2:
				written := numberForm(random, prefix+base)
				value = []byte(written)
				strs = append(strs, written)
				exercised["a BLOB"]++
			case 3:
				if random.Intn(2) == 0 {
					written := numberForm(random, prefix+base) + "\x00" + noiseForm(random)
					value = written
					strs = append(strs, written)
					break
				}
				blank := []string{"", "  ", "\t"}[random.Intn(3)]
				value = blank
				strs = append(strs, blank)
			case 4:
				jid := prefix + base + []string{"@s.whatsapp.net", "@c.us"}[random.Intn(2)]
				value = jid
				strs = append(strs, jid)
			case 5:
				hidden := identifierForm(random, prefix+base)
				value = hidden
				strs = append(strs, hidden)
			case 6:
				noise := noiseForm(random)
				value = noise
				strs = append(strs, noise)
			default:
				written := numberForm(random, randomDigits(random, random.Intn(3))+base)
				value = written
				strs = append(strs, written)
			}
			values = append(values, value)
		}
		marks := make([]string, len(values))
		cut := make([]string, len(values))
		for index := range values {
			marks[index] = "?"
			cut[index], _, _ = strings.Cut(strs[index], "\x00")
		}
		match, keysOf := SQL(append([]string{"?"}, marks...)...), KeysSQL(marks...)
		want := false
		for _, line := range lines {
			var matched int64
			if err := conn.QueryRow("SELECT "+match, append([]any{line}, values...)...).Scan(&matched); err != nil {
				t.Fatalf("query %s: %v", SQLMatch, err)
			}
			want = want || matched == 1
			if matched == 1 && strings.Contains(line, "\x00") {
				exercised["a match through a line with a NUL"]++
			}
			before, _, hasNUL := strings.Cut(line, "\x00")
			if matched == 0 && hasNUL && MatchFirst(before, strs...) {
				exercised["a line that a NUL keeps off a value"]++
			}
			if matched == 0 && hasNUL && MatchFirst(strings.ReplaceAll(line, "\x00", ""), strs...) {
				exercised["a line whose digits only reach a value without its NUL"]++
			}
			if matched == 0 && MatchFirst(line, cut...) {
				exercised["a value that a NUL keeps off a line"]++
			}
		}
		var got int64
		if err := conn.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM json_each(`+keysOf+`) AS value_key
			WHERE value_key.value IN (SELECT value FROM json_each(?)))`, append(values, set)...).Scan(&got); err != nil {
			t.Fatalf("query %s: %v", SQLKeys, err)
		}
		if (got == 1) != want {
			t.Logf("seed %d: lines %q, values %q: through %s %d, through %s %v", seed, lines, strs, SQLKeys, got, SQLMatch, want)
			return false
		}
		var keys sql.NullString
		if err := conn.QueryRow("SELECT "+keysOf, values...).Scan(&keys); err != nil {
			t.Fatalf("query %s: %v", SQLKeys, err)
		}
		var first string
		for _, value := range strs {
			if strings.TrimSpace(value) != "" {
				first = value
				break
			}
		}
		wantKeys := ValueKeys(first)
		if len(wantKeys) == 0 {
			if keys.Valid {
				t.Logf("seed %d: %s(%q) = %q, want NULL", seed, SQLKeys, strs, keys.String)
				return false
			}
		} else {
			var gotKeys []string
			if !keys.Valid || json.Unmarshal([]byte(keys.String), &gotKeys) != nil || !slices.Equal(gotKeys, wantKeys) || len(gotKeys) > 10 {
				t.Logf("seed %d: %s(%q) = %q, want the %d keys %q", seed, SQLKeys, strs, keys.String, len(wantKeys), wantKeys)
				return false
			}
		}
		switch {
		case len(lines) == 0:
			exercised["a set with no lines"]++
		case want:
			exercised["a tuple on one of the lines"]++
		default:
			exercised["a tuple off every line"]++
		}
		if strings.TrimSpace(strs[0]) == "" && first != "" {
			exercised["a tuple whose first value is blank"]++
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 600, Rand: rand.New(rand.NewSource(20261105))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"a set with no lines", "a tuple on one of the lines", "a tuple off every line",
		"a tuple whose first value is blank", "a NULL", "an INTEGER", "a BLOB",
		"a match through a line with a NUL", "a line that a NUL keeps off a value", "a line whose digits only reach a value without its NUL", "a value that a NUL keeps off a line",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s (drew %v)", path, exercised)
		}
	}
	// The function itself, called without KeysSQL's casts, counts an INTEGER as
	// blank, and sees a TEXT argument only up to its first NUL, which is why
	// KeysSQL casts each argument to BLOB.
	for _, c := range []struct {
		query string
		args  []any
		want  bool
	}{
		{"SELECT " + SQLKeys + "(12025550123)", nil, false},
		{"SELECT " + SQLKeys + "(1.5)", nil, false},
		{"SELECT " + SQLKeys + "(NULL)", nil, false},
		{"SELECT " + SQLKeys + "(?)", []any{"+12025550123\x00x"}, true},
		{"SELECT " + KeysSQL("?"), []any{"+12025550123\x00x"}, false},
	} {
		var keys sql.NullString
		if err := conn.QueryRow(c.query, c.args...).Scan(&keys); err != nil {
			t.Fatalf("%s: %v", c.query, err)
		}
		if keys.Valid != c.want {
			t.Errorf("%s with %q = %q, want keys: %v", c.query, c.args, keys.String, c.want)
		}
	}
}
