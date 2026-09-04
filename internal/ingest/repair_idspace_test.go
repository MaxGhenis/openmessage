package ingest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

// The repair test seeds the store exactly as the pre-fix projector left it:
// old-phone threads with their peers, then new-phone rows appended into the
// colliding threads (Shoshana's text and a re-served duplicate in the PayPal
// thread; a campaign text in Karl's thread; a fresh peerless row for a known
// sender's new id), all created inside the repair window.

const repairWindowMS = int64(1788465000000)

func repairSeedIdentity(t *testing.T, harness *i01Harness, raw, name string, self bool) sqlite.Identity {
	t.Helper()
	identity, err := harness.worker.resolveIdentity(i01AccountID, "google", identityRef(raw, name, self))
	if err != nil {
		t.Fatalf("seed identity %s: %v", raw, err)
	}
	return identity
}

func repairSeedConversation(t *testing.T, harness *i01Harness, remoteID, title string, kind sqlite.ConversationKind, createdMS int64, peers ...sqlite.Identity) sqlite.Conversation {
	t.Helper()
	conversation := sqlite.Conversation{
		ConversationID:       v2keys.DeriveID("conversation", i01AccountID, remoteID),
		AccountID:            i01AccountID,
		RemoteConversationID: remoteID,
		Kind:                 kind,
		Title:                title,
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         "{}",
		CreatedAtMS:          createdMS,
		UpdatedAtMS:          createdMS,
	}
	if err := harness.store.UpsertConversation(conversation); err != nil {
		t.Fatalf("seed conversation %s: %v", remoteID, err)
	}
	participants := make([]sqlite.ConversationParticipant, 0, len(peers))
	for _, peer := range peers {
		participants = append(participants, sqlite.ConversationParticipant{
			AccountID: i01AccountID, ConversationID: conversation.ConversationID,
			IdentityID: peer.IdentityID, Role: sqlite.ParticipantRoleMember, IsActive: true,
		})
	}
	if err := harness.store.ReplaceConversationParticipants(conversation.ConversationID, participants); err != nil {
		t.Fatalf("seed participants %s: %v", remoteID, err)
	}
	return conversation
}

// repairSeedMessage writes a row with an explicit created_at (the repair
// window is defined by projection time, which the repository stamps from its
// clock), so the harness clock is moved around it.
func repairSeedMessage(t *testing.T, harness *i01Harness, conversation sqlite.Conversation, remoteMessageID string, sender *sqlite.Identity, body string, occurredMS, createdMS int64) sqlite.Message {
	t.Helper()
	repository, err := sqlite.NewMessageRepository(harness.store, func() time.Time { return time.UnixMilli(createdMS) })
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	message := sqlite.Message{
		MessageID:       v2keys.DeriveID("message", i01AccountID, conversation.RemoteConversationID+"\x1f"+remoteMessageID),
		ConversationID:  conversation.ConversationID,
		AccountID:       i01AccountID,
		RemoteMessageID: remoteMessageID,
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            body,
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    occurredMS,
	}
	if sender != nil {
		message.Direction = sqlite.MessageDirectionIncoming
		message.SenderIdentityID = &sender.IdentityID
	}
	if err := repository.ImportMessage(context.Background(), sqlite.MessageProjection{Message: message}); err != nil {
		t.Fatalf("seed message %s: %v", remoteMessageID, err)
	}
	if err := harness.store.BumpConversationRecency(conversation.ConversationID, occurredMS); err != nil {
		t.Fatalf("bump recency: %v", err)
	}
	return message
}

