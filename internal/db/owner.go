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
// case: "\u0069s_me" names is_me, and "IS_ME" names nothing. Where a key
// repeats, its last value counts, as when encoding/json decodes an object
// into a map, but Texts keeps every text. Anything but a JSON array, as
// encoding/json reads JSON, holds no entries.
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
	element = bytes.TrimSpace(element)
	switch {
	// JSON null decodes into a string too, so a string must start with a quote.
	case bytes.HasPrefix(element, []byte(`"`)):
		var text string
		if json.Unmarshal(element, &text) != nil {
			return ParticipantEntry{}, false
		}
		return ParticipantEntry{Number: strings.TrimSpace(text), Texts: []string{text}}, true
	case bytes.HasPrefix(element, []byte(`{`)):
		return objectEntry(element)
	}
	return ParticipantEntry{}, false
}

// objectEntry reads an object element's members in order.
func objectEntry(object []byte) (entry ParticipantEntry, ok bool) {
	decoder := json.NewDecoder(bytes.NewReader(object))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return ParticipantEntry{}, false
	}
	var number, phone string
	var isMe, isMeCamel bool
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return ParticipantEntry{}, false
		}
		key, _ := token.(string)
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return ParticipantEntry{}, false
		}
		switch key {
		case "name", "email":
			entry.Texts = appendText(entry.Texts, value)
		case "number":
			entry.Texts = appendText(entry.Texts, value)
			number = jsonString(value)
		case "phone":
			entry.Texts = appendText(entry.Texts, value)
			phone = jsonString(value)
		case "is_me":
			isMe = jsonTrue(value)
		case "isMe":
			isMeCamel = jsonTrue(value)
		}
	}
	entry.Number = number
	if entry.Number == "" {
		entry.Number = phone
	}
	entry.IsMe = isMe || isMeCamel
	return entry, true
}

// appendText appends the string raw holds, as it is, when raw holds one.
func appendText(texts []string, raw json.RawMessage) []string {
	var text string
	if !bytes.HasPrefix(raw, []byte(`"`)) || json.Unmarshal(raw, &text) != nil {
		return texts
	}
	return append(texts, text)
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
// It holds the phoneline.LineKeys of each line, so OnLine answers through a
// few lookups however many lines the set holds. The zero value is an empty
// set.
type OwnerLines struct {
	keys map[string]bool
}

// Add puts the line entry gives the owner, if any, in the set.
func (lines *OwnerLines) Add(entry ParticipantEntry) {
	line, ok := OwnerLine(entry)
	if !ok {
		return
	}
	if lines.keys == nil {
		lines.keys = map[string]bool{}
	}
	for _, key := range phoneline.LineKeys(line) {
		lines.keys[key] = true
	}
}

// OnLine reports whether number sits on one of the lines, as phoneline.Match
// says.
func (lines OwnerLines) OnLine(number string) bool {
	for _, key := range phoneline.ValueKeys(number) {
		if lines.keys[key] {
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

// init registers the SQL functions through which the legacy searches read
// participant lists, so that SQLite's own JSON reader never reads one: it
// stops at a NUL and gives up past a depth of 1000, where encoding/json
// reads on. Each function takes a list whole, as a BLOB, since the driver
// cuts a TEXT argument at its first NUL; reads it as ParticipantEntries
// does; returns JSON text that SQLite's JSON functions read back, with every
// NUL escaped; and never returns an error, whatever the argument:
//
//	openmessage_participant_entries(list) returns an array that holds, for
//	each entry ParticipantEntries gives, in order, an object with its
//	"is_me" flag, its "number", its number's phoneline.ValueKeys as
//	"number_keys", and its "texts".
//	openmessage_owner_line_keys(list) returns an array of the
//	phoneline.LineKeys of each line OwnerLine gives an entry of list.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("openmessage_participant_entries", 1, participantEntriesJSON)
	sqlite.MustRegisterDeterministicScalarFunction("openmessage_owner_line_keys", 1, ownerLineKeysJSON)
}

// sqlEntry is an entry as openmessage_participant_entries writes it.
type sqlEntry struct {
	IsMe       bool     `json:"is_me"`
	Number     string   `json:"number"`
	NumberKeys []string `json:"number_keys"`
	Texts      []string `json:"texts"`
}

func participantEntriesJSON(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	entries := ParticipantEntries(sqlList(args[0]))
	rows := make([]sqlEntry, len(entries))
	for index, entry := range entries {
		rows[index] = sqlEntry{
			IsMe:       entry.IsMe,
			Number:     entry.Number,
			NumberKeys: append([]string{}, phoneline.ValueKeys(entry.Number)...),
			Texts:      append([]string{}, entry.Texts...),
		}
	}
	return jsonText(rows)
}

func ownerLineKeysJSON(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	keys := []string{}
	seen := map[string]bool{}
	for _, entry := range ParticipantEntries(sqlList(args[0])) {
		line, ok := OwnerLine(entry)
		if !ok {
			continue
		}
		for _, key := range phoneline.LineKeys(line) {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	return jsonText(keys)
}

// sqlList is the list an SQL argument holds: a BLOB's bytes, a TEXT's text
// as the driver passes it, or "" for anything else.
func sqlList(value driver.Value) string {
	switch value := value.(type) {
	case []byte:
		return string(value)
	case string:
		return value
	}
	return ""
}

// jsonText returns value's JSON as TEXT: SQLite's JSON functions read a
// BLOB as JSONB.
func jsonText(value any) (driver.Value, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil
}
