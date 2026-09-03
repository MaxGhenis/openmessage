package ingest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// IDSpaceRepairOptions scopes a repair of rows projected while Google frames
// carried a re-keyed device ID space (see idspace.go). SinceMS is the row
// creation time (projection time) from which rows are suspect — the moment
// the re-paired device started delivering frames. Reference, when set, is a
// copy of the store taken before that moment; it lets the repair restore the
// title and roster of threads that a re-keyed ConversationEvent overwrote.
type IDSpaceRepairOptions struct {
	AccountID string
	SinceMS   int64
	Now       func() time.Time
	Reference *sqlite.Store
}

// IDSpaceRepairGroup is the verdict for one conversation that received rows
// in the window.
type IDSpaceRepairGroup struct {
	ConversationID       string   `json:"conversation_id"`
	RemoteConversationID string   `json:"remote_conversation_id"`
	Title                string   `json:"title"`
	Kind                 string   `json:"kind"`
	Rows                 int      `json:"rows"`
	OlderRows            int64    `json:"older_rows"`
	Peers                []string `json:"peers"`
	Senders              []string `json:"senders"`
	Verdict              string   `json:"verdict"`
	TargetConversationID string   `json:"target_conversation_id,omitempty"`
	TargetTitle          string   `json:"target_title,omitempty"`
	Moved                int      `json:"moved"`
	Deleted              int      `json:"deleted"`
	Detail               string   `json:"detail,omitempty"`
}

// IDSpaceRepairRestore records one thread whose metadata a re-keyed
// ConversationEvent overwrote and how the repair split it back apart.
type IDSpaceRepairRestore struct {
	ConversationID       string   `json:"conversation_id"`
	RemoteConversationID string   `json:"remote_conversation_id"`
	ReferenceTitle       string   `json:"reference_title"`
	LiveTitle            string   `json:"live_title"`
	ReferencePeers       []string `json:"reference_peers"`
	LivePeers            []string `json:"live_peers"`
	TargetConversationID string   `json:"target_conversation_id,omitempty"`
	Verdict              string   `json:"verdict"`
	Detail               string   `json:"detail,omitempty"`
}

// IDSpaceRepairReport is a dry-run plan: every step it lists is applied, in
// order, by ApplyGoogleIDSpaceRepair.
type IDSpaceRepairReport struct {
	AccountID     string                 `json:"account_id"`
	SinceMS       int64                  `json:"since_ms"`
	CandidateRows int                    `json:"candidate_rows"`
	Groups        []IDSpaceRepairGroup   `json:"groups"`
	Restores      []IDSpaceRepairRestore `json:"restores,omitempty"`
	Steps         []sqlite.RepairStep    `json:"steps"`
	Moves         int                    `json:"moves"`
	Deletes       int                    `json:"deletes"`
	Rebinds       int                    `json:"rebinds"`
	Mints         int                    `json:"mints"`
	Drops         int                    `json:"drops"`
	Restored      int                    `json:"restored"`
	Ambiguous     int                    `json:"ambiguous"`
}

const (
	verdictConsistent        = "consistent"
	verdictFreshOutgoingOnly = "fresh-outgoing-only"
	verdictAmbiguous         = "ambiguous"
	verdictReroutedExisting  = "rerouted-to-existing"
	verdictReroutedMinted    = "rerouted-to-minted"
	verdictMergedFresh       = "merged-fresh-into-existing"
	verdictClobberedExisting = "clobbered-rerouted-to-existing"
	verdictClobberedMinted   = "clobbered-rerouted-to-minted"
	verdictRestoredExisting  = "restored-rebound-existing"
	verdictRestoredMinted    = "restored-minted"
	verdictRestoredAmbiguous = "restored-ambiguous"
)

// clobberInfo is a thread whose live title/roster no longer match the
// reference copy: a re-keyed ConversationEvent for a different thread was
// applied to it. The reference metadata belongs to the row; the live metadata
// belongs to the wire id's new thread.
type clobberInfo struct {
	live         sqlite.Conversation
	reference    sqlite.Conversation
	liveRoster   []sqlite.ConversationParticipant
	refRoster    []sqlite.ConversationParticipant
	livePeerIDs  []string
	refPeerIDs   []string
	livePeers    []string
	refPeers     []string
	target       string
	targetMinted bool
}

