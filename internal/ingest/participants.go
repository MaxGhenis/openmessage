package ingest

import (
	"strings"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// ensureDirectPeerParticipant links an already-resolved remote identity to a
// direct conversation. Conversations minted from message frames (the only
// shape Signal 1:1 threads ever take — the Signal decoder emits no
// ConversationEvent for them) otherwise carry zero participants, which makes
// them render nameless and participant-less on every read surface (#155).
func (w *Worker) ensureDirectPeerParticipant(
	conversation sqlite.Conversation,
	identity sqlite.Identity,
) error {
	if conversation.Kind != sqlite.ConversationKindDirect {
		return nil
	}
	if identity.IsSelf || strings.TrimSpace(identity.IdentityID) == "" {
		return nil
	}
	_, err := w.store.EnsureConversationParticipant(sqlite.ConversationParticipant{
		AccountID:      conversation.AccountID,
		ConversationID: conversation.ConversationID,
		IdentityID:     identity.IdentityID,
		Role:           sqlite.ParticipantRoleMember,
		DisplayName:    identity.DisplayName,
		IsActive:       true,
	})
	return err
}

// ensureDirectPeerFromRemoteID recovers the peer of a direct conversation from
// its remote conversation ID when the message itself carries no usable sender
// (every outgoing message, and inbound frames with an unresolvable sender).
// Only address-shaped remote IDs yield a peer; opaque thread ids (Google
// Messages) are skipped rather than guessed.
func (w *Worker) ensureDirectPeerFromRemoteID(
	accountID string,
	platform bridge.Platform,
	conversation sqlite.Conversation,
	remoteConversationID string,
) error {
	if conversation.Kind != sqlite.ConversationKindDirect {
		return nil
	}
	address := directPeerAddress(platform, remoteConversationID)
	if address == "" {
		return nil
	}
	identity, err := w.resolveIdentity(accountID, platform, bridge.IdentityRef{Raw: address})
	if err != nil {
		// A remote ID that does not canonicalize to a usable identity must not
		// fail the message projection it was incidental to.
		w.logger.Debug().
			Str("account_id", accountID).
			Str("remote_conversation_id", remoteConversationID).
			Err(err).
			Msg("ingest: could not derive direct peer identity from remote conversation ID")
		return nil
	}
	return w.ensureDirectPeerParticipant(conversation, identity)
}

// conversationKindForRemoteID infers a conversation's shape from its remote ID
// and reports whether the ID determines it at all.
//
// Only the Google decoder emits ConversationEvent frames, so every Signal and
// WhatsApp conversation is minted from a message frame — and message events
// carry no kind. Defaulting those to "direct" recorded every post-cutover
// Signal/WhatsApp group as a 1:1 (#155), which both mis-renders the thread and
// would let direct-peer repair attach whoever spoke first as a group's sole
// peer. The remote ID is authoritative for these platforms, so read it.
func conversationKindForRemoteID(
	platform bridge.Platform,
	remoteConversationID string,
) (sqlite.ConversationKind, bool) {
	value := strings.TrimSpace(remoteConversationID)
	if value == "" {
		return "", false
	}
	switch platform {
	case bridge.PlatformSignal:
		if strings.HasPrefix(value, "signal-group:") {
			return sqlite.ConversationKindGroup, true
		}
		if strings.HasPrefix(value, "signal:") {
			return sqlite.ConversationKindDirect, true
		}
		return "", false
	case bridge.PlatformWhatsApp:
		switch {
		case strings.Contains(value, "@g.us"):
			return sqlite.ConversationKindGroup, true
		case strings.Contains(value, "@broadcast"):
			return sqlite.ConversationKindBroadcast, true
		case strings.Contains(value, "@"):
			return sqlite.ConversationKindDirect, true
		}
		return "", false
	default:
		// Google Messages thread ids are opaque ("3031" is a group or a 1:1
		// with equal likelihood); its decoder emits ConversationEvent frames
		// that carry the real kind, so never guess here.
		return "", false
	}
}

// directPeerAddress extracts the remote party's address from a direct
// conversation's remote ID. Group forms and opaque ids return "".
func directPeerAddress(platform bridge.Platform, remoteConversationID string) string {
	value := strings.TrimSpace(remoteConversationID)
	if value == "" {
		return ""
	}
	switch platform {
	case bridge.PlatformSignal:
		if strings.HasPrefix(value, "signal-group:") {
			return ""
		}
		return strings.TrimSpace(strings.TrimPrefix(value, "signal:"))
	case bridge.PlatformWhatsApp:
		jid := strings.TrimSpace(strings.TrimPrefix(value, "whatsapp:"))
		if jid == "" || strings.Contains(jid, "@g.us") || strings.Contains(jid, "@broadcast") {
			return ""
		}
		return jid
	default:
		// Google Messages thread ids ("3031") and everything else are opaque:
		// they name no addressable party, so there is nothing to ensure.
		return ""
	}
}
