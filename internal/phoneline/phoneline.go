// Package phoneline compares phone numbers as send_message compares a
// recipient with a thread's participants: by their digits alone, so
// separators and a country prefix don't matter. Both stores' SQL calls the
// same comparison through SQLMatch, so a search matches a number exactly as
// a send does.
package phoneline

import (
	"database/sql/driver"
	"strings"

	"modernc.org/sqlite"
)

// SQLMatch names the SQLite function om_phone_line_match(line, value, ...).
// It returns 1 when the first value that isn't blank names the phone line
// that line, a Line, names, and 0 otherwise. A value that isn't text counts
// as blank. Every connection the "sqlite" driver opens has the function.
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

// Match reports whether stored names the line that line, a Line, names. One
// side may carry a country prefix the other lacks, so the shorter run of
// digits must end the longer, and a run of fewer than 7 digits matches
// nothing.
func Match(line, stored string) bool {
	storedDigits := Digits(stored)
	if len(storedDigits) < 7 || len(line) < 7 {
		return false
	}
	return strings.HasSuffix(storedDigits, line) || strings.HasSuffix(line, storedDigits)
}

// MatchFirst applies Match to the first of values that isn't blank, as a
// participant's number stands in for its phone.
func MatchFirst(line string, values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return Match(line, value)
		}
	}
	return false
}
