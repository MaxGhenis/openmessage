package ingest

// End-to-end tests for the repair's duplicate deletes. A planned delete must
// not fail on a reaction or read-receipt intent that still names the
// duplicate (NO ACTION foreign keys), and must not cascade away the inbound
// reactions, snapshot fence and attachments recorded on it: everything moves
// to the copy that stays.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const (
	repairMergeDeviceID = "device-repair-local"
	repairMergeBlobHash = "efefefefefefefefefefefefefefefefefefefefefefefefefefefefefefefef"
)

type repairMergeFixture struct {
	harness *i01Harness
	outbox  *sqlite.OutboxRepository
}

func newRepairMergeFixture(t *testing.T) repairMergeFixture {
	t.Helper()
	harness := i01NewHarness(t, &idsScript{}, nil)
	outbox, err := sqlite.NewOutboxRepository(harness.store, func() time.Time { return i01TestTime })
	if err != nil {
		t.Fatalf("NewOutboxRepository(): %v", err)
	}
	if err := harness.store.UpsertDevice(sqlite.Device{
		DeviceID: repairMergeDeviceID, AccountID: i01AccountID,
		Kind: sqlite.DeviceKindLocalInstallation, State: sqlite.DeviceStateActive,
		CreatedAtMS: i01TestTime.UnixMilli(), UpdatedAtMS: i01TestTime.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertDevice(): %v", err)
	}
	return repairMergeFixture{harness: harness, outbox: outbox}
}

func (f repairMergeFixture) enqueueReaction(t *testing.T, id string, target sqlite.Message) string {
	t.Helper()
	item := sqlite.NewOutboxItem{
		OutboxID: "outbox-" + id, AccountID: i01AccountID, ConversationID: target.ConversationID,
		Kind: sqlite.OutboxKindReaction, IdempotencyKey: "idempotency-" + id, PayloadHash: "hash-" + id,
		Operation: "reaction", TransportRequestID: "request-" + id,
	}
	if _, _, err := f.outbox.EnqueueReaction(context.Background(), item, sqlite.OutboxReaction{
		TargetMessageID: target.MessageID, Emoji: "👍", Action: "add",
	}); err != nil {
		t.Fatalf("EnqueueReaction(%s): %v", id, err)
	}
	return item.OutboxID
}

// enqueueReadReceipt queues a receipt on target, which also moves the
// device's cursor onto it.
func (f repairMergeFixture) enqueueReadReceipt(t *testing.T, id string, target sqlite.Message, readAtMS int64) string {
	t.Helper()
	item := sqlite.NewOutboxItem{
		OutboxID: "outbox-" + id, AccountID: i01AccountID, ConversationID: target.ConversationID,
		Kind: sqlite.OutboxKindRead, IdempotencyKey: "idempotency-" + id, PayloadHash: "hash-" + id,
		Operation: "read_receipt", TransportRequestID: "request-" + id,
	}
	messageID := target.MessageID
	if _, _, err := f.outbox.EnqueueReadReceipt(context.Background(), item, sqlite.OutboxReadReceipt{
		DeviceID: repairMergeDeviceID, LastReadMessageID: target.MessageID, ReadAtMS: readAtMS,
	}, sqlite.ReadCursor{
		AccountID: i01AccountID, DeviceID: repairMergeDeviceID, ConversationID: target.ConversationID,
		LastReadMessageID: &messageID, LastReadAtMS: readAtMS, UpdatedAtMS: readAtMS,
	}); err != nil {
		t.Fatalf("EnqueueReadReceipt(%s): %v", id, err)
	}
	return item.OutboxID
}

func (f repairMergeFixture) setCursor(t *testing.T, target sqlite.Message, readAtMS int64) {
	t.Helper()
	f.setDeviceCursor(t, repairMergeDeviceID, target, readAtMS)
}

func (f repairMergeFixture) setDeviceCursor(t *testing.T, deviceID string, target sqlite.Message, readAtMS int64) {
	t.Helper()
	if err := f.harness.store.UpsertDevice(sqlite.Device{
		DeviceID: deviceID, AccountID: i01AccountID,
		Kind: sqlite.DeviceKindLocalInstallation, State: sqlite.DeviceStateActive,
		CreatedAtMS: i01TestTime.UnixMilli(), UpdatedAtMS: i01TestTime.UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertDevice(): %v", err)
	}
	messageID := target.MessageID
	if err := f.harness.store.UpsertReadCursor(sqlite.ReadCursor{
		AccountID: i01AccountID, DeviceID: deviceID, ConversationID: target.ConversationID,
		LastReadMessageID: &messageID, LastReadAtMS: readAtMS, UpdatedAtMS: readAtMS,
	}); err != nil {
		t.Fatalf("UpsertReadCursor(): %v", err)
	}
}

// queueSend records a text send in state whose local copy is message, as
// EnqueueOutgoingMessage leaves it.
func (f repairMergeFixture) queueSend(t *testing.T, id string, message sqlite.Message, state sqlite.OutboxState) {
	t.Helper()
	database := i01OpenInspector(t, f.harness.path)
	defer database.Close()
	insertRepairSend(t, database, id, message, state)
}

func insertRepairSend(t *testing.T, database *sql.DB, id string, message sqlite.Message, state sqlite.OutboxState) {
	t.Helper()
	nowMS := i01TestTime.UnixMilli()
	if _, err := database.Exec(`
		INSERT INTO outbox (
			outbox_id, account_id, conversation_id, kind, idempotency_key, payload_hash, operation,
			state, local_message_id, transport_request_id, scheduled_for_ms, created_at_ms, updated_at_ms
		) VALUES (?, ?, ?, 'text', ?, ?, 'send_text', ?, ?, ?, ?, ?, ?)
	`, "outbox-"+id, i01AccountID, message.ConversationID, "idempotency-"+id, "hash-"+id, string(state),
		message.MessageID, "request-"+id, nowMS, nowMS, nowMS); err != nil {
		t.Fatalf("queue send %s: %v", id, err)
	}
}

// seedInboundChildren records an embedded reaction snapshot (two reactors and
// its fence) and a downloaded attachment on message.
func (f repairMergeFixture) seedInboundChildren(t *testing.T, message sqlite.Message) {
	t.Helper()
	ctx := context.Background()
	reactions, err := sqlite.NewReactionRepository(f.harness.store, func() time.Time { return i01TestTime })
	if err != nil {
		t.Fatalf("NewReactionRepository(): %v", err)
	}
	if _, err := reactions.ReplaceEmbeddedReactions(ctx, message.MessageID, i01AccountID, message.ConversationID,
		[]sqlite.ReactionSnapshotEntry{
			{ReactorKey: "first-reactor", ReactorLabel: "peer", Emoji: "❤️"},
			{ReactorKey: "second-reactor", ReactorIsSelf: true, Emoji: "😂"},
		},
		message.OccurredAtMS+50,
	); err != nil {
		t.Fatalf("ReplaceEmbeddedReactions(): %v", err)
	}
	database := i01OpenInspector(t, f.harness.path)
	defer database.Close()
	if _, err := database.Exec(`
		INSERT INTO message_attachments (
			message_id, ordinal, remote_id, remote_ref, filename, mime, size_bytes,
			state, blob_hash, last_error, created_at_ms, updated_at_ms
		) VALUES (?, 0, 'media-0', x'', 'photo.png', 'image/png', 42, 'downloaded', ?, NULL, ?, ?)
	`, message.MessageID, repairMergeBlobHash, i01TestTime.UnixMilli(), i01TestTime.UnixMilli()); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
}

func (f repairMergeFixture) assertInboundChildrenOn(t *testing.T, message sqlite.Message) {
	t.Helper()
	path := f.harness.path
	for _, reactor := range []string{"first-reactor", "second-reactor"} {
		if n := i01QueryInt64(t, path, `
			SELECT COUNT(*) FROM reactions
			WHERE message_id = ? AND reactor_key = ? AND conversation_id = ? AND state = 'active'
		`, message.MessageID, reactor, message.ConversationID); n != 1 {
			t.Errorf("reaction %s on %s (conversation %s) count = %d, want 1", reactor, message.RemoteMessageID, message.ConversationID, n)
		}
	}
	if n := i01QueryInt64(t, path, `
		SELECT COUNT(*) FROM reaction_snapshot_fences WHERE message_id = ? AND source_seq_ms = ?
	`, message.MessageID, message.OccurredAtMS+50); n != 1 {
		t.Errorf("snapshot fence on %s is missing", message.RemoteMessageID)
	}
	if n := i01QueryInt64(t, path, `
		SELECT COUNT(*) FROM message_attachments
		WHERE message_id = ? AND ordinal = 0 AND state = 'downloaded' AND blob_hash = ?
	`, message.MessageID, repairMergeBlobHash); n != 1 {
		t.Errorf("downloaded attachment on %s is missing", message.RemoteMessageID)
	}
}

func (f repairMergeFixture) assertIntent(t *testing.T, outboxID string, target sqlite.Message) {
	t.Helper()
	item, err := f.outbox.FindByID(context.Background(), outboxID)
	if err != nil {
		t.Fatalf("FindByID(%s): %v", outboxID, err)
	}
	var targetID string
	switch item.Kind {
	case sqlite.OutboxKindReaction:
		reaction, err := f.outbox.GetOutboxReaction(context.Background(), outboxID)
		if err != nil {
			t.Fatalf("GetOutboxReaction(%s): %v", outboxID, err)
		}
		targetID = reaction.TargetMessageID
	default:
		receipt, err := f.outbox.GetOutboxReadReceipt(context.Background(), outboxID)
		if err != nil {
			t.Fatalf("GetOutboxReadReceipt(%s): %v", outboxID, err)
		}
		targetID = receipt.LastReadMessageID
	}
	if targetID != target.MessageID || item.ConversationID != target.ConversationID {
		t.Errorf("intent %s = target %q in %q, want %q in %q",
			outboxID, targetID, item.ConversationID, target.MessageID, target.ConversationID)
	}
}

func (f repairMergeFixture) assertForeignKeysClean(t *testing.T) {
	t.Helper()
	database := i01OpenInspector(t, f.harness.path)
	defer database.Close()
	rows, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("PRAGMA foreign_key_check: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var fk int
		if err := rows.Scan(&table, &rowID, &parent, &fk); err != nil {
			t.Fatalf("scan foreign_key_check: %v", err)
		}
		t.Errorf("foreign_key_check violation: table=%q rowid=%v parent=%q fk=%d", table, rowID, parent, fk)
	}
}

func TestGoogleIDSpaceRepairMergesDuplicatesIntoSurvivors(t *testing.T) {
	fixture := newRepairMergeFixture(t)
	harness := fixture.harness
	ctx := context.Background()
	before := repairWindowMS - 10_000_000
	inWindow := repairWindowMS + 5_000

	self := repairSeedIdentity(t, harness, idsSelfNumber, "Max Ghenis", true)
	shoshana := repairSeedIdentity(t, harness, idsShoshana, "Shoshana Weissmann", false)
	paypal := repairSeedIdentity(t, harness, idsPayPal, "", false)
	alex := repairSeedIdentity(t, harness, "+13479184547", "Alex Armlovich", false)
	paypalThread := repairSeedConversation(t, harness, "2873", "72975", sqlite.ConversationKindDirect, before, paypal, self)
	shoshanaThread := repairSeedConversation(t, harness, "3093", "Shoshana Weissmann", sqlite.ConversationKindDirect, before, shoshana, self)
	alexThread := repairSeedConversation(t, harness, "2764", "Alex Armlovich", sqlite.ConversationKindDirect, before, alex, self)

	paypalOwn := repairSeedMessage(t, harness, paypalThread, "500", &paypal, "Your PayPal code is 123456", before+1, before+1)
	original := repairSeedMessage(t, harness, shoshanaThread, "86133", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, before+2)
	alexOriginal := repairSeedMessage(t, harness, alexThread, "800", &alex, "coffee?", before+4, before+4)

	// Misfiled re-served duplicate: planMoves deletes it in the PayPal thread
	// and keeps the original in Shoshana's thread.
	dupInPayPal := repairSeedMessage(t, harness, paypalThread, "59", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, inWindow)
	liveInPayPal := repairSeedMessage(t, harness, paypalThread, "85793", &shoshana, "see you at 12", idsAnimalsTimeMS+1_000_000, inWindow+1)
	// Re-served duplicate in a consistent thread: dedupeWithin deletes it and
	// keeps the older row in the same thread.
	dupInAlex := repairSeedMessage(t, harness, alexThread, "9001", &alex, "coffee?", before+4, inWindow+5)

	fixture.seedInboundChildren(t, dupInPayPal)
	fixture.seedInboundChildren(t, dupInAlex)
	reactOnPayPalDup := fixture.enqueueReaction(t, "react-paypal-dup", dupInPayPal)
	receiptOnPayPalDup := fixture.enqueueReadReceipt(t, "read-paypal-dup", dupInPayPal, inWindow+10)
	// The device has since read further in the PayPal thread, so only the
	// receipt intent still names the duplicate.
	fixture.setCursor(t, paypalOwn, inWindow+11)
	reactOnLive := fixture.enqueueReaction(t, "react-paypal-live", liveInPayPal)
	reactOnAlexDup := fixture.enqueueReaction(t, "react-alex-dup", dupInAlex)
	// A cursor on a same-thread duplicate follows it to the surviving copy.
	receiptOnAlexDup := fixture.enqueueReadReceipt(t, "read-alex-dup", dupInAlex, inWindow+12)

	report, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime },
	})
	if err != nil {
		t.Fatalf("PlanGoogleIDSpaceRepair: %v", err)
	}
	survivors := map[string]string{}
	for _, step := range report.Steps {
		if step.Op == "delete" {
			survivors[step.MessageID] = step.SurvivorMessageID
		}
	}
	if survivors[dupInPayPal.MessageID] != original.MessageID || survivors[dupInAlex.MessageID] != alexOriginal.MessageID || len(survivors) != 2 {
		t.Fatalf("delete steps' survivors = %v, want %s->%s and %s->%s", survivors,
			dupInPayPal.MessageID, original.MessageID, dupInAlex.MessageID, alexOriginal.MessageID)
	}

	if err := ApplyGoogleIDSpaceRepair(ctx, harness.store, report, i01TestTime); err != nil {
		t.Fatalf("ApplyGoogleIDSpaceRepair: %v", err)
	}

	for _, gone := range []sqlite.Message{dupInPayPal, dupInAlex} {
		if _, err := harness.messages.GetMessage(ctx, gone.MessageID); !errors.Is(err, sqlite.ErrNotFound) {
			t.Fatalf("duplicate %s: GetMessage error = %v, want ErrNotFound", gone.RemoteMessageID, err)
		}
	}
	fixture.assertInboundChildrenOn(t, original)
	fixture.assertInboundChildrenOn(t, alexOriginal)
	fixture.assertIntent(t, reactOnPayPalDup, original)
	fixture.assertIntent(t, receiptOnPayPalDup, original)
	fixture.assertIntent(t, reactOnAlexDup, alexOriginal)
	fixture.assertIntent(t, receiptOnAlexDup, alexOriginal)
	movedLive := liveInPayPal
	movedLive.ConversationID = shoshanaThread.ConversationID
	fixture.assertIntent(t, reactOnLive, movedLive)

	alexCursor, err := harness.store.GetReadCursor(repairMergeDeviceID, alexThread.ConversationID)
	if err != nil || alexCursor.LastReadMessageID == nil || *alexCursor.LastReadMessageID != alexOriginal.MessageID {
		t.Fatalf("Alex cursor = (%+v, %v), want it on the surviving row", alexCursor, err)
	}
	payPalCursor, err := harness.store.GetReadCursor(repairMergeDeviceID, paypalThread.ConversationID)
	if err != nil || payPalCursor.LastReadMessageID == nil || *payPalCursor.LastReadMessageID != paypalOwn.MessageID {
		t.Fatalf("PayPal cursor = (%+v, %v), want it unchanged", payPalCursor, err)
	}
	fixture.assertForeignKeysClean(t)

	again, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime },
	})
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if again.Moves+again.Deletes+again.Rebinds+again.Mints+again.Drops != 0 {
		t.Fatalf("second plan is not a no-op: %+v", again)
	}
}

