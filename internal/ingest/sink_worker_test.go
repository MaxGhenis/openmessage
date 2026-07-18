package ingest

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/rs/zerolog"
)

const (
	i01AccountID            = "account-i01-ingest"
	i01Codec                = "test.i01.scripted"
	i01RemoteConversationID = "conversation-i01-remote"
)

var i01TestTime = time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)

type i01DecoderFunc func(context.Context, bridge.RawIngressRecord) ([]bridge.Event, error)

func (decode i01DecoderFunc) Decode(
	ctx context.Context,
	record bridge.RawIngressRecord,
) ([]bridge.Event, error) {
	return decode(ctx, record)
}

type i01EchoRecorder struct {
	mu      sync.Mutex
	calls   []messaging.TransportEcho
	outcome messaging.EchoOutcome
	err     error
}

func (recorder *i01EchoRecorder) ObserveTransportEcho(
	_ context.Context,
	echo messaging.TransportEcho,
) (messaging.EchoOutcome, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.calls = append(recorder.calls, echo)
	if recorder.err != nil {
		return "", recorder.err
	}
	if recorder.outcome != "" {
		return recorder.outcome, nil
	}
	return messaging.EchoReconciled, nil
}

func (recorder *i01EchoRecorder) snapshot() []messaging.TransportEcho {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]messaging.TransportEcho(nil), recorder.calls...)
}

type i01Harness struct {
	path     string
	store    *sqlite.Store
	messages *sqlite.MessageRepository
	counters *Counters
	worker   *Worker
	sink     *Sink
}

func TestI01SinkDedupeReplayAndStaleProcessedFrame(t *testing.T) {
	decoder := i01DecoderFunc(func(
		_ context.Context,
		record bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		return i01OutgoingMessageEvents("remote-i01-dedupe", string(record.Payload), ""), nil
	})
	harness := i01NewHarness(t, decoder, nil)
	i01StartWorker(t, harness.worker)

	frameA := i01IngressRecord("msg:remote-i01-dedupe:hash-a", []byte("body-a"))
	i01MustAppend(t, harness.sink, frameA)
	i01WaitFor(t, "first frame projection", func() bool {
		message, err := i01GetMessage(harness.messages, "remote-i01-dedupe")
		return err == nil && message.Body == "body-a" &&
			harness.counters.Snapshot(i01AccountID).Projected == 1
	})

	// An exact replay resolves to the original inbox row and is a successful
	// idempotent projection.
	i01MustAppend(t, harness.sink, frameA)
	i01WaitFor(t, "exact replay", func() bool {
		snapshot := harness.counters.Snapshot(i01AccountID)
		return snapshot.Deduped == 1 && snapshot.Projected == 2
	})

	// A content hash change gets its own durable inbox row and updates the one
	// message selected by its remote natural key.
	frameB := i01IngressRecord("msg:remote-i01-dedupe:hash-b", []byte("body-b"))
	i01MustAppend(t, harness.sink, frameB)
	i01WaitFor(t, "changed-content projection", func() bool {
		message, err := i01GetMessage(harness.messages, "remote-i01-dedupe")
		return err == nil && message.Body == "body-b" &&
			harness.counters.Snapshot(i01AccountID).Projected == 3
	})

	// Replaying the now-superseded original content is classified as stale. It
	// must not restore the old body or surface as a worker failure.
	i01MustAppend(t, harness.sink, frameA)
	i01WaitFor(t, "stale processed replay classification", func() bool {
		return harness.counters.Snapshot(i01AccountID).StaleReplays == 1
	})

	message, err := i01GetMessage(harness.messages, "remote-i01-dedupe")
	if err != nil {
		t.Fatalf("GetMessageByRemote() after stale replay: %v", err)
	}
	if message.Body != "body-b" {
		t.Fatalf("message body after stale replay = %q, want body-b", message.Body)
	}
	snapshot := harness.counters.Snapshot(i01AccountID)
	if snapshot.Appended != 2 || snapshot.Deduped != 2 ||
		snapshot.DecodedEvents != 4 || snapshot.Projected != 3 ||
		snapshot.StaleReplays != 1 || snapshot.Quarantined != 0 {
		t.Fatalf("dedupe/replay counters = %+v", snapshot)
	}
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM inbox`); got != 2 {
		t.Fatalf("inbox rows = %d, want 2 content-distinct rows", got)
	}
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM messages`); got != 1 {
		t.Fatalf("message rows = %d, want 1 natural-key row", got)
	}
	i01AssertNoPending(t, harness.messages)
}

