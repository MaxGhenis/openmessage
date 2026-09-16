// Package sim models the phone's SIM cards as Google Messages exposes them.
//
// A dual-SIM phone shows up in the protocol as two self participants per
// conversation (IsMe=true), each carrying its own SIMPayload and phone number.
// Conversation.DefaultOutgoingID names the self participant the phone would
// send from. Messages do not carry a SIM field of their own: an outgoing
// message's sender participant identifies the SIM it left from, and an
// incoming message is attributed to the thread's SIM. Settings.SIMCards adds
// carrier names and colors, keyed by the same self participant IDs.
package sim

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

// Slot is one SIM card as seen through a conversation's self participants.
type Slot struct {
	// Slot is the phone's 1-based SIM slot (SIMPayload.SIMNumber); 0 if unknown.
	Slot int `json:"slot,omitempty"`
	// Number is the SIM's own phone number.
	Number string `json:"number,omitempty"`
	// ParticipantID is the Google participant ID of the self participant.
	ParticipantID string `json:"participant_id,omitempty"`
	// Carrier is the network name from Settings.SIMCards, when known.
	Carrier string `json:"carrier,omitempty"`
	// IsDefault marks the SIM the phone itself would send from in this thread.
	IsDefault bool `json:"is_default,omitempty"`

	payload *gmproto.SIMPayload
}

// Payload returns the libgm SIMPayload to attach to sends for this SIM.
func (s Slot) Payload() *gmproto.SIMPayload { return s.payload }

// Label renders a short human label such as "SIM 2 (+1 555 010 0001, Play)".
func (s Slot) Label() string {
	var b strings.Builder
	if s.Slot > 0 {
		fmt.Fprintf(&b, "SIM %d", s.Slot)
	} else {
		b.WriteString("SIM")
	}
	var details []string
	if s.Number != "" {
		details = append(details, s.Number)
	}
	if s.Carrier != "" {
		details = append(details, s.Carrier)
	}
	if len(details) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(details, ", "))
	}
	return b.String()
}

// Registry remembers the SIM cards reported by the phone's Settings event so
// slots can be enriched with carrier names. Safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	cards []*gmproto.SIMCard
}

// SetCards replaces the known SIM cards. Nil or empty input is ignored so a
// partial Settings event never wipes what an earlier one reported.
func (r *Registry) SetCards(cards []*gmproto.SIMCard) {
	if r == nil || len(cards) == 0 {
		return
	}
	r.mu.Lock()
	r.cards = cards
	r.mu.Unlock()
}

// Reset forgets the phone's SIM cards, e.g. on unpair, so a re-pair with a
// different phone never shows the previous phone's carriers.
func (r *Registry) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.cards = nil
	r.mu.Unlock()
}

// Cards returns the SIM cards last reported by the phone.
func (r *Registry) Cards() []*gmproto.SIMCard {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cards
}

// Slots renders the registry as slots (phone-wide, not per conversation).
func (r *Registry) Slots() []Slot {
	var out []Slot
	for _, c := range r.Cards() {
		d := c.GetSIMData()
		s := Slot{
			Slot:          int(d.GetSIMPayload().GetSIMNumber()),
			Number:        firstNonEmpty(d.GetFormattedPhoneNumber(), d.GetInternationalPhoneNumber()),
			ParticipantID: c.GetSIMParticipant().GetID(),
			Carrier:       d.GetCarrierName(),
			payload:       d.GetSIMPayload(),
		}
		out = append(out, s)
	}
	sortSlots(out)
	return out
}

func (r *Registry) carrierFor(participantID string, slot int) string {
	for _, c := range r.Cards() {
		if participantID != "" && c.GetSIMParticipant().GetID() == participantID {
			return c.GetSIMData().GetCarrierName()
		}
	}
	if slot > 0 {
		for _, c := range r.Cards() {
			if int(c.GetSIMData().GetSIMPayload().GetSIMNumber()) == slot {
				return c.GetSIMData().GetCarrierName()
			}
		}
	}
	return ""
}

// FromConversation lists the SIMs a conversation can send from: one slot per
// self participant, default first, then by slot number. reg may be nil.
func FromConversation(conv *gmproto.Conversation, reg *Registry) []Slot {
	if conv == nil {
		return nil
	}
	defaultID := conv.GetDefaultOutgoingID()
	var out []Slot
	for _, p := range conv.GetParticipants() {
		if !p.GetIsMe() {
			continue
		}
		s := Slot{
			Slot: int(p.GetSimPayload().GetSIMNumber()),
			// ID.Number only: it doubles as the send participant ID, which the
			// pre-existing send path always took from this field.
			Number:        p.GetID().GetNumber(),
			ParticipantID: p.GetID().GetParticipantID(),
			payload:       p.GetSimPayload(),
		}
		if s.payload == nil {
			s.payload = conv.GetSimCard().GetSIMData().GetSIMPayload()
			if s.Slot == 0 {
				s.Slot = int(s.payload.GetSIMNumber())
			}
		}
		s.IsDefault = defaultID != "" && s.ParticipantID == defaultID
		s.Carrier = reg.carrierFor(s.ParticipantID, s.Slot)
		out = append(out, s)
	}
	// Without a DefaultOutgoingID match, treat the first self participant as
	// the default - that is what the phone falls back to as well.
	if len(out) > 0 && !hasDefault(out) {
		out[0].IsDefault = true
	}
	sortSlots(out)
	return out
}

