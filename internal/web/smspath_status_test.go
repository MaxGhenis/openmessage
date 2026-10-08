package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/freshness"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// smsPathInbox is a v2 store whose Google inbox the test fills with real
// protobuf frames, received at controlled times.
type smsPathInbox struct {
	t          *testing.T
	store      *sqlite.Store
	repository *sqlite.MessageRepository
	clock      time.Time
	n          int
}

func newSMSPathInbox(t *testing.T) *smsPathInbox {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	nowMS := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: "google-primary", BridgeKey: "google_messages", DisplayName: "Google",
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatal(err)
	}
	inbox := &smsPathInbox{t: t, store: store}
	inbox.repository, err = sqlite.NewMessageRepository(store, func() time.Time { return inbox.clock })
	if err != nil {
		t.Fatal(err)
	}
	return inbox
}

// receive appends one incoming message frame of the given protobuf type
// (field 11) and its conversation frame of the given type (field 22).
func (i *smsPathInbox) receive(at time.Time, messageType int64, conversationType gmproto.ConversationType) {
	i.t.Helper()
	i.n++
	conversationID := fmt.Sprintf("c%d", i.n%5)
	conversationPayload, _, err := ingest.MarshalGoogleConversationFrame(&gmproto.Conversation{
		ConversationID: conversationID, Type: conversationType,
	})
	if err != nil {
		i.t.Fatal(err)
	}
	messagePayload, _, err := ingest.MarshalGoogleMessageFrame(&libgm.WrappedMessage{Message: &gmproto.Message{
		MessageID:      fmt.Sprintf("m%d", i.n),
		ConversationID: conversationID,
		Type:           messageType,
		Timestamp:      at.UnixMicro(),
		MessageStatus:  &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
		MessageInfo: []*gmproto.MessageInfo{{Data: &gmproto.MessageInfo_MessageContent{
			MessageContent: &gmproto.MessageContent{Content: "hello"},
		}}},
	}})
	if err != nil {
		i.t.Fatal(err)
	}
	i.clock = at
	for kind, payload := range map[string][]byte{"conv": conversationPayload, "msg": messagePayload} {
		if _, err := i.repository.AppendInbox(context.Background(), sqlite.InboxRecord{
			InboxID:      fmt.Sprintf("%s-%d", kind, i.n),
			AccountID:    "google-primary",
			Generation:   1,
			DedupeKey:    fmt.Sprintf("%s:%d", kind, i.n),
			Codec:        ingest.GoogleCodec,
			CodecVersion: int64(ingest.GoogleCodecVersion),
			Payload:      payload,
		}); err != nil {
			i.t.Fatal(err)
		}
	}
}

func (i *smsPathInbox) monitor() *freshness.SMSPathMonitor {
	return freshness.NewSMSPathMonitor(ingest.GoogleSMSPathLoader(i.store))
}

// history fills the inbox with 30 days of four SMS a day (07:00, 11:00, 15:00,
// 19:00) and hourly RCS, up to lastSMS, then RCS only, hourly, up to rcsUntil.
func (i *smsPathInbox) history(lastSMS, rcsUntil time.Time) {
	i.t.Helper()
	for at := lastSMS.Add(-30 * 24 * time.Hour); at.Before(lastSMS); at = at.Add(time.Hour) {
		if hour := at.Hour(); hour == 7 || hour == 11 || hour == 15 || hour == 19 {
			i.receive(at, 1, gmproto.ConversationType_SMS)
		} else {
			i.receive(at, 4, gmproto.ConversationType_RCS)
		}
	}
	i.receive(lastSMS, 1, gmproto.ConversationType_SMS)
	for at := lastSMS.Add(time.Hour); !at.After(rcsUntil); at = at.Add(time.Hour) {
		i.receive(at, 4, gmproto.ConversationType_RCS)
	}
}

func fetchStatusFreshness(t *testing.T, store *db.Store, opts APIOptions) map[string]any {
	t.Helper()
	srv := httptest.NewServer(APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, opts))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	fresh, ok := payload["freshness"].(map[string]any)
	if !ok {
		t.Fatalf("freshness = %v", payload["freshness"])
	}
	return fresh
}

