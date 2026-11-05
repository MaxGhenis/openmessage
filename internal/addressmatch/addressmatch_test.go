package addressmatch

import (
	"database/sql"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"unicode"
	"unicode/utf8"
)

func TestKey(t *testing.T) {
	cases := []struct {
		value, want string
	}{
		{"support@example.com", "support@example.com"},
		{"  Support@Example.COM\t", "support@example.com"},
		{"\u3000\u00a0Support@Example.com\u2028\u0085", "support@example.com"},
		{"\u1680\u2000\u200a\u202f\u205f55501\r\n\v\f", "55501"},
		// Format characters aren't white space, so they stay.
		{"\u200bsupport@example.com", "\u200bsupport@example.com"},
		{"support@example.com\ufeff", "support@example.com\ufeff"},
		{"\u180esupport@example.com", "\u180esupport@example.com"},
		// Only ASCII letters change case.
		{"\u00c9LISE@EXAMPLE.COM", "\u00c9lise@example.com"},
		{"\u212a@example.com", "\u212a@example.com"},
		// A NUL and bytes that aren't UTF-8 stay as they are.
		{"a\x00B", "a\x00b"},
		{"\xe3\u3000", "\xe3"},
		{"\u00a0\x80ABC", "\x80abc"},
		{" \t\n\u3000", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := Key(c.value); got != c.want {
			t.Errorf("Key(%q) = %q, want %q", c.value, got, c.want)
		}
	}
}

// keyPieces build values that mix white space, characters that only look
// like it, letters of both cases inside and beyond ASCII, a NUL and bytes
// that aren't UTF-8.
var keyPieces = []string{
	" ", "\t", "\n", "\v", "\f", "\r", "\u0085", "\u00a0", "\u1680", "\u2000",
	"\u2007", "\u200a", "\u2028", "\u2029", "\u202f", "\u205f", "\u3000",
	"\u200b", "\u200d", "\u2060", "\ufeff", "\u180e", "\u0084", "\u00a1",
	"a", "B", "z", "Q", "@", ".", "-", "5", "\u00c9", "\u00e9", "\u212a",
	"\x00", "\x80", "\xe3", "\xff",
}

// keyValue strings together up to eight random pieces.
func keyValue(random *rand.Rand) string {
	var b strings.Builder
	for count := random.Intn(9); count > 0; count-- {
		b.WriteString(keyPieces[random.Intn(len(keyPieces))])
	}
	return b.String()
}

// spaceAround puts up to three white-space runes on each side of value.
func spaceAround(random *rand.Rand, value string) string {
	spaces := []string{" ", "\t", "\n", "\u0085", "\u00a0", "\u2028", "\u3000"}
	pad := func() string {
		var b strings.Builder
		for count := random.Intn(4); count > 0; count-- {
			b.WriteString(spaces[random.Intn(len(spaces))])
		}
		return b.String()
	}
	return pad() + value + pad()
}

// flipCase swaps the case of some of value's ASCII letters.
func flipCase(random *rand.Rand, value string) string {
	b := []byte(value)
	for index, c := range b {
		if random.Intn(2) == 0 {
			continue
		}
		switch {
		case 'a' <= c && c <= 'z':
			b[index] = c - 'a' + 'A'
		case 'A' <= c && c <= 'Z':
			b[index] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// sameAddressModel is a reference for whether two values name one address:
// once strings.TrimSpace has trimmed each, they hold the same bytes, except
// that an ASCII letter may stand in for itself in the other case.
func sameAddressModel(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if len(a) != len(b) {
		return false
	}
	for index := 0; index < len(a); index++ {
		x, y := a[index], b[index]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// keyCase is one property-test draw: a value, and another that names the
// same address or a different one.
type keyCase struct {
	Value, Other string
}

func (keyCase) Generate(random *rand.Rand, _ int) reflect.Value {
	value := keyValue(random)
	var other string
	switch random.Intn(4) {
	case 0:
		other = keyValue(random)
	case 1:
		other = spaceAround(random, flipCase(random, value))
	case 2:
		// Change one byte, which may or may not be an ASCII letter's case.
		other = spaceAround(random, value)
		if len(other) > 0 {
			b := []byte(other)
			b[random.Intn(len(b))] ^= byte(1 << random.Intn(8))
			other = string(b)
		}
	default:
		other = spaceAround(random, value) + keyPieces[random.Intn(len(keyPieces))]
	}
	return reflect.ValueOf(keyCase{Value: value, Other: other})
}

func TestKeyNamesOneAddressForValuesThatDifferOnlyInCaseAndWhiteSpaceAround(t *testing.T) {
	var same, different, invalid int
	property := func(c keyCase) bool {
		key, other := Key(c.Value), Key(c.Other)
		if (key == other) != sameAddressModel(c.Value, c.Other) {
			t.Logf("Key(%q) = %q and Key(%q) = %q, but the model says same = %v", c.Value, key, c.Other, other, sameAddressModel(c.Value, c.Other))
			return false
		}
		if key == other {
			same++
		} else {
			different++
		}
		if !utf8.ValidString(c.Value) {
			invalid++
		}
		if Key(key) != key || strings.TrimSpace(key) != key {
			t.Logf("Key(%q) = %q, which Key or strings.TrimSpace would change again", c.Value, key)
			return false
		}
		for index := 0; index < len(key); index++ {
			if 'A' <= key[index] && key[index] <= 'Z' {
				t.Logf("Key(%q) = %q, which holds an upper-case ASCII letter", c.Value, key)
				return false
			}
		}
		random := rand.New(rand.NewSource(int64(len(c.Value))))
		if padded := spaceAround(random, flipCase(random, c.Value)); Key(padded) != key {
			t.Logf("Key(%q) = %q, but Key(%q) = %q", padded, Key(padded), c.Value, key)
			return false
		}
		return true
	}
	config := &quick.Config{MaxCount: 4000, Rand: rand.New(rand.NewSource(20261104))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
	if same == 0 || different == 0 || invalid == 0 {
		t.Fatalf("draws: %d with one key, %d with two, %d with bytes that aren't UTF-8; want each", same, different, invalid)
	}
}

func TestKeyTrimsEachWhiteSpaceRuneAndNothingElse(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) {
			continue
		}
		value := string(r) + "x" + string(r)
		want := value
		if unicode.IsSpace(r) {
			want = "x"
		} else if 'A' <= r && r <= 'Z' {
			want = strings.ToLower(value)
		}
		if got := Key(value); got != want {
			t.Fatalf("Key(%q) = %q, want %q", value, got, want)
		}
	}
}

// openSQLite opens an in-memory database through the "sqlite" driver, whose
// connections all have SQLMatch.
func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSQLMatch(t *testing.T) {
	db := openSQLite(t)
	cases := []struct {
		name string
		sql  string
		args []any
		want int64
	}{
		{"a value with other case and white space around it", SQL("?", "?"), []any{"support@example.com", "\u00a0Support@Example.com\t"}, 1},
		{"the key must be a Key", SQL("?", "?"), []any{"Support@example.com", "support@example.com"}, 0},
		{"any of several values", SQL("?", "NULL", "?", "?"), []any{"55501", "55502", " 55501 "}, 1},
		{"none of several values", SQL("?", "NULL", "?", "?"), []any{"55501", "55502", "555011"}, 0},
		{"a value that holds the key", SQL("?", "?"), []any{"support@example.com", "other-support@example.com"}, 0},
		{"values that differ after a NUL", SQL("?", "?"), []any{"a\x00b@example.com", "a\x00c@example.com"}, 0},
		{"a value that a NUL ends early", SQL("?", "?"), []any{"support@example.com", "support@example.com\x00x"}, 0},
		{"values that match through a NUL", SQL("?", "?"), []any{"a\x00b@example.com", "A\x00B@example.com "}, 1},
		{"a byte that isn't UTF-8", SQL("?", "?"), []any{"\x80abc", "\u00a0\x80ABC"}, 1},
		{"a BLOB reads as its text", SQL("?", "?"), []any{[]byte("support@example.com"), []byte("Support@example.com")}, 1},
		{"an empty key matches nothing", SQL("?", "?"), []any{"", " "}, 0},
		{"a NULL key matches nothing", SQL("NULL", "?"), []any{"support@example.com"}, 0},
		{"a key alone matches nothing", SQL("?"), []any{"support@example.com"}, 0},
		{"no arguments match nothing", SQL(), nil, 0},
		// The function itself, called without SQL's casts.
		{"an INTEGER matches nothing", `om_address_match(?, 55501)`, []any{"55501"}, 0},
		{"a REAL matches nothing", `om_address_match(?, 1.5)`, []any{"1.5"}, 0},
		{"a NULL value matches nothing", `om_address_match(?, NULL)`, []any{"x"}, 0},
		// The driver hands the function a TEXT argument only up to its first
		// NUL, which is why SQL casts each argument to BLOB.
		{"a bare call loses a TEXT argument's bytes after a NUL", `om_address_match(?, ?)`, []any{"a\x00b@example.com", "a\x00c@example.com"}, 1},
	}
	for _, c := range cases {
		var got int64
		if err := db.QueryRow(`SELECT `+c.sql, c.args...).Scan(&got); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: SELECT %s with %q = %d, want %d", c.name, c.sql, c.args, got, c.want)
		}
	}
}

func TestSQLMatchAgreesWithKey(t *testing.T) {
	db := openSQLite(t)
	statement, err := db.Prepare(`SELECT ` + SQL("?", "?") + `, ` + SQL("?", "?"))
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	var matches int
	property := func(c keyCase) bool {
		key := Key(c.Value)
		var text, blob int64
		if err := statement.QueryRow(key, c.Other, []byte(key), []byte(c.Other)).Scan(&text, &blob); err != nil {
			t.Logf("om_address_match(%q, %q): %v", key, c.Other, err)
			return false
		}
		want := int64(0)
		if key != "" && Key(c.Other) == key {
			want = 1
			matches++
		}
		if text != want || blob != want {
			t.Logf("om_address_match(%q, %q) = %d as TEXT and %d as BLOB, want %d", key, c.Other, text, blob, want)
			return false
		}
		return true
	}
	config := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261105))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
	if matches == 0 {
		t.Fatal("no draw matched; want some")
	}
}
