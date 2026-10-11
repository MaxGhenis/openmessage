package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/importer"
)

// Invariants pinned here:
//   L1  The importers run iff transports are on and the daemon is not demo,
//       not sandboxed and not v2-primary (all 16 gate inputs).
//   L2  An off daemon calls no importer, on the poll or the full rescan, under
//       every per-poll condition, so it writes nothing to the legacy store.
//   L3  An on daemon calls exactly the importers whose per-poll condition
//       holds, and the full rescan is the only full Signal scan.
//   L4  v2-primary leaves the legacy store's per-platform stats, which
//       /api/status compares v2 against for projection_stalled, unchanged.

func TestLocalImportsOffReasonCoversEveryGateInput(t *testing.T) {
	for mask := 0; mask < 16; mask++ {
		transports, demo, sandboxed, v2Primary := mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0
		got := localImportsOffReason(transports, demo, sandboxed, v2Primary)

		var want string
		switch {
		case demo:
			want = localImportsOffDemo
		case !transports:
			want = localImportsOffTransports
		case sandboxed:
			want = localImportsOffAppSandbox
		case v2Primary:
			want = localImportsOffV2Primary
		}
		if got != want {
			t.Errorf("localImportsOffReason(transports=%v, demo=%v, sandboxed=%v, v2Primary=%v) = %q, want %q",
				transports, demo, sandboxed, v2Primary, got, want)
		}
		if on := got == ""; on != (transports && !demo && !sandboxed && !v2Primary) {
			t.Errorf("mask %04b: on=%v disagrees with the rule", mask, on)
		}
	}
}

// importCalls records which importers a localImports called.
type importCalls struct {
	calls []string
}

func (c *importCalls) record(name string) {
	c.calls = append(c.calls, name)
}

func (c *importCalls) sorted() []string {
	out := append([]string(nil), c.calls...)
	sort.Strings(out)
	return out
}

type pollConditions struct {
	liveBridge, signalPaired, imessage bool
}

func allPollConditions() []pollConditions {
	var out []pollConditions
	for mask := 0; mask < 8; mask++ {
		out = append(out, pollConditions{
			liveBridge:   mask&1 != 0,
			signalPaired: mask&2 != 0,
			imessage:     mask&4 != 0,
		})
	}
	return out
}

func countingLocalImports(offReason string, cond pollConditions, calls *importCalls, imported int) *localImports {
	result := func() (*importer.ImportResult, error) {
		return &importer.ImportResult{MessagesImported: imported}, nil
	}
	return newLocalImports(localImportsConfig{
		Logger:             zerolog.Nop(),
		OffReason:          offReason,
		WhatsAppLiveBridge: func() bool { return cond.liveBridge },
		SignalAccount:      func() (string, bool) { return "+15550001111", cond.signalPaired },
		IMessageSupported:  cond.imessage,
		ImportWhatsApp: func(*db.Store) (*importer.ImportResult, error) {
			calls.record("whatsapp")
			return result()
		},
		ImportSignal: func(_ *db.Store, account string, full bool) (*importer.ImportResult, error) {
			calls.record(fmt.Sprintf("signal account=%s full=%v", account, full))
			return result()
		},
		ImportIMessage: func(*db.Store) (*importer.ImportResult, error) {
			calls.record("imessage")
			return result()
		},
	})
}

func TestLocalImportsOffCallsNoImporter(t *testing.T) {
	for _, reason := range []string{
		localImportsOffDemo,
		localImportsOffTransports,
		localImportsOffAppSandbox,
		localImportsOffV2Primary,
	} {
		for _, cond := range allPollConditions() {
			calls := &importCalls{}
			l := countingLocalImports(reason, cond, calls, 3)
			if l.enabled() {
				t.Fatalf("%s: enabled() = true", reason)
			}
			if l.syncOnce() {
				t.Errorf("%s %+v: syncOnce reported a change", reason, cond)
			}
			if n := l.fullSignalRescanOnce(); n != 0 {
				t.Errorf("%s %+v: full rescan stored %d", reason, cond, n)
			}
			if len(calls.calls) != 0 {
				t.Errorf("%s %+v: importers called: %v", reason, cond, calls.calls)
			}
		}
	}
}

