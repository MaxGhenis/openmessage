package phoneline

import (
	"database/sql"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
)

func TestMatch(t *testing.T) {
	for _, test := range []struct {
		line, stored string
		want         bool
	}{
		{"2025550123", "+12025550123", true},
		{"2025550123", "+1 (202) 555-0123", true},
		{"2025550123", "202.555.0123", true},
		{"2025550123", "5550123", true},
		{"5550123", "+12025550123", true},
		{"5550123", "+447555012399", false},
		{"2025550123", "+12025550124", false},
		{"2025550123", "550123", false},
		{"550123", "550123", false},
		{"2025550123", "", false},
		{"", "+12025550123", false},
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
}

// numberForm writes digits as people write numbers: with separators, an
// optional +, spaces around it, and characters that aren't digits.
func numberForm(random *rand.Rand, digits string) string {
	var b strings.Builder
	if random.Intn(2) == 0 {
		b.WriteString([]string{"+", " +", "(", "tel:"}[random.Intn(4)])
	}
	for index, r := range digits {
		if index > 0 && random.Intn(4) == 0 {
			b.WriteString([]string{" ", "-", ".", ") ", "/", " "}[random.Intn(6)])
		}
		b.WriteRune(r)
	}
	if random.Intn(5) == 0 {
		b.WriteString([]string{" ext", " ", ";"}[random.Intn(3)])
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

// Properties, for any digits d and any way of writing them: a written form
// of d, with or without a country prefix, matches the line of d whenever d
// has 7 digits or more, and the line's digits matter only where they end a
// number, never where they sit inside a longer one. The SQLite function
// that both stores call agrees with MatchFirst on every draw, so a search
// and a send match the same numbers.
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
		if len(national) >= 7 {
			exercised["a written form of a line"]++
		}
		inside := numberForm(random, randomDigits(random, 1+random.Intn(3))+national+randomDigits(random, 1+random.Intn(3)))
		if Match(line, inside) && !strings.HasSuffix(Digits(inside), line) && !strings.HasSuffix(line, Digits(inside)) {
			t.Logf("seed %d: Match(%q, %q) holds for digits inside the number", seed, line, inside)
			return false
		}
		values := []any{line}
		strs := []string{}
		for count := 1 + random.Intn(3); count > 0; count-- {
			value := []string{"", "  ", written, inside, numberForm(random, randomDigits(random, random.Intn(14)))}[random.Intn(5)]
			values = append(values, value)
			strs = append(strs, value)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(values)), ", ")
		var fromSQL int64
		if err := conn.QueryRow("SELECT "+SQLMatch+"("+placeholders+")", values...).Scan(&fromSQL); err != nil {
			t.Fatalf("query %s: %v", SQLMatch, err)
		}
		if fromGo := MatchFirst(line, strs...); fromGo != (fromSQL == 1) {
			t.Logf("seed %d: %s%v = %d, MatchFirst says %v", seed, SQLMatch, values, fromSQL, fromGo)
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
	for _, path := range []string{"a written form of a line", "an SQL match", "an SQL mismatch"} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s (drew %v)", path, exercised)
		}
	}
	var none int64
	if err := conn.QueryRow("SELECT "+SQLMatch+"(?, NULL, 15555550123, ?)", "5555550123", "+15555550123").Scan(&none); err != nil || none != 1 {
		t.Errorf("non-text values must count as blank: got %d, %v", none, err)
	}
}