type repairPlanner struct {
	ctx      context.Context
	store    *sqlite.Store
	messages *sqlite.MessageRepository
	opts     IDSpaceRepairOptions
	selfID   string
	report   *IDSpaceRepairReport

	clobbered map[string]*clobberInfo
	mints     []sqlite.RepairStep
	rowSteps  []sqlite.RepairStep
	rebinds   []sqlite.RepairStep
	metas     []sqlite.RepairStep
	drops     []sqlite.RepairStep
	recency   map[string]struct{}
	planned   map[string]map[string]struct{} // target conversation → content keys planned into it
	mintedIDs map[string]struct{}
}

// PlanGoogleIDSpaceRepair inspects every message row created since
// opts.SinceMS and decides, per conversation, whether the rows belong there.
// A direct thread's rows belong when their single sender is the thread's peer
// (or the thread has no peer evidence at all); a group's rows belong when a
// sender overlaps its roster. Misfiled rows move to the sender's own thread
// (found by sole peer, or minted under the wire id), duplicates of already
// stored content are deleted instead of moved, and the wire id follows the
// rows. With a Reference store, threads whose title/roster were overwritten
// by a re-keyed ConversationEvent get their metadata back and the overwrite
// becomes the wire id's own thread. The plan is pure: nothing is written
// until ApplyGoogleIDSpaceRepair.
func PlanGoogleIDSpaceRepair(
	ctx context.Context,
	store *sqlite.Store,
	messages *sqlite.MessageRepository,
	opts IDSpaceRepairOptions,
) (IDSpaceRepairReport, error) {
	if store == nil || messages == nil {
		return IDSpaceRepairReport{}, fmt.Errorf("plan id-space repair: store is nil")
	}
	if strings.TrimSpace(opts.AccountID) == "" {
		return IDSpaceRepairReport{}, fmt.Errorf("plan id-space repair: account is empty")
	}
	if opts.SinceMS <= 0 {
		return IDSpaceRepairReport{}, fmt.Errorf("plan id-space repair: since must be positive")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	report := IDSpaceRepairReport{AccountID: opts.AccountID, SinceMS: opts.SinceMS}
	p := &repairPlanner{
		ctx:       ctx,
		store:     store,
		messages:  messages,
		opts:      opts,
		report:    &report,
		clobbered: make(map[string]*clobberInfo),
		recency:   make(map[string]struct{}),
		planned:   make(map[string]map[string]struct{}),
		mintedIDs: make(map[string]struct{}),
	}
	selfID, _, err := store.SelfIdentityID(opts.AccountID)
	if err != nil {
		return IDSpaceRepairReport{}, err
	}
	p.selfID = selfID

	if opts.Reference != nil {
		if err := p.detectClobbers(); err != nil {
			return IDSpaceRepairReport{}, err
		}
	}

	rows, err := store.ListMessagesCreatedSince(opts.AccountID, opts.SinceMS)
	if err != nil {
		return IDSpaceRepairReport{}, err
	}
	report.CandidateRows = len(rows)
	byConversation := make(map[string][]sqlite.Message)
	order := make([]string, 0)
	for _, row := range rows {
		if _, seen := byConversation[row.ConversationID]; !seen {
			order = append(order, row.ConversationID)
		}
		byConversation[row.ConversationID] = append(byConversation[row.ConversationID], row)
	}
	sort.Strings(order)
	for _, conversationID := range order {
		if err := p.planGroup(conversationID, byConversation[conversationID]); err != nil {
			return IDSpaceRepairReport{}, err
		}
	}

	if err := p.planRestores(); err != nil {
		return IDSpaceRepairReport{}, err
	}

	report.Steps = append(report.Steps, p.mints...)
	report.Steps = append(report.Steps, p.rowSteps...)
	report.Steps = append(report.Steps, p.rebinds...)
	report.Steps = append(report.Steps, p.metas...)
	report.Steps = append(report.Steps, p.drops...)
	recency := make([]string, 0, len(p.recency))
	for id := range p.recency {
		recency = append(recency, id)
	}
	sort.Strings(recency)
	for _, id := range recency {
		report.Steps = append(report.Steps, sqlite.RepairStep{Op: "recency", ConversationID: id})
	}
	return report, nil
}

// ApplyGoogleIDSpaceRepair executes a plan atomically.
func ApplyGoogleIDSpaceRepair(
	ctx context.Context,
	store *sqlite.Store,
	report IDSpaceRepairReport,
	now time.Time,
) error {
	if len(report.Steps) == 0 {
		return nil
	}
	return store.ApplyRepairPlan(ctx, report.AccountID, report.Steps, now.UnixMilli())
}

// detectClobbers finds threads whose roster changed in the window relative to
// the reference copy. A rename alone is a legitimate contact refresh; a roster
// with no overlap in the reference is a different thread's ConversationEvent
// applied under a colliding id. Threads with no reference peers have nothing
// authoritative to restore and are left as adopted.
func (p *repairPlanner) detectClobbers() error {
	live, err := p.store.ListConversationsUpdatedSince(p.opts.AccountID, p.opts.SinceMS)
	if err != nil {
		return err
	}
	for _, conversation := range live {
		reference, err := p.opts.Reference.GetConversation(conversation.ConversationID)
		if errors.Is(err, sqlite.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		refPeers, err := p.opts.Reference.ListConversationPeerIdentities(p.opts.AccountID, conversation.ConversationID)
		if err != nil {
			return err
		}
		if len(refPeers) == 0 {
			continue
		}
		livePeers, err := p.store.ListConversationPeerIdentities(p.opts.AccountID, conversation.ConversationID)
		if err != nil {
			return err
		}
		info := &clobberInfo{live: conversation, reference: reference}
		for _, peer := range refPeers {
			info.refPeerIDs = append(info.refPeerIDs, peer.IdentityID)
			info.refPeers = append(info.refPeers, peer.CanonicalValue)
		}
		for _, peer := range livePeers {
			info.livePeerIDs = append(info.livePeerIDs, peer.IdentityID)
			info.livePeers = append(info.livePeers, peer.CanonicalValue)
		}
		if sameIDSet(info.refPeerIDs, info.livePeerIDs) {
			continue
		}
		if info.refRoster, err = p.opts.Reference.ListParticipants(conversation.ConversationID); err != nil {
			return err
		}
		if info.liveRoster, err = p.store.ListParticipants(conversation.ConversationID); err != nil {
			return err
		}
		p.clobbered[conversation.ConversationID] = info
	}
	return nil
}

func sameIDSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, id := range a {
		set[id] = struct{}{}
	}
	for _, id := range b {
		if _, ok := set[id]; !ok {
			return false
		}
	}
	return true
}

func (p *repairPlanner) planGroup(conversationID string, rows []sqlite.Message) error {
	conversation, err := p.store.GetConversation(conversationID)
	if err != nil {
		return err
	}
	group := IDSpaceRepairGroup{
		ConversationID:       conversation.ConversationID,
		RemoteConversationID: conversation.RemoteConversationID,
		Title:                conversation.Title,
		Kind:                 string(conversation.Kind),
		Rows:                 len(rows),
	}
	peers, err := p.store.ListConversationPeerIdentities(p.opts.AccountID, conversationID)
	if err != nil {
		return err
	}
	for _, peer := range peers {
		group.Peers = append(group.Peers, peer.CanonicalValue)
	}
	olderCount, err := p.store.CountMessagesCreatedBefore(conversationID, p.opts.SinceMS)
	if err != nil {
		return err
	}
	group.OlderRows = olderCount
	olderSenders, err := p.store.ListInboundSenderIdentityIDsBefore(conversationID, p.opts.SinceMS)
	if err != nil {
		return err
	}

	senderIDs := make([]string, 0)
	seenSenders := make(map[string]struct{})
	for _, row := range rows {
		if row.Direction != sqlite.MessageDirectionIncoming || row.SenderIdentityID == nil {
			continue
		}
		if _, ok := seenSenders[*row.SenderIdentityID]; ok {
			continue
		}
		seenSenders[*row.SenderIdentityID] = struct{}{}
		senderIDs = append(senderIDs, *row.SenderIdentityID)
	}
	sort.Strings(senderIDs)
	for _, id := range senderIDs {
		identity, err := p.store.GetIdentity(id)
		if err != nil {
			return err
		}
		group.Senders = append(group.Senders, identity.CanonicalValue)
	}

	// A thread whose metadata was overwritten by the wire id's new thread:
	// every window row came from that wire id and belongs with the new
	// thread, whatever its direction.
	if info, ok := p.clobbered[conversationID]; ok {
		group.Peers = info.refPeers
		target, minted, err := p.resolveClobberTarget(info)
		if err != nil {
			return err
		}
		if target == "" {
			group.Verdict = verdictAmbiguous
			group.Detail = "metadata was overwritten but the wire id is already displaced and no thread matches the new roster"
			p.report.Ambiguous++
			p.report.Groups = append(p.report.Groups, group)
			return nil
		}
		if minted {
			group.Verdict = verdictClobberedMinted
		} else {
			group.Verdict = verdictClobberedExisting
		}
		group.TargetConversationID = target
		group.TargetTitle = info.live.Title
		p.planMoves(conversationID, target, rows, &group)
		if olderCount == 0 && group.Moved+group.Deleted == len(rows) {
			p.drops = append(p.drops, sqlite.RepairStep{Op: "drop", ConversationID: conversationID})
			p.report.Drops++
		}
		p.recency[conversationID] = struct{}{}
		p.recency[target] = struct{}{}
		p.report.Groups = append(p.report.Groups, group)
		return nil
	}

	evidence := make(map[string]struct{})
	for _, peer := range peers {
		evidence[peer.IdentityID] = struct{}{}
	}
	if len(evidence) == 0 {
		for _, id := range olderSenders {
			evidence[id] = struct{}{}
		}
	}

	if len(senderIDs) == 0 {
		if olderCount == 0 {
			group.Verdict = verdictFreshOutgoingOnly
		} else {
			group.Verdict = verdictAmbiguous
			group.Detail = "only outgoing or unattributed rows in the window; recipient cannot be verified"
			p.report.Ambiguous++
		}
		if err := p.dedupeWithin(conversationID, rows, &group); err != nil {
			return err
		}
		p.report.Groups = append(p.report.Groups, group)
		return nil
	}

	consistent := false
	switch conversation.Kind {
	case sqlite.ConversationKindDirect:
		if len(senderIDs) == 1 {
			_, isPeer := evidence[senderIDs[0]]
			consistent = len(evidence) == 0 || isPeer
			if len(evidence) == 0 {
				// No peer evidence here, but the sender may already own a
				// thread elsewhere: a fresh row minted under their new device
				// id (or an outgoing-only stub) splits their history unless it
				// is merged back into that thread.
				elsewhere, err := p.store.FindDirectConversationBySolePeer(p.opts.AccountID, senderIDs[0])
				if err == nil && elsewhere.ConversationID != conversationID {
					consistent = false
				} else if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
					return err
				}
			}
		}
	default:
		if len(evidence) == 0 {
			consistent = true
		}
		for _, id := range senderIDs {
			if _, ok := evidence[id]; ok {
				consistent = true
			}
		}
	}
	if consistent {
		group.Verdict = verdictConsistent
		if err := p.dedupeWithin(conversationID, rows, &group); err != nil {
			return err
		}
		p.report.Groups = append(p.report.Groups, group)
		return nil
	}

	// Misfiled: the wire id that delivered these rows is the conversation's
	// current binding unless the live guard already displaced it.
	wireID := conversation.RemoteConversationID
	if strings.HasPrefix(wireID, sqlite.DisplacedRemoteIDPrefix) {
		wireID = ""
	}
	var target sqlite.Conversation
	minted := false
	if len(senderIDs) == 1 {
		found, err := p.store.FindDirectConversationBySolePeer(p.opts.AccountID, senderIDs[0])
		switch {
		case err == nil && found.ConversationID != conversationID:
			target = found
		case err == nil || errors.Is(err, sqlite.ErrNotFound):
			if wireID == "" {
				group.Verdict = verdictAmbiguous
				group.Detail = "sender has no sole-peer thread and the wire id is already displaced"
				p.report.Ambiguous++
				p.report.Groups = append(p.report.Groups, group)
				return nil
			}
			target, err = p.mint(wireID, sqlite.ConversationKindDirect, "", p.rosterFromIDs(senderIDs))
			if err != nil {
				return err
			}
			minted = true
		default:
			return err
		}
	} else {
		found, err := p.store.FindGroupConversationByPeerSet(p.opts.AccountID, senderIDs)
		switch {
		case err == nil && found.ConversationID != conversationID:
			target = found
		case err == nil || errors.Is(err, sqlite.ErrNotFound):
			if wireID == "" {
				group.Verdict = verdictAmbiguous
				group.Detail = "no group matches the senders and the wire id is already displaced"
				p.report.Ambiguous++
				p.report.Groups = append(p.report.Groups, group)
				return nil
			}
			target, err = p.mint(wireID, sqlite.ConversationKindGroup, "", p.rosterFromIDs(senderIDs))
			if err != nil {
				return err
			}
			minted = true
			group.Detail = "minted group carries only the senders seen so far; the next conversation frame completes its roster"
		default:
			return err
		}
	}

	group.TargetConversationID = target.ConversationID
	group.TargetTitle = target.Title
	if minted {
		group.Verdict = verdictReroutedMinted
	} else if olderCount == 0 && len(peers) == 0 {
		group.Verdict = verdictMergedFresh
	} else {
		group.Verdict = verdictReroutedExisting
	}

	p.planMoves(conversationID, target.ConversationID, rows, &group)
	if wireID != "" && !minted {
		p.rebinds = append(p.rebinds, sqlite.RepairStep{
			Op: "rebind", RemoteConversationID: wireID, TargetConversationID: target.ConversationID,
			ConversationID: conversationID,
		})
		p.report.Rebinds++
	}
	if olderCount == 0 && group.Moved+group.Deleted == len(rows) {
		p.drops = append(p.drops, sqlite.RepairStep{Op: "drop", ConversationID: conversationID})
		p.report.Drops++
	}
	p.recency[conversationID] = struct{}{}
	p.recency[target.ConversationID] = struct{}{}
	p.report.Groups = append(p.report.Groups, group)
	return nil
}

