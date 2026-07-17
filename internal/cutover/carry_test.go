package cutover

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const carryTestNowMS int64 = 2_100_000_000_000

func TestCutoverCarriesPendingOutbox(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(carryTestNowMS)
	old := openCarryTestEndpoint(t, "old", now)
	fresh := openCarryTestEndpoint(t, "fresh", now.Add(time.Second))
	seedCarryAccount(t, old.Store, "account-a")
	seedCarryAccount(t, fresh.Store, "account-a")
	seedCarryConversation(t, old.Store, "old-conversation", "account-a", "remote-thread")
	seedCarryConversation(t, fresh.Store, "fresh-conversation", "account-a", "remote-thread")

	futureMS := now.Add(24 * time.Hour).UnixMilli()
	queued := enqueueCarryText(t, ctx, old, "queued", "old-conversation", futureMS)

	mediaBytes := []byte("pending attachment bytes")
	mediaRef, err := old.BlobStore.Put(ctx, bytes.NewReader(mediaBytes), "image/png", int64(len(mediaBytes)))
	if err != nil {
		t.Fatalf("put old media blob: %v", err)
	}
	notDispatched := enqueueCarryMedia(
		t,
		ctx,
		old,
		"not-dispatched",
		"old-conversation",
		carryTestNowMS,
		mediaRef,
	)
	notDispatchedLease := leaseCarryItem(t, ctx, old, now)
	if notDispatchedLease.OutboxID != notDispatched.OutboxID {
		t.Fatalf("leased outbox ID = %q, want %q", notDispatchedLease.OutboxID, notDispatched.OutboxID)
	}
	if err := old.OutboxRepository.MarkNotDispatched(
		ctx,
		notDispatched.OutboxID,
		*notDispatchedLease.LeaseToken,
		"transport",
		"offline",
		"scripted pre-call failure",
		now.Add(time.Minute),
	); err != nil {
		t.Fatalf("mark not dispatched: %v", err)
	}

	confirmed := enqueueCarryText(t, ctx, old, "confirmed", "old-conversation", carryTestNowMS)
	confirmedLease := leaseCarryItem(t, ctx, old, now)
	markCarryTransportCalled(t, ctx, old, confirmedLease)
	if err := old.OutboxRepository.Confirm(ctx, sqlite.Confirmation{
		OutboxID:       confirmed.OutboxID,
		LeaseToken:     *confirmedLease.LeaseToken,
		ResultRemoteID: "confirmed-remote",
	}); err != nil {
		t.Fatalf("confirm outbox row: %v", err)
	}

	uncertain := enqueueCarryText(t, ctx, old, "uncertain", "old-conversation", carryTestNowMS)
	uncertainLease := leaseCarryItem(t, ctx, old, now)
	markCarryTransportCalled(t, ctx, old, uncertainLease)
	if err := old.OutboxRepository.MarkUncertain(
		ctx,
		uncertain.OutboxID,
		*uncertainLease.LeaseToken,
		"transport",
		"timeout",
		"result unknown",
	); err != nil {
		t.Fatalf("mark uncertain: %v", err)
	}

	dispatching := enqueueCarryText(t, ctx, old, "dispatching", "old-conversation", carryTestNowMS)
	dispatchingLease := leaseCarryItem(t, ctx, old, now)
	if dispatchingLease.OutboxID != dispatching.OutboxID {
		t.Fatalf("dispatching lease ID = %q, want %q", dispatchingLease.OutboxID, dispatching.OutboxID)
	}

	storeFailed := enqueueCarryText(t, ctx, old, "store-failed", "old-conversation", carryTestNowMS)
	storeFailedLease := leaseCarryItem(t, ctx, old, now)
	markCarryTransportCalled(t, ctx, old, storeFailedLease)
	if err := old.OutboxRepository.MarkStoreFailed(
		ctx,
		storeFailed.OutboxID,
		*storeFailedLease.LeaseToken,
		"accepted-remote",
		"projection failed",
	); err != nil {
		t.Fatalf("mark store failed: %v", err)
	}

	rejected := enqueueCarryText(t, ctx, old, "rejected", "old-conversation", carryTestNowMS)
	rejectedLease := leaseCarryItem(t, ctx, old, now)
	if err := old.OutboxRepository.Reject(
		ctx,
		rejected.OutboxID,
		*rejectedLease.LeaseToken,
		"remote",
		"invalid",
		"rejected",
	); err != nil {
		t.Fatalf("reject outbox row: %v", err)
	}

	canceled := enqueueCarryText(t, ctx, old, "canceled", "old-conversation", carryTestNowMS)
	if err := old.OutboxRepository.Cancel(ctx, canceled.OutboxID); err != nil {
		t.Fatalf("cancel outbox row: %v", err)
	}

	report, err := CarryPendingOutbox(ctx, old, fresh)
	if err != nil {
		t.Fatalf("CarryPendingOutbox(): %v", err)
	}
	assertCarryEntryIDs(t, "carried", report.Carried, []string{queued.OutboxID, notDispatched.OutboxID})
	assertCarryEntryStates(t, "reported", report.Reported, []sqlite.OutboxState{
		sqlite.OutboxUncertain,
		sqlite.OutboxDispatching,
		sqlite.OutboxStoreFailed,
	})
	if len(report.Uncarryable) != 0 {
		t.Fatalf("uncarryable = %+v, want none", report.Uncarryable)
	}

	freshRows, err := fresh.OutboxRepository.ListCarryable(ctx)
	if err != nil {
		t.Fatalf("fresh ListCarryable(): %v", err)
	}
	if len(freshRows) != 2 {
		t.Fatalf("fresh carryable rows = %+v, want exactly two", freshRows)
	}
	byID := make(map[string]sqlite.CarryableIntent, len(freshRows))
	for _, row := range freshRows {
		byID[row.OutboxID] = row
		if row.ConversationID != "fresh-conversation" || row.RemoteConversationID != "remote-thread" {
			t.Fatalf("carried conversation mapping = %+v", row)
		}
	}
	if got := byID[queued.OutboxID]; got.IdempotencyKey != queued.IdempotencyKey ||
		got.ScheduledForMS != futureMS || got.Body != "body-queued" ||
		got.ReplyToRemoteID == nil || *got.ReplyToRemoteID != "reply-remote-queued" {
		t.Fatalf("carried queued row = %+v", got)
	}
	if got := byID[notDispatched.OutboxID]; got.IdempotencyKey != notDispatched.IdempotencyKey ||
		got.ScheduledForMS != carryTestNowMS || got.BlobHash != mediaRef.Hash ||
		got.BlobSizeBytes != int64(len(mediaBytes)) || got.MIME != "image/png" ||
		got.Filename != "pending.png" {
		t.Fatalf("carried not-dispatched media row = %+v", got)
	}
	reader, err := fresh.BlobStore.Open(blob.BlobRef{Hash: mediaRef.Hash})
	if err != nil {
		t.Fatalf("open carried media blob: %v", err)
	}
	carriedMedia, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read carried media blob: read=%v close=%v", readErr, closeErr)
	}
	if !bytes.Equal(carriedMedia, mediaBytes) {
		t.Fatalf("carried media bytes = %q, want %q", carriedMedia, mediaBytes)
	}

	second, err := CarryPendingOutbox(ctx, old, fresh)
	if err != nil {
		t.Fatalf("CarryPendingOutbox(second): %v", err)
	}
	if len(second.Carried) != 0 {
		t.Fatalf("second carried = %+v, want no double-enqueue", second.Carried)
	}
	assertCarryEntryStates(t, "second reported", second.Reported, []sqlite.OutboxState{
		sqlite.OutboxUncertain,
		sqlite.OutboxDispatching,
		sqlite.OutboxStoreFailed,
	})
	secondRows, err := fresh.OutboxRepository.ListCarryable(ctx)
	if err != nil {
		t.Fatalf("fresh ListCarryable(second): %v", err)
	}
	if len(secondRows) != 2 {
		t.Fatalf("fresh rows after second carry = %d, want 2", len(secondRows))
	}
}

