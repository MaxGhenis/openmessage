// Package phonelinetest states phoneline's rule a second way, for tests that
// check phoneline, and the stores' SQL that calls it, against an independent
// reading: regular expressions in place of phoneline's scan of a value's
// characters. Only tests import it.
package phonelinetest

import (
	"regexp"
	"strings"
)

// space is white space as Unicode defines it, the set strings.TrimSpace
// trims. sep adds what else may sit between a written number's digits: a
// dash of any kind, a dot, a slash or a parenthesis.
const (
	space = `[\p{Z}\t\n\v\f\r\x{85}]`
	sep   = `(?:` + space + `|[\p{Pd}./()])`
)

var (
	// written is a phone number as people write one: digits, separators,
	// and at most one + before the first digit.
	written = regexp.MustCompile(`^` + sep + `*(?:\+` + sep + `*)?[0-9](?:[0-9]|` + sep + `)*$`)
	// whatsApp is a WhatsApp JID on the phone-number server, whose user is
	// the number's digits.
	whatsApp = regexp.MustCompile(`^` + space + `*([0-9]+)@(?:[sS]\.[wW][hH][aA][tT][sS][aA][pP][pP]\.[nN][eE][tT]|[cC]\.[uU][sS])` + space + `*$`)
	blank    = regexp.MustCompile(`^` + space + `*$`)
	digit    = regexp.MustCompile(`[0-9]`)
)

// Digits returns the digits that put value on a phone line, and whether any
// do: a number written as one, or a WhatsApp JID whose user is one, with 7
// to 15 digits.
func Digits(value string) (string, bool) {
	var digits string
	if match := whatsApp.FindStringSubmatch(value); match != nil {
		digits = match[1]
	} else if written.MatchString(value) {
		digits = strings.Join(digit.FindAllString(value, -1), "")
	} else {
		return "", false
	}
	if len(digits) < 7 || len(digits) > 15 {
		return "", false
	}
	return digits, true
}

// Match reports whether value is on the line that line, a phoneline.Line,
// names: value's digits end with the line's, or the line's end with value's.
func Match(line, value string) bool {
	digits, ok := Digits(value)
	if !ok || len(line) < 7 {
		return false
	}
	return strings.HasSuffix(digits, line) || strings.HasSuffix(line, digits)
}

// MatchFirst applies Match to the first of values that isn't blank.
func MatchFirst(line string, values ...string) bool {
	for _, value := range values {
		if !blank.MatchString(value) {
			return Match(line, value)
		}
	}
	return false
}
