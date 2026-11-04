package db

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"strings"

	"modernc.org/sqlite"

	"github.com/maxghenis/openmessage/internal/phoneline"
)

// Most conversations list the account's owner among their participants, so
// anything that reads participant lists to find a person must skip the
// owner's entries. An entry is an object element, whose number is its
// "number" (or, without one, its "phone") text, or a string element, whose
// number is the string. The owner rule counts an entry as the owner's when
// it is an object that carries is_me or isMe as JSON true, or when its
// number sits on one of the owner's phone lines: the phoneline.Line of a
// flagged entry's number that names one, as phoneline.Match reads a number.
// Google's participant lists often repeat the owner without the flag, and
// the lines catch those copies. send_message reads lists with these
// helpers, and the legacy searches call them from SQL through the functions
// init registers, so all three read every entry alike.

// ParticipantEntry is what the owner rule reads from one object or string
// element of a participants list.
type ParticipantEntry struct {
	// Number is an object's "number" string, trimmed, or else its "phone"
	// string; a value of another JSON type counts as absent. For a string
	// element it is the string, trimmed.
	Number string
	// IsMe holds when an object's "is_me" or "isMe" is JSON true.
	IsMe bool
	// Texts are the texts a metadata search matches: a string element's
	// text, or each "name", "number", "phone" and "email" member of an
	// object that holds a JSON string, in order, a repeated key each time.
	Texts []string
}

// ParticipantEntries reads each object or string element of a participants
// list on its own, so an element of another JSON type, or a field of another
// JSON type, costs only itself. Keys match after JSON unescaping and with
// case: "\u0069s_me" names is_me, and "IS_ME" names nothing. Anything but a
// JSON array holds no entries.
func ParticipantEntries(participants string) []ParticipantEntry {
	var elements []json.RawMessage
	if json.Unmarshal([]byte(participants), &elements) != nil {
		return nil
	}
	entries := make([]ParticipantEntry, 0, len(elements))
	for _, element := range elements {
		if entry, ok := participantEntry(element); ok {
			entries = append(entries, entry)
		}
	}
	return entries
}

// participantEntry reads one element of a participants list; ok is false
// when the element is neither a JSON object nor a JSON string.
func participantEntry(element []byte) (entry ParticipantEntry, ok bool) {
	// JSON null decodes into a string too, so a string must start with a quote.
	if bytes.HasPrefix(bytes.TrimSpace(element), []byte(`"`)) {
		var text string
		if json.Unmarshal(element, &text) != nil {
			return ParticipantEntry{}, false
		}
		return ParticipantEntry{Number: strings.TrimSpace(text)}, true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(element, &fields) != nil || fields == nil {
		return ParticipantEntry{}, false
	}
	entry.Number = jsonString(fields["number"])
	if entry.Number == "" {
		entry.Number = jsonString(fields["phone"])
	}
	entry.IsMe = jsonTrue(fields["is_me"]) || jsonTrue(fields["isMe"])
	return entry, true
}

// jsonString is the string raw holds, trimmed, or "" when raw holds none.
func jsonString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// jsonTrue reports whether raw is the JSON literal true.
func jsonTrue(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "true"
}

// OwnerLine is the phone line an entry gives the owner, the phoneline.Line
// of its number, when the entry carries the owner's flag and its number
// names that line, as phoneline.Match reads a number: a phone number as
// people write one, or a WhatsApp JID of one. ok is false for any other
// entry; a flagged WhatsApp LID, for one, gives no line, and nor does a
// flagged name.
func OwnerLine(entry ParticipantEntry) (line string, ok bool) {
	if !entry.IsMe {
		return "", false
	}
	line = phoneline.Line(entry.Number)
	if !phoneline.Match(line, entry.Number) {
		return "", false
	}
	return line, true
}

// OwnerLines is a set of the owner's phone lines, as OwnerLine gives them.
type OwnerLines map[string]bool

// Add puts the line entry gives the owner, if any, in the set.
func (lines OwnerLines) Add(entry ParticipantEntry) {
	if line, ok := OwnerLine(entry); ok {
		lines[line] = true
	}
}

// OnLine reports whether number sits on one of the lines, as phoneline.Match
// says.
func (lines OwnerLines) OnLine(number string) bool {
	for line := range lines {
		if phoneline.Match(line, number) {
			return true
		}
	}
	return false
}

// Owner reports whether entry is the owner's: it carries the flag, or its
// number sits on one of the lines.
func (lines OwnerLines) Owner(entry ParticipantEntry) bool {
	return entry.IsMe || lines.OnLine(entry.Number)
}

// init registers the owner rule as SQL functions over a json_each row of a
// participants list:
//
//	openmessage_owner_line(type, value) returns the line OwnerLine gives the
//	element, or NULL.
//	openmessage_owner_entry(type, value, lines) returns 1 when the rule counts
//	the element as the owner's, given the owner's lines with commas between
//	them, and 0 otherwise.
//
// Both read a row by its type: an 'object' row's value as the object's
// JSON, and a 'text' row's value as the string itself, since json_each gives
// a string element's bare text, which could read as an object. Rows of other
// types hold no entry. Neither fails on any input, so SQLite may evaluate
// them in any order.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("openmessage_owner_line", 2, sqlOwnerLine)
	sqlite.MustRegisterDeterministicScalarFunction("openmessage_owner_entry", 3, sqlOwnerEntry)
}

// sqlEntry reads a json_each row as ParticipantEntries reads the element
// behind it: an 'object' row through its JSON, a 'text' row as its string.
func sqlEntry(elementType, value driver.Value) (ParticipantEntry, bool) {
	var text string
	switch value := value.(type) {
	case string:
		text = value
	case []byte:
		text = string(value)
	default:
		return ParticipantEntry{}, false
	}
	switch elementType {
	case "object":
		return participantEntry([]byte(text))
	case "text":
		return ParticipantEntry{Number: strings.TrimSpace(text)}, true
	}
	return ParticipantEntry{}, false
}

func sqlOwnerLine(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	entry, ok := sqlEntry(args[0], args[1])
	if !ok {
		return nil, nil
	}
	if line, ok := OwnerLine(entry); ok {
		return line, nil
	}
	return nil, nil
}

func sqlOwnerEntry(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	entry, ok := sqlEntry(args[0], args[1])
	if !ok {
		return false, nil
	}
	lines := OwnerLines{}
	if joined, isText := args[2].(string); isText {
		for _, line := range strings.Split(joined, ",") {
			lines[line] = true
		}
	}
	return lines.Owner(entry), nil
}
