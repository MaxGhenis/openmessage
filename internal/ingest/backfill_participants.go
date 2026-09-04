package ingest

import (
	"fmt"
	"strings"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// ParticipantBackfillReport counts what a participant backfill sweep changed.
type ParticipantBackfillReport struct {
	Scanned    int
	Linked     int
	Unresolved int
	KindsFixed int
}

// BackfillDirectParticipants repairs direct conversations that hold no
// participant rows — the rows message-frame projection minted before peers
// were ensured (#155). For each one it links the most recent inbound sender
// identity, falling back to the peer named by an address-shaped remote
// conversation ID. Opaque-id conversations with no attributed inbound message
// are counted unresolved and left alone.
//
// It is idempotent, additive (never deletes a link), and best-effort per
// conversation: one bad row does not abort the sweep.
func (w *Worker) BackfillDirectParticipants() (ParticipantBackfillReport, error) {
	report := ParticipantBackfillReport{}
	conversations, err := w.store.ListDirectConversationsWithoutParticipants()
	if err != nil {
		return report, fmt.Errorf("backfill direct participants: %w", err)
	}
	accounts, err := w.store.ListAccounts()
	if err != nil {
		return report, fmt.Errorf("backfill direct participants: list accounts: %w", err)
	}
	platformByAccount := make(map[string]bridge.Platform, len(accounts))
	for _, account := range accounts {
		platformByAccount[account.AccountID] = platformForBridgeKey(account.BridgeKey)
	}

	for _, conversation := range conversations {
		report.Scanned++
		platform, ok := platformByAccount[conversation.AccountID]
		if !ok {
			report.Unresolved++
			continue
		}
		// A conversation stored as direct whose remote ID says group is a
		// mislabeled row from before kind inference existed. Correct the kind
		// and leave the roster alone: linking "the peer" of a group would
		// fabricate a 1:1 relationship out of whoever happened to speak.
		if kind, known := conversationKindForRemoteID(
			platform, conversation.RemoteConversationID,
		); known && kind != conversation.Kind {
			conversation.Kind = kind
			if err := w.store.UpsertConversation(conversation); err != nil {
				w.logger.Warn().
					Str("conversation_id", conversation.ConversationID).
					Err(err).
					Msg("ingest: could not correct mislabeled conversation kind")
				report.Unresolved++
				continue
			}
			report.KindsFixed++
			if kind != sqlite.ConversationKindDirect {
				continue
			}
		}
		if conversation.Kind != sqlite.ConversationKindDirect {
			continue
		}
		linked, err := w.backfillOneDirectConversation(conversation, platform)
		if err != nil {
			w.logger.Warn().
				Str("conversation_id", conversation.ConversationID).
				Err(err).
				Msg("ingest: participant backfill failed for one direct conversation")
			report.Unresolved++
			continue
		}
		if linked {
			report.Linked++
		} else {
			report.Unresolved++
		}
	}
	return report, nil
}

func (w *Worker) backfillOneDirectConversation(
	conversation sqlite.Conversation,
	platform bridge.Platform,
) (bool, error) {
	// A remote ID that does not itself confirm "direct" (an opaque Google thread
	// id) leaves group-ness unknown. Naming such a thread after one sender would
	// render a group as a 1:1 with whoever spoke, so require the only evidence
	// available: exactly one distinct inbound sender. Multi-sender threads are
	// left for the ConversationEvent that carries the real roster.
	if _, confirmed := conversationKindForRemoteID(platform, conversation.RemoteConversationID); !confirmed {
		senders, err := w.store.CountDistinctInboundSenders(conversation.ConversationID)
		if err != nil {
			return false, err
		}
		if senders > 1 {
			return false, nil
		}
	}
	if identityID, ok, err := w.store.LatestInboundSenderIdentityID(
		conversation.ConversationID,
	); err != nil {
		return false, err
	} else if ok {
		identity, err := w.store.GetIdentity(identityID)
		if err != nil {
			return false, err
		}
		if err := w.ensureDirectPeerParticipant(conversation, identity); err != nil {
			return false, err
		}
		return true, nil
	}

	address := directPeerAddress(platform, conversation.RemoteConversationID)
	if address == "" {
		return false, nil
	}
	identity, err := w.resolveIdentity(
		conversation.AccountID, platform, bridge.IdentityRef{Raw: address},
	)
	if err != nil {
		return false, err
	}
	if err := w.ensureDirectPeerParticipant(conversation, identity); err != nil {
		return false, err
	}
	return true, nil
}

// platformForBridgeKey maps a stored account bridge key onto the ingest
// platform vocabulary. It mirrors the bridge registry's own mapping so a
// backfill sweep can run without a live adapter registered.
func platformForBridgeKey(bridgeKey string) bridge.Platform {
	switch strings.TrimSpace(bridgeKey) {
	case "google_messages":
		return bridge.PlatformGoogle
	case "whatsmeow":
		return bridge.PlatformWhatsApp
	case "signal_cli":
		return bridge.PlatformSignal
	default:
		return bridge.Platform(strings.TrimSpace(bridgeKey))
	}
}