// Select picks the SIM a send should leave from. selector may be empty (the
// thread's default), a slot number ("1", "2", "sim2"), the SIM's phone number
// a carrier name, or the SIM's phone number - exact digits, or a suffix of at
// least 7 digits so a one-digit typo fails instead of picking a card. It returns the send participant ID in the same form
// existing sends use (the self participant's number field) and the matching
// SIMPayload. With no self participants at all it falls back to the
// conversation-level SIM card, as before.
func Select(conv *gmproto.Conversation, selector string, reg *Registry) (participantID string, payload *gmproto.SIMPayload, chosen *Slot, err error) {
	slots := FromConversation(conv, reg)
	selector = strings.TrimSpace(selector)
	if len(slots) == 0 {
		if selector != "" {
			return "", nil, nil, fmt.Errorf("conversation has no SIM to choose from")
		}
		return "", conv.GetSimCard().GetSIMData().GetSIMPayload(), nil, nil
	}
	var pick *Slot
	if selector == "" {
		for i := range slots {
			if slots[i].IsDefault {
				pick = &slots[i]
				break
			}
		}
		if pick == nil {
			pick = &slots[0]
		}
	} else {
		pick = match(slots, selector)
		if pick == nil {
			return "", nil, nil, fmt.Errorf("no SIM matches %q; available: %s", selector, Describe(slots))
		}
	}
	return pick.Number, pick.payload, pick, nil
}

// minNumberSuffixDigits is the shortest number fragment that may select a
// SIM by suffix: anything shorter (a typo like "3") would silently pick a card
// instead of failing.
const minNumberSuffixDigits = 7

func match(slots []Slot, selector string) *Slot {
	lower := strings.ToLower(selector)
	if n, ok := parseSlotNumber(lower); ok {
		for i := range slots {
			if slots[i].Slot == n {
				return &slots[i]
			}
		}
	}
	for i := range slots {
		if slots[i].ParticipantID != "" && slots[i].ParticipantID == selector {
			return &slots[i]
		}
	}
	// Carrier before number: "O2" must not fall through to a number ending in 2.
	for i := range slots {
		if slots[i].Carrier != "" && strings.EqualFold(slots[i].Carrier, selector) {
			return &slots[i]
		}
	}
	want := digits(selector)
	if want == "" {
		return nil
	}
	for i := range slots {
		if have := digits(slots[i].Number); have != "" && have == want {
			return &slots[i]
		}
	}
	if len(want) >= minNumberSuffixDigits {
		for i := range slots {
			have := digits(slots[i].Number)
			if have != "" && (strings.HasSuffix(have, want) || strings.HasSuffix(want, have)) {
				return &slots[i]
			}
		}
	}
	return nil
}

func parseSlotNumber(s string) (int, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "sim"), " "))
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 10 {
		return n, true
	}
	return 0, false
}

func hasDefault(slots []Slot) bool {
	for _, s := range slots {
		if s.IsDefault {
			return true
		}
	}
	return false
}

func sortSlots(slots []Slot) {
	sort.SliceStable(slots, func(i, j int) bool {
		if slots[i].IsDefault != slots[j].IsDefault {
			return slots[i].IsDefault
		}
		if slots[i].Slot != slots[j].Slot {
			if slots[i].Slot == 0 {
				return false
			}
			if slots[j].Slot == 0 {
				return true
			}
			return slots[i].Slot < slots[j].Slot
		}
		return slots[i].Number < slots[j].Number
	})
}