func TestPlanAndApplyGoogleIDSpaceRepair(t *testing.T) {
	harness := i01NewHarness(t, &idsScript{}, nil)
	ctx := context.Background()
	before := repairWindowMS - 10_000_000
	inWindow := repairWindowMS + 5_000

	self := repairSeedIdentity(t, harness, idsSelfNumber, "Max Ghenis", true)
	shoshana := repairSeedIdentity(t, harness, idsShoshana, "Shoshana Weissmann", false)
	paypal := repairSeedIdentity(t, harness, idsPayPal, "", false)
	karl := repairSeedIdentity(t, harness, idsKarl, "", false)
	spam := repairSeedIdentity(t, harness, idsCampaignSpam, "", false)
	alex := repairSeedIdentity(t, harness, "+13479184547", "Alex Armlovich", false)

	paypalThread := repairSeedConversation(t, harness, "2873", "72975", sqlite.ConversationKindDirect, before, paypal, self)
	shoshanaThread := repairSeedConversation(t, harness, "3093", "Shoshana Weissmann", sqlite.ConversationKindDirect, before, shoshana, self)
	karlThread := repairSeedConversation(t, harness, "2916", "(202) 602-2529", sqlite.ConversationKindDirect, before, karl, self)
	alexThread := repairSeedConversation(t, harness, "2764", "Alex Armlovich", sqlite.ConversationKindDirect, before, alex, self)

	repairSeedMessage(t, harness, paypalThread, "500", &paypal, "Your PayPal code is 123456", before+1, before+1)
	original := repairSeedMessage(t, harness, shoshanaThread, "86133", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, before+2)
	repairSeedMessage(t, harness, karlThread, "700", &karl, "Vote Racine", before+3, before+3)
	repairSeedMessage(t, harness, alexThread, "800", &alex, "coffee?", before+4, before+4)

	// Pre-fix projector damage, all created inside the window.
	dupInPayPal := repairSeedMessage(t, harness, paypalThread, "59", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, inWindow)
	liveInPayPal := repairSeedMessage(t, harness, paypalThread, "85793", &shoshana, "see you at 12", idsAnimalsTimeMS+1_000_000, inWindow+1)
	outgoingInPayPal := repairSeedMessage(t, harness, paypalThread, "85794", nil, "sounds good", idsAnimalsTimeMS+1_100_000, inWindow+2)
	spamInKarl := repairSeedMessage(t, harness, karlThread, "85624", &spam, "Donate before midnight", idsAnimalsTimeMS+2_000_000, inWindow+3)
	// Alex's new-phone id 255 minted a fresh, peerless thread instead of
	// continuing his old one.
	freshAlex := repairSeedConversation(t, harness, "255", "", sqlite.ConversationKindDirect, inWindow)
	alexInFresh := repairSeedMessage(t, harness, freshAlex, "85868", &alex, "running late", idsAnimalsTimeMS+3_000_000, inWindow+4)
	// A consistent thread that merely got a re-served duplicate of its own row.
	dupInAlex := repairSeedMessage(t, harness, alexThread, "9001", &alex, "coffee?", before+4, inWindow+5)

	report, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime },
	})
	if err != nil {
		t.Fatalf("PlanGoogleIDSpaceRepair: %v", err)
	}
	if report.CandidateRows != 6 {
		t.Fatalf("candidate rows = %d, want 6", report.CandidateRows)
	}
	verdicts := make(map[string]IDSpaceRepairGroup)
	for _, group := range report.Groups {
		verdicts[group.ConversationID] = group
	}
	if g := verdicts[paypalThread.ConversationID]; g.Verdict != verdictReroutedExisting || g.TargetConversationID != shoshanaThread.ConversationID || g.Moved != 2 || g.Deleted != 1 {
		t.Fatalf("PayPal group = %+v", g)
	}
	if g := verdicts[karlThread.ConversationID]; g.Verdict != verdictReroutedMinted || g.Moved != 1 {
		t.Fatalf("Karl group = %+v", g)
	}
	if g := verdicts[freshAlex.ConversationID]; g.Verdict != verdictMergedFresh || g.TargetConversationID != alexThread.ConversationID || g.Moved != 1 {
		t.Fatalf("fresh Alex group = %+v", g)
	}
	if g := verdicts[alexThread.ConversationID]; g.Verdict != verdictConsistent || g.Deleted != 1 {
		t.Fatalf("Alex group = %+v", g)
	}
	if report.Moves != 4 || report.Deletes != 2 || report.Rebinds != 2 || report.Mints != 1 || report.Drops != 1 || report.Ambiguous != 0 {
		t.Fatalf("report totals = %+v", report)
	}

	// Dry run wrote nothing.
	if got := idsMessageCount(t, harness, paypalThread.ConversationID); got != 4 {
		t.Fatalf("PayPal rows after dry run = %d, want 4", got)
	}

	if err := ApplyGoogleIDSpaceRepair(ctx, harness.store, report, i01TestTime); err != nil {
		t.Fatalf("ApplyGoogleIDSpaceRepair: %v", err)
	}

	// PayPal: only its own message remains; binding displaced; peers intact.
	if got := idsMessageCount(t, harness, paypalThread.ConversationID); got != 1 {
		t.Fatalf("PayPal rows after repair = %d, want 1", got)
	}
	paypalNow, _ := harness.store.GetConversation(paypalThread.ConversationID)
	if !strings.HasPrefix(paypalNow.RemoteConversationID, sqlite.DisplacedRemoteIDPrefix+"2873:") || paypalNow.Title != "72975" {
		t.Fatalf("PayPal row after repair = %+v", paypalNow)
	}
	if paypalNow.LastMessageAtMS != before+1 {
		t.Fatalf("PayPal recency = %d, want %d (inflated bump undone)", paypalNow.LastMessageAtMS, before+1)
	}

	// Shoshana: original kept, duplicate gone, live + outgoing moved in, bound to 2873.
	if got := idsMessageCount(t, harness, shoshanaThread.ConversationID); got != 3 {
		t.Fatalf("Shoshana rows after repair = %d, want 3", got)
	}
	if _, err := harness.store.GetConversation(original.ConversationID); err != nil {
		t.Fatalf("original row lookup: %v", err)
	}
	if n := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM messages WHERE message_id = ?`, dupInPayPal.MessageID); n != 0 {
		t.Fatal("duplicate row survived repair")
	}
	for _, moved := range []sqlite.Message{liveInPayPal, outgoingInPayPal} {
		if n := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM messages WHERE message_id = ? AND conversation_id = ?`, moved.MessageID, shoshanaThread.ConversationID); n != 1 {
			t.Fatalf("row %s not moved to Shoshana's thread", moved.RemoteMessageID)
		}
	}
	bound, err := harness.store.GetConversationByRemote(i01AccountID, "2873")
	if err != nil || bound.ConversationID != shoshanaThread.ConversationID {
		t.Fatalf("2873 bound to %q (err %v), want Shoshana's thread", bound.ConversationID, err)
	}
	if bound.LastMessageAtMS != idsAnimalsTimeMS+1_100_000 {
		t.Fatalf("Shoshana recency = %d, want %d", bound.LastMessageAtMS, idsAnimalsTimeMS+1_100_000)
	}

	// Karl: spam moved to a minted thread under 2916 whose peer is the spammer.
	if got := idsMessageCount(t, harness, karlThread.ConversationID); got != 1 {
		t.Fatalf("Karl rows after repair = %d, want 1", got)
	}
	minted, err := harness.store.GetConversationByRemote(i01AccountID, "2916")
	if err != nil {
		t.Fatalf("GetConversationByRemote(2916): %v", err)
	}
	if minted.ConversationID == karlThread.ConversationID || minted.Kind != sqlite.ConversationKindDirect {
		t.Fatalf("minted thread = %+v", minted)
	}
	if n := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM messages WHERE message_id = ? AND conversation_id = ?`, spamInKarl.MessageID, minted.ConversationID); n != 1 {
		t.Fatal("spam row not moved to the minted thread")
	}
	if peers := idsPeerNumbers(t, harness, minted.ConversationID); len(peers) != 1 || peers[0] != idsCampaignSpam {
		t.Fatalf("minted thread peers = %v", peers)
	}
	if n := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ? AND identity_id = ?`, minted.ConversationID, self.IdentityID); n != 1 {
		t.Fatal("minted thread lacks the self participant")
	}

	// Alex: fresh row merged into his old thread and dropped; 255 re-bound; duplicate removed.
	if _, err := harness.store.GetConversation(freshAlex.ConversationID); err == nil {
		t.Fatal("fresh peerless thread should have been dropped after merge")
	}
	if n := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM messages WHERE message_id = ? AND conversation_id = ?`, alexInFresh.MessageID, alexThread.ConversationID); n != 1 {
		t.Fatal("Alex's new-id row not merged into his thread")
	}
	if n := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM messages WHERE message_id = ?`, dupInAlex.MessageID); n != 0 {
		t.Fatal("re-served duplicate in Alex's thread survived")
	}
	alexBound, err := harness.store.GetConversationByRemote(i01AccountID, "255")
	if err != nil || alexBound.ConversationID != alexThread.ConversationID {
		t.Fatalf("255 bound to %q (err %v), want Alex's thread", alexBound.ConversationID, err)
	}
	if got := idsMessageCount(t, harness, alexThread.ConversationID); got != 2 {
		t.Fatalf("Alex rows after repair = %d, want 2", got)
	}

	// Idempotent: a second plan finds nothing left to do.
	again, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime },
	})
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if again.Moves+again.Deletes+again.Rebinds+again.Mints+again.Drops != 0 || again.Ambiguous != 0 {
		t.Fatalf("second plan is not a no-op: %+v", again)
	}
}

