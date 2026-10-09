//go:build replayharness

// Replay harness for ingest changes (build tag replayharness; CI only vets
// it). TestReplayInboxCorpus replays every inbox frame of a store COPY, in
// receipt order, through the real decoders into a fresh store, one drain per
// frame, with every clock pinned to the frame's received_at_ms. Run it at two
// commits and compare the outputs with TestReplayStoresMatch for a
// differential check of a change on real traffic. It never opens the live
// store: point OM_REPLAY_SOURCE at a copy (see docs/agent-runbook.md).
//
//	OM_REPLAY_SOURCE   store copy to read frames from (opened read-only)
//	OM_REPLAY_OUT      fresh target store path (must not exist)
//	OM_REPLAY_METRICS  JSON metrics output path
//	OM_REPLAY_CODECS   optional comma list of codecs to replay (default all)
//	OM_REPLAY_LIMIT    optional max frames
//	OM_REPLAY_LOG      print the worker's log (quarantine causes) to stderr
//	OM_REPLAY_STUCK    TestReplayStuckBacklog: number of conversation frames to
//	                   leave stuck (transient failure on every attempt)
//	OM_REPLAY_A/_B     TestReplayStoresMatch: the two outputs to compare
//	OM_REPLAY_IGNORE   TestReplayStoresMatch: table.column list reported but
//	                   not required to match
//
// Row writes and commits are counted with SQLite's pre-update and commit hooks
// on the target store's connections only; wal_autocheckpoint is disabled there
// so the final WAL size is the WAL volume the replay wrote.
package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/rs/zerolog"
	moderncsqlite "modernc.org/sqlite"
)

type replayFrame struct {
	InboxID      string
	AccountID    string
	Generation   int64
	DedupeKey    string
	Codec        string
	CodecVersion int64
	ReceivedAtMS int64
	Payload      []byte
}

type replayMeter struct {
	mu      sync.Mutex
	writes  map[string]map[string]int64
	commits atomic.Int64
	enabled atomic.Bool
}

var (
	replayHookOnce sync.Once
	replayMeters   sync.Map // marker -> *replayMeter
)

func replayOpName(op int32) string {
	switch op {
	case 18:
		return "insert"
	case 9:
		return "delete"
	case 23:
		return "update"
	default:
		return strconv.Itoa(int(op))
	}
}

func installReplayHook() {
	replayHookOnce.Do(func() {
		moderncsqlite.RegisterConnectionHook(func(conn moderncsqlite.ExecQuerierContext, dsn string) error {
			var meter *replayMeter
			replayMeters.Range(func(key, value any) bool {
				if strings.Contains(dsn, key.(string)) {
					meter = value.(*replayMeter)
					return false
				}
				return true
			})
			if meter == nil {
				return nil
			}
			if _, err := conn.ExecContext(context.Background(), "PRAGMA wal_autocheckpoint=0", nil); err != nil {
				return err
			}
			registerer, ok := conn.(moderncsqlite.HookRegisterer)
			if !ok {
				return fmt.Errorf("replay hook: connection does not register hooks")
			}
			registerer.RegisterPreUpdateHook(func(data moderncsqlite.SQLitePreUpdateData) {
				if !meter.enabled.Load() {
					return
				}
				meter.mu.Lock()
				defer meter.mu.Unlock()
				byOp := meter.writes[data.TableName]
				if byOp == nil {
					byOp = make(map[string]int64)
					meter.writes[data.TableName] = byOp
				}
				byOp[replayOpName(data.Op)]++
			})
			registerer.RegisterCommitHook(func() int32 {
				if meter.enabled.Load() {
					meter.commits.Add(1)
				}
				return 0
			})
			return nil
		})
	})
}

func newReplayMeter(marker string) *replayMeter {
	installReplayHook()
	meter := &replayMeter{writes: make(map[string]map[string]int64)}
	replayMeters.Store(marker, meter)
	return meter
}