// planMoves moves window rows to the target, deleting those whose content the
// target already holds and leaving rows a read cursor still points at.
func (p *repairPlanner) planMoves(conversationID, target string, rows []sqlite.Message, group *IDSpaceRepairGroup) {
	for _, row := range rows {
		cursor, err := p.store.MessageHasReadCursor(row.MessageID)
		if err == nil && cursor {
			group.Detail = strings.TrimSpace(group.Detail + " left message " + row.MessageID + " in place: a read cursor references it")
			continue
		}
		duplicate, _ := p.isPlannedOrStoredDuplicate(target, row)
		if duplicate {
			p.rowSteps = append(p.rowSteps, sqlite.RepairStep{
				Op: "delete", MessageID: row.MessageID, ConversationID: conversationID,
				Reason: "duplicate of content already in " + target,
			})
			group.Deleted++
			p.report.Deletes++
			continue
		}
		p.rowSteps = append(p.rowSteps, sqlite.RepairStep{
			Op: "move", MessageID: row.MessageID, ConversationID: conversationID,
			TargetConversationID: target,
		})
		p.rememberPlanned(target, row)
		group.Moved++
		p.report.Moves++
	}
}

// resolveClobberTarget finds or mints the thread the overwriting
// ConversationEvent actually described — the wire id's thread on the new
// device — carrying that event's title and roster. Returns "" when the wire
// id was already displaced and no thread matches the roster.
func (p *repairPlanner) resolveClobberTarget(info *clobberInfo) (string, bool, error) {
	if info.target != "" {
		return info.target, info.targetMinted, nil
	}
	wireID := info.live.RemoteConversationID
	if strings.HasPrefix(wireID, sqlite.DisplacedRemoteIDPrefix) {
		wireID = ""
	}
	direct := info.live.Kind != sqlite.ConversationKindGroup
	var found sqlite.Conversation
	var err error
	switch {
	case len(info.livePeerIDs) == 0:
		err = sqlite.ErrNotFound
	case direct && len(info.livePeerIDs) == 1:
		found, err = p.store.FindDirectConversationBySolePeer(p.opts.AccountID, info.livePeerIDs[0])
	default:
		found, err = p.store.FindGroupConversationByPeerSet(p.opts.AccountID, info.livePeerIDs)
	}
	if err == nil && found.ConversationID != info.live.ConversationID {
		if wireID != "" {
			p.rebinds = append(p.rebinds, sqlite.RepairStep{
				Op: "rebind", RemoteConversationID: wireID, TargetConversationID: found.ConversationID,
				ConversationID: info.live.ConversationID,
			})
			p.report.Rebinds++
		}
		p.metas = append(p.metas, sqlite.RepairStep{
			Op: "meta", ConversationID: found.ConversationID, Title: info.live.Title,
			Kind: string(info.live.Kind), Participants: repairRoster(info.liveRoster),
			Reason: "metadata from the wire id's conversation event",
		})
		info.target = found.ConversationID
		return info.target, false, nil
	}
	if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
		return "", false, err
	}
	if wireID == "" {
		return "", false, nil
	}
	minted, err := p.mint(wireID, info.live.Kind, info.live.Title, repairRoster(info.liveRoster))
	if err != nil {
		return "", false, err
	}
	info.target = minted.ConversationID
	info.targetMinted = true
	return info.target, true, nil
}