func TestCarryPendingOutboxReportsUncarryableRows(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(carryTestNowMS)
	old := openCarryTestEndpoint(t, "old", now)
	fresh := openCarryTestEndpoint(t, "fresh", now)
	seedCarryAccount(t, old.Store, "account-a")
	seedCarryAccount(t, fresh.Store, "account-a")
	seedCarryConversation(t, old.Store, "old-with-fresh-match", "account-a", "remote-with-match")
	seedCarryConversation(t, fresh.Store, "fresh-match", "account-a", "remote-with-match")
	seedCarryConversation(t, old.Store, "old-without-fresh-match", "account-a", "remote-without-match")

	missingRef := blob.BlobRef{
		Hash: "abababababababababababababababababababababababababababababababab",
		Size: 25,
		MIME: "image/png",
	}
	missingBlob := enqueueCarryMedia(
		t,
		ctx,
		old,
		"missing-blob",
		"old-with-fresh-match",
		carryTestNowMS,
		missingRef,
	)
	unresolved := enqueueCarryText(
		t,
		ctx,
		old,
		"unresolved-conversation",
		"old-without-fresh-match",
		carryTestNowMS,
	)

	report, err := CarryPendingOutbox(ctx, old, fresh)
	if err != nil {
		t.Fatalf("CarryPendingOutbox(): %v", err)
	}
	if len(report.Carried) != 0 || len(report.Reported) != 0 || len(report.Uncarryable) != 2 {
		t.Fatalf("report = %+v, want two uncarryable rows only", report)
	}
	reasons := map[string]string{}
	for _, entry := range report.Uncarryable {
		reasons[entry.OutboxID] = entry.Reason
	}
	if !strings.Contains(reasons[missingBlob.OutboxID], "blob") ||
		!strings.Contains(reasons[unresolved.OutboxID], "conversation") {
		t.Fatalf("uncarryable reasons = %+v", reasons)
	}
	rows, err := fresh.OutboxRepository.ListCarryable(ctx)
	if err != nil {
		t.Fatalf("fresh ListCarryable(): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("fresh carryable rows = %+v, want none", rows)
	}
}

func openCarryTestEndpoint(t *testing.T, name string, now time.Time) Endpoint {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create %s store root: %v", name, err)
	}
	store, err := sqlite.Open(filepath.Join(root, "store.sqlite3"))
	if err != nil {
		t.Fatalf("open %s sqlite store: %v", name, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close %s sqlite store: %v", name, err)
		}
	})
	blobs, err := blob.New(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatalf("open %s blob store: %v", name, err)
	}
	endpoint, err := NewEndpoint(store, blobs, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewEndpoint(%s): %v", name, err)
	}
	return endpoint
}

