package dataversion_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/dataversion"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestProbeStableWithoutCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	writer := openWALWriter(t, path, 4)
	probe := newProbe(t, path)

	before := readVersion(t, probe)
	var count int
	if err := writer.QueryRow(`SELECT COUNT(*) FROM scratch`).Scan(&count); err != nil {
		t.Fatalf("read scratch rows: %v", err)
	}
	if after := readVersion(t, probe); after != before {
		t.Fatalf("data_version moved from %d to %d with no commit", before, after)
	}
}

func TestProbeMovesOnCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	writer := openWALWriter(t, path, 4)
	probe := newProbe(t, path)

	before := readVersion(t, probe)
	insertScratch(t, writer, 1)
	if after := readVersion(t, probe); after == before {
		t.Fatalf("data_version stayed %d after a commit", before)
	}
}

func TestProbeIgnoresWritesThatCommitNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	writer := openWALWriter(t, path, 4)
	insertScratch(t, writer, 1)
	probe := newProbe(t, path)

	before := readVersion(t, probe)
	if _, err := writer.Exec(`UPDATE scratch SET value = value + 1 WHERE 0`); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	tx, err := writer.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO scratch (value) VALUES (2)`); err != nil {
		t.Fatalf("insert in rolled-back transaction: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if after := readVersion(t, probe); after != before {
		t.Fatalf("data_version moved from %d to %d although nothing committed", before, after)
	}
}

// TestProbeChangedIffCommitted runs random interleavings of commits from two
// writer pools (one capped at a single connection, like the legacy store, and
// one unbounded, like the v2 store), writes that commit nothing, plain reads
// and WAL checkpoints. Between any two probe reads the value must change if
// anything committed (no missed change) and stay put otherwise (no spurious
// change). The one intended exception: a TRUNCATE checkpoint resets the WAL,
// which moves data_version with nothing committed (PASSIVE, FULL and RESTART
// do not). That errs toward one extra refetch, never a missed change, so the
// test accepts either outcome after one.
func TestProbeChangedIffCommitted(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.sqlite3")
			writers := []*sql.DB{openWALWriter(t, path, 1), openWALWriter(t, path, 4)}
			probe := newProbe(t, path)
			random := rand.New(rand.NewSource(seed))

			last := readVersion(t, probe)
			committed, truncated := false, false
			var trace []string
			for step := 0; step < 60; step++ {
				writer := writers[random.Intn(len(writers))]
				switch random.Intn(6) {
				case 0, 1:
					insertScratch(t, writer, step)
					committed = true
					trace = append(trace, "commit")
				case 2:
					if _, err := writer.Exec(`UPDATE scratch SET value = value WHERE 0`); err != nil {
						t.Fatalf("no-op update: %v", err)
					}
					trace = append(trace, "no-op update")
				case 3:
					mode := []string{"PASSIVE", "FULL", "RESTART", "TRUNCATE"}[random.Intn(4)]
					if _, err := writer.Exec(`PRAGMA wal_checkpoint(` + mode + `)`); err != nil {
						t.Fatalf("checkpoint %s: %v", mode, err)
					}
					truncated = truncated || mode == "TRUNCATE"
					trace = append(trace, "checkpoint "+mode)
				default:
					current := readVersion(t, probe)
					changed := current != last
					if committed && !changed {
						t.Fatalf("step %d: data_version stayed %d after a commit; ops since last read: %v", step, current, trace)
					}
					if !committed && !truncated && changed {
						t.Fatalf("step %d: data_version %d -> %d with nothing committed; ops since last read: %v", step, last, current, trace)
					}
					last = current
					committed, truncated = false, false
					trace = trace[:0]
				}
			}
		})
	}
}

// The notifier watches both real stores. A draft is the case that motivated
// watching the legacy store: the draft_message MCP tool writes it there, often
// from another process, and publishes nothing.
func TestProbeSeesLegacyStoreDraftWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	legacy, err := db.New(path)
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = legacy.Close() })
	probe := newProbe(t, path)

	before := readVersion(t, probe)
	if err := legacy.UpsertDraft(&db.Draft{
		DraftID:        "draft-1",
		ConversationID: "conversation-1",
		Body:           "drafted by an agent",
		CreatedAt:      time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("UpsertDraft(): %v", err)
	}
	if after := readVersion(t, probe); after == before {
		t.Fatalf("data_version stayed %d after a legacy draft write", before)
	}
}

func TestProbeSeesV2StoreWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	probe := newProbe(t, path)

	before := readVersion(t, probe)
	nowMS := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID:   "account-1",
		BridgeKey:   "bridge-1",
		DisplayName: "Account",
		Mode:        sqlite.AccountModeArchive,
		ConfigJSON:  "{}",
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatalf("UpsertAccount(): %v", err)
	}
	if after := readVersion(t, probe); after == before {
		t.Fatalf("data_version stayed %d after a v2 store write", before)
	}
}

// The probe opens read-only: it must neither create a missing database nor be
// able to write an existing one.
func TestProbeMissingFileFailsWithoutCreatingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite3")
	probe := newProbe(t, path)
	if _, err := probe.Read(context.Background()); err == nil {
		t.Fatal("Read() of a missing database succeeded")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("probe created %s (stat err = %v)", path, err)
	}
}

func TestProbeConnectionIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	openWALWriter(t, path, 1)
	probe := newProbe(t, path)
	readVersion(t, probe)
	if err := dataversion.ExecOnProbeConnForTest(probe, `INSERT INTO scratch (value) VALUES (1)`); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Fatalf("write on the probe connection error = %v, want a read-only error", err)
	}
}

func TestProbeAfterCloseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	openWALWriter(t, path, 1)
	probe := dataversion.New(path)
	readVersion(t, probe)
	if err := probe.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatalf("second Close(): %v", err)
	}
	if _, err := probe.Read(context.Background()); !errors.Is(err, dataversion.ErrClosed) {
		t.Fatalf("Read() after Close error = %v, want ErrClosed", err)
	}
}

func TestProbeCanceledReadDropsConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	openWALWriter(t, path, 1)
	probe := newProbe(t, path)
	readVersion(t, probe)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := probe.Read(ctx); err == nil {
		t.Fatal("Read() with a canceled context succeeded")
	}
	// The failed read dropped its connection; the next read opens another.
	readVersion(t, probe)
}

// openWALWriter opens a writer pool on path in WAL mode, as both stores do,
// with a scratch table to write to.
func openWALWriter(t *testing.T, path string, maxOpen int) *sql.DB {
	t.Helper()
	writer, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	writer.SetMaxOpenConns(maxOpen)
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := writer.Exec(`CREATE TABLE IF NOT EXISTS scratch (value INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create scratch table: %v", err)
	}
	return writer
}

func newProbe(t *testing.T, path string) *dataversion.Probe {
	t.Helper()
	probe := dataversion.New(path)
	// Cleanups run last-registered first, so the probe closes before the
	// writers that were opened ahead of it.
	t.Cleanup(func() {
		if err := probe.Close(); err != nil {
			t.Errorf("probe Close(): %v", err)
		}
	})
	return probe
}

func readVersion(t *testing.T, probe *dataversion.Probe) int64 {
	t.Helper()
	version, err := probe.Read(context.Background())
	if err != nil {
		t.Fatalf("Read(): %v", err)
	}
	return version
}

func insertScratch(t *testing.T, writer *sql.DB, value int) {
	t.Helper()
	if _, err := writer.Exec(`INSERT INTO scratch (value) VALUES (?)`, value); err != nil {
		t.Fatalf("insert scratch row: %v", err)
	}
}
