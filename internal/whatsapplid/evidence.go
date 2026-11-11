// Package whatsapplid tells a WhatsApp LID from a phone number. Both are
// all-digit user parts ("<digits>@lid" and "<digits>@s.whatsapp.net"), so once
// a LID's digits are stored as "+<digits>" nothing in the digits says which
// they were. The evidence here comes from records that keep the server: the
// whatsmeow session store (its LID map, device row and contacts) and WhatsApp
// Desktop's databases. Every reader opens its database read-only.
package whatsapplid

import (
	"sort"
	"strings"

	watypes "go.mau.fi/whatsmeow/types"
)

// PhoneUser returns the user part of a phone-number JID
// ("<digits>@s.whatsapp.net", with or without a device), and false for
// anything else.
func PhoneUser(raw string) (string, bool) {
	return userOn(raw, watypes.DefaultUserServer)
}

// LIDUser returns the user part of a LID ("<digits>@lid", with or without a
// device), and false for anything else.
func LIDUser(raw string) (string, bool) {
	return userOn(raw, watypes.HiddenUserServer)
}

func userOn(raw, server string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if strings.Count(raw, "@") != 1 {
		return "", false
	}
	jid, err := watypes.ParseJID(raw)
	if err != nil || jid.Server != server || !AllDigits(jid.User) {
		return "", false
	}
	return jid.User, true
}

// phoneDigits reads a phone number written as digits, "+<digits>" or a
// phone-number JID, the forms the mapping tables use.
func phoneDigits(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if user, ok := PhoneUser(raw); ok {
		return user, true
	}
	if digits := strings.TrimPrefix(raw, "+"); AllDigits(digits) {
		return digits, true
	}
	return "", false
}

// explicitPhone reads a phone number written so that it can be nothing else:
// a phone-number JID or "+<digits>".
func explicitPhone(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if user, ok := PhoneUser(raw); ok {
		return user, true
	}
	if digits, ok := strings.CutPrefix(raw, "+"); ok && AllDigits(digits) {
		return digits, true
	}
	return "", false
}

// lidDigits reads a LID written as a LID JID or, where the column holds only
// LIDs (whatsmeow_lid_map.lid, Desktop's ZLID), bare digits.
func lidDigits(raw string, bareIsLID bool) (string, bool) {
	raw = strings.TrimSpace(raw)
	if user, ok := LIDUser(raw); ok {
		return user, true
	}
	if bareIsLID && AllDigits(raw) {
		return raw, true
	}
	return "", false
}

// AllDigits reports whether value is a non-empty run of ASCII digits.
func AllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Evidence records, by user part, what each source says a user is: a LID, a
// phone number, or a LID paired with a phone number. A user part can be
// attested as both; callers decide what that means (the repair refuses it).
type Evidence struct {
	lids   map[string]map[string]struct{}
	phones map[string]map[string]struct{}
	pairs  map[string]map[string]map[string]struct{}
}

// NewEvidence returns empty evidence.
func NewEvidence() *Evidence {
	return &Evidence{
		lids:   map[string]map[string]struct{}{},
		phones: map[string]map[string]struct{}{},
		pairs:  map[string]map[string]map[string]struct{}{},
	}
}

func addSource(set map[string]map[string]struct{}, user, source string) {
	if set[user] == nil {
		set[user] = map[string]struct{}{}
	}
	set[user][source] = struct{}{}
}

// AddLID records that source names user as a LID.
func (e *Evidence) AddLID(user, source string) {
	if AllDigits(user) {
		addSource(e.lids, user, source)
	}
}

// AddPhone records that source names user as a phone number.
func (e *Evidence) AddPhone(user, source string) {
	if AllDigits(user) {
		addSource(e.phones, user, source)
	}
}

// AddPair records that source pairs LID lid with phone number pn, which also
// names each as what it is.
func (e *Evidence) AddPair(lid, pn, source string) {
	if !AllDigits(lid) || !AllDigits(pn) {
		return
	}
	e.AddLID(lid, source)
	e.AddPhone(pn, source)
	if e.pairs[lid] == nil {
		e.pairs[lid] = map[string]map[string]struct{}{}
	}
	addSource(e.pairs[lid], pn, source)
}