// planRestores gives every clobbered thread its reference metadata back and
// makes sure the overwriting event's thread exists under the wire id even
// when no message rows accompanied it.
func (p *repairPlanner) planRestores() error {
	ids := make([]string, 0, len(p.clobbered))
	for id := range p.clobbered {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		info := p.clobbered[id]
		restore := IDSpaceRepairRestore{
			ConversationID:       id,
			RemoteConversationID: info.live.RemoteConversationID,
			ReferenceTitle:       info.reference.Title,
			LiveTitle:            info.live.Title,
			ReferencePeers:       info.refPeers,
			LivePeers:            info.livePeers,
		}
		target, minted, err := p.resolveClobberTarget(info)
		if err != nil {
			return err
		}
		switch {
		case target == "":
			restore.Verdict = verdictRestoredAmbiguous
			restore.Detail = "wire id already displaced and no thread matches the new roster; metadata restored only"
			p.report.Ambiguous++
		case minted:
			restore.Verdict = verdictRestoredMinted
		default:
			restore.Verdict = verdictRestoredExisting
		}
		restore.TargetConversationID = target
		p.metas = append(p.metas, sqlite.RepairStep{
			Op: "meta", ConversationID: id, Title: info.reference.Title,
			Kind: string(info.reference.Kind), Participants: repairRoster(info.refRoster),
			Reason: "restored from reference copy",
		})
		p.recency[id] = struct{}{}
		if target != "" {
			p.recency[target] = struct{}{}
		}
		p.report.Restored++
		p.report.Restores = append(p.report.Restores, restore)
	}
	return nil
}