func (m *replayMeter) snapshot() (map[string]map[string]int64, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]map[string]int64, len(m.writes))
	for table, ops := range m.writes {
		copied := make(map[string]int64, len(ops))
		for op, n := range ops {
			copied[op] = n
		}
		out[table] = copied
	}
	return out, m.commits.Load()
}

func loadReplayFrames(t *testing.T, source string, codecs map[string]bool, limit int) []replayFrame {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+source+"?mode=ro")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`
		SELECT inbox_id, account_id, generation, dedupe_key, codec, codec_version, received_at_ms, payload
		FROM inbox ORDER BY received_at_ms, inbox_id`)
	if err != nil {
		t.Fatalf("read source inbox: %v", err)
	}
	defer rows.Close()
	frames := make([]replayFrame, 0, 32768)
	for rows.Next() {
		var f replayFrame
		if err := rows.Scan(&f.InboxID, &f.AccountID, &f.Generation, &f.DedupeKey, &f.Codec, &f.CodecVersion, &f.ReceivedAtMS, &f.Payload); err != nil {
			t.Fatalf("scan source frame: %v", err)
		}
		if len(codecs) > 0 && !codecs[f.Codec] {
			continue
		}
		frames = append(frames, f)
		if limit > 0 && len(frames) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate source frames: %v", err)
	}
	return frames
}

func seedReplayAccounts(t *testing.T, source string, store *sqlite.Store) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+source+"?mode=ro")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer db.Close()
	accounts, err := db.Query(`SELECT account_id, bridge_key, remote_account_id, display_name, mode, enabled, config_json, created_at_ms, updated_at_ms FROM accounts ORDER BY account_id`)
	if err != nil {
		t.Fatalf("read accounts: %v", err)
	}
	var seeded []sqlite.Account
	for accounts.Next() {
		var a sqlite.Account
		var remote sql.NullString
		if err := accounts.Scan(&a.AccountID, &a.BridgeKey, &remote, &a.DisplayName, &a.Mode, &a.Enabled, &a.ConfigJSON, &a.CreatedAtMS, &a.UpdatedAtMS); err != nil {
			t.Fatalf("scan account: %v", err)
		}
		if remote.Valid {
			value := remote.String
			a.RemoteAccountID = &value
		}
		seeded = append(seeded, a)
	}
	accounts.Close()
	for _, a := range seeded {
		if err := store.UpsertAccount(a); err != nil {
			t.Fatalf("seed account %q: %v", a.AccountID, err)
		}
	}
	devices, err := db.Query(`SELECT device_id, account_id, remote_device_id, kind, display_name, state, is_current, last_seen_at_ms, created_at_ms, updated_at_ms FROM devices ORDER BY device_id`)
	if err != nil {
		t.Fatalf("read devices: %v", err)
	}
	var seededDevices []sqlite.Device
	for devices.Next() {
		var d sqlite.Device
		var remote sql.NullString
		var lastSeen sql.NullInt64
		if err := devices.Scan(&d.DeviceID, &d.AccountID, &remote, &d.Kind, &d.DisplayName, &d.State, &d.IsCurrent, &lastSeen, &d.CreatedAtMS, &d.UpdatedAtMS); err != nil {
			t.Fatalf("scan device: %v", err)
		}
		if remote.Valid {
			value := remote.String
			d.RemoteDeviceID = &value
		}
		if lastSeen.Valid {
			value := lastSeen.Int64
			d.LastSeenAtMS = &value
		}
		seededDevices = append(seededDevices, d)
	}
	devices.Close()
	for _, d := range seededDevices {
		if err := store.UpsertDevice(d); err != nil {
			t.Fatalf("seed device %q: %v", d.DeviceID, err)
		}
	}
}

// replayLogger prints the worker's log (quarantine causes among it) when
// OM_REPLAY_LOG is set.
func replayLogger() zerolog.Logger {
	if os.Getenv("OM_REPLAY_LOG") == "" {
		return zerolog.Nop()
	}
	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr})
}

type replayEchoes struct{}