func seedCarryAccount(t *testing.T, store *sqlite.Store, accountID string) {
	t.Helper()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   accountID,
		BridgeKey:   "scripted",
		DisplayName: "Scripted",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: carryTestNowMS,
		UpdatedAtMS: carryTestNowMS,
	}); err != nil {
		t.Fatalf("seed account %q: %v", accountID, err)
	}
}

func seedCarryConversation(
	t *testing.T,
	store *sqlite.Store,
	conversationID, accountID, remoteConversationID string,
) {
	t.Helper()
	if err := store.UpsertConversation(sqlite.Conversation{
		ConversationID:       conversationID,
		AccountID:            accountID,
		RemoteConversationID: remoteConversationID,
		Kind:                 sqlite.ConversationKindDirect,
		Title:                remoteConversationID,
		NotificationMode:     sqlite.NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          carryTestNowMS,
		UpdatedAtMS:          carryTestNowMS,
	}); err != nil {
		t.Fatalf("seed conversation %q: %v", conversationID, err)
	}
}

func enqueueCarryText(
	t *testing.T,
	ctx context.Context,
	endpoint Endpoint,
	id, conversationID string,
	scheduledForMS int64,
) sqlite.OutboxItem {
	t.Helper()
	item := carryTestItem(id, conversationID, sqlite.OutboxKindText, scheduledForMS)
	reply := "reply-remote-" + id
	row, disposition, err := endpoint.OutboxRepository.EnqueueOutgoingMessage(ctx, item, sqlite.Message{
		MessageID:       item.LocalMessageID,
		ConversationID:  conversationID,
		AccountID:       item.AccountID,
		RemoteMessageID: item.TransportRequestID,
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            "body-" + id,
		ReplyToRemoteID: &reply,
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    carryTestNowMS,
	})
	if err != nil {
		t.Fatalf("enqueue text %q: %v", id, err)
	}
	if disposition != sqlite.EnqueueInserted {
		t.Fatalf("enqueue text %q disposition = %q", id, disposition)
	}
	return row
}

