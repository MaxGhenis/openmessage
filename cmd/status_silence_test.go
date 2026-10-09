package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"
	_ "time/tzdata" // the incident replays run in America/New_York on any CI image

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/freshness"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
	"github.com/maxghenis/openmessage/internal/web"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// statusV2Account is one v2 account a status fixture seeds: its bridge key
// (which PlatformStats labels) and the ingest codec its frames arrive under.
type statusV2Account struct {
	id, bridgeKey, codec string
}

var (
	statusGoogleAccount   = statusV2Account{"google-primary", "google_messages", ingest.GoogleCodec}
	statusWhatsAppAccount = statusV2Account{"whatsapp-primary", "whatsmeow", whatsapplive.IngressCodec}
)

// statusV2Seed is one account's stored history: an incoming message at
// lastMessage (so PlatformStats lists the platform) and inbox frames received
// at each of frames.
type statusV2Seed struct {
	account     statusV2Account
	lastMessage time.Time
	frames      []time.Time
}

// seedStatusV2Store writes a migrated v2 store into dataDir/v2.
func seedStatusV2Store(t *testing.T, dataDir string, seeds ...statusV2Seed) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dataDir, "v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(filepath.Join(dataDir, "v2", "store.sqlite3"))
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, seed := range seeds {
		createdMS := seed.lastMessage.Add(-30 * 24 * time.Hour).UnixMilli()
		if err := store.UpsertAccount(sqlite.Account{
			AccountID: seed.account.id, BridgeKey: seed.account.bridgeKey, DisplayName: seed.account.id,
			Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
			CreatedAtMS: createdMS, UpdatedAtMS: createdMS,
		}); err != nil {
			t.Fatalf("UpsertAccount(%s): %v", seed.account.id, err)
		}
		conversationID := seed.account.id + "-conversation"
		if err := store.UpsertConversation(sqlite.Conversation{
			ConversationID: conversationID, AccountID: seed.account.id,
			RemoteConversationID: "remote-" + conversationID, Kind: sqlite.ConversationKindDirect,
			Title: "Thread", NotificationMode: sqlite.NotificationModeAll, MetadataJSON: "{}",
			CreatedAtMS: createdMS, UpdatedAtMS: createdMS,
		}); err != nil {
			t.Fatalf("UpsertConversation(%s): %v", conversationID, err)
		}
		var clock time.Time
		repository, err := sqlite.NewMessageRepository(store, func() time.Time { return clock })
		if err != nil {
			t.Fatal(err)
		}
		clock = seed.lastMessage
		if err := repository.ImportMessage(ctx, sqlite.MessageProjection{Message: sqlite.Message{
			MessageID: seed.account.id + "-message", ConversationID: conversationID, AccountID: seed.account.id,
			RemoteMessageID: "remote-message", Direction: sqlite.MessageDirectionIncoming, Body: "hi",
			State: sqlite.MessageStateActive, OccurredAtMS: seed.lastMessage.UnixMilli(),
		}}); err != nil {
			t.Fatalf("ImportMessage(%s): %v", seed.account.id, err)
		}
		for i, at := range seed.frames {
			clock = at
			if _, err := repository.AppendInbox(ctx, sqlite.InboxRecord{
				InboxID: fmt.Sprintf("%s-frame-%d", seed.account.id, i), AccountID: seed.account.id,
				Generation: 1, DedupeKey: fmt.Sprintf("frame-%d", i),
				Codec: seed.account.codec, CodecVersion: 1, Payload: []byte("{}"),
			}); err != nil {
				t.Fatalf("AppendInbox(%s #%d): %v", seed.account.id, i, err)
			}
		}
	}
}

// setStatusEnv points the CLI at dataDir, reading the v2 store when v2 is set
// and the legacy store otherwise.
func setStatusEnv(t *testing.T, dataDir string, v2 bool) {
	t.Helper()
	t.Setenv("OPENMESSAGES_DATA_DIR", dataDir)
	t.Setenv("OPENMESSAGES_DEMO", "0")
	t.Setenv("OPENMESSAGES_APP_SANDBOX", "1")
	if v2 {
		t.Setenv("OPENMESSAGES_V2_PRIMARY", "1")
		t.Setenv("OPENMESSAGES_V2_SEND", "")
		t.Setenv("OPENMESSAGES_V2_INGEST", "")
	} else {
		t.Setenv("OPENMESSAGES_V2_PRIMARY", "0")
		t.Setenv("OPENMESSAGES_V2_SEND", "0")
		t.Setenv("OPENMESSAGES_V2_INGEST", "0")
	}
}

// openStatusSession opens dataDir the way `openmessage status` does.
func openStatusSession(t *testing.T, dataDir string, v2 bool) *commandReadSession {
	t.Helper()
	setStatusEnv(t, dataDir, v2)
	session, err := openCommandReadSource(zerolog.Nop(), io.Discard)
	if err != nil {
		t.Fatalf("openCommandReadSource(): %v", err)
	}
	t.Cleanup(session.Close)
	return session
}

func runStatusOutput(t *testing.T, session *commandReadSession, deps statusDeps, asJSON bool) string {
	t.Helper()
	var out bytes.Buffer
	deps.output = &out
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.loc == nil {
		deps.loc = time.Local
	}
	if err := runStatus(context.Background(), session, deps, asJSON); err != nil {
		t.Fatalf("runStatus(): %v", err)
	}
	return out.String()
}

// statusJSON is the slice of `openmessage status --json` the tests read.
type statusJSON struct {
	Platforms []struct {
		Platform        string          `json:"platform"`
		BehindDays      *int            `json:"behind_days"`
		Silence         json.RawMessage `json:"silence"`
		SilenceJudgedBy string          `json:"silence_judged_by"`
	} `json:"platforms"`
	SilenceNote string `json:"silence_note"`
}

func decodeStatusJSON(t *testing.T, raw string) statusJSON {
	t.Helper()
	var out statusJSON
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode status --json: %v\n%s", err, raw)
	}
	return out
}

func silenceFields(t *testing.T, block json.RawMessage) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(block, &fields); err != nil {
		t.Fatalf("decode silence block %s: %v", block, err)
	}
	return fields
}

