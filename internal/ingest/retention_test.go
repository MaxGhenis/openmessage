package ingest

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/rs/zerolog"
)

const retentionTestDay = 24 * time.Hour

// retentionHarness is the i01 sink/worker pair over a repository whose clock
// the test can move, so frames can age past retention.
type retentionHarness struct {
	*i01Harness
	nowMS *atomic.Int64
}

func newRetentionHarness(t *testing.T, decoder bridge.Decoder) *retentionHarness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	i01SeedAccount(t, store)
	nowMS := &atomic.Int64{}
	nowMS.Store(i01TestTime.UnixMilli())
	messages, err := sqlite.NewMessageRepository(store, func() time.Time {
		return time.UnixMilli(nowMS.Load())
	})
	if err != nil {
		t.Fatalf("sqlite.NewMessageRepository(): %v", err)
	}
	counters := &Counters{}
	worker := i01NewWorker(t, store, messages, counters, decoder, nil)
	sink := i01NewSink(t, messages, worker, counters, "inbox-retention")
	return &retentionHarness{
		i01Harness: &i01Harness{
			path:     path,
			store:    store,
			messages: messages,
			counters: counters,
			worker:   worker,
			sink:     sink,
		},
		nowMS: nowMS,
	}
}

func (h *retentionHarness) advance(d time.Duration) {
	h.nowMS.Add(d.Milliseconds())
}

func retentionInboxRow(t *testing.T, path, dedupeKey string) (payload []byte, processed, quarantined, pruned sql.NullInt64) {
	t.Helper()
	state := retentionInboxState(t, path, dedupeKey)
	return state.payload, state.processed, state.quarantined, state.pruned
}

type retentionRowState struct {
	payload                                 []byte
	processed, applied, quarantined, pruned sql.NullInt64
}

func retentionInboxState(t *testing.T, path, dedupeKey string) retentionRowState {
	t.Helper()
	database := i01OpenInspector(t, path)
	defer database.Close()
	var state retentionRowState
	if err := database.QueryRow(`
		SELECT payload, processed_at_ms, applied_at_ms, quarantined_at_ms, payload_pruned_at_ms
		FROM inbox
		WHERE account_id = ? AND dedupe_key = ?
	`, i01AccountID, dedupeKey).Scan(
		&state.payload, &state.processed, &state.applied, &state.quarantined, &state.pruned,
	); err != nil {
		t.Fatalf("read inbox %q: %v", dedupeKey, err)
	}
	return state
}

// appendAppliedFrame stores a frame the worker has fully applied, without
// running a worker.
func appendAppliedFrame(t *testing.T, messages *sqlite.MessageRepository, inboxID, dedupeKey string, payload []byte) {
	t.Helper()
	ctx := context.Background()
	if _, err := messages.AppendInbox(ctx, sqlite.InboxRecord{
		InboxID:      inboxID,
		AccountID:    i01AccountID,
		Generation:   1,
		DedupeKey:    dedupeKey,
		Codec:        i01Codec,
		CodecVersion: 1,
		Payload:      payload,
	}); err != nil {
		t.Fatalf("AppendInbox(%s): %v", inboxID, err)
	}
	if err := messages.MarkInboxProcessed(ctx, inboxID, i01AccountID); err != nil {
		t.Fatalf("MarkInboxProcessed(%s): %v", inboxID, err)
	}
	if err := messages.MarkInboxApplied(ctx, inboxID, i01AccountID); err != nil {
		t.Fatalf("MarkInboxApplied(%s): %v", inboxID, err)
	}
}

