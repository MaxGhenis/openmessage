// Package addressmatch compares the values that the route tool looks up as
// whole addresses: email addresses, SMS short codes, Signal UUIDs and other
// values that name someone without naming a phone line. Two values name one
// address when their keys match, as Key writes them, so a card and a thread
// that write an address with its ASCII letters in other case, or with white
// space around it, still meet. Both stores' SQL calls the same comparison
// through SQLMatch, so the route tool looks up two values once exactly when
// the stores can't tell them apart.
package addressmatch

import (
	"database/sql/driver"
	"strings"

	"modernc.org/sqlite"
)

// SQLMatch names the SQLite function om_address_match(key, value, ...). It
// returns 1 when key isn't empty and the Key of some value equals it, and 0
// otherwise. TEXT and BLOB arguments compare as text; NULL, INTEGER and REAL
// values match nothing. Every connection the "sqlite" driver opens has the
// function. Call it through SQL, which keeps every byte of each argument.
const SQLMatch = "om_address_match"

// SQL returns an SQL expression that calls SQLMatch on args: SQL expressions
// for the key and then the values. It casts each argument to BLOB, which
// SQLMatch reads as text. The "sqlite" driver hands a Go function a TEXT
// argument only up to its first NUL, so without the cast, values that differ
// only after a NUL would match. CAST also turns an INTEGER or REAL into its
// text, so each argument must hold TEXT, a BLOB or NULL, as a column with
// TEXT affinity does.
func SQL(args ...string) string {
	cast := make([]string, len(args))
	for index, arg := range args {
		cast[index] = "CAST(" + arg + " AS BLOB)"
	}
	return SQLMatch + "(" + strings.Join(cast, ", ") + ")"
}

func init() {
	sqlite.MustRegisterDeterministicScalarFunction(SQLMatch, -1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if len(args) < 2 {
			return int64(0), nil
		}
		key, ok := text(args[0])
		if !ok || key == "" {
			return int64(0), nil
		}
		for _, arg := range args[1:] {
			if value, ok := text(arg); ok && Key(value) == key {
				return int64(1), nil
			}
		}
		return int64(0), nil
	})
}

// text returns the string an SQL argument holds, and false for a NULL,
// INTEGER or REAL one.
func text(value driver.Value) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case []byte:
		return string(value), true
	}
	return "", false
}

// Key returns value without the white space around it, as strings.TrimSpace
// trims it, and with its ASCII letters lowered. It keeps every other byte, a
// NUL, a letter beyond ASCII and a byte that isn't UTF-8 among them, so two
// keys match only when they hold the same bytes. SQLite can't compare values
// this way by itself: NOCASE stops comparing at a NUL, and trim() with every
// white-space rune takes about 20 times as long per row as this function.
func Key(value string) string {
	b := []byte(strings.TrimSpace(value))
	for index, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[index] = c + 'a' - 'A'
		}
	}
	return string(b)
}