// AddJID records what a JID's server says its user is. Other servers (groups,
// newsletters, broadcast lists) are ignored.
func (e *Evidence) AddJID(raw, source string) {
	if user, ok := LIDUser(raw); ok {
		e.AddLID(user, source)
	} else if user, ok := PhoneUser(raw); ok {
		e.AddPhone(user, source)
	}
}

// AddJIDPair records two JIDs a source gives for one person (a chat or member
// JID and its contact identifier). When one is a LID and the other a phone
// number they are paired; otherwise each is recorded on its own. A phone
// number counts only as a phone-number JID or "+<digits>": bare digits in an
// identifier column could be any number, and pairing a LID with one would
// trade one wrong phone number for another.
func (e *Evidence) AddJIDPair(first, second, source string) {
	if lid, ok := LIDUser(first); ok {
		if pn, ok := explicitPhone(second); ok {
			e.AddPair(lid, pn, source)
			return
		}
	}
	if lid, ok := LIDUser(second); ok {
		if pn, ok := explicitPhone(first); ok {
			e.AddPair(lid, pn, source)
			return
		}
	}
	e.AddJID(first, source)
	e.AddJID(second, source)
}

// Merge adds every record in other.
func (e *Evidence) Merge(other *Evidence) {
	if other == nil {
		return
	}
	for user, sources := range other.lids {
		for source := range sources {
			addSource(e.lids, user, source)
		}
	}
	for user, sources := range other.phones {
		for source := range sources {
			addSource(e.phones, user, source)
		}
	}
	for lid, numbers := range other.pairs {
		for pn, sources := range numbers {
			for source := range sources {
				e.AddPair(lid, pn, source)
			}
		}
	}
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// LIDSources returns the sources that name user as a LID, sorted.
func (e *Evidence) LIDSources(user string) []string { return sortedKeys(e.lids[user]) }

// PhoneSources returns the sources that name user as a phone number, sorted.
func (e *Evidence) PhoneSources(user string) []string { return sortedKeys(e.phones[user]) }

// IsLID reports whether any source names user as a LID.
func (e *Evidence) IsLID(user string) bool { return len(e.lids[user]) > 0 }

// IsPhone reports whether any source names user as a phone number.
func (e *Evidence) IsPhone(user string) bool { return len(e.phones[user]) > 0 }

// PhonesForLID returns every phone number a source pairs lid with, sorted.
func (e *Evidence) PhonesForLID(lid string) []string {
	numbers := make([]string, 0, len(e.pairs[lid]))
	for pn := range e.pairs[lid] {
		numbers = append(numbers, pn)
	}
	sort.Strings(numbers)
	return numbers
}

// PhoneForLID returns the phone number lid stands for when the evidence names
// exactly one and nothing also names that number a LID. Sources that disagree,
// or a "number" that is itself attested as a LID, give no number.
func (e *Evidence) PhoneForLID(lid string) (string, bool) {
	numbers := e.PhonesForLID(lid)
	if len(numbers) != 1 || e.IsLID(numbers[0]) {
		return "", false
	}
	return numbers[0], true
}

// Address returns the form a store records a JID by, as the live bridge's
// StoredAddress does: "+<digits>" for a phone-number JID, and for a LID whose
// number the evidence names (PhoneForLID); the LID JID "<digits>@lid" for any
// other LID; the JID itself for a hosted, bot or newsletter sender; and "" for
// a JID that names no sender (a group, a broadcast list).
func (e *Evidence) Address(raw string) string {
	if user, ok := PhoneUser(raw); ok {
		return "+" + user
	}
	lid, ok := LIDUser(raw)
	if !ok {
		jid, err := watypes.ParseJID(strings.TrimSpace(raw))
		if err != nil || strings.Count(raw, "@") != 1 || jid.User == "" {
			return ""
		}
		switch jid.Server {
		case watypes.HostedServer, watypes.HostedLIDServer, watypes.BotServer, watypes.NewsletterServer:
			return jid.ToNonAD().String()
		default:
			return ""
		}
	}
	if e != nil {
		if pn, ok := e.PhoneForLID(lid); ok {
			return "+" + pn
		}
	}
	return lid + "@" + watypes.HiddenUserServer
}