func enqueueCarryMedia(
	t *testing.T,
	ctx context.Context,
	endpoint Endpoint,
	id, conversationID string,
	scheduledForMS int64,
	ref blob.BlobRef,
) sqlite.OutboxItem {
	t.Helper()
	item := carryTestItem(id, conversationID, sqlite.OutboxKindMedia, scheduledForMS)
	row, disposition, err := endpoint.OutboxRepository.EnqueueOutgoingMediaMessage(ctx, item, sqlite.Message{
		MessageID:       item.LocalMessageID,
		ConversationID:  conversationID,
		AccountID:       item.AccountID,
		RemoteMessageID: item.TransportRequestID,
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            "caption-" + id,
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    carryTestNowMS,
	}, sqlite.OutboxAttachment{
		BlobHash:  ref.Hash,
		SizeBytes: ref.Size,
		MIME:      ref.MIME,
		Filename:  "pending.png",
	})
	if err != nil {
		t.Fatalf("enqueue media %q: %v", id, err)
	}
	if disposition != sqlite.EnqueueInserted {
		t.Fatalf("enqueue media %q disposition = %q", id, disposition)
	}
	return row
}

func carryTestItem(
	id, conversationID string,
	kind sqlite.OutboxKind,
	scheduledForMS int64,
) sqlite.NewOutboxItem {
	operation := "send_text"
	if kind == sqlite.OutboxKindMedia {
		operation = "send_media"
	}
	return sqlite.NewOutboxItem{
		OutboxID:           "outbox-" + id,
		AccountID:          "account-a",
		ConversationID:     conversationID,
		Kind:               kind,
		IdempotencyKey:     "idempotency-" + id,
		PayloadHash:        "payload-" + id,
		Operation:          operation,
		LocalMessageID:     "message-" + id,
		TransportRequestID: "request-" + id,
		ScheduledForMS:     scheduledForMS,
	}
}

func leaseCarryItem(
	t *testing.T,
	ctx context.Context,
	endpoint Endpoint,
	now time.Time,
) sqlite.Lease {
	t.Helper()
	leases, err := endpoint.OutboxRepository.LeaseDue(ctx, sqlite.LeaseRequest{
		Owner:    "cutover-test",
		Now:      now,
		Duration: time.Minute,
		Limit:    1,
	})
	if err != nil {
		t.Fatalf("LeaseDue(): %v", err)
	}
	if len(leases) != 1 {
		t.Fatalf("LeaseDue() rows = %+v, want one", leases)
	}
	return leases[0]
}

func markCarryTransportCalled(
	t *testing.T,
	ctx context.Context,
	endpoint Endpoint,
	lease sqlite.Lease,
) {
	t.Helper()
	if err := endpoint.OutboxRepository.MarkTransportCalled(ctx, sqlite.Attempt{
		OutboxID:   lease.OutboxID,
		LeaseToken: *lease.LeaseToken,
	}); err != nil {
		t.Fatalf("MarkTransportCalled(%q): %v", lease.OutboxID, err)
	}
}