// statusRowLine returns the table line for platform.
func statusRowLine(t *testing.T, text, platform string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, platform+" ") {
			return line
		}
	}
	t.Fatalf("no %s row in:\n%s", platform, text)
	return ""
}

// fakeDaemon serves body as /api/status and returns a client for it.
func fakeDaemon(t *testing.T, body string) *localapi.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return localapi.NewClient(server.URL, "")
}

// downDaemon returns a client whose daemon refuses connections, the way
// internal/localapi's tests reach one.
func downDaemon(t *testing.T) *localapi.Client {
	t.Helper()
	return localapi.NewClient("http://127.0.0.1:1", "")
}

// pinLocal sets time.Local for the test. The daemon judges silence in
// time.Local, and pinning it to a zone other than the one a test passes the
// CLI (statusDeps.loc) proves the CLI used the one it was given.
func pinLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	saved := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = saved })
}

// statusPlatform returns the --json row for platform.
func statusPlatform(t *testing.T, status statusJSON, platform string) (row struct {
	Platform        string          `json:"platform"`
	BehindDays      *int            `json:"behind_days"`
	Silence         json.RawMessage `json:"silence"`
	SilenceJudgedBy string          `json:"silence_judged_by"`
}) {
	t.Helper()
	for _, candidate := range status.Platforms {
		if candidate.Platform == platform {
			return candidate
		}
	}
	t.Fatalf("no %s row in %+v", platform, status.Platforms)
	return row
}

func newYork(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// stall1006Frames is shaped like the Google ingest history behind the
// 2026-10-06 stall: daytime traffic (a frame every half hour, 08:00 to 22:30)
// on each of the 14 days before, then a last frame at 01:11 on 10/6 and
// nothing after it.
func stall1006Frames(loc *time.Location) []time.Time {
	var frames []time.Time
	for day := 22; day <= 35; day++ { // 2026-09-22 .. 2026-10-05
		for minute := 8 * 60; minute < 23*60; minute += 30 {
			frames = append(frames, time.Date(2026, 9, day, 0, minute, 0, 0, loc))
		}
	}
	return append(frames, time.Date(2026, 10, 6, 1, 11, 0, 0, loc))
}

// A 10/6-shaped Google stall through the CLI with the app down: the last
// frame reached the v2 inbox at 01:11 and the 14 days before carried daytime
// traffic. Before #190 nothing flagged the real one: Google was the newest
// platform, so it was never "behind". Judged locally on the v2 inbox against
// this baseline, it is quiet but expected at 13:30 (5.5 expected-active hours,
// under the 6-hour limit) and stalled at 14:12. time.Local is UTC here, so
// the verdict must come from the zone the CLI was given.
func TestRunStatusFlagsThe20261006GoogleStallLocally(t *testing.T) {
	loc := newYork(t)
	pinLocal(t, time.UTC)
	dataDir := t.TempDir()
	seedStatusV2Store(t, dataDir, statusV2Seed{
		account:     statusGoogleAccount,
		lastMessage: time.Date(2026, 10, 5, 22, 40, 0, 0, loc),
		frames:      stall1006Frames(loc),
	})
	session := openStatusSession(t, dataDir, true)
	at := func(day, hour, minute int) func() time.Time {
		return func() time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, loc) }
	}

	quiet := runStatusOutput(t, session, statusDeps{daemon: downDaemon(t), now: at(6, 13, 30), loc: loc}, false)
	if line := statusRowLine(t, quiet, "sms"); strings.Contains(line, "⚠") {
		t.Fatalf("at 13:30 the sms row = %q, want no warning (5.5 expected-active hours)", line)
	}

	text := runStatusOutput(t, session, statusDeps{daemon: downDaemon(t), now: at(6, 14, 12), loc: loc}, false)
	line := statusRowLine(t, text, "sms")
	if !strings.HasSuffix(line, "⚠ silent 13h") {
		t.Fatalf("at 14:12 the sms row = %q, want it to end in %q", line, "⚠ silent 13h")
	}
	if strings.Contains(text, "longer than usual") || strings.Contains(line, "behind") {
		t.Fatalf("status text = %q, want only the silent warning", text)
	}
	if !strings.Contains(text, "Silence judged locally from the v2 inbox (the app isn't running).") {
		t.Fatalf("status text = %q, want the local-verdict note", text)
	}

	raw := runStatusOutput(t, session, statusDeps{daemon: downDaemon(t), now: at(6, 14, 12), loc: loc}, true)
	status := decodeStatusJSON(t, raw)
	if len(status.Platforms) != 1 || status.Platforms[0].Platform != "sms" {
		t.Fatalf("platforms = %+v, want one sms row", status.Platforms)
	}
	row := status.Platforms[0]
	if row.SilenceJudgedBy != "local" || row.BehindDays == nil || *row.BehindDays != 0 {
		t.Fatalf("sms row judged by %q, behind %v; want local, 0", row.SilenceJudgedBy, row.BehindDays)
	}
	silence := silenceFields(t, row.Silence)
	want := map[string]any{
		"source":                       freshness.SourceV2Inbox,
		"last_event_ms":                float64(time.Date(2026, 10, 6, 1, 11, 0, 0, loc).UnixMilli()),
		"silent_ms":                    float64((13*time.Hour + time.Minute).Milliseconds()),
		"expected_active_hours":        6.2,
		"expected_active_hours_limit":  6.0,
		"max_silent_ms":                float64((16 * time.Hour).Milliseconds()),
		"long_silent_ms":               float64((72 * time.Hour).Milliseconds()),
		"baseline_days":                14.0,
		"baseline_active_days":         14.0,
		"baseline_events":              420.0,
		"baseline_median_daily_events": 30.0,
		"evaluated":                    true,
		"stalled":                      true,
		"rule":                         freshness.RuleExpectedActivity,
	}
	if !reflect.DeepEqual(silence, want) {
		t.Fatalf("silence = %v\nwant      %v", silence, want)
	}
	if status.SilenceNote != "Silence judged locally from the v2 inbox (the app isn't running)." {
		t.Fatalf("silence_note = %q", status.SilenceNote)
	}

	// At 15:00 on 10/7, half an hour before the phone restart that ended the
	// real stall, it is 37 hours old.
	late := runStatusOutput(t, session, statusDeps{daemon: downDaemon(t), now: at(7, 15, 0), loc: loc}, false)
	if line := statusRowLine(t, late, "sms"); !strings.HasSuffix(line, "⚠ silent 37h") {
		t.Fatalf("on 10/7 15:00 the sms row = %q, want %q", line, "⚠ silent 37h")
	}
}