func TestPlanGoogleIDSpaceRepairFlagsOutgoingOnlyCollisions(t *testing.T) {
	harness := i01NewHarness(t, &idsScript{}, nil)
	before := repairWindowMS - 10_000_000
	self := repairSeedIdentity(t, harness, idsSelfNumber, "Max Ghenis", true)
	karl := repairSeedIdentity(t, harness, idsKarl, "", false)
	karlThread := repairSeedConversation(t, harness, "2916", "(202) 602-2529", sqlite.ConversationKindDirect, before, karl, self)
	repairSeedMessage(t, harness, karlThread, "700", &karl, "Vote Racine", before+3, before+3)
	repairSeedMessage(t, harness, karlThread, "85700", nil, "who is this?", repairWindowMS+1, repairWindowMS+1)

	report, err := PlanGoogleIDSpaceRepair(context.Background(), harness.store, harness.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime },
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if report.Ambiguous != 1 || report.Moves != 0 || len(report.Groups) != 1 || report.Groups[0].Verdict != verdictAmbiguous {
		t.Fatalf("outgoing-only collision should be flagged, not moved: %+v", report)
	}
}

func identityRef(raw, name string, self bool) bridge.IdentityRef {
	return bridge.IdentityRef{Raw: raw, Name: name, IsSelf: self}
}