func assertCarryEntryIDs(t *testing.T, label string, entries []CarryEntry, want []string) {
	t.Helper()
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.OutboxID)
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s IDs = %v, want %v", label, got, want)
	}
}

func assertCarryEntryStates(
	t *testing.T,
	label string,
	entries []CarryEntry,
	want []sqlite.OutboxState,
) {
	t.Helper()
	got := make([]sqlite.OutboxState, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.State)
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s states = %v, want %v", label, got, want)
	}
}

// A foreign idempotency collision (the fresh store already holds a different
// intent under the key) must report that one row un-carryable and keep the
// batch moving, never abort every later row or corrupt the pre-existing row.
func TestCarryPendingOutboxForeignCollisionReportsAndContinues(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(carryTestNowMS)
	old := openCarryTestEndpoint(t, "old", now)
	fresh := openCarryTestEndpoint(t, "fresh", now)
	seedCarryAccount(t, old.Store, "account-a")
	seedCarryConversation(t, old.Store, "conv-a", "account-a", "remote-conv-a")
	seedCarryAccount(t, fresh.Store, "account-a")
	seedCarryConversation(t, fresh.Store, "fresh-conv-a", "account-a", "remote-conv-a")

	// Old store: a colliding row (shares the fresh row's key) and a clean row.
	enqueueCarryText(t, ctx, old, "collides", "conv-a", carryTestNowMS)
	enqueueCarryText(t, ctx, old, "clean", "conv-a", carryTestNowMS)

	// Fresh store: pre-seed a DIFFERENT intent under the colliding row's key.
	foreign := carryTestItem("collides", "fresh-conv-a", sqlite.OutboxKindText, carryTestNowMS)
	foreign.OutboxID = "fresh-foreign"
	foreign.LocalMessageID = "fresh-foreign-msg"
	foreign.TransportRequestID = "fresh-foreign-req"
	foreign.PayloadHash = "different-payload"
	freshReply := "fresh-reply"
	if _, disp, err := fresh.OutboxRepository.EnqueueOutgoingMessage(ctx, foreign, sqlite.Message{
		MessageID:       foreign.LocalMessageID,
		ConversationID:  "fresh-conv-a",
		AccountID:       "account-a",
		RemoteMessageID: foreign.TransportRequestID,
		Direction:       sqlite.MessageDirectionOutgoing,
		Body:            "a different message under the same key",
		ReplyToRemoteID: &freshReply,
		State:           sqlite.MessageStateActive,
		OccurredAtMS:    carryTestNowMS,
	}); err != nil || disp != sqlite.EnqueueInserted {
		t.Fatalf("seed foreign fresh intent: disp=%q err=%v", disp, err)
	}

	report, err := CarryPendingOutbox(ctx, old, fresh)
	if err != nil {
		t.Fatalf("CarryPendingOutbox() aborted the batch on a foreign collision: %v", err)
	}
	// The clean row carried; the colliding row is reported un-carryable.
	if len(report.Carried) != 1 || report.Carried[0].OutboxID != "outbox-clean" {
		t.Fatalf("carried = %+v, want exactly the clean row", report.Carried)
	}
	var collided *CarryEntry
	for i := range report.Uncarryable {
		if report.Uncarryable[i].OutboxID == "outbox-collides" {
			collided = &report.Uncarryable[i]
		}
	}
	if collided == nil {
		t.Fatalf("colliding row not reported un-carryable: %+v", report.Uncarryable)
	}
	if !strings.Contains(collided.Reason, "idempotency") {
		t.Fatalf("un-carryable reason = %q, want it to name the idempotency collision", collided.Reason)
	}
	// The pre-existing foreign fresh row is byte-unchanged.
	got, err := fresh.OutboxRepository.FindByID(ctx, "fresh-foreign")
	if err != nil {
		t.Fatalf("fresh foreign row lookup: %v", err)
	}
	if got.PayloadHash != "different-payload" {
		t.Fatalf("foreign fresh row mutated: payload_hash = %q", got.PayloadHash)
	}
}
