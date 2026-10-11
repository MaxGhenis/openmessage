package signaldesktop

import (
	"regexp"
	"strings"

	"github.com/maxghenis/openmessage/internal/v2keys"
)

var (
	e164Pattern = regexp.MustCompile(`^\+[0-9]{6,15}$`)
	aciPattern  = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// NormalizeE164 returns a "+digits" phone number, or "" when value is not one.
func NormalizeE164(value string) string {
	value = strings.TrimSpace(value)
	if e164Pattern.MatchString(value) {
		return value
	}
	return ""
}

// NormalizeACI returns a lowercase ACI UUID, or "" when value is not one. A
// PNI ("PNI:<uuid>") is not an ACI: messages are never authored by a PNI, and
// the live bridge never keys a thread by one.
func NormalizeACI(value string) string {
	value = strings.TrimSpace(value)
	if aciPattern.MatchString(value) {
		return strings.ToLower(value)
	}
	return ""
}

// MessageKey is one stored v2 message: its primary key and its wire identity.
type MessageKey struct {
	MessageID            string
	RemoteConversationID string
	RemoteMessageID      string
}

// Facts is what v2 already holds for the Signal account, read once per scan.
//
// The live bridge keys a contact by phone number when signal-cli knows it and
// by ACI otherwise, and the same thread and message ids follow from that
// choice. Desktop usually knows both, so the feed asks v2 which one the live
// bridge used for each contact (see ChooseAddress), and checks every form a
// message could already be stored under before sending it (HasMessage).
type Facts struct {
	AccountID  string
	SelfE164   string
	identities map[string]bool
	bound      map[string]bool
	messageIDs map[string]bool
	remoteKeys map[string]bool
}

// NewFacts builds the per-scan view. identities holds canonical identity
// values ("+digits" or lowercase ACI); bound holds the remote conversation IDs
// v2 has threads for.
func NewFacts(accountID, selfE164 string, identities, bound []string, messages []MessageKey) *Facts {
	facts := &Facts{
		AccountID:  accountID,
		SelfE164:   NormalizeE164(selfE164),
		identities: make(map[string]bool, len(identities)),
		bound:      make(map[string]bool, len(bound)),
		messageIDs: make(map[string]bool, len(messages)),
		remoteKeys: make(map[string]bool, len(messages)),
	}
	for _, identity := range identities {
		if value := NormalizeE164(identity); value != "" {
			facts.identities[value] = true
		} else if value := NormalizeACI(identity); value != "" {
			facts.identities[value] = true
		}
	}
	for _, remoteID := range bound {
		facts.bound[v2keys.NormalizeRemoteConversationID("signal", remoteID)] = true
	}
	for _, message := range messages {
		facts.messageIDs[message.MessageID] = true
		facts.remoteKeys[remoteKey(message.RemoteConversationID, message.RemoteMessageID)] = true
	}
	return facts
}

func remoteKey(conversationID, messageID string) string {
	return v2keys.NormalizeRemoteConversationID("signal", conversationID) + "\x1f" + strings.TrimSpace(messageID)
}

// ChooseAddress picks the address the live bridge would key this contact by:
// the one a v2 direct thread is bound to; else the one v2 knows as an
// identity when it knows only one; else the phone number when there is one,
// which is the live bridge's own precedence.
func (f *Facts) ChooseAddress(e164, aci string) string {
	switch {
	case e164 != "" && f.bound["signal:"+e164]:
		return e164
	case aci != "" && f.bound["signal:"+aci]:
		return aci
	}
	e164Known := e164 != "" && f.identities[e164]
	aciKnown := aci != "" && f.identities[aci]
	switch {
	case e164Known && !aciKnown:
		return e164
	case aciKnown && !e164Known:
		return aci
	case e164 != "":
		return e164
	}
	return aci
}

// Bound reports whether v2 has a thread for the remote conversation ID.
func (f *Facts) Bound(remoteConversationID string) bool {
	return f.bound[v2keys.NormalizeRemoteConversationID("signal", remoteConversationID)]
}

// HasMessage reports whether v2 holds the message with this wire identity,
// under its derived primary key or under its remote ID.
func (f *Facts) HasMessage(remoteConversationID, remoteMessageID string) bool {
	conversation := v2keys.NormalizeRemoteConversationID("signal", remoteConversationID)
	derived := v2keys.DeriveID("message", f.AccountID, conversation+"\x1f"+strings.TrimSpace(remoteMessageID))
	return f.messageIDs[derived] || f.remoteKeys[remoteKey(conversation, remoteMessageID)]
}
