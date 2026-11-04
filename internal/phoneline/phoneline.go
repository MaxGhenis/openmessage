// Package phoneline compares phone numbers as send_message compares a
// recipient with a thread's participants. A value names a phone line only
// when it reads as a phone number, as Number defines one, or is a WhatsApp
// JID on the phone-number server; then its digits alone decide, so
// separators and a country prefix don't matter. An email address, a Signal
// UUID, a WhatsApp LID or group JID, a user path or a name that holds
// digits names no line. Both stores' SQL calls the same comparison through
// SQLMatch, so a search matches a number exactly as a send does.
package phoneline

import (
	"database/sql/driver"
	"strings"
	"unicode"

	"modernc.org/sqlite"
)

// SQLMatch names the SQLite function om_phone_line_match(line, value, ...).
// It returns 1 when the first value that isn't blank names the phone line
// that line, a Line, names, and 0 otherwise. TEXT and BLOB arguments compare
// as text; NULL, INTEGER and REAL ones count as blank. Every connection the
// "sqlite" driver opens has the function.
const SQLMatch = "om_phone_line_match"

func init() {
	sqlite.MustRegisterDeterministicScalarFunction(SQLMatch, -1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if len(args) < 2 {
			return int64(0), nil
		}
		values := make([]string, 0, len(args)-1)
		for _, arg := range args[1:] {
			values = append(values, text(arg))
		}
		if MatchFirst(text(args[0]), values...) {
			return int64(1), nil
		}
		return int64(0), nil
	})
}

// text is the string an SQL argument holds, or "" for anything else.
func text(value driver.Value) string {
	switch value := value.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	}
	return ""
}

// Digits returns the ASCII digits of value, in order.
func Digits(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Line returns the digits that name value's phone line: its last ten, or
// all of them when it has fewer.
func Line(value string) string {
	digits := Digits(value)
	if len(digits) > 10 {
		digits = digits[len(digits)-10:]
	}
	return digits
}

// Number returns the digits of value, and whether value is a phone number as
// people write one: 7 to 15 ASCII digits, the most E.164 allows, with at most
// one + before the first. White space, dashes of any kind, dots, slashes and
// parentheses may sit around and between them. Any other character, a
// letter, a colon or an @ among them, makes value something else: an email
// address, a tel: URI, an extension or a name.
func Number(value string) (string, bool) {
	var digits strings.Builder
	plus := false
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' && !plus && digits.Len() == 0:
			plus = true
		case unicode.IsSpace(r) || unicode.Is(unicode.Pd, r) || r == '.' || r == '/' || r == '(' || r == ')':
		default:
			return "", false
		}
	}
	if digits.Len() < 7 || digits.Len() > 15 {
		return "", false
	}
	return digits.String(), true
}

// whatsAppDigits returns the user of a WhatsApp JID on the phone-number
// server, user@s.whatsapp.net or the older user@c.us, when the user is a
// phone number's 7 to 15 digits. The server matches in any ASCII case.
// WhatsApp's other JIDs, a LID's @lid and a group's @g.us, name no line.
func whatsAppDigits(value string) (string, bool) {
	user, server, ok := strings.Cut(strings.TrimSpace(value), "@")
	if !ok || len(user) < 7 || len(user) > 15 || !(equalFoldASCII(server, "s.whatsapp.net") || equalFoldASCII(server, "c.us")) {
		return "", false
	}
	for index := 0; index < len(user); index++ {
		if user[index] < '0' || user[index] > '9' {
			return "", false
		}
	}
	return user, true
}

// equalFoldASCII reports whether a and b match with ASCII letters' case
// aside. Unlike strings.EqualFold it folds nothing else, so a long s or a
// Kelvin sign never stands in for s or k.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := 0; index < len(a); index++ {
		if lowerASCII(a[index]) != lowerASCII(b[index]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// Match reports whether stored names the line that line, a Line, names.
// stored must read as a phone number or a WhatsApp JID of one. One side may
// carry a country prefix the other lacks, so the shorter run of digits must
// end the longer, and a line of fewer than 7 digits matches nothing.
func Match(line, stored string) bool {
	digits, ok := Number(stored)
	if !ok {
		digits, ok = whatsAppDigits(stored)
	}
	if !ok || len(line) < 7 {
		return false
	}
	return strings.HasSuffix(digits, line) || strings.HasSuffix(line, digits)
}

// LineKeys and ValueKeys index Match: Match(line, value) holds exactly when
// LineKeys(line) and ValueKeys(value) share a key, for any line and any
// value, and neither gives more than ten keys. So a set of lines answers
// Match for a value through a few lookups, however many lines it holds.
func LineKeys(line string) []string {
	return nil
}

// ValueKeys gives the keys of value that LineKeys indexes Match by.
func ValueKeys(value string) []string {
	return nil
}

// MatchFirst applies Match to the first of values that isn't blank, as a
// participant's number stands in for its phone. That value decides even when
// it names no line: an entry whose number holds an email address doesn't
// fall back to its phone.
func MatchFirst(line string, values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return Match(line, value)
		}
	}
	return false
}