func repairRoster(roster []sqlite.ConversationParticipant) []sqlite.RepairParticipant {
	result := make([]sqlite.RepairParticipant, 0, len(roster))
	for _, participant := range roster {
		result = append(result, sqlite.RepairParticipant{
			IdentityID:  participant.IdentityID,
			DisplayName: participant.DisplayName,
			Role:        string(participant.Role),
			IsActive:    participant.IsActive,
		})
	}
	return result
}

func (p *repairPlanner) rosterFromIDs(peerIDs []string) []sqlite.RepairParticipant {
	roster := make([]sqlite.RepairParticipant, 0, len(peerIDs)+1)
	for _, id := range peerIDs {
		roster = append(roster, sqlite.RepairParticipant{IdentityID: id, Role: "member", IsActive: true})
	}
	if p.selfID != "" {
		roster = append(roster, sqlite.RepairParticipant{IdentityID: p.selfID, Role: "member", IsActive: true})
	}
	return roster
}

// dedupeWithin deletes window rows that restate content the conversation
// already held (an older row, or an earlier row in the same window).
func (p *repairPlanner) dedupeWithin(conversationID string, rows []sqlite.Message, group *IDSpaceRepairGroup) error {
	for _, row := range rows {
		if strings.TrimSpace(row.Body) == "" {
			continue
		}
		existing, found, err := p.messages.FindMessageContentDuplicate(
			p.ctx, p.opts.AccountID, conversationID, row.RemoteMessageID,
			row.Direction, row.SenderIdentityID, row.OccurredAtMS, row.Body,
		)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		older := existing.CreatedAtMS < row.CreatedAtMS ||
			(existing.CreatedAtMS == row.CreatedAtMS && existing.MessageID < row.MessageID)
		if !older {
			continue
		}
		cursor, err := p.store.MessageHasReadCursor(row.MessageID)
		if err != nil {
			return err
		}
		if cursor {
			continue
		}
		p.rowSteps = append(p.rowSteps, sqlite.RepairStep{
			Op: "delete", MessageID: row.MessageID, ConversationID: conversationID,
			Reason: "duplicate of " + existing.MessageID,
		})
		group.Deleted++
		p.report.Deletes++
		p.recency[conversationID] = struct{}{}
	}
	return nil
}