// A misfiled row whose remote id the target thread already uses can't move
// there: UNIQUE(account_id, conversation_id, remote_message_id) would fail
// the whole plan. Same content means it's the same message, so it's deleted
// into that row; different content leaves it in place.
func TestGoogleIDSpaceRepairHandlesRemoteIDTakenInTarget(t *testing.T) {
	fixture := newRepairMergeFixture(t)
	harness := fixture.harness
	ctx := context.Background()
	before := repairWindowMS - 10_000_000
	inWindow := repairWindowMS + 5_000

	self := repairSeedIdentity(t, harness, idsSelfNumber, "Max Ghenis", true)
	shoshana := repairSeedIdentity(t, harness, idsShoshana, "Shoshana Weissmann", false)
	paypal := repairSeedIdentity(t, harness, idsPayPal, "", false)
	paypalThread := repairSeedConversation(t, harness, "2873", "72975", sqlite.ConversationKindDirect, before, paypal, self)
	shoshanaThread := repairSeedConversation(t, harness, "3093", "Shoshana Weissmann", sqlite.ConversationKindDirect, before, shoshana, self)

	repairSeedMessage(t, harness, paypalThread, "500", &paypal, "Your PayPal code is 123456", before+1, before+1)
	sameContent := repairSeedMessage(t, harness, shoshanaThread, "61", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, before+2)
	repairSeedMessage(t, harness, shoshanaThread, "62", &shoshana, "an old-phone message", idsAnimalsTimeMS+10, before+3)

	// New-space ids 61 and 62 collide with old-space ids in Shoshana's thread.
	sameInPayPal := repairSeedMessage(t, harness, paypalThread, "61", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, inWindow)
	differentInPayPal := repairSeedMessage(t, harness, paypalThread, "62", &shoshana, "a new-phone message", idsAnimalsTimeMS+20, inWindow+1)
	fixture.seedInboundChildren(t, sameInPayPal)
	reaction := fixture.enqueueReaction(t, "react-same", sameInPayPal)

	report, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime },
	})
	if err != nil {
		t.Fatalf("PlanGoogleIDSpaceRepair: %v", err)
	}
	if report.Moves != 0 || report.Deletes != 1 || report.Drops != 0 {
		t.Fatalf("report = %+v, want one delete and no move or drop", report)
	}
	if err := ApplyGoogleIDSpaceRepair(ctx, harness.store, report, i01TestTime); err != nil {
		t.Fatalf("ApplyGoogleIDSpaceRepair: %v", err)
	}
	if _, err := harness.messages.GetMessage(ctx, sameInPayPal.MessageID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("same-content row: GetMessage error = %v, want ErrNotFound", err)
	}
	fixture.assertInboundChildrenOn(t, sameContent)
	fixture.assertIntent(t, reaction, sameContent)
	left, err := harness.messages.GetMessage(ctx, differentInPayPal.MessageID)
	if err != nil || left.ConversationID != paypalThread.ConversationID {
		t.Fatalf("different-content row = (%+v, %v), want it left in the PayPal thread", left, err)
	}
	fixture.assertForeignKeysClean(t)
}