// End to end: a quarantined frame is durably marked and keeps its payload
// past retention; a processed frame loses only its payload, and replaying it
// afterwards collapses onto the kept row without re-projecting.
func TestPrunedFrameReplayDedupesAndQuarantinedFrameKeepsPayload(t *testing.T) {
	decoder := i01DecoderFunc(func(
		_ context.Context,
		record bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		if string(record.Payload) == "undecodable" {
			return nil, fmt.Errorf("scripted decoder failure")
		}
		return i01OutgoingMessageEvents("remote-retention", string(record.Payload), ""), nil
	})
	harness := newRetentionHarness(t, decoder)
	i01StartWorker(t, harness.worker)

	frame := i01IngressRecord("msg:remote-retention:hash-a", []byte("body-a"))
	i01MustAppend(t, harness.sink, frame)
	i01MustAppend(t, harness.sink, i01IngressRecord("quarantine:retention", []byte("undecodable")))
	i01WaitFor(t, "projection and quarantine", func() bool {
		snapshot := harness.counters.Snapshot(i01AccountID)
		return snapshot.Projected == 1 && snapshot.Quarantined == 1
	})
	i01AssertNoPending(t, harness.messages)
	before, err := i01GetMessage(harness.messages, "remote-retention")
	if err != nil {
		t.Fatalf("GetMessageByRemote(): %v", err)
	}

	_, _, quarantined, _ := retentionInboxRow(t, harness.path, "quarantine:retention")
	if !quarantined.Valid || quarantined.Int64 <= 0 {
		t.Fatalf("quarantined_at_ms = %+v, want a durable quarantine mark", quarantined)
	}
	if state := retentionInboxState(t, harness.path, frame.DedupeKey); state.quarantined.Valid || !state.applied.Valid {
		t.Fatalf("projected frame applied=%+v quarantined=%+v, want applied only", state.applied, state.quarantined)
	}
	if state := retentionInboxState(t, harness.path, "quarantine:retention"); state.applied.Valid {
		t.Fatalf("quarantined frame applied_at_ms = %d, want NULL", state.applied.Int64)
	}

	harness.advance(sqlite.DefaultInboxPayloadRetention + retentionTestDay)
	pruner, err := NewPayloadPruner(PayloadPrunerConfig{
		Messages:  harness.messages,
		Logger:    zerolog.Nop(),
		Retention: sqlite.DefaultInboxPayloadRetention,
	})
	if err != nil {
		t.Fatalf("NewPayloadPruner(): %v", err)
	}
	summary, err := pruner.PruneOnce(context.Background())
	if err != nil {
		t.Fatalf("PruneOnce(): %v", err)
	}
	if summary.Rows != 1 || summary.Bytes != int64(len("body-a")) {
		t.Fatalf("prune summary = %+v, want the one processed frame", summary)
	}
	payload, _, _, pruned := retentionInboxRow(t, harness.path, frame.DedupeKey)
	if len(payload) != 0 || !pruned.Valid {
		t.Fatalf("processed frame after prune: payload %q pruned %+v", payload, pruned)
	}
	quarantinedPayload, _, _, quarantinedPruned := retentionInboxRow(t, harness.path, "quarantine:retention")
	if !bytes.Equal(quarantinedPayload, []byte("undecodable")) || quarantinedPruned.Valid {
		t.Fatalf("quarantined frame after prune: payload %q pruned %+v", quarantinedPayload, quarantinedPruned)
	}

	// The transport re-delivers the frame byte for byte.
	i01MustAppend(t, harness.sink, frame)
	i01WaitFor(t, "replay of pruned frame", func() bool {
		snapshot := harness.counters.Snapshot(i01AccountID)
		return snapshot.Deduped == 1 && snapshot.Projected == 2
	})
	// The same key with different bytes is a stale replay, as it was before
	// pruning.
	i01MustAppend(t, harness.sink, i01IngressRecord(frame.DedupeKey, []byte("body-stale")))
	i01WaitFor(t, "stale replay of pruned frame", func() bool {
		return harness.counters.Snapshot(i01AccountID).StaleReplays == 1
	})

	snapshot := harness.counters.Snapshot(i01AccountID)
	if snapshot.Appended != 2 || snapshot.Deduped != 2 || snapshot.Quarantined != 1 {
		t.Fatalf("counters after replays = %+v, want 2 appended, 2 deduped, 1 quarantined", snapshot)
	}
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM inbox`); got != 2 {
		t.Fatalf("inbox rows = %d, want 2 (replays add none)", got)
	}
	after, err := i01GetMessage(harness.messages, "remote-retention")
	if err != nil {
		t.Fatalf("GetMessageByRemote() after replays: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("message changed across replays:\nbefore: %+v\nafter:  %+v", before, after)
	}
	i01AssertNoPending(t, harness.messages)
}

func TestPayloadPrunerPrunesInBatchesUntilDone(t *testing.T) {
	harness := newRetentionHarness(t, i01DecoderFunc(func(context.Context, bridge.RawIngressRecord) ([]bridge.Event, error) {
		return nil, nil
	}))
	ctx := context.Background()
	const frames = 7
	for i := 0; i < frames; i++ {
		inboxID := fmt.Sprintf("inbox-batch-%d", i)
		appendAppliedFrame(t, harness.messages, inboxID, "key-"+inboxID, []byte("0123456789"))
	}
	harness.advance(sqlite.MinInboxPayloadRetention + time.Millisecond)

	pruner, err := NewPayloadPruner(PayloadPrunerConfig{
		Messages:   harness.messages,
		Logger:     zerolog.Nop(),
		Retention:  sqlite.MinInboxPayloadRetention,
		BatchSize:  3,
		BatchPause: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewPayloadPruner(): %v", err)
	}
	summary, err := pruner.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("PruneOnce(): %v", err)
	}
	if summary.Rows != frames || summary.Bytes != frames*10 || summary.Batches != 3 {
		t.Fatalf("summary = %+v, want %d rows, %d bytes, 3 batches", summary, frames, frames*10)
	}
	again, err := pruner.PruneOnce(ctx)
	if err != nil {
		t.Fatalf("PruneOnce(again): %v", err)
	}
	if again.Rows != 0 || again.Batches != 0 {
		t.Fatalf("second pass = %+v, want nothing left", again)
	}
}

func TestPayloadPrunerRunPrunesAfterStartDelayAndStopsOnCancel(t *testing.T) {
	harness := newRetentionHarness(t, i01DecoderFunc(func(context.Context, bridge.RawIngressRecord) ([]bridge.Event, error) {
		return nil, nil
	}))
	ctx := context.Background()
	appendAppliedFrame(t, harness.messages, "inbox-run", "key-run", []byte("payload"))
	harness.advance(sqlite.DefaultInboxPayloadRetention + time.Millisecond)

	pruner, err := NewPayloadPruner(PayloadPrunerConfig{
		Messages:   harness.messages,
		Logger:     zerolog.Nop(),
		Retention:  sqlite.DefaultInboxPayloadRetention,
		StartDelay: time.Millisecond,
		Interval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPayloadPruner(): %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- pruner.Run(runCtx) }()
	i01WaitFor(t, "first pruning pass", func() bool {
		_, _, _, pruned := retentionInboxRow(t, harness.path, "key-run")
		return pruned.Valid
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
}

func TestNewPayloadPrunerValidatesConfig(t *testing.T) {
	harness := newRetentionHarness(t, i01DecoderFunc(func(context.Context, bridge.RawIngressRecord) ([]bridge.Event, error) {
		return nil, nil
	}))
	for _, test := range []struct {
		name   string
		config PayloadPrunerConfig
	}{
		{"nil repository", PayloadPrunerConfig{Retention: sqlite.DefaultInboxPayloadRetention}},
		{"below floor", PayloadPrunerConfig{Messages: harness.messages, Retention: sqlite.MinInboxPayloadRetention - time.Second}},
		{"zero retention", PayloadPrunerConfig{Messages: harness.messages}},
		{"negative batch", PayloadPrunerConfig{Messages: harness.messages, Retention: sqlite.DefaultInboxPayloadRetention, BatchSize: -1}},
		{"negative interval", PayloadPrunerConfig{Messages: harness.messages, Retention: sqlite.DefaultInboxPayloadRetention, Interval: -time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewPayloadPruner(test.config); err == nil {
				t.Fatal("NewPayloadPruner() error = nil, want rejection")
			}
		})
	}
	pruner, err := NewPayloadPruner(PayloadPrunerConfig{
		Messages:  harness.messages,
		Retention: sqlite.MinInboxPayloadRetention,
	})
	if err != nil {
		t.Fatalf("NewPayloadPruner(floor): %v", err)
	}
	if pruner.startDelay != defaultPayloadPruneStartDelay || pruner.interval != defaultPayloadPruneInterval ||
		pruner.batchSize != defaultPayloadPruneBatchSize || pruner.batchPause != defaultPayloadPruneBatchPause {
		t.Fatalf("defaults not applied: %+v", pruner)
	}
	if err := pruner.Run(nil); err == nil { //nolint:staticcheck // nil context is the case under test
		t.Fatal("Run(nil) error = nil, want rejection")
	}
}

func TestPayloadPrunerRunSkipsPassesWhileHoldFileExists(t *testing.T) {
	harness := newRetentionHarness(t, i01DecoderFunc(func(context.Context, bridge.RawIngressRecord) ([]bridge.Event, error) {
		return nil, nil
	}))
	ctx := context.Background()
	appendAppliedFrame(t, harness.messages, "inbox-held", "key-held", []byte("payload"))
	harness.advance(sqlite.DefaultInboxPayloadRetention + time.Millisecond)

	holdPath := filepath.Join(t.TempDir(), "inbox-retention-hold")
	if err := os.WriteFile(holdPath, nil, 0o600); err != nil {
		t.Fatalf("write hold file: %v", err)
	}
	pruner, err := NewPayloadPruner(PayloadPrunerConfig{
		Messages:   harness.messages,
		Logger:     zerolog.Nop(),
		Retention:  sqlite.DefaultInboxPayloadRetention,
		StartDelay: time.Millisecond,
		Interval:   10 * time.Millisecond,
		HoldPath:   holdPath,
	})
	if err != nil {
		t.Fatalf("NewPayloadPruner(): %v", err)
	}
	type pass struct {
		summary PayloadPruneSummary
		held    bool
	}
	passes := make(chan pass, 64)
	pruner.afterPass = func(summary PayloadPruneSummary, held bool) {
		passes <- pass{summary: summary, held: held}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- pruner.Run(runCtx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run() did not stop after cancellation")
		}
	}()

	nextPass := func() pass {
		t.Helper()
		select {
		case p := <-passes:
			return p
		case <-time.After(5 * time.Second):
			t.Fatal("no pruning pass ran")
			return pass{}
		}
	}
	for i := 0; i < 3; i++ {
		if p := nextPass(); !p.held || p.summary.Rows != 0 {
			t.Fatalf("pass %d with hold file = %+v, want a skipped pass", i, p)
		}
	}
	if payload, _, _, pruned := retentionInboxRow(t, harness.path, "key-held"); pruned.Valid || string(payload) != "payload" {
		t.Fatalf("held frame was pruned: payload %q pruned %+v", payload, pruned)
	}

	if err := os.Remove(holdPath); err != nil {
		t.Fatalf("remove hold file: %v", err)
	}
	for {
		p := nextPass()
		if p.held {
			continue // a pass that checked before the removal
		}
		if p.summary.Rows != 1 {
			t.Fatalf("first pass after release = %+v, want the one eligible frame", p.summary)
		}
		break
	}
}

func TestPayloadPrunerTreatsAnUncheckableHoldFileAsHeld(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	pruner := &PayloadPruner{logger: zerolog.Nop(), holdPath: filepath.Join(locked, "inbox-retention-hold")}
	if !pruner.held() {
		t.Fatal("held() = false for a hold path that cannot be checked, want true")
	}
	pruner.holdPath = filepath.Join(t.TempDir(), "absent")
	if pruner.held() {
		t.Fatal("held() = true for an absent hold file, want false")
	}
	pruner.holdPath = ""
	if pruner.held() {
		t.Fatal("held() = true with no hold path, want false")
	}
}

// A failed replay is counted, but it does not quarantine the stored frame:
// the row holds the original delivery, which projected, not the bytes that
// failed. The original still ages out.
func TestFailedReplayDoesNotQuarantineTheStoredFrame(t *testing.T) {
	decoder := i01DecoderFunc(func(
		_ context.Context,
		record bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		if string(record.Payload) == "undecodable" {
			return nil, fmt.Errorf("scripted decoder failure")
		}
		return i01OutgoingMessageEvents("remote-replay", string(record.Payload), ""), nil
	})
	harness := newRetentionHarness(t, decoder)
	i01StartWorker(t, harness.worker)

	frame := i01IngressRecord("msg:remote-replay:hash", []byte("body"))
	i01MustAppend(t, harness.sink, frame)
	i01WaitFor(t, "projection", func() bool {
		return retentionInboxState(t, harness.path, frame.DedupeKey).applied.Valid
	})

	i01MustAppend(t, harness.sink, i01IngressRecord(frame.DedupeKey, []byte("undecodable")))
	i01WaitFor(t, "failed replay", func() bool {
		return harness.counters.Snapshot(i01AccountID).Quarantined == 1
	})
	state := retentionInboxState(t, harness.path, frame.DedupeKey)
	if state.quarantined.Valid || !state.applied.Valid || string(state.payload) != "body" {
		t.Fatalf("stored frame after failed replay = %+v, want applied, unquarantined, payload intact", state)
	}

	harness.advance(sqlite.DefaultInboxPayloadRetention + retentionTestDay)
	result, err := harness.messages.PruneInboxPayloads(context.Background(), sqlite.DefaultInboxPayloadRetention, 10)
	if err != nil {
		t.Fatalf("PruneInboxPayloads(): %v", err)
	}
	if result.Rows != 1 {
		t.Fatalf("pruned rows = %d, want the stored frame to age out", result.Rows)
	}
}

// A successful replay does not mark the stored frame applied either: the
// replayed bytes are not the stored ones, so they cannot vouch for them.
func TestReplayNeverMarksAnUnappliedFrameApplied(t *testing.T) {
	decoder := i01DecoderFunc(func(
		_ context.Context,
		record bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		return i01OutgoingMessageEvents("remote-unapplied", string(record.Payload), ""), nil
	})
	harness := newRetentionHarness(t, decoder)
	frame := i01IngressRecord("msg:remote-unapplied:hash", []byte("body"))
	i01MustAppend(t, harness.sink, frame)

	// Process the stored frame as a replay would: everything lands, but the
	// applied mark belongs only to the frame's own first handling.
	pending, err := harness.messages.Unprocessed(context.Background())
	if err != nil || len(pending) != 1 {
		t.Fatalf("Unprocessed() = %d rows, %v; want 1", len(pending), err)
	}
	harness.worker.handleRecord(context.Background(), pending[0].InboxID, rawIngressRecord(pending[0]), true)
	state := retentionInboxState(t, harness.path, frame.DedupeKey)
	if !state.processed.Valid || state.applied.Valid {
		t.Fatalf("frame handled only as a replay = %+v, want processed and unapplied", state)
	}

	harness.advance(sqlite.DefaultInboxPayloadRetention + retentionTestDay)
	result, err := harness.messages.PruneInboxPayloads(context.Background(), sqlite.DefaultInboxPayloadRetention, 10)
	if err != nil {
		t.Fatalf("PruneInboxPayloads(): %v", err)
	}
	if result.Rows != 0 {
		t.Fatalf("pruned rows = %d, want the unapplied frame kept", result.Rows)
	}
}

// twoMessageHarness drives one frame carrying two message events through
// handleRecord, with a hook on the repository clock that can cancel the
// worker's context partway through the frame.
type twoMessageHarness struct {
	*retentionHarness
	calls atomic.Int64
	hook  atomic.Pointer[func(call int64)]
}

func newTwoMessageHarness(t *testing.T) *twoMessageHarness {
	t.Helper()
	h := &twoMessageHarness{}
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	i01SeedAccount(t, store)
	nowMS := &atomic.Int64{}
	nowMS.Store(i01TestTime.UnixMilli())
	messages, err := sqlite.NewMessageRepository(store, func() time.Time {
		call := h.calls.Add(1)
		if hook := h.hook.Load(); hook != nil {
			(*hook)(call)
		}
		return time.UnixMilli(nowMS.Load())
	})
	if err != nil {
		t.Fatalf("sqlite.NewMessageRepository(): %v", err)
	}
	decoder := i01DecoderFunc(func(context.Context, bridge.RawIngressRecord) ([]bridge.Event, error) {
		return append(
			i01OutgoingMessageEvents("remote-first", "first", ""),
			i01OutgoingMessageEvents("remote-second", "second", "")...,
		), nil
	})
	counters := &Counters{}
	worker := i01NewWorker(t, store, messages, counters, decoder, nil)
	sink := i01NewSink(t, messages, worker, counters, "inbox-two")
	h.retentionHarness = &retentionHarness{
		i01Harness: &i01Harness{
			path:     path,
			store:    store,
			messages: messages,
			counters: counters,
			worker:   worker,
			sink:     sink,
		},
		nowMS: nowMS,
	}
	return h
}

// cancelOnCall returns a context that is cancelled when the repository clock
// is read for the nth time from now: call 1 is the first message's
// ProjectMessage, call 2 the second message's ImportMessage.
func (h *twoMessageHarness) cancelOnCall(n int64) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	target := h.calls.Load() + n
	hook := func(call int64) {
		if call == target {
			cancel()
		}
	}
	h.hook.Store(&hook)
	return ctx, cancel
}

func (h *twoMessageHarness) drain(t *testing.T, ctx context.Context) {
	t.Helper()
	pending, err := h.messages.Unprocessed(context.Background())
	if err != nil {
		t.Fatalf("Unprocessed(): %v", err)
	}
	for _, record := range pending {
		h.worker.handleRecord(ctx, record.InboxID, rawIngressRecord(record), false)
	}
}

// A worker that stops after the frame's first projection committed leaves the
// frame processed, so no drain retries its second message. The frame was never
// marked applied, so retention keeps the payload that still carries it. The
// stop here is a cancelled context; a killed process leaves the same rows.
func TestFrameInterruptedAfterProjectionKeepsItsPayload(t *testing.T) {
	h := newTwoMessageHarness(t)
	frame := i01IngressRecord("msg:two-messages", []byte("two messages"))
	i01MustAppend(t, h.sink, frame)

	ctx, cancel := h.cancelOnCall(2)
	defer cancel()
	h.drain(t, ctx)
	h.hook.Store(nil)

	if _, err := i01GetMessage(h.messages, "remote-first"); err != nil {
		t.Fatalf("first message: %v, want projected before the stop", err)
	}
	if _, err := i01GetMessage(h.messages, "remote-second"); err == nil {
		t.Fatal("second message projected; the stop did not interrupt the frame")
	}
	state := retentionInboxState(t, h.path, frame.DedupeKey)
	if !state.processed.Valid || state.applied.Valid || state.quarantined.Valid {
		t.Fatalf("interrupted frame = %+v, want processed, unapplied, unquarantined", state)
	}
	// The restart drain never sees a processed frame again.
	h.drain(t, context.Background())
	if _, err := i01GetMessage(h.messages, "remote-second"); err == nil {
		t.Fatal("second message projected on the restart drain")
	}

	h.advance(1000 * retentionTestDay)
	result, err := h.messages.PruneInboxPayloads(context.Background(), sqlite.MinInboxPayloadRetention, 10)
	if err != nil {
		t.Fatalf("PruneInboxPayloads(): %v", err)
	}
	state = retentionInboxState(t, h.path, frame.DedupeKey)
	if result.Rows != 0 || state.pruned.Valid || string(state.payload) != "two messages" {
		t.Fatalf("interrupted frame after retention: rows=%d state=%+v, want payload kept", result.Rows, state)
	}
}

// A worker that stops before the projection commits leaves the frame
// unprocessed: the next drain applies all of it, marks it applied, and it
// then ages out normally.
func TestFrameInterruptedBeforeProjectionIsAppliedByTheNextDrain(t *testing.T) {
	h := newTwoMessageHarness(t)
	frame := i01IngressRecord("msg:two-messages-early", []byte("two messages"))
	i01MustAppend(t, h.sink, frame)

	ctx, cancel := h.cancelOnCall(1)
	defer cancel()
	h.drain(t, ctx)
	h.hook.Store(nil)

	if state := retentionInboxState(t, h.path, frame.DedupeKey); state.processed.Valid || state.applied.Valid {
		t.Fatalf("interrupted frame = %+v, want unprocessed and unapplied", state)
	}
	h.drain(t, context.Background())
	for _, remoteID := range []string{"remote-first", "remote-second"} {
		if _, err := i01GetMessage(h.messages, remoteID); err != nil {
			t.Fatalf("%s after the restart drain: %v", remoteID, err)
		}
	}
	state := retentionInboxState(t, h.path, frame.DedupeKey)
	if !state.processed.Valid || !state.applied.Valid || state.quarantined.Valid {
		t.Fatalf("retried frame = %+v, want processed and applied", state)
	}

	h.advance(sqlite.DefaultInboxPayloadRetention + retentionTestDay)
	result, err := h.messages.PruneInboxPayloads(context.Background(), sqlite.DefaultInboxPayloadRetention, 10)
	if err != nil {
		t.Fatalf("PruneInboxPayloads(): %v", err)
	}
	if result.Rows != 1 {
		t.Fatalf("pruned rows = %d, want the fully applied frame to age out", result.Rows)
	}
}