func contentKey(row sqlite.Message) string {
	sender := ""
	if row.SenderIdentityID != nil {
		sender = *row.SenderIdentityID
	}
	return string(row.Direction) + "\x1f" + sender + "\x1f" + strconv.FormatInt(row.OccurredAtMS, 10) + "\x1f" + row.Body
}

func (p *repairPlanner) rememberPlanned(target string, row sqlite.Message) {
	if p.planned[target] == nil {
		p.planned[target] = make(map[string]struct{})
	}
	p.planned[target][contentKey(row)] = struct{}{}
}

func (p *repairPlanner) isPlannedOrStoredDuplicate(target string, row sqlite.Message) (bool, error) {
	if strings.TrimSpace(row.Body) == "" {
		return false, nil
	}
	if _, ok := p.planned[target][contentKey(row)]; ok {
		return true, nil
	}
	if _, minted := p.mintedIDs[target]; minted {
		return false, nil
	}
	_, found, err := p.messages.FindMessageContentDuplicate(
		p.ctx, p.opts.AccountID, target, row.RemoteMessageID,
		row.Direction, row.SenderIdentityID, row.OccurredAtMS, row.Body,
	)
	return found, err
}

// mint plans a fresh conversation bound to the wire id, keyed past any
// primary key a displaced former holder still owns.
func (p *repairPlanner) mint(wireID string, kind sqlite.ConversationKind, title string, roster []sqlite.RepairParticipant) (sqlite.Conversation, error) {
	candidate := v2keys.DeriveID("conversation", p.opts.AccountID, wireID)
	for salt := 1; ; salt++ {
		_, err := p.store.GetConversation(candidate)
		if errors.Is(err, sqlite.ErrNotFound) {
			if _, taken := p.mintedIDs[candidate]; !taken {
				break
			}
		} else if err != nil {
			return sqlite.Conversation{}, err
		}
		candidate = v2keys.DeriveID("conversation", p.opts.AccountID, wireID+"\x1frebind\x1f"+strconv.Itoa(salt))
	}
	p.mints = append(p.mints, sqlite.RepairStep{
		Op:                   "mint",
		ConversationID:       candidate,
		RemoteConversationID: wireID,
		Kind:                 string(kind),
		Title:                title,
		Participants:         roster,
		CreatedAtMS:          p.opts.Now().UnixMilli(),
	})
	p.mintedIDs[candidate] = struct{}{}
	p.report.Mints++
	return sqlite.Conversation{
		ConversationID:       candidate,
		AccountID:            p.opts.AccountID,
		RemoteConversationID: wireID,
		Kind:                 kind,
		Title:                title,
	}, nil
}