func TestI01WorkerQuarantinesDecoderFailuresAndContinues(t *testing.T) {
	decoder := i01DecoderFunc(func(
		_ context.Context,
		record bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		switch string(record.Payload) {
		case "decoder-error-payload":
			return nil, errors.New("scripted decoder failure")
		case "decoder-panic-payload":
			panic("scripted decoder panic")
		case "continuation-payload":
			return i01OutgoingMessageEvents(
				"remote-i01-after-quarantine",
				"worker-continued",
				"",
			), nil
		default:
			return nil, fmt.Errorf("unexpected scripted payload %q", record.Payload)
		}
	})
	harness := i01NewHarness(t, decoder, nil)
	i01StartWorker(t, harness.worker)

	i01MustAppend(t, harness.sink, i01IngressRecord(
		"quarantine:error",
		[]byte("decoder-error-payload"),
	))
	i01MustAppend(t, harness.sink, i01IngressRecord(
		"quarantine:panic",
		[]byte("decoder-panic-payload"),
	))
	i01MustAppend(t, harness.sink, i01IngressRecord(
		"quarantine:continuation",
		[]byte("continuation-payload"),
	))

	i01WaitFor(t, "quarantine and continuation", func() bool {
		snapshot := harness.counters.Snapshot(i01AccountID)
		message, err := i01GetMessage(harness.messages, "remote-i01-after-quarantine")
		return snapshot.Quarantined == 2 && snapshot.Projected == 1 &&
			err == nil && message.Body == "worker-continued"
	})

	snapshot := harness.counters.Snapshot(i01AccountID)
	if snapshot.Appended != 3 || snapshot.DecodedEvents != 1 ||
		snapshot.Quarantined != 2 || snapshot.Projected != 1 {
		t.Fatalf("quarantine counters = %+v", snapshot)
	}
	for _, test := range []struct {
		dedupeKey string
		payload   []byte
	}{
		{dedupeKey: "quarantine:error", payload: []byte("decoder-error-payload")},
		{dedupeKey: "quarantine:panic", payload: []byte("decoder-panic-payload")},
	} {
		gotPayload, processedAt := i01InboxPayload(t, harness.path, test.dedupeKey)
		if !bytes.Equal(gotPayload, test.payload) {
			t.Errorf("retained payload for %q = %q, want %q", test.dedupeKey, gotPayload, test.payload)
		}
		if !processedAt.Valid || processedAt.Int64 <= 0 {
			t.Errorf("processed_at_ms for quarantined %q = %+v, want positive", test.dedupeKey, processedAt)
		}
	}
	if got := i01QueryInt64(t, harness.path, `SELECT COUNT(*) FROM inbox`); got != 3 {
		t.Fatalf("inbox rows after quarantine = %d, want all 3 retained", got)
	}
	i01AssertNoPending(t, harness.messages)
}

func TestI01WorkerRoutesGenericEchoNotFoundAndProjects(t *testing.T) {
	const (
		clientRequestID = "client-request-i01"
		remoteMessageID = "remote-i01-echo"
	)
	recorder := &i01EchoRecorder{outcome: messaging.EchoNotFound}
	decoder := i01DecoderFunc(func(
		_ context.Context,
		_ bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		return i01OutgoingMessageEvents(remoteMessageID, "echo body", clientRequestID), nil
	})
	harness := i01NewHarness(t, decoder, recorder)
	i01StartWorker(t, harness.worker)

	i01MustAppend(t, harness.sink, i01IngressRecord(
		"echo:remote-i01-echo",
		[]byte("fake-decoder-echo-frame"),
	))
	i01WaitFor(t, "echo route and projection", func() bool {
		return len(recorder.snapshot()) == 1 &&
			harness.counters.Snapshot(i01AccountID).Projected == 1
	})

	calls := recorder.snapshot()
	want := messaging.TransportEcho{
		AccountID:          i01AccountID,
		TransportRequestID: clientRequestID,
		RemoteMessageID:    remoteMessageID,
	}
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("ObserveTransportEcho calls = %+v, want exactly %+v", calls, want)
	}
	snapshot := harness.counters.Snapshot(i01AccountID)
	if snapshot.EchoNotFound != 1 || snapshot.EchoReconciled != 0 ||
		snapshot.Projected != 1 || snapshot.Quarantined != 0 {
		t.Fatalf("echo-not-found counters = %+v", snapshot)
	}
	message, err := i01GetMessage(harness.messages, remoteMessageID)
	if err != nil {
		t.Fatalf("GetMessageByRemote() after echo not-found: %v", err)
	}
	if message.Body != "echo body" {
		t.Fatalf("projected echo body = %q, want echo body", message.Body)
	}
	i01AssertNoPending(t, harness.messages)
}