// A misfiled row's content match in the target can itself be a duplicate the
// plan deletes. The step must name the copy that stays: re-served row R in
// the PayPal thread shares remote id 61 with the original O in Shoshana's
// thread, so the content lookup skips O and finds F, a re-served copy of O
// that dedupeWithin deletes into O.
func TestGoogleIDSpaceRepairResolvesSurvivorChains(t *testing.T) {
	fixture := newRepairMergeFixture(t)
	harness := fixture.harness
	ctx := context.Background()
	before := repairWindowMS - 10_000_000
	inWindow := repairWindowMS + 5_000

	self := repairSeedIdentity(t, harness, idsSelfNumber, "Max Ghenis", true)
	shoshana := repairSeedIdentity(t, harness, idsShoshana, "Shoshana Weissmann", false)
	paypal := repairSeedIdentity(t, harness, idsPayPal, "", false)
	paypalThread := repairSeedConversation(t, harness, "2873", "72975", sqlite.ConversationKindDirect, before, paypal, self)
	shoshanaThread := repairSeedConversation(t, harness, "3093", "Shoshana Weissmann", sqlite.ConversationKindDirect, before, shoshana, self)

	repairSeedMessage(t, harness, paypalThread, "500", &paypal, "Your PayPal code is 123456", before+1, before+1)
	original := repairSeedMessage(t, harness, shoshanaThread, "61", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, before+2)
	copyInThread := repairSeedMessage(t, harness, shoshanaThread, "9", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, inWindow)
	misfiled := repairSeedMessage(t, harness, paypalThread, "61", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, inWindow+1)
	fixture.seedInboundChildren(t, misfiled)
	reaction := fixture.enqueueReaction(t, "react-chain", misfiled)

	report, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime },
	})
	if err != nil {
		t.Fatalf("PlanGoogleIDSpaceRepair: %v", err)
	}
	survivors := map[string]string{}
	for _, step := range report.Steps {
		if step.Op == "delete" {
			survivors[step.MessageID] = step.SurvivorMessageID
		}
	}
	if len(survivors) != 2 || survivors[copyInThread.MessageID] != original.MessageID || survivors[misfiled.MessageID] != original.MessageID {
		t.Fatalf("delete survivors = %v, want both copies merged into the original %s", survivors, original.MessageID)
	}
	if err := ApplyGoogleIDSpaceRepair(ctx, harness.store, report, i01TestTime); err != nil {
		t.Fatalf("ApplyGoogleIDSpaceRepair: %v", err)
	}
	fixture.assertInboundChildrenOn(t, original)
	fixture.assertIntent(t, reaction, original)
	if got := idsMessageCount(t, harness, shoshanaThread.ConversationID); got != 1 {
		t.Fatalf("Shoshana rows = %d, want only the original", got)
	}
	fixture.assertForeignKeysClean(t)
}