func TestPlanGoogleIDSpaceRepairRestoresClobberedMetadataFromReference(t *testing.T) {
	// The reference is an independent store seeded with the same derived ids
	// as the live store held before the incident.
	reference := i01NewHarness(t, &idsScript{}, nil)
	live := i01NewHarness(t, &idsScript{}, nil)
	before := repairWindowMS - 10_000_000
	inWindow := repairWindowMS + 5_000

	var refUnited, liveUnited sqlite.Conversation
	var liveSelf, liveMiro sqlite.Identity
	for _, h := range []*i01Harness{reference, live} {
		self := repairSeedIdentity(t, h, idsSelfNumber, "Max Ghenis", true)
		united := repairSeedIdentity(t, h, "+18005551234", "United Airlines", false)
		miro := repairSeedIdentity(t, h, "+17077997029", "Miro", false)
		unitedThread := repairSeedConversation(t, h, "2917", "United Airlines", sqlite.ConversationKindDirect, before, united, self)
		repairSeedMessage(t, h, unitedThread, "100", &united, "Your flight is on time", before+1, before+1)
		if h == reference {
			refUnited = unitedThread
		} else {
			liveUnited, liveSelf, liveMiro = unitedThread, self, miro
		}
	}
	if refUnited.ConversationID != liveUnited.ConversationID {
		t.Fatal("reference and live seeds diverged")
	}

	// The pre-fix projector applied Miro's new-phone ConversationEvent (id
	// 2917) to the United Airlines row, then filed Max's outgoing texts there.
	clobbered := liveUnited
	clobbered.Title = "Miro"
	clobbered.UpdatedAtMS = inWindow
	if err := live.store.UpsertConversation(clobbered); err != nil {
		t.Fatalf("clobber title: %v", err)
	}
	if err := live.store.ReplaceConversationParticipants(liveUnited.ConversationID, []sqlite.ConversationParticipant{
		{AccountID: i01AccountID, ConversationID: liveUnited.ConversationID, IdentityID: liveSelf.IdentityID, Role: sqlite.ParticipantRoleMember, DisplayName: "Max Ghenis", IsActive: true},
		{AccountID: i01AccountID, ConversationID: liveUnited.ConversationID, IdentityID: liveMiro.IdentityID, Role: sqlite.ParticipantRoleMember, DisplayName: "Miro", IsActive: true},
	}); err != nil {
		t.Fatalf("clobber roster: %v", err)
	}
	outgoing := repairSeedMessage(t, live, liveUnited, "85900", nil, "on my way", idsAnimalsTimeMS+5_000_000, inWindow+1)

	report, err := PlanGoogleIDSpaceRepair(context.Background(), live.store, live.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime }, Reference: reference.store,
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if report.Restored != 1 || report.Mints != 1 || report.Moves != 1 || report.Ambiguous != 0 {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Groups) != 1 || report.Groups[0].Verdict != verdictClobberedMinted {
		t.Fatalf("groups = %+v", report.Groups)
	}
	if err := ApplyGoogleIDSpaceRepair(context.Background(), live.store, report, i01TestTime); err != nil {
		t.Fatalf("apply: %v", err)
	}

	restored, err := live.store.GetConversation(liveUnited.ConversationID)
	if err != nil {
		t.Fatalf("restored row: %v", err)
	}
	if restored.Title != "United Airlines" || !strings.HasPrefix(restored.RemoteConversationID, sqlite.DisplacedRemoteIDPrefix+"2917:") {
		t.Fatalf("restored row = %+v", restored)
	}
	if peers := idsPeerNumbers(t, live, liveUnited.ConversationID); len(peers) != 1 || peers[0] != "+18005551234" {
		t.Fatalf("restored peers = %v", peers)
	}
	if got := idsMessageCount(t, live, liveUnited.ConversationID); got != 1 {
		t.Fatalf("restored row messages = %d, want 1", got)
	}
	miroThread, err := live.store.GetConversationByRemote(i01AccountID, "2917")
	if err != nil {
		t.Fatalf("GetConversationByRemote(2917): %v", err)
	}
	if miroThread.Title != "Miro" || miroThread.ConversationID == liveUnited.ConversationID {
		t.Fatalf("minted Miro thread = %+v", miroThread)
	}
	if peers := idsPeerNumbers(t, live, miroThread.ConversationID); len(peers) != 1 || peers[0] != "+17077997029" {
		t.Fatalf("Miro thread peers = %v", peers)
	}
	if n := i01QueryInt64(t, live.path, `SELECT COUNT(*) FROM messages WHERE message_id = ? AND conversation_id = ?`, outgoing.MessageID, miroThread.ConversationID); n != 1 {
		t.Fatal("outgoing text did not follow the wire id to Miro's thread")
	}
}