func TestI01WorkerStartupDrainsDurableFramesAfterCrash(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	firstStore, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("first sqlite.Open(): %v", err)
	}
	firstClosed := false
	t.Cleanup(func() {
		if !firstClosed {
			if err := firstStore.Close(); err != nil {
				t.Errorf("close first store: %v", err)
			}
		}
	})
	i01SeedAccount(t, firstStore)
	firstMessages := i01NewMessageRepository(t, firstStore)
	decoder := i01DecoderFunc(func(
		_ context.Context,
		record bridge.RawIngressRecord,
	) ([]bridge.Event, error) {
		remoteMessageID := string(record.Payload)
		return i01OutgoingMessageEvents(
			remoteMessageID,
			"recovered "+remoteMessageID,
			"",
		), nil
	})

	// The sink requires a worker for notification, but this first worker never
	// runs. Its in-memory notifications disappear with the simulated process.
	firstCounters := &Counters{}
	firstWorker := i01NewWorker(t, firstStore, firstMessages, firstCounters, decoder, nil)
	firstSink := i01NewSink(t, firstMessages, firstWorker, firstCounters, "crashed-inbox")
	i01MustAppend(t, firstSink, i01IngressRecord("crash:one", []byte("remote-i01-crash-one")))
	i01MustAppend(t, firstSink, i01IngressRecord("crash:two", []byte("remote-i01-crash-two")))
	pending, err := firstMessages.Unprocessed(ctx)
	if err != nil {
		t.Fatalf("Unprocessed() before crash: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("unprocessed frames before crash = %d, want 2", len(pending))
	}
	if got := i01QueryInt64(t, path, `SELECT COUNT(*) FROM messages`); got != 0 {
		t.Fatalf("messages before worker startup = %d, want 0", got)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatalf("close first store after durable appends: %v", err)
	}
	firstClosed = true

	secondStore, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("second sqlite.Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := secondStore.Close(); err != nil {
			t.Errorf("close second store: %v", err)
		}
	})
	secondMessages := i01NewMessageRepository(t, secondStore)
	pending, err = secondMessages.Unprocessed(ctx)
	if err != nil {
		t.Fatalf("Unprocessed() after reopen: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("durable frames after reopen = %d, want 2", len(pending))
	}

	secondCounters := &Counters{}
	secondWorker := i01NewWorker(t, secondStore, secondMessages, secondCounters, decoder, nil)
	i01StartWorker(t, secondWorker)
	i01WaitFor(t, "startup recovery drain", func() bool {
		first, firstErr := i01GetMessage(secondMessages, "remote-i01-crash-one")
		second, secondErr := i01GetMessage(secondMessages, "remote-i01-crash-two")
		pending, pendingErr := secondMessages.Unprocessed(ctx)
		return firstErr == nil && first.Body == "recovered remote-i01-crash-one" &&
			secondErr == nil && second.Body == "recovered remote-i01-crash-two" &&
			pendingErr == nil && len(pending) == 0
	})
	snapshot := secondCounters.Snapshot(i01AccountID)
	if snapshot.Appended != 0 || snapshot.DecodedEvents != 2 || snapshot.Projected != 2 {
		t.Fatalf("startup-drain counters = %+v", snapshot)
	}
	if got := i01QueryInt64(t, path, `SELECT COUNT(*) FROM messages`); got != 2 {
		t.Fatalf("messages after startup drain = %d, want 2", got)
	}
}

func i01NewHarness(
	t *testing.T,
	decoder bridge.Decoder,
	echoes EchoObserver,
) *i01Harness {
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
	messages := i01NewMessageRepository(t, store)
	counters := &Counters{}
	worker := i01NewWorker(t, store, messages, counters, decoder, echoes)
	sink := i01NewSink(t, messages, worker, counters, "inbox-i01")
	return &i01Harness{
		path:     path,
		store:    store,
		messages: messages,
		counters: counters,
		worker:   worker,
		sink:     sink,
	}
}

func i01NewMessageRepository(
	t *testing.T,
	store *sqlite.Store,
) *sqlite.MessageRepository {
	t.Helper()
	messages, err := sqlite.NewMessageRepository(store, func() time.Time { return i01TestTime })
	if err != nil {
		t.Fatalf("sqlite.NewMessageRepository(): %v", err)
	}
	return messages
}

