// dryrun reads Signal Desktop's database read-only and prints counts only:
// never message text, names, numbers or IDs.
//
// It reads the SQLCipher key from the "Signal Safe Storage" keychain item,
// which can raise a macOS prompt. With -v2 it matches the archive against a
// v2 store; point it at a COPY of the live store, because opening a store
// applies pending migrations. With -apply it also appends the frames to that
// copy through the real ingest worker and reports what landed.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/signaldesktop"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	_ "modernc.org/sqlite"
)

const accountID = "signal-primary"

func main() {
	supportDir := flag.String("support-dir", signaldesktop.DefaultSupportDir(), "Signal Desktop data folder")
	v2Path := flag.String("v2", "", "path to a COPY of the v2 store.sqlite3 (required)")
	sinceMS := flag.Int64("since-ms", 1787102162676, "count rows sent at or after this time (default: v2's newest Signal message on 2026-10-11)")
	apply := flag.Bool("apply", false, "append the frames to the v2 copy through the ingest worker")
	legacyLatestMS := flag.Int64("legacy-latest-ms", 1790553694249, "the legacy store's newest Signal message (/api/status freshness.signal.legacy_latest_ms)")
	flag.Parse()
	if *v2Path == "" {
		fail("-v2 is required (a copy of the v2 store)")
	}
	ctx := context.Background()
	store, err := sqlite.Open(*v2Path)
	if err != nil {
		fail("open the v2 copy")
	}
	defer store.Close()
	facts, err := signaldesktop.LoadFacts(ctx, store, accountID)
	if err != nil {
		fail("read the v2 Signal account: " + err.Error())
	}
	node, err := signaldesktop.NodePath()
	if err != nil {
		fail(err.Error())
	}
	addon, err := signaldesktop.AddonPath()
	if err != nil {
		fail(err.Error())
	}
	var once sync.Once
	var key string
	var keyErr error
	runner := &signaldesktop.NodeRunner{
		DBPath: signaldesktop.DBPath(*supportDir), NodePath: node, AddonPath: addon,
		Key: func(ctx context.Context) (string, error) {
			once.Do(func() { key, keyErr = signaldesktop.SQLKey(ctx, *supportDir) })
			return key, keyErr
		},
	}

	var worker *ingest.Worker
	var sink *ingest.Sink
	counters := &ingest.Counters{}
	if *apply {
		messages, err := sqlite.NewMessageRepository(store, time.Now)
		if err != nil {
			fail("message repository")
		}
		reactions, err := sqlite.NewReactionRepository(store, time.Now)
		if err != nil {
			fail("reaction repository")
		}
		worker, err = ingest.NewWorker(ingest.WorkerConfig{
			Store: store, Messages: messages, Reactions: reactions, Counters: counters, Logger: zerolog.Nop(),
			QueueCapacity: 4096,
			Decoders: []ingest.DecoderRegistration{
				{Codec: ingest.SignalJSONRPCCodec, Platform: bridge.PlatformSignal, Decoder: ingest.NewSignalDecoder()},
				ingest.NewSignalDesktopHistoryDecoderRegistration(),
			},
		})
		if err != nil {
			fail("worker: " + err.Error())
		}
		sink, err = ingest.NewSink(ingest.SinkConfig{Messages: messages, Worker: worker, Counters: counters})
		if err != nil {
			fail("sink")
		}
	}

	var chunks, messages, reactions, newestMS int64
	byKind := map[string]int64{}
	stats, err := (&signaldesktop.Reader{Runner: runner}).Scan(ctx, facts, signaldesktop.ScanOptions{SinceMS: *sinceMS}, func(chunk signaldesktop.Chunk) error {
		chunks++
		messages += int64(len(chunk.Messages))
		kind := "direct"
		if chunk.Chat.Group {
			kind = "group"
		}
		if facts.Bound(chunk.Chat.RemoteID) {
			kind += "_bound"
		} else {
			kind += "_new"
		}
		byKind[kind] += int64(len(chunk.Messages))
		for _, message := range chunk.Messages {
			reactions += int64(len(message.Reactions))
			if message.SentAtMS > newestMS {
				newestMS = message.SentAtMS
			}
		}
		if sink == nil {
			return nil
		}
		record, err := ingest.SignalDesktopHistoryRecord(accountID, 0, chunk.HistorySnapshot(facts.SelfE164), time.Now())
		if err != nil {
			return err
		}
		return sink.AppendHistoryIngress(ctx, record)
	})
	if err != nil {
		// Source errors can carry paths; keep them out of this count-only
		// report except for the account checks, which carry no values.
		message := "Signal Desktop read failed"
		if err == signaldesktop.ErrAccountMismatch || err == signaldesktop.ErrAccountUnverified {
			message = err.Error()
		}
		fail(message)
	}
	output := map[string]any{
		"scan":                      stats,
		"chunks":                    chunks,
		"messages_to_send":          messages,
		"messages_to_send_by_chat":  byKind,
		"reactions_to_send":         reactions,
		"newest_message_to_send_at": formatMS(newestMS),
	}
	if worker != nil {
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- worker.Run(runCtx) }()
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			snapshot := counters.Snapshot(accountID)
			if snapshot.HistoryAppended+snapshot.HistoryDeduped == uint64(chunks) &&
				snapshot.HistoryImported+snapshot.HistoryExisting+snapshot.HistorySkipped >= uint64(messages) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(500 * time.Millisecond)
		cancel()
		<-done
		snapshot := counters.Snapshot(accountID)
		output["applied"] = map[string]any{
			"history_appended":      snapshot.HistoryAppended,
			"history_imported":      snapshot.HistoryImported,
			"history_existing":      snapshot.HistoryExisting,
			"history_skipped":       snapshot.HistorySkipped,
			"history_conversations": snapshot.HistoryConversations,
			"reactions_applied":     snapshot.ReactionsApplied,
			"quarantined":           snapshot.Quarantined,
			"appended_live":         snapshot.Appended,
			"projected_live":        snapshot.Projected,
		}
		var newest, count, afterLegacy int64
		raw, err := sql.Open("sqlite", "file:"+*v2Path+"?mode=ro")
		if err != nil {
			fail("open the v2 copy read-only")
		}
		defer raw.Close()
		if err := raw.QueryRowContext(ctx, `SELECT COALESCE(MAX(occurred_at_ms), 0), COUNT(*), COALESCE(SUM(occurred_at_ms > ?), 0) FROM messages WHERE account_id = ?`, *legacyLatestMS, accountID).Scan(&newest, &count, &afterLegacy); err != nil {
			fail("count v2 Signal messages")
		}
		output["v2_signal_messages"] = count
		output["v2_signal_messages_after_legacy_latest"] = afterLegacy
		output["v2_newest_signal_message_at"] = formatMS(newest)
		// /api/status flags projection_stalled when the legacy store's newest
		// Signal message is more than five minutes past v2's.
		output["projection_stalled_after"] = *legacyLatestMS-newest > 5*60*1000

		// A second pass over the same archive after applying should find
		// everything in v2: the feed is a fixed point on real data too.
		again, err := signaldesktop.LoadFacts(ctx, store, accountID)
		if err != nil {
			fail("reload v2 facts")
		}
		var secondMessages int64
		second, err := (&signaldesktop.Reader{Runner: runner}).Scan(ctx, again, signaldesktop.ScanOptions{}, func(chunk signaldesktop.Chunk) error {
			secondMessages += int64(len(chunk.Messages))
			return nil
		})
		if err != nil {
			fail("second pass failed")
		}
		output["second_pass"] = map[string]any{
			"messages_to_send":       secondMessages,
			"rows_in_v2":             second.RowsInV2,
			"rows_skipped_by_reason": second.RowsSkippedByReason,
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		os.Exit(1)
	}
}

func formatMS(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04 MST")
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