func TestLocalImportsOnCallsEachImporterUnderItsCondition(t *testing.T) {
	for _, cond := range allPollConditions() {
		for _, imported := range []int{0, 2} {
			calls := &importCalls{}
			l := countingLocalImports("", cond, calls, imported)

			changed := l.syncOnce()
			var want []string
			if !cond.liveBridge {
				want = append(want, "whatsapp")
			}
			if cond.signalPaired {
				want = append(want, "signal account=+15550001111 full=false")
			}
			if cond.imessage {
				want = append(want, "imessage")
			}
			sort.Strings(want)
			if got := calls.sorted(); !reflect.DeepEqual(got, want) {
				t.Errorf("%+v poll: importers called %v, want %v", cond, got, want)
			}
			if wantChanged := imported > 0 && len(want) > 0; changed != wantChanged {
				t.Errorf("%+v imported=%d: syncOnce changed=%v, want %v", cond, imported, changed, wantChanged)
			}

			calls.calls = nil
			stored := l.fullSignalRescanOnce()
			want = nil
			wantStored := 0
			if cond.signalPaired {
				want = []string{"signal account=+15550001111 full=true"}
				wantStored = imported
			}
			if got := calls.sorted(); !reflect.DeepEqual(got, want) {
				t.Errorf("%+v full rescan: importers called %v, want %v", cond, got, want)
			}
			if stored != wantStored {
				t.Errorf("%+v full rescan stored %d, want %d", cond, stored, wantStored)
			}
		}
	}
}

func TestLocalImportsImporterErrorIsNotAChange(t *testing.T) {
	l := newLocalImports(localImportsConfig{
		Logger:             zerolog.Nop(),
		WhatsAppLiveBridge: func() bool { return false },
		ImportWhatsApp: func(*db.Store) (*importer.ImportResult, error) {
			return nil, fmt.Errorf("WhatsApp Desktop database not found")
		},
	})
	if l.syncOnce() {
		t.Fatal("a failed import reported a change")
	}
	if l.lastErr["whatsapp"] == "" {
		t.Fatal("the failure was not remembered for log de-duplication")
	}
}

// TestLocalImportsLogsRowsNotWritten: rows an importer could not write (the
// iMessage importer retries them next poll) are logged once, not on every
// repeat of the same failure, and are not a change by themselves.
func TestLocalImportsLogsRowsNotWritten(t *testing.T) {
	var logged bytes.Buffer
	l := newLocalImports(localImportsConfig{
		Logger:            zerolog.New(&logged),
		IMessageSupported: true,
		ImportIMessage: func(*db.Store) (*importer.ImportResult, error) {
			return &importer.ImportResult{Errors: []string{"imessage:g1: UNIQUE constraint failed"}}, nil
		},
	})
	for i := 0; i < 3; i++ {
		if l.syncOnce() {
			t.Fatal("a poll that wrote nothing reported a change")
		}
	}
	if n := strings.Count(logged.String(), "1 rows not written, first: imessage:g1: UNIQUE constraint failed"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, logged.String())
	}
}