func i01NewWorker(
	t *testing.T,
	store *sqlite.Store,
	messages *sqlite.MessageRepository,
	counters *Counters,
	decoder bridge.Decoder,
	echoes EchoObserver,
) *Worker {
	t.Helper()
	reactions, err := sqlite.NewReactionRepository(
		store,
		func() time.Time { return i01TestTime },
	)
	if err != nil {
		t.Fatalf("sqlite.NewReactionRepository(): %v", err)
	}
	worker, err := NewWorker(WorkerConfig{
		Store:        store,
		Messages:     messages,
		Reactions:    reactions,
		EchoObserver: echoes,
		Counters:     counters,
		Logger:       zerolog.Nop(),
		Now:          func() time.Time { return i01TestTime },
		Decoders: []DecoderRegistration{{
			Codec:    i01Codec,
			Platform: bridge.PlatformGoogle,
			Decoder:  decoder,
		}},
	})
	if err != nil {
		t.Fatalf("NewWorker(): %v", err)
	}
	return worker
}

func i01NewSink(
	t *testing.T,
	messages *sqlite.MessageRepository,
	worker *Worker,
	counters *Counters,
	idPrefix string,
) *Sink {
	t.Helper()
	var next atomic.Uint64
	sink, err := NewSink(SinkConfig{
		Messages: messages,
		Worker:   worker,
		Counters: counters,
		IDs: messaging.IDSourceFunc(func() (string, error) {
			return fmt.Sprintf("%s-%04d", idPrefix, next.Add(1)), nil
		}),
	})
	if err != nil {
		t.Fatalf("NewSink(): %v", err)
	}
	return sink
}

func i01SeedAccount(t *testing.T, store *sqlite.Store) {
	t.Helper()
	err := store.UpsertAccount(sqlite.Account{
		AccountID:   i01AccountID,
		BridgeKey:   "google_messages",
		DisplayName: "I01 ingest test",
		Mode:        sqlite.AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: i01TestTime.UnixMilli(),
		UpdatedAtMS: i01TestTime.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
}

func i01StartWorker(t *testing.T, worker *Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Worker.Run(): %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Worker.Run() did not stop after cancellation")
		}
	})
}

func i01IngressRecord(dedupeKey string, payload []byte) bridge.RawIngressRecord {
	return bridge.RawIngressRecord{
		AccountID:    i01AccountID,
		Generation:   17,
		DedupeKey:    dedupeKey,
		Codec:        i01Codec,
		CodecVersion: 1,
		ReceivedAt:   i01TestTime,
		Payload:      payload,
	}
}

func i01OutgoingMessageEvents(
	remoteMessageID string,
	body string,
	clientRequestID string,
) []bridge.Event {
	return []bridge.Event{{
		Kind: bridge.EventMessage,
		Message: &bridge.MessageEvent{
			RemoteConversationID: i01RemoteConversationID,
			RemoteMessageID:      remoteMessageID,
			ClientRequestID:      clientRequestID,
			Direction:            string(sqlite.MessageDirectionOutgoing),
			Body:                 body,
			OccurredAt:           i01TestTime.Add(-time.Minute),
		},
	}}
}

func i01MustAppend(t *testing.T, sink *Sink, record bridge.RawIngressRecord) {
	t.Helper()
	if err := sink.AppendIngress(context.Background(), record); err != nil {
		t.Fatalf("AppendIngress(%q): %v", record.DedupeKey, err)
	}
}

func i01GetMessage(
	messages *sqlite.MessageRepository,
	remoteMessageID string,
) (sqlite.Message, error) {
	return messages.GetMessageByRemote(
		context.Background(),
		i01AccountID,
		v2keys.DeriveID("conversation", i01AccountID, i01RemoteConversationID),
		remoteMessageID,
	)
}

func i01WaitFor(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if ready() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func i01AssertNoPending(t *testing.T, messages *sqlite.MessageRepository) {
	t.Helper()
	pending, err := messages.Unprocessed(context.Background())
	if err != nil {
		t.Fatalf("Unprocessed(): %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("unprocessed frames = %d, want 0", len(pending))
	}
}

func i01QueryInt64(t *testing.T, path, query string, args ...any) int64 {
	t.Helper()
	database := i01OpenInspector(t, path)
	defer database.Close()
	var value int64
	if err := database.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value
}

func i01InboxPayload(
	t *testing.T,
	path string,
	dedupeKey string,
) ([]byte, sql.NullInt64) {
	t.Helper()
	database := i01OpenInspector(t, path)
	defer database.Close()
	var payload []byte
	var processedAt sql.NullInt64
	if err := database.QueryRow(`
		SELECT payload, processed_at_ms
		FROM inbox
		WHERE account_id = ? AND dedupe_key = ?
	`, i01AccountID, dedupeKey).Scan(&payload, &processedAt); err != nil {
		t.Fatalf("read inbox payload for %q: %v", dedupeKey, err)
	}
	return payload, processedAt
}

func i01OpenInspector(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(inspector): %v", err)
	}
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		_ = database.Close()
		t.Fatalf("inspector Ping(): %v", err)
	}
	return database
}