// Describe lists slots for error messages and tool output, default first.
func Describe(slots []Slot) string {
	parts := make([]string, 0, len(slots))
	for _, s := range slots {
		l := s.Label()
		if s.IsDefault {
			l += " [default]"
		}
		parts = append(parts, l)
	}
	return strings.Join(parts, "; ")
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// StoredParticipant is the subset of the conversation participants JSON the
// store keeps that matters for SIM attribution.
type StoredParticipant struct {
	Number    string `json:"number"`
	IsMe      bool   `json:"is_me,omitempty"`
	ID        string `json:"id,omitempty"`
	Slot      int    `json:"sim,omitempty"`
	IsDefault bool   `json:"sim_default,omitempty"`
}

// SlotsFromParticipantsJSON reconstructs slots from the store's participants
// JSON (written by the Google event handler) for read-side labelling, where no
// live conversation proto is at hand.
func SlotsFromParticipantsJSON(participantsJSON string) []Slot {
	var ps []StoredParticipant
	if err := json.Unmarshal([]byte(participantsJSON), &ps); err != nil {
		return nil
	}
	var out []Slot
	for _, p := range ps {
		if !p.IsMe {
			continue
		}
		out = append(out, Slot{Slot: p.Slot, Number: p.Number, ParticipantID: p.ID, IsDefault: p.IsDefault})
	}
	// Unlike FromConversation, no default is invented here: a stored row
	// without sim_default predates slot recording, and participant order is
	// arbitrary, so guessing would mislabel incoming messages.
	sortSlots(out)
	return out
}

// Attribution says how a message was tied to a SIM.
type Attribution int

const (
	// NotAttributed: single-SIM thread, or nothing reliable to go on.
	NotAttributed Attribution = iota
	// SentFrom: an outgoing message whose sender number is one of the cards.
	SentFrom
	// ThreadDefault: an incoming message; the protocol does not say which
	// card received it, so this is the thread's default card, not a fact
	// about the message.
	ThreadDefault
)

// AttributeMessage says which SIM a stored message belongs to. Outgoing
// messages are matched by sender number. Incoming messages get the thread's
// recorded default (ThreadDefault) - never a guess: with no recorded default
// they stay unattributed. Single-SIM threads are never attributed.
func AttributeMessage(slots []Slot, isFromMe bool, senderNumber string) (Slot, Attribution) {
	if len(slots) < 2 {
		return Slot{}, NotAttributed
	}
	if isFromMe {
		want := digits(senderNumber)
		for _, s := range slots {
			if want != "" && digits(s.Number) == want {
				return s, SentFrom
			}
		}
		return Slot{}, NotAttributed
	}
	for _, s := range slots {
		if s.IsDefault {
			return s, ThreadDefault
		}
	}
	return Slot{}, NotAttributed
}

// MessageLabel renders an attribution for display: the card's label for
// outgoing messages, and "thread default: ..." for incoming ones so readers
// (including agents) do not take it for the receiving card.
func MessageLabel(slot Slot, how Attribution) string {
	switch how {
	case SentFrom:
		return slot.Label()
	case ThreadDefault:
		return "thread default: " + slot.Label()
	default:
		return ""
	}
}

// LabelFor returns the SIM label for one stored message given the thread's
// participants JSON, or "" when the thread is not dual-SIM.
func LabelFor(participantsJSON string, isFromMe bool, senderNumber string) string {
	return MessageLabel(AttributeMessage(SlotsFromParticipantsJSON(participantsJSON), isFromMe, senderNumber))
}

// Labeler labels messages across conversations, caching each thread's slots.
// lookup returns a conversation's participants JSON ("" when unknown). reg,
// when non-nil, fills in slot numbers and carriers by participant ID for rows
// stored before slots were recorded.
type Labeler struct {
	lookup func(conversationID string) string
	reg    *Registry
	cache  map[string][]Slot
}

// NewLabeler builds a Labeler over a participants lookup and an optional
// registry of the phone's SIM cards.
func NewLabeler(lookup func(conversationID string) string, reg *Registry) *Labeler {
	return &Labeler{lookup: lookup, reg: reg, cache: map[string][]Slot{}}
}

// Enrich fills missing slot numbers and carriers from the registry, matching
// by participant ID. Slots the registry does not know are left as they are.
func (r *Registry) Enrich(slots []Slot) {
	if r == nil {
		return
	}
	for i := range slots {
		for _, c := range r.Cards() {
			if slots[i].ParticipantID == "" || c.GetSIMParticipant().GetID() != slots[i].ParticipantID {
				continue
			}
			if slots[i].Slot == 0 {
				slots[i].Slot = int(c.GetSIMData().GetSIMPayload().GetSIMNumber())
			}
			if slots[i].Carrier == "" {
				slots[i].Carrier = c.GetSIMData().GetCarrierName()
			}
			if slots[i].payload == nil {
				slots[i].payload = c.GetSIMData().GetSIMPayload()
			}
		}
	}
	sortSlots(slots)
}

// Label returns the SIM label for a message in conversationID, or "".
func (l *Labeler) Label(conversationID string, isFromMe bool, senderNumber string) string {
	if l == nil || l.lookup == nil || conversationID == "" {
		return ""
	}
	slots, ok := l.cache[conversationID]
	if !ok {
		slots = SlotsFromParticipantsJSON(l.lookup(conversationID))
		l.reg.Enrich(slots)
		l.cache[conversationID] = slots
	}
	return MessageLabel(AttributeMessage(slots, isFromMe, senderNumber))
}

// AnnotateMessages fills Message.SIM for Google messages on dual-SIM threads.
// lookup returns a conversation's stored participants JSON ("" when unknown
// or not a Google thread); reg may be nil. Shared by the MCP tools and the
// HTTP API so both label identically.
func AnnotateMessages(msgs []*db.Message, lookup func(conversationID string) string, reg *Registry) {
	if lookup == nil || len(msgs) == 0 {
		return
	}
	labeler := NewLabeler(lookup, reg)
	for _, m := range msgs {
		if m == nil || (m.SourcePlatform != "" && m.SourcePlatform != "sms") {
			continue
		}
		m.SIM = labeler.Label(m.ConversationID, m.IsFromMe, m.SenderNumber)
	}
}