// TestLocalImportsV2PrimaryLeavesLegacyFreshnessUnchanged runs importers that
// really write the legacy store (a newer WhatsApp and Signal message each
// call) and checks the per-platform stats /api/status uses for
// projection_stalled. The enabled control proves the check can see a write.
func TestLocalImportsV2PrimaryLeavesLegacyFreshnessUnchanged(t *testing.T) {
	newStore := func(t *testing.T) *db.Store {
		t.Helper()
		store, err := db.New(filepath.Join(t.TempDir(), "messages.db"))
		if err != nil {
			t.Fatalf("db.New: %v", err)
		}
		t.Cleanup(func() { store.Close() })
		for _, m := range []*db.Message{
			{MessageID: "whatsapp:base", ConversationID: "whatsapp:1@s.whatsapp.net", Body: "hi", TimestampMS: 1_000, SourcePlatform: "whatsapp", SourceID: "base"},
			{MessageID: "signal:base", ConversationID: "signal:+15550002222", Body: "hi", TimestampMS: 1_000, SourcePlatform: "signal", SourceID: "base"},
		} {
			if err := store.UpsertMessage(m); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		return store
	}
	stats := func(t *testing.T, store *db.Store) map[string]db.PlatformStat {
		t.Helper()
		rows, err := store.PlatformStats()
		if err != nil {
			t.Fatalf("PlatformStats: %v", err)
		}
		out := map[string]db.PlatformStat{}
		for _, row := range rows {
			out[row.Platform] = row
		}
		return out
	}
	writer := func(platform string) func(*db.Store) (*importer.ImportResult, error) {
		next := int64(10_000)
		return func(store *db.Store) (*importer.ImportResult, error) {
			next += 1_000
			id := fmt.Sprintf("%s:new-%d", platform, next)
			if err := store.UpsertMessage(&db.Message{
				MessageID: id, ConversationID: platform + ":desktop", Body: "from the desktop app",
				TimestampMS: next, SourcePlatform: platform, SourceID: id,
			}); err != nil {
				return nil, err
			}
			return &importer.ImportResult{MessagesImported: 1}, nil
		}
	}
	run := func(t *testing.T, offReason string) (before, after map[string]db.PlatformStat) {
		store := newStore(t)
		signalWriter := writer("signal")
		l := newLocalImports(localImportsConfig{
			Logger:             zerolog.Nop(),
			Store:              store,
			OffReason:          offReason,
			WhatsAppLiveBridge: func() bool { return false },
			SignalAccount:      func() (string, bool) { return "+15550001111", true },
			IMessageSupported:  true,
			ImportWhatsApp:     writer("whatsapp"),
			ImportSignal: func(store *db.Store, _ string, _ bool) (*importer.ImportResult, error) {
				return signalWriter(store)
			},
			ImportIMessage: writer("imessage"),
		})
		before = stats(t, store)
		for i := 0; i < 3; i++ {
			l.syncOnce()
			l.fullSignalRescanOnce()
		}
		return before, stats(t, store)
	}

	before, after := run(t, localImportsOffReason(true, false, false, true))
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("v2-primary changed legacy stats:\nbefore %+v\nafter  %+v", before, after)
	}

	before, after = run(t, localImportsOffReason(true, false, false, false))
	for _, platform := range []string{"whatsapp", "signal"} {
		if after[platform].LatestMS <= before[platform].LatestMS {
			t.Fatalf("control: enabled imports did not advance %s (%+v -> %+v)", platform, before[platform], after[platform])
		}
	}
}

func TestLocalImportsStatusJSON(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		sandboxed bool
		imessage  bool
		want      string
	}{
		{reason: "", want: `{"enabled":true,"app_sandbox":false,"imessage":false}`},
		{reason: "", imessage: true, want: `{"enabled":true,"app_sandbox":false,"imessage":true}`},
		{reason: localImportsOffV2Primary, imessage: true, want: `{"enabled":false,"off_reason":"v2_primary","app_sandbox":false,"imessage":false}`},
		{reason: localImportsOffAppSandbox, sandboxed: true, want: `{"enabled":false,"off_reason":"app_sandbox","app_sandbox":true,"imessage":false}`},
	} {
		cfg := localImportsConfig{Logger: zerolog.Nop(), OffReason: tc.reason, Sandboxed: tc.sandboxed, IMessageSupported: true}
		if tc.imessage {
			cfg.ImportIMessage = func(*db.Store) (*importer.ImportResult, error) { return &importer.ImportResult{}, nil }
		}
		l := newLocalImports(cfg)
		raw, err := json.Marshal(l.status())
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != tc.want {
			t.Errorf("status(%q) = %s, want %s", tc.reason, raw, tc.want)
		}
	}
}
