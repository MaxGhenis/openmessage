package ingest

import (
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// SendTargetCheck is the verdict on a remote conversation a send is about to
// go to: whether its current snapshot names a different thread than the stored
// conversation the send belongs to, and the shapes behind that verdict.
// It carries counts and kinds only, never who the people are.
type SendTargetCheck struct {
	Contradicts bool
	// StoredKind and RemoteKind are the stored conversation's kind and the
	// snapshot's ("direct" or "group").
	StoredKind string
	RemoteKind string
	// StoredPeers and RemotePeers count the distinct people other than this
	// account on each side; Shared counts those on both.
	StoredPeers int
	RemotePeers int
	Shared      int
}

// CheckSendTarget applies historySnapshotContradicts' rule to a send: the
// stored conversation comes from the dispatcher's ConversationRef and the
// snapshot from the transport's lookup of RemoteID just before sending. A ref
// without Kind or Peers, or a snapshot that names no one but this account,
// proves nothing either way. Otherwise the kinds must agree and the rosters
// must pass #176's consistency rule: a direct thread's peer set must match
// exactly, and a group's must share at least one member. It leaves out a
// snapshot entry whose address v2keys cannot key, and any entry that is this
// account, flagged or not (the phone sometimes lists its own number
// unflagged), so such entries never cause a refusal.
func CheckSendTarget(
	accountID string,
	platform bridge.Platform,
	ref bridge.ConversationRef,
	snapshot bridge.ConversationEvent,
) SendTargetCheck {
	remoteKind := string(sqlite.ConversationKindDirect)
	if snapshot.Kind == string(sqlite.ConversationKindGroup) {
		remoteKind = string(sqlite.ConversationKindGroup)
	}
	check := SendTargetCheck{StoredKind: ref.Kind, RemoteKind: remoteKind}

	storedPeers := sendTargetKeys(accountID, ref.Peers)
	self := make(map[string]struct{}, len(ref.Self))
	for _, identity := range sendTargetKeys(accountID, ref.Self) {
		self[identity.IdentityID] = struct{}{}
	}
	stored := make(map[string]struct{}, len(storedPeers))
	for _, peer := range storedPeers {
		stored[peer.IdentityID] = struct{}{}
	}
	remotePeers := make([]sqlite.Identity, 0, len(snapshot.Participants))
	seen := make(map[string]struct{}, len(snapshot.Participants))
	for _, participant := range snapshot.Participants {
		if participant.Identity.IsSelf || identityRaw(participant.Identity) == "" {
			continue
		}
		key, err := v2keys.IdentityKey(accountID, string(platform), identityRaw(participant.Identity))
		if err != nil {
			continue
		}
		id := sendTargetIdentityID(accountID, key.Kind, key.Canonical)
		if _, isSelf := self[id]; isSelf {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		remotePeers = append(remotePeers, sqlite.Identity{IdentityID: id})
		if _, shared := stored[id]; shared {
			check.Shared++
		}
	}
	check.StoredPeers = len(storedPeers)
	check.RemotePeers = len(remotePeers)

	if ref.Kind == "" || len(storedPeers) == 0 {
		return check
	}
	if remoteKind != ref.Kind {
		check.Contradicts = true
		return check
	}
	if len(remotePeers) == 0 {
		return check
	}
	direct := ref.Kind == string(sqlite.ConversationKindDirect)
	check.Contradicts = !rostersConsistent(direct, remotePeers, storedPeers)
	return check
}

// CheckGoogleSendTarget decodes a Google conversation the way live ingest
// decodes a conversation frame and checks it with CheckSendTarget.
func CheckGoogleSendTarget(
	accountID string,
	ref bridge.ConversationRef,
	conversation *gmproto.Conversation,
) SendTargetCheck {
	event := googleConversationEvent(conversation)
	return CheckSendTarget(accountID, bridge.PlatformGoogle, ref, *event.Conversation)
}

// sendTargetKeys turns a ref's identities into roster entries keyed by their
// natural key, deduplicated. It drops entries without a kind or canonical
// value: they can match nothing.
func sendTargetKeys(accountID string, identities []bridge.IdentityRef) []sqlite.Identity {
	keys := make([]sqlite.Identity, 0, len(identities))
	seen := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		if identity.Kind == "" || identity.Canonical == "" {
			continue
		}
		id := sendTargetIdentityID(accountID, identity.Kind, identity.Canonical)
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		keys = append(keys, sqlite.Identity{IdentityID: id})
	}
	return keys
}

// sendTargetIdentityID is the identity ID ingest mints for a natural key. The
// store keeps one identity per account, kind and canonical value, so comparing
// these IDs compares identities without a store lookup.
func sendTargetIdentityID(accountID, kind, canonical string) string {
	return v2keys.DeriveID("identity", accountID, kind+"\x1f"+canonical)
}