func (replayEchoes) ObserveTransportEcho(context.Context, messaging.TransportEcho) (messaging.EchoOutcome, error) {
	return messaging.EchoNotFound, nil
}

type replayDecoder struct {
	inner   bridge.Decoder
	current *atomic.Value
	decodes *sync.Map // dedupe key -> *atomic.Int64
}

func (d replayDecoder) Decode(ctx context.Context, record bridge.RawIngressRecord) ([]bridge.Event, error) {
	d.current.Store(record.DedupeKey)
	if d.decodes != nil {
		counter, _ := d.decodes.LoadOrStore(record.DedupeKey, new(atomic.Int64))
		counter.(*atomic.Int64).Add(1)
	}
	return d.inner.Decode(ctx, record)
}

type replayRig struct {
	store    *sqlite.Store
	messages *sqlite.MessageRepository
	worker   *Worker
	sink     *Sink
	counters *Counters
	clock    *atomic.Int64
	nextID   *atomic.Value
	current  *atomic.Value
	decodes  *sync.Map
}

func newReplayRig(t *testing.T, store *sqlite.Store, clock *atomic.Int64, nextID *atomic.Value) *replayRig {
	t.Helper()
	now := func() time.Time { return time.UnixMilli(clock.Load()) }
	messages, err := sqlite.NewMessageRepository(store, now)
	if err != nil {
		t.Fatal(err)
	}
	reactions, err := sqlite.NewReactionRepository(store, now)
	if err != nil {
		t.Fatal(err)
	}
	counters := &Counters{}
	current := &atomic.Value{}
	current.Store("")
	decodes := &sync.Map{}
	whatsapp := NewWhatsAppDecoderRegistration()
	worker, err := NewWorker(WorkerConfig{
		Store:        store,
		Messages:     messages,
		Reactions:    reactions,
		EchoObserver: replayEchoes{},
		Counters:     counters,
		Logger:       replayLogger(),
		Now:          now,
		Decoders: []DecoderRegistration{
			{Codec: GoogleCodec, Platform: bridge.PlatformGoogle, Decoder: replayDecoder{inner: NewGoogleDecoder(counters), current: current, decodes: decodes}},
			{Codec: GoogleHistoryCodec, Platform: bridge.PlatformGoogle, Decoder: replayDecoder{inner: NewGoogleDecoder(counters), current: current, decodes: decodes}, History: true},
			{Codec: whatsapp.Codec, Platform: whatsapp.Platform, Decoder: replayDecoder{inner: whatsapp.Decoder, current: current, decodes: decodes}},
			{Codec: SignalJSONRPCCodec, Platform: bridge.PlatformSignal, Decoder: replayDecoder{inner: NewSignalDecoder(), current: current, decodes: decodes}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewSink(SinkConfig{
		Messages: messages,
		Worker:   worker,
		Counters: counters,
		IDs: messaging.IDSourceFunc(func() (string, error) {
			return nextID.Load().(string), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &replayRig{store: store, messages: messages, worker: worker, sink: sink, counters: counters, clock: clock, nextID: nextID, current: current, decodes: decodes}
}

// pump mirrors Run: handle queued replays, then drain.
func (r *replayRig) pump(ctx context.Context) {
	for {
		select {
		case item := <-r.worker.work:
			if item.replay {
				r.worker.handleRecord(ctx, item.inboxID, item.record)
			}
			continue
		default:
		}
		break
	}
	r.worker.drain(ctx)
}

func (r *replayRig) append(t *testing.T, ctx context.Context, f replayFrame) {
	t.Helper()
	r.clock.Store(f.ReceivedAtMS)
	r.nextID.Store(f.InboxID)
	if err := r.sink.AppendIngress(ctx, bridge.RawIngressRecord{
		AccountID:    f.AccountID,
		Generation:   bridge.Generation(f.Generation),
		DedupeKey:    f.DedupeKey,
		Codec:        f.Codec,
		CodecVersion: uint32(f.CodecVersion),
		ReceivedAt:   time.UnixMilli(f.ReceivedAtMS),
		Payload:      f.Payload,
	}); err != nil {
		t.Fatalf("append frame %s: %v", f.InboxID, err)
	}
}

type replayCodecStats struct {
	Frames     int     `json:"frames"`
	DrainMS    float64 `json:"drain_ms_total"`
	DrainP50MS float64 `json:"drain_ms_p50"`
	DrainP95MS float64 `json:"drain_ms_p95"`
	DrainMaxMS float64 `json:"drain_ms_max"`
	durations  []time.Duration
}

type replayMetrics struct {
	Label            string                       `json:"label"`
	Source           string                       `json:"source"`
	Frames           int                          `json:"frames"`
	WallMS           float64                      `json:"wall_ms"`
	PerCodec         map[string]*replayCodecStats `json:"per_codec"`
	RowWrites        map[string]map[string]int64  `json:"row_writes"`
	RowWritesTotal   int64                        `json:"row_writes_total"`
	Commits          int64                        `json:"commits"`
	WALBytes         int64                        `json:"wal_bytes"`
	Unprocessed      int                          `json:"unprocessed_after"`
	Counters         map[string]CounterSnapshot   `json:"counters"`
	StuckFrames      int                          `json:"stuck_frames,omitempty"`
	StuckUnprocessed int                          `json:"stuck_unprocessed,omitempty"`
	StuckDecodes     int64                        `json:"stuck_frame_decodes,omitempty"`
	AfterStuckFrames int                          `json:"frames_after_stuck,omitempty"`
	AfterStuckMS     float64                      `json:"drain_ms_after_stuck_total,omitempty"`
	RestartDrainMS   float64                      `json:"restart_drain_ms,omitempty"`
}

func finalizeCodecStats(stats map[string]*replayCodecStats) {
	for _, s := range stats {
		sort.Slice(s.durations, func(i, j int) bool { return s.durations[i] < s.durations[j] })
		if n := len(s.durations); n > 0 {
			s.DrainP50MS = float64(s.durations[n/2].Microseconds()) / 1000
			s.DrainP95MS = float64(s.durations[(n*95)/100].Microseconds()) / 1000
			s.DrainMaxMS = float64(s.durations[n-1].Microseconds()) / 1000
		}
	}
}

func replayEnv(t *testing.T) (source, out, metricsPath string, codecs map[string]bool, limit int) {
	t.Helper()
	source = os.Getenv("OM_REPLAY_SOURCE")
	out = os.Getenv("OM_REPLAY_OUT")
	metricsPath = os.Getenv("OM_REPLAY_METRICS")
	if source == "" || out == "" || metricsPath == "" {
		t.Skip("set OM_REPLAY_SOURCE, OM_REPLAY_OUT and OM_REPLAY_METRICS to run the replay harness")
	}
	if strings.Contains(source, "Application Support/OpenMessage") {
		t.Fatalf("OM_REPLAY_SOURCE must be a copy, not the live store")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatalf("OM_REPLAY_OUT %q already exists", out)
	}
	if raw := os.Getenv("OM_REPLAY_CODECS"); raw != "" {
		codecs = make(map[string]bool)
		for _, codec := range strings.Split(raw, ",") {
			codecs[strings.TrimSpace(codec)] = true
		}
	}
	if raw := os.Getenv("OM_REPLAY_LIMIT"); raw != "" {
		limit, _ = strconv.Atoi(raw)
	}
	return source, out, metricsPath, codecs, limit
}

func writeReplayMetrics(t *testing.T, path string, metrics *replayMetrics) {
	t.Helper()
	data, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("metrics: %s", data)
}

func countUnprocessed(t *testing.T, messages *sqlite.MessageRepository) int {
	t.Helper()
	pending, err := messages.Unprocessed(context.Background())
	if err != nil {
		t.Fatalf("Unprocessed(): %v", err)
	}
	return len(pending)
}

func TestReplayInboxCorpus(t *testing.T) {
	source, out, metricsPath, codecs, limit := replayEnv(t)
	ctx := context.Background()
	frames := loadReplayFrames(t, source, codecs, limit)
	meter := newReplayMeter(filepath.Base(out))
	store, err := sqlite.Open(out)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	seedReplayAccounts(t, source, store)
	clock := &atomic.Int64{}
	clock.Store(1)
	nextID := &atomic.Value{}
	nextID.Store("")
	rig := newReplayRig(t, store, clock, nextID)

	metrics := &replayMetrics{Label: os.Getenv("OM_REPLAY_LABEL"), Source: source, Frames: len(frames), PerCodec: map[string]*replayCodecStats{}}
	meter.enabled.Store(true)
	start := time.Now()
	for _, f := range frames {
		rig.append(t, ctx, f)
		began := time.Now()
		rig.pump(ctx)
		elapsed := time.Since(began)
		stats := metrics.PerCodec[f.Codec]
		if stats == nil {
			stats = &replayCodecStats{}
			metrics.PerCodec[f.Codec] = stats
		}
		stats.Frames++
		stats.DrainMS += float64(elapsed.Microseconds()) / 1000
		stats.durations = append(stats.durations, elapsed)
	}
	metrics.WallMS = float64(time.Since(start).Microseconds()) / 1000
	meter.enabled.Store(false)
	finalizeCodecStats(metrics.PerCodec)
	metrics.RowWrites, metrics.Commits = meter.snapshot()
	for _, ops := range metrics.RowWrites {
		for _, n := range ops {
			metrics.RowWritesTotal += n
		}
	}
	if info, err := os.Stat(out + "-wal"); err == nil {
		metrics.WALBytes = info.Size()
	}
	metrics.Unprocessed = countUnprocessed(t, rig.messages)
	metrics.Counters = rig.counters.PerAccount()
	if err := store.Close(); err != nil {
		t.Fatalf("close target: %v", err)
	}
	writeReplayMetrics(t, metricsPath, metrics)
	if metrics.Unprocessed != 0 {
		t.Fatalf("%d frames left unprocessed after replay", metrics.Unprocessed)
	}
}

// replayBusyError returns a real SQLITE_BUSY error, the kind the worker
// retries, by contending for a write lock held by another connection.
func replayBusyError(t *testing.T) error {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "busy.sqlite3") + "?_pragma=busy_timeout(0)"
	locker, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	contender, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = contender.Close() })
	if _, err := locker.Exec(`CREATE TABLE busy_probe (value INTEGER)`); err != nil {
		t.Fatal(err)
	}
	connection, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, err := connection.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	_, busy := contender.Exec(`INSERT INTO busy_probe (value) VALUES (1)`)
	_, _ = connection.ExecContext(context.Background(), `ROLLBACK`)
	if busy == nil || !isTransientDBError(busy) {
		t.Fatalf("contending insert error = %v, want a transient SQLite error", busy)
	}
	return busy
}

// TestReplayStuckBacklog replays Google frames with the first OM_REPLAY_STUCK
// conversation frames failing transiently on every attempt (the worker leaves
// them unprocessed), measures what every later frame's drain costs while they
// stay stuck, then clears the fault, restarts the worker and checks that no
// frame was lost.
func TestReplayStuckBacklog(t *testing.T) {
	source, out, metricsPath, codecs, limit := replayEnv(t)
	stuckCount, _ := strconv.Atoi(os.Getenv("OM_REPLAY_STUCK"))
	if stuckCount <= 0 {
		t.Skip("set OM_REPLAY_STUCK")
	}
	ctx := context.Background()
	frames := loadReplayFrames(t, source, codecs, limit)
	probe := NewGoogleDecoder(&Counters{})
	stuck := make(map[string]bool)
	for _, f := range frames {
		if len(stuck) >= stuckCount {
			break
		}
		if f.Codec != GoogleCodec {
			continue
		}
		events, err := probe.Decode(ctx, bridge.RawIngressRecord{AccountID: f.AccountID, DedupeKey: f.DedupeKey, Codec: f.Codec, CodecVersion: uint32(f.CodecVersion), ReceivedAt: time.UnixMilli(f.ReceivedAtMS), Payload: f.Payload})
		if err != nil {
			continue
		}
		for _, event := range events {
			if event.Kind == bridge.EventConversation {
				stuck[f.DedupeKey] = true
				break
			}
		}
	}
	busy := replayBusyError(t)

	meter := newReplayMeter(filepath.Base(out))
	store, err := sqlite.Open(out)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	seedReplayAccounts(t, source, store)
	clock := &atomic.Int64{}
	clock.Store(1)
	nextID := &atomic.Value{}
	nextID.Store("")
	rig := newReplayRig(t, store, clock, nextID)
	var faulting atomic.Bool
	faulting.Store(true)
	rig.worker.fault = func(point string) error {
		if point != "conversation-upserted" || !faulting.Load() {
			return nil
		}
		if stuck[rig.current.Load().(string)] {
			return busy
		}
		return nil
	}

	metrics := &replayMetrics{Label: os.Getenv("OM_REPLAY_LABEL"), Source: source, Frames: len(frames), PerCodec: map[string]*replayCodecStats{}, StuckFrames: len(stuck)}
	meter.enabled.Store(true)
	start := time.Now()
	seenStuck := 0
	for _, f := range frames {
		rig.append(t, ctx, f)
		began := time.Now()
		rig.pump(ctx)
		elapsed := time.Since(began)
		if stuck[f.DedupeKey] {
			seenStuck++
		} else if seenStuck == len(stuck) {
			metrics.AfterStuckFrames++
			metrics.AfterStuckMS += float64(elapsed.Microseconds()) / 1000
		}
		stats := metrics.PerCodec[f.Codec]
		if stats == nil {
			stats = &replayCodecStats{}
			metrics.PerCodec[f.Codec] = stats
		}
		stats.Frames++
		stats.DrainMS += float64(elapsed.Microseconds()) / 1000
		stats.durations = append(stats.durations, elapsed)
	}
	metrics.WallMS = float64(time.Since(start).Microseconds()) / 1000
	finalizeCodecStats(metrics.PerCodec)
	for key := range stuck {
		if counter, ok := rig.decodes.Load(key); ok {
			metrics.StuckDecodes += counter.(*atomic.Int64).Load()
		}
	}
	// A selected frame can fail validation before the fault point and be
	// quarantined instead; count the ones that really stayed stuck.
	metrics.StuckUnprocessed = countUnprocessed(t, rig.messages)
	if metrics.StuckUnprocessed == 0 {
		t.Errorf("no frame stayed stuck")
	}

	// Clear the fault and restart: a fresh worker (empty in-memory state) must
	// pick every stuck frame up from the durable inbox.
	faulting.Store(false)
	restarted := newReplayRig(t, store, clock, nextID)
	began := time.Now()
	restarted.pump(ctx)
	metrics.RestartDrainMS = float64(time.Since(began).Microseconds()) / 1000
	meter.enabled.Store(false)
	metrics.RowWrites, metrics.Commits = meter.snapshot()
	for _, ops := range metrics.RowWrites {
		for _, n := range ops {
			metrics.RowWritesTotal += n
		}
	}
	metrics.Unprocessed = countUnprocessed(t, restarted.messages)
	metrics.Counters = rig.counters.PerAccount()
	if err := store.Close(); err != nil {
		t.Fatalf("close target: %v", err)
	}
	writeReplayMetrics(t, metricsPath, metrics)
	if metrics.Unprocessed != 0 {
		t.Fatalf("%d frames left unprocessed after restart", metrics.Unprocessed)
	}
}

// TestReplayStoresMatch compares two replay outputs table by table. Columns
// in OM_REPLAY_IGNORE (table.column, comma separated) are reported but not
// required to match; every other column of every row must be identical.
func TestReplayStoresMatch(t *testing.T) {
	left, right := os.Getenv("OM_REPLAY_A"), os.Getenv("OM_REPLAY_B")
	if left == "" || right == "" {
		t.Skip("set OM_REPLAY_A and OM_REPLAY_B")
	}
	ignored := make(map[string]bool)
	for _, column := range strings.Split(os.Getenv("OM_REPLAY_IGNORE"), ",") {
		if column = strings.TrimSpace(column); column != "" {
			ignored[column] = true
		}
	}
	a, err := sql.Open("sqlite", "file:"+left+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := sql.Open("sqlite", "file:"+right+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	tables := replayTables(t, a)
	if got := replayTables(t, b); strings.Join(got, ",") != strings.Join(tables, ",") {
		t.Fatalf("table sets differ: %v vs %v", tables, got)
	}
	report := map[string]any{}
	for _, table := range tables {
		columns := replayColumns(t, a, table)
		order := strings.Join(columns, ", ")
		rowsA := replayDump(t, a, table, order)
		rowsB := replayDump(t, b, table, order)
		entry := map[string]any{"rows_a": len(rowsA), "rows_b": len(rowsB)}
		if len(rowsA) != len(rowsB) {
			t.Errorf("%s: row counts differ: %d vs %d", table, len(rowsA), len(rowsB))
		}
		// Rows are ordered by every column, so align on the non-ignored key.
		keyed := func(rows [][]any) map[string][]any {
			out := make(map[string][]any, len(rows))
			for _, row := range rows {
				var key strings.Builder
				for index, column := range columns {
					if ignored[table+"."+column] {
						continue
					}
					fmt.Fprintf(&key, "%v\x1f", row[index])
				}
				out[key.String()] = row
			}
			return out
		}
		ka, kb := keyed(rowsA), keyed(rowsB)
		onlyA, onlyB := 0, 0
		for key := range ka {
			if _, ok := kb[key]; !ok {
				onlyA++
				if onlyA <= 3 {
					t.Errorf("%s: row only in A: %v", table, ka[key])
				}
			}
		}
		for key := range kb {
			if _, ok := ka[key]; !ok {
				onlyB++
				if onlyB <= 3 {
					t.Errorf("%s: row only in B: %v", table, kb[key])
				}
			}
		}
		entry["only_a"], entry["only_b"] = onlyA, onlyB
		ignoredDiffs := map[string]map[string]int{}
		for key, rowA := range ka {
			rowB, ok := kb[key]
			if !ok {
				continue
			}
			for index, column := range columns {
				if !ignored[table+"."+column] {
					continue
				}
				diff := ignoredDiffs[column]
				if diff == nil {
					diff = map[string]int{}
					ignoredDiffs[column] = diff
				}
				va, vb := fmt.Sprint(rowA[index]), fmt.Sprint(rowB[index])
				switch {
				case va == vb:
					diff["equal"]++
				default:
					ia, errA := strconv.ParseInt(va, 10, 64)
					ib, errB := strconv.ParseInt(vb, 10, 64)
					if errA == nil && errB == nil && ib < ia {
						diff["b_older"]++
					} else if errA == nil && errB == nil && ib > ia {
						diff["b_newer"]++
					} else {
						diff["other"]++
					}
				}
			}
		}
		if len(ignoredDiffs) > 0 {
			entry["ignored_columns"] = ignoredDiffs
		}
		report[table] = entry
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("comparison: %s", data)
	if path := os.Getenv("OM_REPLAY_COMPARE_OUT"); path != "" {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func replayTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	return tables
}

func replayColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if len(columns) == 0 {
		t.Fatal(errors.New("no columns for " + table))
	}
	return columns
}

func replayDump(t *testing.T, db *sql.DB, table, order string) [][]any {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM "` + table + `" ORDER BY ` + order)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, _ := rows.Columns()
	var out [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		for i, v := range values {
			if raw, ok := v.([]byte); ok {
				values[i] = string(raw)
			}
		}
		out = append(out, values)
	}
	return out
}