// daemonStatusWithGoogleSilence is a post-#190 /api/status for dataDir whose
// Google silence verdict is block.
func daemonStatusWithGoogleSilence(dataDir, block string) string {
	auth, _ := json.Marshal(map[string]any{"data_dir": dataDir})
	return `{"v2_primary":true,"auth":` + string(auth) + `,"freshness":{"newest_ms":1,"silence_stalled":true,` +
		`"google":{"behind_days":0,"stale":true,"stale_reason":"silent","silence":` + block + `}}}`
}

// With the app running on the same data dir, its verdict is the one shown,
// unchanged as JSON, even where the store alone would judge otherwise (the
// daemon may have carried a verdict over or seen frames the CLI has not).
func TestRunStatusShowsTheRunningAppsSilenceVerdict(t *testing.T) {
	loc := newYork(t)
	dataDir := t.TempDir()
	seedStatusV2Store(t, dataDir, statusV2Seed{
		account:     statusGoogleAccount,
		lastMessage: time.Date(2026, 10, 5, 22, 40, 0, 0, loc),
		frames:      stall1006Frames(loc),
	})
	session := openStatusSession(t, dataDir, true)

	// At 13:30 the store alone says "quiet but expected"; the app says stalled.
	stalledBlock := `{"source":"v2_inbox","last_event_ms":1791263460000,"silent_ms":47100000,"stalled":true,` +
		`"rule":"max_silence","carried_over":true,"evaluated":false}`
	daemon := fakeDaemon(t, daemonStatusWithGoogleSilence(dataDir, stalledBlock))
	now := func() time.Time { return time.Date(2026, 10, 6, 13, 30, 0, 0, loc) }

	text := runStatusOutput(t, session, statusDeps{daemon: daemon, now: now, loc: loc}, false)
	if line := statusRowLine(t, text, "sms"); !strings.HasSuffix(line, "⚠ silent 13h") {
		t.Fatalf("sms row = %q, want the app's verdict %q", line, "⚠ silent 13h")
	}
	if !strings.Contains(text, "Silence judged by the running app from the v2 inbox.") {
		t.Fatalf("status text = %q, want the daemon-verdict note", text)
	}

	status := decodeStatusJSON(t, runStatusOutput(t, session, statusDeps{daemon: daemon, now: now, loc: loc}, true))
	row := status.Platforms[0]
	var got bytes.Buffer
	if err := json.Compact(&got, row.Silence); err != nil {
		t.Fatal(err)
	}
	if got.String() != stalledBlock || row.SilenceJudgedBy != "daemon" {
		t.Fatalf("silence = %s judged by %q, want the daemon's block %s verbatim", got.String(), row.SilenceJudgedBy, stalledBlock)
	}

	// And the other way: 37 hours in, the store alone says stalled, but a
	// fresh verdict from the app wins.
	freshBlock := `{"source":"v2_inbox","last_event_ms":1791393000000,"silent_ms":60000,"stalled":false,"rule":""}`
	fresh := runStatusOutput(t, session, statusDeps{
		daemon: fakeDaemon(t, daemonStatusWithGoogleSilence(dataDir, freshBlock)),
		now:    func() time.Time { return time.Date(2026, 10, 7, 15, 0, 0, 0, loc) },
		loc:    loc,
	}, false)
	if line := statusRowLine(t, fresh, "sms"); strings.Contains(line, "⚠") {
		t.Fatalf("sms row = %q, want the app's fresh verdict (no warning)", line)
	}
}