// Two misfiled threads routed into each other, each holding a copy of the
// same outgoing message: each copy is a duplicate of content in the other's
// target. Deleting both would lose the message; one copy must stay.
func TestGoogleIDSpaceRepairKeepsOneCopyWhenThreadsSwap(t *testing.T) {
	fixture := newRepairMergeFixture(t)
	harness := fixture.harness
	ctx := context.Background()
	before := repairWindowMS - 10_000_000
	inWindow := repairWindowMS + 5_000

	self := repairSeedIdentity(t, harness, idsSelfNumber, "Max Ghenis", true)
	shoshana := repairSeedIdentity(t, harness, idsShoshana, "Shoshana Weissmann", false)
	karl := repairSeedIdentity(t, harness, idsKarl, "", false)
	shoshanaThread := repairSeedConversation(t, harness, "3093", "Shoshana Weissmann", sqlite.ConversationKindDirect, before, shoshana, self)
	karlThread := repairSeedConversation(t, harness, "2916", "(202) 602-2529", sqlite.ConversationKindDirect, before, karl, self)
	repairSeedMessage(t, harness, shoshanaThread, "86133", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, before+1)
	repairSeedMessage(t, harness, karlThread, "700", &karl, "Vote Racine", before+2, before+2)

	// The re-keyed ids swapped the two threads: Karl's texts land in
	// Shoshana's thread and hers in his, and the outgoing reply was re-served
	// into both.
	repairSeedMessage(t, harness, shoshanaThread, "41", &karl, "Donate today", idsAnimalsTimeMS+1_000, inWindow)
	inShoshana := repairSeedMessage(t, harness, shoshanaThread, "42", nil, "who is this?", idsAnimalsTimeMS+2_000, inWindow+1)
	repairSeedMessage(t, harness, karlThread, "43", &shoshana, "lunch?", idsAnimalsTimeMS+3_000, inWindow+2)
	inKarl := repairSeedMessage(t, harness, karlThread, "44", nil, "who is this?", idsAnimalsTimeMS+2_000, inWindow+3)
	fixture.seedInboundChildren(t, inShoshana)
	reactShoshana := fixture.enqueueReaction(t, "react-swap-shoshana", inShoshana)
	reactKarl := fixture.enqueueReaction(t, "react-swap-karl", inKarl)

	report, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime },
	})
	if err != nil {
		t.Fatalf("PlanGoogleIDSpaceRepair: %v", err)
	}
	if report.Deletes != 1 {
		t.Fatalf("report deletes = %d, want exactly one of the two copies: %+v", report.Deletes, report.Steps)
	}
	if err := ApplyGoogleIDSpaceRepair(ctx, harness.store, report, i01TestTime); err != nil {
		t.Fatalf("ApplyGoogleIDSpaceRepair: %v", err)
	}
	if n := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM messages WHERE body = 'who is this?'`); n != 1 {
		t.Fatalf("copies of the outgoing reply = %d, want 1", n)
	}
	var kept sqlite.Message
	for _, candidate := range []sqlite.Message{inShoshana, inKarl} {
		if message, err := harness.messages.GetMessage(ctx, candidate.MessageID); err == nil {
			kept = message
		}
	}
	fixture.assertInboundChildrenOn(t, kept)
	fixture.assertIntent(t, reactShoshana, kept)
	fixture.assertIntent(t, reactKarl, kept)
	fixture.assertForeignKeysClean(t)
}

// An unfinished send reads its local copy inside its own conversation at
// dispatch, so the repair must neither delete that row as a duplicate nor
// move it to another thread, even when a cursor would follow it.
func TestGoogleIDSpaceRepairLeavesRowsAnOpenSendNames(t *testing.T) {
	fixture := newRepairMergeFixture(t)
	harness := fixture.harness
	ctx := context.Background()
	before := repairWindowMS - 10_000_000
	inWindow := repairWindowMS + 5_000

	self := repairSeedIdentity(t, harness, idsSelfNumber, "Max Ghenis", true)
	shoshana := repairSeedIdentity(t, harness, idsShoshana, "Shoshana Weissmann", false)
	paypal := repairSeedIdentity(t, harness, idsPayPal, "", false)
	karl := repairSeedIdentity(t, harness, idsKarl, "", false)
	paypalThread := repairSeedConversation(t, harness, "2873", "72975", sqlite.ConversationKindDirect, before, paypal, self)
	repairSeedConversation(t, harness, "3093", "Shoshana Weissmann", sqlite.ConversationKindDirect, before, shoshana, self)
	karlThread := repairSeedConversation(t, harness, "2916", "(202) 602-2529", sqlite.ConversationKindDirect, before, karl, self)

	// Karl's thread holds only outgoing rows in the window, so dedupeWithin
	// plans it. The optimistic row restates an older outgoing row exactly.
	repairSeedMessage(t, harness, karlThread, "700", &karl, "Vote Racine", before+1, before+1)
	repairSeedMessage(t, harness, karlThread, "701", nil, "hello", idsAnimalsTimeMS, before+2)
	optimistic := repairSeedMessage(t, harness, karlThread, "request-1", nil, "hello", idsAnimalsTimeMS, inWindow)
	fixture.setCursor(t, optimistic, inWindow+1)
	fixture.queueSend(t, "send-karl", optimistic, sqlite.OutboxQueued)

	// A misfiled PayPal-thread window carrying an outgoing reply that is
	// still sending: the incoming row moves to Shoshana's thread, the reply
	// stays with its send.
	repairSeedMessage(t, harness, paypalThread, "500", &paypal, "Your PayPal code is 123456", before+3, before+3)
	repairSeedMessage(t, harness, paypalThread, "85793", &shoshana, "see you at 12", idsAnimalsTimeMS+1_000, inWindow+2)
	sending := repairSeedMessage(t, harness, paypalThread, "request-2", nil, "sounds good", idsAnimalsTimeMS+2_000, inWindow+3)
	fixture.queueSend(t, "send-paypal", sending, sqlite.OutboxUncertain)

	report, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime },
	})
	if err != nil {
		t.Fatalf("PlanGoogleIDSpaceRepair: %v", err)
	}
	for _, step := range report.Steps {
		if step.MessageID == optimistic.MessageID || step.MessageID == sending.MessageID {
			t.Fatalf("plan touches a row an open send names: %+v", step)
		}
	}
	if report.Moves != 1 {
		t.Fatalf("report moves = %d, want only the incoming PayPal-thread row: %+v", report.Moves, report.Steps)
	}
	if err := ApplyGoogleIDSpaceRepair(ctx, harness.store, report, i01TestTime); err != nil {
		t.Fatalf("ApplyGoogleIDSpaceRepair: %v", err)
	}
	for _, kept := range []sqlite.Message{optimistic, sending} {
		message, err := harness.messages.GetMessage(ctx, kept.MessageID)
		if err != nil || message.ConversationID != kept.ConversationID {
			t.Fatalf("row %s = (%+v, %v), want it kept in %s", kept.RemoteMessageID, message, err, kept.ConversationID)
		}
	}
	fixture.assertForeignKeysClean(t)
}

// Rows a cursor pins stay in their thread; a later pinned copy of the same
// message is merged into the earlier one, not only into the oldest copy (which
// here moves to its sender's thread).
func TestGoogleIDSpaceRepairMergesPinnedCopiesIntoOneAnother(t *testing.T) {
	fixture := newRepairMergeFixture(t)
	harness := fixture.harness
	ctx := context.Background()
	before := repairWindowMS - 10_000_000
	inWindow := repairWindowMS + 5_000

	self := repairSeedIdentity(t, harness, idsSelfNumber, "Max Ghenis", true)
	shoshana := repairSeedIdentity(t, harness, idsShoshana, "Shoshana Weissmann", false)
	paypal := repairSeedIdentity(t, harness, idsPayPal, "", false)
	paypalThread := repairSeedConversation(t, harness, "2873", "72975", sqlite.ConversationKindDirect, before, paypal, self)
	shoshanaThread := repairSeedConversation(t, harness, "3093", "Shoshana Weissmann", sqlite.ConversationKindDirect, before, shoshana, self)
	repairSeedMessage(t, harness, paypalThread, "500", &paypal, "Your PayPal code is 123456", before+1, before+1)
	repairSeedMessage(t, harness, shoshanaThread, "86133", &shoshana, "older", idsAnimalsTimeMS-1_000, before+2)

	first := repairSeedMessage(t, harness, paypalThread, "61", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, inWindow)
	second := repairSeedMessage(t, harness, paypalThread, "62", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, inWindow+1)
	third := repairSeedMessage(t, harness, paypalThread, "63", &shoshana, idsAnimalsBody, idsAnimalsTimeMS, inWindow+2)
	fixture.setDeviceCursor(t, "device-repair-phone", second, inWindow+3)
	fixture.setDeviceCursor(t, "device-repair-laptop", third, inWindow+4)
	fixture.seedInboundChildren(t, third)

	report, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime },
	})
	if err != nil {
		t.Fatalf("PlanGoogleIDSpaceRepair: %v", err)
	}
	if err := ApplyGoogleIDSpaceRepair(ctx, harness.store, report, i01TestTime); err != nil {
		t.Fatalf("ApplyGoogleIDSpaceRepair: %v", err)
	}
	moved, err := harness.messages.GetMessage(ctx, first.MessageID)
	if err != nil || moved.ConversationID != shoshanaThread.ConversationID {
		t.Fatalf("first copy = (%+v, %v), want it moved to Shoshana's thread", moved, err)
	}
	if _, err := harness.messages.GetMessage(ctx, third.MessageID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("third copy: GetMessage error = %v, want it merged into the second", err)
	}
	fixture.assertInboundChildrenOn(t, second)
	for _, device := range []string{"device-repair-phone", "device-repair-laptop"} {
		cursor, err := harness.store.GetReadCursor(device, paypalThread.ConversationID)
		if err != nil || cursor.LastReadMessageID == nil || *cursor.LastReadMessageID != second.MessageID {
			t.Fatalf("%s cursor = (%+v, %v), want it on the second copy", device, cursor, err)
		}
	}
	fixture.assertForeignKeysClean(t)

	again, err := PlanGoogleIDSpaceRepair(ctx, harness.store, harness.messages, IDSpaceRepairOptions{
		AccountID: i01AccountID, SinceMS: repairWindowMS, Now: func() time.Time { return i01TestTime },
	})
	if err != nil || again.Deletes != 0 {
		t.Fatalf("second plan = (%+v, %v), want no deletes", again.Steps, err)
	}
}