func legacyWithFreshGoogle(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UnixMilli()
	if err := store.UpsertMessage(&db.Message{
		MessageID: "rcs:fresh", ConversationID: "c", Body: "hi",
		TimestampMS: now, SourcePlatform: "sms", SourceID: "fresh",
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// The 2026-10-03 shape: Google is the newest platform and not stale, while
// carrier SMS has been silent far past its usual pace and RCS keeps arriving.
func TestStatusReportsGoogleSMSPathStalledWhileRCSFlows(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	inbox := newSMSPathInbox(t)
	lastSMS := now.Add(-40 * time.Hour)
	inbox.history(lastSMS, now)

	fresh := fetchStatusFreshness(t, legacyWithFreshGoogle(t), APIOptions{GoogleSMSPath: inbox.monitor()})
	google := fresh["google"].(map[string]any)
	if stale, _ := google["stale"].(bool); stale {
		t.Fatalf("google stale = true; the SMS path must not mark Google stale (the app would ask for a re-pair)")
	}
	path, ok := google["sms_path"].(map[string]any)
	if !ok {
		t.Fatalf("google freshness = %v, want an sms_path block", google)
	}
	if stalled, _ := path["stalled"].(bool); !stalled || path["reason"] != freshness.SMSPathStalled {
		t.Fatalf("sms_path = %v, want stalled", path)
	}
	if got := int64(path["last_sms_ms"].(float64)); got != lastSMS.UnixMilli() {
		t.Fatalf("last_sms_ms = %d, want %d", got, lastSMS.UnixMilli())
	}
	if expected := path["expected_arrivals"].(float64); expected < 6 {
		t.Fatalf("expected_arrivals = %v, want at least 6", expected)
	}
	if path["source"] != "v2_inbox" || path["frames"].(float64) == 0 || path["history_loaded_at_ms"].(float64) == 0 {
		t.Fatalf("sms_path = %v, want the load described", path)
	}
	if stalled, _ := fresh["sms_path_stalled"].(bool); !stalled {
		t.Fatalf("freshness = %v, want top-level sms_path_stalled", fresh)
	}
}

func TestStatusReportsGoogleSMSPathFlowing(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	inbox := newSMSPathInbox(t)
	inbox.history(now.Add(-2*time.Hour), now)
	fresh := fetchStatusFreshness(t, legacyWithFreshGoogle(t), APIOptions{GoogleSMSPath: inbox.monitor()})
	path := fresh["google"].(map[string]any)["sms_path"].(map[string]any)
	if stalled, _ := path["stalled"].(bool); stalled || path["reason"] != freshness.SMSPathFlowing {
		t.Fatalf("sms_path = %v, want flowing", path)
	}
	if stalled, _ := fresh["sms_path_stalled"].(bool); stalled {
		t.Fatalf("sms_path_stalled = true")
	}
}

// While the daemon cannot reach the phone, missing SMS says nothing about the
// phone's SMS path.
func TestStatusWithholdsTheSMSPathStallWhileGoogleIsUnreachable(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	inbox := newSMSPathInbox(t)
	inbox.history(now.Add(-40*time.Hour), now)
	for name, opts := range map[string]APIOptions{
		"disconnected": {
			GoogleSMSPath: inbox.monitor(),
			GoogleStatus:  func() any { return app.GoogleStatusSnapshot{Connected: false, Paired: true, PhoneResponding: true} },
		},
		"phone not responding": {
			GoogleSMSPath:         inbox.monitor(),
			GooglePhoneResponding: func() bool { return false },
		},
	} {
		fresh := fetchStatusFreshness(t, legacyWithFreshGoogle(t), opts)
		path := fresh["google"].(map[string]any)["sms_path"].(map[string]any)
		if stalled, _ := path["stalled"].(bool); stalled || path["reason"] != freshness.SMSPathGoogleUnreachable {
			t.Fatalf("%s: sms_path = %v, want %q", name, path, freshness.SMSPathGoogleUnreachable)
		}
		if stalled, _ := fresh["sms_path_stalled"].(bool); stalled {
			t.Fatalf("%s: sms_path_stalled = true", name)
		}
	}
}

func TestStatusOmitsGoogleSMSPathWithoutAMonitorOrSMS(t *testing.T) {
	fresh := fetchStatusFreshness(t, legacyWithFreshGoogle(t), APIOptions{})
	if _, ok := fresh["google"].(map[string]any)["sms_path"]; ok {
		t.Fatalf("google freshness has sms_path without a monitor: %v", fresh["google"])
	}
	if _, ok := fresh["sms_path_stalled"]; ok {
		t.Fatalf("freshness has sms_path_stalled without a monitor: %v", fresh)
	}
	// A v2 install whose inbox has never held a Google SMS (a WhatsApp-only
	// user) gets no synthesized Google entry.
	empty := freshness.NewSMSPathMonitor(func(context.Context, time.Time) (freshness.SMSPathLoad, error) {
		return freshness.SMSPathLoad{}, nil
	})
	out := map[string]any{"newest_ms": int64(0)}
	addGoogleSMSPath(out, empty, time.Now(), nil)
	if len(out) != 1 {
		t.Fatalf("out = %v, want untouched", out)
	}
}

func TestAddGoogleSMSPathSynthesizesAMissingGoogleEntry(t *testing.T) {
	now := time.Now()
	lastSMS := now.Add(-40 * time.Hour)
	var events []freshness.TransportEvent
	for at := lastSMS.Add(-30 * 24 * time.Hour); !at.After(lastSMS); at = at.Add(6 * time.Hour) {
		events = append(events, freshness.TransportEvent{At: at, Transport: freshness.TransportSMS})
	}
	for at := lastSMS.Add(time.Hour); at.Before(now); at = at.Add(time.Hour) {
		events = append(events, freshness.TransportEvent{At: at, Transport: freshness.TransportRCS})
	}
	monitor := freshness.NewSMSPathMonitor(func(context.Context, time.Time) (freshness.SMSPathLoad, error) {
		return freshness.SMSPathLoad{Events: events}, nil
	})
	out := map[string]any{"newest_ms": int64(0)}
	addGoogleSMSPath(out, monitor, now, func() bool { return true })
	google, ok := out["google"].(map[string]any)
	if !ok || google["stale"] != false {
		t.Fatalf("google entry = %v", out["google"])
	}
	if path := google["sms_path"].(freshness.SMSPathReport); !path.Stalled || path.LastSMSMS != lastSMS.UnixMilli() {
		t.Fatalf("sms_path = %+v, want stalled since %d", path, lastSMS.UnixMilli())
	}
	if out["sms_path_stalled"] != true {
		t.Fatalf("out = %v", out)
	}
}

// failingReads serves platform stats until told to fail.
type failingReads struct {
	stubReads
	fail *bool
}

func (f failingReads) PlatformStats() ([]db.PlatformStat, error) {
	if *f.fail {
		return nil, fmt.Errorf("database is locked")
	}
	return f.stubReads.PlatformStats()
}

// When platform stats start failing, freshness keeps its last good value, but
// a stall in it must not outlive the phone becoming unreachable.
func TestStatusRejudgesTheSMSPathWhenPlatformStatsFail(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	inbox := newSMSPathInbox(t)
	inbox.history(now.Add(-40*time.Hour), now)
	fail, reachable := false, true
	nowMS := time.Now().UnixMilli()
	opts := APIOptions{
		GoogleSMSPath: inbox.monitor(),
		Reads: failingReads{
			stubReads: stubReads{stats: []db.PlatformStat{{Platform: "sms", Count: 1, LatestMS: nowMS, LatestRecvMS: nowMS}}},
			fail:      &fail,
		},
		GooglePhoneResponding: func() bool { return reachable },
	}
	srv := httptest.NewServer(APIHandlerWithOptions(legacyWithFreshGoogle(t), nil, zerolog.Nop(), nil, opts))
	defer srv.Close()
	fetch := func() map[string]any {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/status")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return payload["freshness"].(map[string]any)
	}
	if fresh := fetch(); fresh["sms_path_stalled"] != true {
		t.Fatalf("freshness = %v, want a stall cached", fresh)
	}

	fail, reachable = true, false
	// Step past the 30-second freshness cache by asking the helper directly,
	// as computeFreshness does on a stats error.
	cached := fetch()
	rejudged := withFreshGoogleSMSPath(cached, opts.GoogleSMSPath, time.Now(), func() bool { return reachable })
	path := rejudged["google"].(map[string]any)["sms_path"].(freshness.SMSPathReport)
	if path.Stalled || path.Reason != freshness.SMSPathGoogleUnreachable || rejudged["sms_path_stalled"] != false {
		t.Fatalf("rejudged = %+v, want the stall withheld as %q", path, freshness.SMSPathGoogleUnreachable)
	}
	if cached["sms_path_stalled"] != true {
		t.Fatal("the cached value was changed in place; responses still being written share it")
	}
	if rejudged["google"].(map[string]any)["latest_ms"] == nil || rejudged["newest_ms"] == nil {
		t.Fatalf("rejudged = %v, want the rest of the cached value kept", rejudged)
	}
}