// The app's verdict counts only when the app serves this data dir and
// reports one. Otherwise the CLI judges its own store and says why.
func TestRunStatusJudgesLocallyWhenTheAppCannotSpeakForThisStore(t *testing.T) {
	loc := newYork(t)
	dataDir := t.TempDir()
	seedStatusV2Store(t, dataDir, statusV2Seed{
		account:     statusGoogleAccount,
		lastMessage: time.Date(2026, 10, 5, 22, 40, 0, 0, loc),
		frames:      stall1006Frames(loc),
	})
	session := openStatusSession(t, dataDir, true)
	now := func() time.Time { return time.Date(2026, 10, 6, 14, 12, 0, 0, loc) }
	// The app's verdict says fresh; every case below must ignore it and
	// report the local stall instead.
	freshBlock := `{"source":"v2_inbox","last_event_ms":1,"silent_ms":1,"stalled":false,"rule":""}`
	otherDir := t.TempDir()

	cases := []struct {
		name   string
		daemon *localapi.Client
		why    string
	}{
		{"app down", downDaemon(t), "the app isn't running"},
		{"app serves another data dir", fakeDaemon(t, daemonStatusWithGoogleSilence(otherDir, freshBlock)), "the running app serves " + otherDir},
		{"app predates silence checks", fakeDaemon(t, `{"v2_primary":true,"auth":{"data_dir":`+jsonString(dataDir)+`},"freshness":{"newest_ms":1,"google":{"behind_days":0,"stale":false}}}`), "the running app doesn't report silence"},
		{"app predates auth.data_dir", fakeDaemon(t, `{"freshness":{"google":{"silence":`+freshBlock+`}}}`), "the running app doesn't report its data dir"},
		{"app measures another source", fakeDaemon(t, daemonStatusWithGoogleSilence(dataDir, `{"source":"messages","silent_ms":1,"stalled":false}`)), "the running app measures stored incoming messages, not the v2 inbox"},
		{"app answers garbage", fakeDaemon(t, `not json`), "the running app's /api/status was unreadable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := runStatusOutput(t, session, statusDeps{daemon: c.daemon, now: now, loc: loc}, false)
			if line := statusRowLine(t, text, "sms"); !strings.HasSuffix(line, "⚠ silent 13h") {
				t.Fatalf("sms row = %q, want the local stall", line)
			}
			wantNote := "Silence judged locally from the v2 inbox (" + c.why + ")."
			if !strings.Contains(text, wantNote) {
				t.Fatalf("status text = %q, want note %q", text, wantNote)
			}
		})
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A platform the app reports no verdict for is judged locally, and the note
// says which verdict came from where.
func TestRunStatusFillsInPlatformsTheAppDidNotJudge(t *testing.T) {
	loc := newYork(t)
	dataDir := t.TempDir()
	whatsappLast := time.Date(2026, 10, 6, 12, 0, 0, 0, loc)
	seedStatusV2Store(t, dataDir,
		statusV2Seed{account: statusGoogleAccount, lastMessage: time.Date(2026, 10, 5, 22, 40, 0, 0, loc), frames: stall1006Frames(loc)},
		statusV2Seed{account: statusWhatsAppAccount, lastMessage: whatsappLast, frames: []time.Time{whatsappLast}},
	)
	session := openStatusSession(t, dataDir, true)
	googleBlock := `{"source":"v2_inbox","last_event_ms":1791263460000,"silent_ms":47520000,"stalled":true,"rule":"expected_activity"}`
	deps := statusDeps{
		daemon: fakeDaemon(t, daemonStatusWithGoogleSilence(dataDir, googleBlock)),
		now:    func() time.Time { return time.Date(2026, 10, 6, 14, 12, 0, 0, loc) },
		loc:    loc,
	}

	status := decodeStatusJSON(t, runStatusOutput(t, session, deps, true))
	judgedBy := map[string]string{}
	for _, row := range status.Platforms {
		judgedBy[row.Platform] = row.SilenceJudgedBy
	}
	if judgedBy["sms"] != "daemon" || judgedBy["whatsapp"] != "local" {
		t.Fatalf("judged by = %v, want sms by the daemon and whatsapp locally", judgedBy)
	}
	want := "Silence: Google Messages judged by the running app from the v2 inbox; WhatsApp judged locally from the v2 inbox (the running app reported none)."
	if status.SilenceNote != want {
		t.Fatalf("silence_note = %q\nwant          %q", status.SilenceNote, want)
	}
	// One WhatsApp frame two hours ago, with no baseline: not a stall.
	text := runStatusOutput(t, session, deps, false)
	if line := statusRowLine(t, text, "whatsapp"); strings.Contains(line, "⚠") {
		t.Fatalf("whatsapp row = %q, want no warning", line)
	}
}

// When reads come from the legacy store, silence is measured on its incoming
// messages, as the daemon's freshnessActivitySource does: an outgoing row
// (even a failed send) after the last incoming message does not end the
// silence.
func TestRunStatusJudgesLegacyStoresOnIncomingMessages(t *testing.T) {
	loc := newYork(t)
	dataDir := t.TempDir()
	legacy, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	for i, at := range stall1006Frames(loc) {
		if err := legacy.UpsertMessage(&db.Message{
			MessageID: fmt.Sprintf("in-%d", i), ConversationID: "sms:c", Body: "x",
			TimestampMS: at.UnixMilli(), SourcePlatform: "sms", SourceID: fmt.Sprintf("in-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	sentAt := time.Date(2026, 10, 6, 9, 0, 0, 0, loc)
	if err := legacy.UpsertMessage(&db.Message{
		MessageID: "out", ConversationID: "sms:c", Body: "x", TimestampMS: sentAt.UnixMilli(),
		SourcePlatform: "sms", SourceID: "out", IsFromMe: true, Status: "OUTGOING_FAILED:UNKNOWN",
	}); err != nil {
		t.Fatal(err)
	}
	legacy.Close()
	session := openStatusSession(t, dataDir, false)
	if session.V2Store != nil {
		t.Fatal("legacy session opened a v2 store")
	}

	deps := statusDeps{
		daemon: downDaemon(t),
		now:    func() time.Time { return time.Date(2026, 10, 6, 14, 12, 0, 0, loc) },
		loc:    loc,
	}
	text := runStatusOutput(t, session, deps, false)
	if line := statusRowLine(t, text, "sms"); !strings.HasSuffix(line, "⚠ silent 13h") {
		t.Fatalf("sms row = %q, want silent since the 01:11 incoming message", line)
	}
	if !strings.Contains(text, "Silence judged locally from stored incoming messages (the app isn't running).") {
		t.Fatalf("status text = %q, want the legacy-source note", text)
	}
	silence := silenceFields(t, decodeStatusJSON(t, runStatusOutput(t, session, deps, true)).Platforms[0].Silence)
	if silence["source"] != freshness.SourceMessages ||
		silence["last_event_ms"] != float64(time.Date(2026, 10, 6, 1, 11, 0, 0, loc).UnixMilli()) {
		t.Fatalf("silence = %v, want source %s with the 01:11 incoming message last", silence, freshness.SourceMessages)
	}

	// A v2-primary app on the same data dir measures its v2 inbox. This
	// command reads the legacy store (OPENMESSAGES_V2_PRIMARY unset), so the
	// app's verdict does not describe it, however fresh.
	deps.daemon = fakeDaemon(t, daemonStatusWithGoogleSilence(dataDir,
		`{"source":"v2_inbox","last_event_ms":1,"silent_ms":60000,"stalled":false,"rule":""}`))
	mismatched := runStatusOutput(t, session, deps, false)
	if line := statusRowLine(t, mismatched, "sms"); !strings.HasSuffix(line, "⚠ silent 13h") {
		t.Fatalf("sms row = %q, want the legacy store's own stall", line)
	}
	if !strings.Contains(mismatched, "Silence judged locally from stored incoming messages (the running app measures the v2 inbox, not stored incoming messages).") {
		t.Fatalf("status text = %q, want the source-mismatch note", mismatched)
	}
}

// Demo data is a frozen fixture: like the daemon, status judges no silence
// on it and never asks the running app.
func TestRunStatusSkipsSilenceInDemoMode(t *testing.T) {
	loc := newYork(t)
	dataDir := t.TempDir()
	seedStatusV2Store(t, dataDir, statusV2Seed{
		account:     statusGoogleAccount,
		lastMessage: time.Date(2026, 10, 5, 22, 40, 0, 0, loc),
		frames:      stall1006Frames(loc),
	})
	session := openStatusSession(t, dataDir, true)
	var probed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { probed.Store(true) }))
	t.Cleanup(server.Close)
	deps := statusDeps{
		daemon: localapi.NewClient(server.URL, ""),
		now:    func() time.Time { return time.Date(2026, 10, 7, 15, 0, 0, 0, loc) },
		loc:    loc,
		demo:   true,
	}
	raw := runStatusOutput(t, session, deps, true)
	status := decodeStatusJSON(t, raw)
	for _, row := range status.Platforms {
		if row.Silence != nil || row.SilenceJudgedBy != "" {
			t.Fatalf("demo row %s carries a silence verdict: %s", row.Platform, raw)
		}
	}
	if status.SilenceNote != "" || probed.Load() {
		t.Fatalf("demo status = %s (probed %v), want no silence note and no probe", raw, probed.Load())
	}
}

// Differential: on the same store, the verdict `openmessage status` judges
// locally matches the one the daemon puts in /api/status, for both activity
// sources. Both read time.Now, so the two fields that grow with the clock may
// differ by the moments between the calls; everything else must match.
// time.Local is New York, and the traffic is New York daytime on exactly the
// 14 days before a last frame at 01:11 New York. A side that judged in UTC
// would end the baseline window at 20:00 or 19:00 New York the evening
// before, drop those hours' frames and gain none at the start, so the zone
// shows in baseline_events.
func TestStatusLocalSilenceMatchesTheDaemon(t *testing.T) {
	loc := newYork(t)
	pinLocal(t, loc)
	now := time.Now().In(loc)
	last := time.Date(now.Year(), now.Month(), now.Day(), 1, 11, 0, 0, loc)
	if last.After(now) {
		last = last.AddDate(0, 0, -1)
	}
	var frames []time.Time
	for day := -14; day <= -1; day++ {
		date := last.AddDate(0, 0, day)
		for minute := 8 * 60; minute < 23*60; minute += 30 {
			frames = append(frames, time.Date(date.Year(), date.Month(), date.Day(), 0, minute, 0, 0, loc))
		}
	}
	frames = append(frames, last)

	t.Run("v2 inbox", func(t *testing.T) {
		dataDir := t.TempDir()
		seedStatusV2Store(t, dataDir, statusV2Seed{account: statusGoogleAccount, lastMessage: last, frames: frames})
		session := openStatusSession(t, dataDir, true)
		daemonBlock := daemonSilence(t, web.APIOptions{
			Reads:    v2read.New(session.V2Store),
			Activity: freshnessActivitySource(&v2Stack{Store: session.V2Store}, nil, true),
		})
		compareSilence(t, daemonBlock, localSilence(t, session))
	})

	t.Run("legacy incoming messages", func(t *testing.T) {
		dataDir := t.TempDir()
		legacy, err := db.New(filepath.Join(dataDir, "messages.db"))
		if err != nil {
			t.Fatal(err)
		}
		for i, at := range frames {
			id := fmt.Sprintf("m-%d", i)
			if err := legacy.UpsertMessage(&db.Message{
				MessageID: id, ConversationID: "sms:c", Body: "x", TimestampMS: at.UnixMilli(),
				SourcePlatform: "sms", SourceID: id,
			}); err != nil {
				t.Fatal(err)
			}
		}
		legacy.Close()
		session := openStatusSession(t, dataDir, false)
		store := session.Reads.(*db.Store)
		daemonBlock := daemonSilence(t, web.APIOptions{
			Reads:    store,
			Activity: freshnessActivitySource(nil, store, false),
		})
		compareSilence(t, daemonBlock, localSilence(t, session))
	})
}

// daemonSilence serves /api/status with opts and returns freshness.google.silence.
func daemonSilence(t *testing.T, opts web.APIOptions) map[string]any {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := httptest.NewServer(web.APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, opts))
	t.Cleanup(server.Close)
	response, err := http.Get(server.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload struct {
		Freshness struct {
			Google struct {
				Silence map[string]any `json:"silence"`
			} `json:"google"`
		} `json:"freshness"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	block := payload.Freshness.Google.Silence
	if block == nil {
		t.Fatal("daemon reported no google silence")
	}
	return block
}

func localSilence(t *testing.T, session *commandReadSession) map[string]any {
	t.Helper()
	status := decodeStatusJSON(t, runStatusOutput(t, session, statusDeps{}, true))
	for _, row := range status.Platforms {
		if row.Platform == "sms" {
			return silenceFields(t, row.Silence)
		}
	}
	t.Fatalf("no sms row in %+v", status)
	return nil
}

func compareSilence(t *testing.T, daemon, local map[string]any) {
	t.Helper()
	if d, l := daemon["silent_ms"].(float64), local["silent_ms"].(float64); math.Abs(d-l) > 5000 {
		t.Fatalf("silent_ms: daemon %v, local %v", d, l)
	}
	if d, l := daemon["expected_active_hours"].(float64), local["expected_active_hours"].(float64); math.Abs(d-l) > 0.02 {
		t.Fatalf("expected_active_hours: daemon %v, local %v", d, l)
	}
	for _, clock := range []string{"silent_ms", "expected_active_hours"} {
		delete(daemon, clock)
		delete(local, clock)
	}
	if !reflect.DeepEqual(daemon, local) {
		t.Fatalf("silence verdicts differ\ndaemon %v\nlocal  %v", daemon, local)
	}
	if daemon["evaluated"] != true {
		t.Fatalf("fixture verdict = %v, want a baseline rich enough to evaluate", daemon)
	}
}

// stubStatusActivity is an ActivitySource over fixed event times whose
// baseline query can be made to fail. A failing query still returns its
// rows, as a partial read might, so callers must discard them on the error.
type stubStatusActivity struct {
	events     map[string][]time.Time
	latestErr  error
	betweenErr error
}

func (stubStatusActivity) Name() string { return freshness.SourceV2Inbox }

func (s stubStatusActivity) Latest(context.Context) (map[string]time.Time, error) {
	if s.latestErr != nil {
		return nil, s.latestErr
	}
	latest := map[string]time.Time{}
	for platform, events := range s.events {
		for _, at := range events {
			if at.After(latest[platform]) {
				latest[platform] = at
			}
		}
	}
	return latest, nil
}

func (s stubStatusActivity) Between(_ context.Context, platform string, from, to time.Time) ([]time.Time, error) {
	var out []time.Time
	for _, at := range s.events[platform] {
		if !at.Before(from) && !at.After(to) {
			out = append(out, at)
		}
	}
	return out, s.betweenErr
}

// Property: for any activity, clock and zone, the local verdict is
// freshness.EvaluateSilence over the platform's latest event and its events,
// rendered by freshness.SilenceBlock; a baseline that can't be read yields
// the verdict judged without one, marked baseline_unavailable. A platform
// with no activity gets no verdict.
func TestJudgeSilenceLocallyIsEvaluateSilence(t *testing.T) {
	zones := []string{"UTC", "America/New_York", "Asia/Kolkata", "Australia/Lord_Howe"}
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	property := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		loc, err := time.LoadLocation(zones[r.Intn(len(zones))])
		if err != nil {
			t.Fatal(err)
		}
		var events []time.Time
		n := r.Intn(800)
		if r.Intn(10) == 0 {
			n = 0
		}
		last := start
		for i := 0; i < n; i++ {
			at := start.Add(time.Duration(r.Int63n(int64(20 * 24 * time.Hour))))
			events = append(events, at)
			if at.After(last) {
				last = at
			}
		}
		// Mostly after the last event, so most cases have a silence to judge.
		now := last.Add(time.Duration(r.Int63n(int64(4*24*time.Hour))) - time.Hour)
		var betweenErr error
		if r.Intn(4) == 0 {
			betweenErr = errors.New("baseline query failed")
		}
		source := stubStatusActivity{events: map[string][]time.Time{"google": events}, betweenErr: betweenErr}

		blocks, err := judgeSilenceLocally(context.Background(), source, now, loc, []string{"google", "signal"})
		if err != nil || blocks["signal"] != nil {
			return false
		}
		if len(events) == 0 {
			return len(blocks) == 0
		}
		latest, _ := source.Latest(context.Background())
		baseline := events
		if betweenErr != nil {
			baseline = nil
		}
		verdict := freshness.EvaluateSilence(latest["google"], baseline, now, loc, freshness.DefaultSilenceConfig)
		want, _ := json.Marshal(freshness.SilenceBlock(source.Name(), verdict, freshness.DefaultSilenceConfig, betweenErr != nil))
		return bytes.Equal(blocks["google"], want)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 300, Rand: rand.New(rand.NewSource(1006))}); err != nil {
		t.Fatal(err)
	}
}

// Property: whatever verdict the running app reports for this data dir,
// status --json carries it unchanged and the text row warns exactly when it
// is a stall, with its whole silent hours.
func TestRunStatusPassesAnyDaemonVerdictThrough(t *testing.T) {
	dataDir := t.TempDir()
	last := time.Date(2026, 10, 5, 22, 40, 0, 0, time.UTC)
	seedStatusV2Store(t, dataDir, statusV2Seed{account: statusGoogleAccount, lastMessage: last, frames: []time.Time{last}})
	session := openStatusSession(t, dataDir, true)

	// The handler runs on the server's goroutine, so the block is atomic.
	var served atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, daemonStatusWithGoogleSilence(dataDir, served.Load().(string)))
	}))
	t.Cleanup(server.Close)
	deps := statusDeps{daemon: localapi.NewClient(server.URL, ""), now: func() time.Time { return last }}

	property := func(silentMS uint32, stalled bool, rule uint8, carried bool) bool {
		fields := map[string]any{
			"source": freshness.SourceV2Inbox, "last_event_ms": last.UnixMilli(),
			"silent_ms": int64(silentMS) * 100, "stalled": stalled,
			"rule": []string{"", freshness.RuleExpectedActivity, freshness.RuleMaxSilence, freshness.RuleLongSilence}[rule%4],
		}
		if carried {
			fields["carried_over"] = true
		}
		encoded, _ := json.Marshal(fields)
		block := string(encoded)
		served.Store(block)

		var got bytes.Buffer
		status := decodeStatusJSON(t, runStatusOutput(t, session, deps, true))
		if json.Compact(&got, status.Platforms[0].Silence) != nil || got.String() != block {
			return false
		}
		line := statusRowLine(t, runStatusOutput(t, session, deps, false), "sms")
		switch ms := int64(silentMS) * 100; {
		case !stalled:
			return !strings.Contains(line, "⚠")
		case ms == 0:
			return strings.HasSuffix(line, "⚠ silent")
		default:
			return strings.HasSuffix(line, fmt.Sprintf("⚠ silent %dh", ms/time.Hour.Milliseconds()))
		}
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 60, Rand: rand.New(rand.NewSource(190))}); err != nil {
		t.Fatal(err)
	}
}

// When the local activity query fails, the note says so and no row carries
// a verdict; with the app's verdict for some platforms, the note says the
// rest went unjudged.
func TestRunStatusNotesAFailedLocalQuery(t *testing.T) {
	loc := newYork(t)
	dataDir := t.TempDir()
	seedStatusV2Store(t, dataDir,
		statusV2Seed{account: statusGoogleAccount, lastMessage: time.Date(2026, 10, 5, 22, 40, 0, 0, loc), frames: stall1006Frames(loc)},
		statusV2Seed{account: statusWhatsAppAccount, lastMessage: time.Date(2026, 10, 6, 12, 0, 0, 0, loc)},
	)
	session := openStatusSession(t, dataDir, true)
	failing := stubStatusActivity{latestErr: errors.New("database is locked")}
	now := func() time.Time { return time.Date(2026, 10, 6, 14, 12, 0, 0, loc) }

	t.Run("app down", func(t *testing.T) {
		deps := statusDeps{daemon: downDaemon(t), source: failing, now: now, loc: loc}
		status := decodeStatusJSON(t, runStatusOutput(t, session, deps, true))
		for _, row := range status.Platforms {
			if row.Silence != nil {
				t.Fatalf("%s row carries a verdict %s after a failed query", row.Platform, row.Silence)
			}
		}
		want := "Silence not judged (the app isn't running; local query failed: database is locked)."
		if status.SilenceNote != want {
			t.Fatalf("silence_note = %q\nwant          %q", status.SilenceNote, want)
		}
		if text := runStatusOutput(t, session, deps, false); !strings.Contains(text, want) {
			t.Fatalf("status text = %q, want note %q", text, want)
		}
	})

	t.Run("app judged some", func(t *testing.T) {
		googleBlock := `{"source":"v2_inbox","last_event_ms":1791263460000,"silent_ms":47520000,"stalled":true,"rule":"expected_activity"}`
		deps := statusDeps{daemon: fakeDaemon(t, daemonStatusWithGoogleSilence(dataDir, googleBlock)), source: failing, now: now, loc: loc}
		status := decodeStatusJSON(t, runStatusOutput(t, session, deps, true))
		if statusPlatform(t, status, "sms").SilenceJudgedBy != "daemon" || statusPlatform(t, status, "whatsapp").Silence != nil {
			t.Fatalf("platforms = %+v, want sms judged by the daemon and whatsapp unjudged", status.Platforms)
		}
		want := "Silence: Google Messages judged by the running app from the v2 inbox; the rest not judged (the running app reported none; local query failed: database is locked)."
		if status.SilenceNote != want {
			t.Fatalf("silence_note = %q\nwant          %q", status.SilenceNote, want)
		}
	})
}

// A verdict judged without its baseline can only be flagged by the 72-hour
// floor, so 37 silent hours on a busy platform show no warning. The note
// says why, so the quiet row is not read as healthy.
func TestRunStatusNotesAVerdictJudgedWithoutItsBaseline(t *testing.T) {
	loc := newYork(t)
	dataDir := t.TempDir()
	frames := stall1006Frames(loc)
	seedStatusV2Store(t, dataDir, statusV2Seed{account: statusGoogleAccount, lastMessage: time.Date(2026, 10, 5, 22, 40, 0, 0, loc)})
	session := openStatusSession(t, dataDir, true)
	deps := statusDeps{
		daemon: downDaemon(t),
		source: stubStatusActivity{events: map[string][]time.Time{"google": frames}, betweenErr: context.DeadlineExceeded},
		now:    func() time.Time { return time.Date(2026, 10, 7, 15, 0, 0, 0, loc) },
		loc:    loc,
	}
	text := runStatusOutput(t, session, deps, false)
	if line := statusRowLine(t, text, "sms"); strings.Contains(line, "⚠") {
		t.Fatalf("sms row = %q, want no warning before the 72h floor", line)
	}
	want := "Silence judged locally from the v2 inbox (the app isn't running). Google Messages judged without a baseline, so only the 72h floor applies."
	if !strings.Contains(text, want) {
		t.Fatalf("status text = %q, want note %q", text, want)
	}
	silence := silenceFields(t, statusPlatform(t, decodeStatusJSON(t, runStatusOutput(t, session, deps, true)), "sms").Silence)
	if silence["baseline_unavailable"] != true || silence["evaluated"] != false {
		t.Fatalf("silence = %v, want baseline_unavailable and not evaluated", silence)
	}
}

// A platform with stored messages but no activity in the source (here a v2
// store whose Google account has no inbox frames) has nothing to judge; the
// note says so instead of leaving the row looking fresh.
func TestRunStatusNotesAPlatformWithNoActivity(t *testing.T) {
	loc := newYork(t)
	dataDir := t.TempDir()
	seedStatusV2Store(t, dataDir, statusV2Seed{account: statusGoogleAccount, lastMessage: time.Date(2026, 9, 29, 12, 0, 0, 0, loc)})
	session := openStatusSession(t, dataDir, true)
	deps := statusDeps{daemon: downDaemon(t), now: func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, loc) }, loc: loc}
	status := decodeStatusJSON(t, runStatusOutput(t, session, deps, true))
	if row := statusPlatform(t, status, "sms"); row.Silence != nil {
		t.Fatalf("sms row carries a verdict %s with no activity", row.Silence)
	}
	want := "Google Messages not judged: no activity recorded in the v2 inbox (the app isn't running)."
	if status.SilenceNote != want {
		t.Fatalf("silence_note = %q\nwant          %q", status.SilenceNote, want)
	}
}

// A platform both days behind and silent warns "behind", and --json carries
// behind_days and the literal silence_judged_by values documented in
// CLAUDE.md.
func TestRunStatusBehindOutranksSilentPerRow(t *testing.T) {
	loc := newYork(t)
	dataDir := t.TempDir()
	whatsappLast := time.Date(2026, 10, 11, 6, 0, 0, 0, loc)
	seedStatusV2Store(t, dataDir,
		statusV2Seed{account: statusGoogleAccount, lastMessage: time.Date(2026, 10, 5, 22, 40, 0, 0, loc), frames: stall1006Frames(loc)},
		statusV2Seed{account: statusWhatsAppAccount, lastMessage: whatsappLast, frames: []time.Time{whatsappLast}},
	)
	session := openStatusSession(t, dataDir, true)
	deps := statusDeps{daemon: downDaemon(t), now: func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, loc) }, loc: loc}

	text := runStatusOutput(t, session, deps, false)
	if line := statusRowLine(t, text, "sms"); !strings.HasSuffix(line, "⚠ 5d behind") {
		t.Fatalf("sms row = %q, want %q", line, "⚠ 5d behind")
	}
	status := decodeStatusJSON(t, runStatusOutput(t, session, deps, true))
	sms := statusPlatform(t, status, "sms")
	if sms.BehindDays == nil || *sms.BehindDays != 5 || sms.SilenceJudgedBy != "local" {
		t.Fatalf("sms row behind %v judged by %q, want 5 and local", sms.BehindDays, sms.SilenceJudgedBy)
	}
	if silenceFields(t, sms.Silence)["stalled"] != true {
		t.Fatalf("sms silence = %s, want the stall kept in --json", sms.Silence)
	}
	if whatsapp := statusPlatform(t, status, "whatsapp"); whatsapp.BehindDays == nil || *whatsapp.BehindDays != 0 {
		t.Fatalf("whatsapp behind_days = %v, want 0", whatsapp.BehindDays)
	}
}

// Daemon truth on a legacy-reading CLI: an app in legacy mode measures the
// same stored incoming messages, so its verdict is shown on both Google rows
// (sms and rcs) and on no import row.
func TestRunStatusShowsTheAppsVerdictOnLegacyRows(t *testing.T) {
	loc := newYork(t)
	dataDir := t.TempDir()
	legacy, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	rows := []*db.Message{
		{MessageID: "sms-1", ConversationID: "sms:c", Body: "x", TimestampMS: time.Date(2026, 10, 6, 1, 11, 0, 0, loc).UnixMilli(), SourcePlatform: "sms", SourceID: "sms-1"},
		{MessageID: "rcs-1", ConversationID: "sms:c", Body: "x", TimestampMS: time.Date(2026, 10, 5, 22, 40, 0, 0, loc).UnixMilli(), SourcePlatform: "rcs", SourceID: "rcs-1"},
		{MessageID: "im-1", ConversationID: "im:c", Body: "x", TimestampMS: time.Date(2026, 10, 5, 20, 0, 0, 0, loc).UnixMilli(), SourcePlatform: "imessage", SourceID: "im-1"},
	}
	for _, row := range rows {
		if err := legacy.UpsertMessage(row); err != nil {
			t.Fatal(err)
		}
	}
	legacy.Close()
	session := openStatusSession(t, dataDir, false)
	block := `{"source":"messages","last_event_ms":1791263460000,"silent_ms":47520000,"stalled":true,"rule":"max_silence"}`
	deps := statusDeps{
		daemon: fakeDaemon(t, daemonStatusWithGoogleSilence(dataDir, block)),
		now:    func() time.Time { return time.Date(2026, 10, 6, 14, 12, 0, 0, loc) },
		loc:    loc,
	}
	status := decodeStatusJSON(t, runStatusOutput(t, session, deps, true))
	for _, platform := range []string{"sms", "rcs"} {
		row := statusPlatform(t, status, platform)
		var got bytes.Buffer
		if err := json.Compact(&got, row.Silence); err != nil || got.String() != block || row.SilenceJudgedBy != "daemon" {
			t.Fatalf("%s row silence = %s judged by %q, want the app's block", platform, row.Silence, row.SilenceJudgedBy)
		}
	}
	if row := statusPlatform(t, status, "imessage"); row.Silence != nil || row.SilenceJudgedBy != "" {
		t.Fatalf("imessage row = %+v, want no verdict (imports have no transport)", row)
	}
	if status.SilenceNote != "Silence judged by the running app from stored incoming messages." {
		t.Fatalf("silence_note = %q", status.SilenceNote)
	}
}

// The production entry point wires the probe to OPENMESSAGES_PORT and the
// data dir's control token, and writes --json to stdout.
func TestRunStatusProbesTheAppOnItsPort(t *testing.T) {
	dataDir := t.TempDir()
	last := time.Now().Add(-time.Hour)
	seedStatusV2Store(t, dataDir, statusV2Seed{account: statusGoogleAccount, lastMessage: last, frames: []time.Time{last}})
	setStatusEnv(t, dataDir, true)
	if err := os.WriteFile(filepath.Join(dataDir, localapi.ControlTokenFile), []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	block := `{"source":"v2_inbox","last_event_ms":1,"silent_ms":3600000,"stalled":false,"rule":""}`
	var authorization atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization.Store(r.Header.Get("Authorization"))
		io.WriteString(w, daemonStatusWithGoogleSilence(dataDir, block))
	}))
	t.Cleanup(server.Close)
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]
	t.Setenv("OPENMESSAGES_PORT", port)

	stdout := os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = write
	runErr := RunStatus(zerolog.Nop(), "--json")
	os.Stdout = stdout
	write.Close()
	raw, _ := io.ReadAll(read)
	if runErr != nil {
		t.Fatalf("RunStatus(): %v", runErr)
	}
	row := statusPlatform(t, decodeStatusJSON(t, string(raw)), "sms")
	if row.SilenceJudgedBy != "daemon" {
		t.Fatalf("sms row judged by %q, want daemon (output %s)", row.SilenceJudgedBy, raw)
	}
	if got, _ := authorization.Load().(string); got != "Bearer test-token" {
		t.Fatalf("probe Authorization = %q, want the data dir's control token", got)
	}
}

// The note tells a refused connection (nothing listening) from a timeout (an
// app too slow to answer), using the errors a real probe returns.
func TestUnreachableReasonTellsRefusedFromSlow(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); slow.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, reachable, err := localapi.NewClient(slow.URL, "").Status(ctx)
	if err == nil || reachable {
		t.Fatalf("slow probe = reachable %v, err %v; want an unreachable timeout", reachable, err)
	}
	if got, want := unreachableReason(err), "the running app didn't answer within 3s"; got != want {
		t.Fatalf("slow app: reason %q, want %q (err %v)", got, want, err)
	}

	_, _, err = downDaemon(t).Status(context.Background())
	if got := unreachableReason(err); got != "the app isn't running" {
		t.Fatalf("refused: reason %q (err %v)", got, err)
	}

	_, _, err = localapi.NewClient("http://127.0.0.1:notaport", "").Status(context.Background())
	if got := unreachableReason(err); !strings.HasPrefix(got, "the app couldn't be reached: ") {
		t.Fatalf("bad address: reason %q (err %v)", got, err)
	}
}

// A daemon block is foreign input to the text output: silent_ms written as a
// float, missing, negative or of the wrong type still yields a sane row.
func TestNewPlatformSilenceReadsForeignBlocks(t *testing.T) {
	cases := []struct {
		block    string
		silentMS int64
		warning  string
	}{
		{`{"silent_ms":46800000,"stalled":true}`, 46_800_000, "  ⚠ silent 13h"},
		{`{"silent_ms":4.68e7,"stalled":true}`, 46_800_000, "  ⚠ silent 13h"},
		{`{"silent_ms":-7200000,"stalled":true}`, 0, "  ⚠ silent"},
		{`{"stalled":true}`, 0, "  ⚠ silent"},
		{`{"silent_ms":"13h","stalled":true}`, 0, "  ⚠ silent"},
		{`{"silent_ms":46800000,"stalled":false}`, 46_800_000, ""},
	}
	for _, c := range cases {
		verdict := newPlatformSilence(json.RawMessage(c.block), silenceJudgedByDaemon)
		if verdict.SilentMS != c.silentMS {
			t.Errorf("%s: SilentMS = %d, want %d", c.block, verdict.SilentMS, c.silentMS)
		}
		if got := staleWarning(0, &verdict); got != c.warning {
			t.Errorf("%s: warning %q, want %q", c.block, got, c.warning)
		}
	}
}
