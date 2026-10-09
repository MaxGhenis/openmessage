package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/freshness"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func openSMSPathTestStore(t *testing.T) (*sqlite.Store, func(at time.Time, id string, messageType int64)) {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, smsPathReceiver(t, store)
}

// smsPathReceiver returns a func that appends an incoming Google message frame
// of the given protobuf type (field 11) to store's inbox, received at its time.
func smsPathReceiver(t *testing.T, store *sqlite.Store) func(at time.Time, id string, messageType int64) {
	t.Helper()
	nowMS := time.Now().UnixMilli()
	if err := store.UpsertAccount(sqlite.Account{
		AccountID: "google-primary", BridgeKey: "google_messages", Mode: sqlite.AccountModeLive,
		Enabled: true, ConfigJSON: "{}", CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	}); err != nil {
		t.Fatal(err)
	}
	var clock time.Time
	repository, err := sqlite.NewMessageRepository(store, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	receive := func(at time.Time, id string, messageType int64) {
		t.Helper()
		payload, _, err := ingest.MarshalGoogleMessageFrame(&libgm.WrappedMessage{Message: &gmproto.Message{
			MessageID: id, ConversationID: "c", Type: messageType, Timestamp: at.UnixMicro(),
			MessageStatus: &gmproto.MessageStatus{Status: gmproto.MessageStatusType_INCOMING_COMPLETE},
			MessageInfo: []*gmproto.MessageInfo{{Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: "hello"},
			}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		clock = at
		if _, err := repository.AppendInbox(context.Background(), sqlite.InboxRecord{
			InboxID: id, AccountID: "google-primary", Generation: 1, DedupeKey: "msg:" + id,
			Codec: ingest.GoogleCodec, CodecVersion: int64(ingest.GoogleCodecVersion), Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return receive
}

func TestStatusSMSPathFlagsSMSSilenceWhileRCSFlows(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	store, receive := openSMSPathTestStore(t)
	lastSMS := now.Add(-50 * time.Hour)
	for i := 0; i < 30*3; i++ { // three a day, eight hours apart, for 30 days
		receive(lastSMS.Add(-time.Duration(i)*8*time.Hour), fmt.Sprintf("sms-%d", i), 1)
	}
	for i := 0; i < 49; i++ {
		receive(lastSMS.Add(time.Duration(i+1)*time.Hour), fmt.Sprintf("rcs-%d", i), 4)
	}

	report := statusSMSPath(context.Background(), store, now)
	if report == nil || !report.Stalled || report.LastSMSMS != lastSMS.UnixMilli() {
		t.Fatalf("report = %+v, want stalled since %d", report, lastSMS.UnixMilli())
	}
	line := smsPathStatusLine(*report)
	for _, want := range []string{"⚠ Google SMS", "(2d)", "usual 3.0 arrivals a day", "about 6 would have come",
		"in the last 24h", "try restarting the phone"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q lacks %q", line, want)
		}
	}
}

// runStatus writes the SMS-path line to its own output and --json carries the
// same verdict as google_sms_path; demo mode, like silence, judges nothing.
func TestRunStatusReportsTheSMSPath(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	lastSMS := now.Add(-50 * time.Hour)
	dataDir := t.TempDir()
	seedStatusV2Store(t, dataDir, statusV2Seed{account: statusGoogleAccount, lastMessage: lastSMS})
	store, err := sqlite.Open(filepath.Join(dataDir, "v2", "store.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	receive := smsPathReceiver(t, store)
	for i := 0; i < 30*3; i++ { // three a day, eight hours apart, for 30 days
		receive(lastSMS.Add(-time.Duration(i)*8*time.Hour), fmt.Sprintf("sms-%d", i), 1)
	}
	for i := 0; i < 49; i++ {
		receive(lastSMS.Add(time.Duration(i+1)*time.Hour), fmt.Sprintf("rcs-%d", i), 4)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	session := openStatusSession(t, dataDir, true)
	deps := statusDeps{daemon: downDaemon(t), now: func() time.Time { return now }}

	text := runStatusOutput(t, session, deps, false)
	if !strings.Contains(text, "\n⚠ Google SMS: no incoming SMS since "+fmtTS(lastSMS.UnixMilli())) {
		t.Fatalf("status output lacks the stalled SMS-path line:\n%s", text)
	}
	var status struct {
		SMSPath *freshness.SMSPathReport `json:"google_sms_path"`
	}
	raw := runStatusOutput(t, session, deps, true)
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatalf("decode status --json: %v\n%s", err, raw)
	}
	if status.SMSPath == nil || !status.SMSPath.Stalled || status.SMSPath.LastSMSMS != lastSMS.UnixMilli() {
		t.Fatalf("google_sms_path = %+v, want stalled since %d", status.SMSPath, lastSMS.UnixMilli())
	}

	deps.demo = true
	if text := runStatusOutput(t, session, deps, false); strings.Contains(text, "Google SMS") {
		t.Fatalf("demo status judged the SMS path:\n%s", text)
	}
	if raw := runStatusOutput(t, session, deps, true); strings.Contains(raw, "google_sms_path") {
		t.Fatalf("demo status --json carries google_sms_path:\n%s", raw)
	}
}

func TestSMSPathStatusLineCoversEveryReason(t *testing.T) {
	cfg := freshness.DefaultSMSPathConfig
	base := freshness.NewSMSPathReport(freshness.SMSPathVerdict{
		LastSMS:          time.Date(2026, 10, 3, 16, 16, 0, 0, time.Local),
		Silence:          61 * time.Hour,
		ArrivalsPerDay:   5.8,
		ExpectedArrivals: 14.7,
		BaselineSpan:     20 * 24 * time.Hour,
		LongGaps:         3,
		BaselineArrivals: 40,
	}, cfg)
	cases := map[string]string{
		freshness.SMSPathStalled:           "try restarting the phone",
		freshness.SMSPathUsualPace:         "within this phone's usual pace (5.8 arrivals a day)",
		freshness.SMSPathRCSQuiet:          "not an SMS-only failure",
		freshness.SMSPathHistoryStale:      "could not be reloaded",
		freshness.SMSPathFlowing:           "last incoming SMS 2026-10-03 16:16 (2d)",
		freshness.SMSPathThinBaseline:      "went a whole 24h without one 3 time(s) (allowed 1), over 20d of history (needs 21d) and 40 separate arrivals (needs 10)",
		freshness.SMSPathNoHistory:         "no incoming SMS in the Google inbox",
		freshness.SMSPathGoogleUnreachable: "no incoming SMS in the Google inbox",
	}
	for reason, want := range cases {
		report := base
		report.Reason = reason
		if line := smsPathStatusLine(report); !strings.Contains(line, want) {
			t.Errorf("%s: line %q lacks %q", reason, line, want)
		}
	}
}

func TestGoogleSMSPathMonitorNeedsTheV2StackAndIngest(t *testing.T) {
	if googleSMSPathMonitor(nil, true) != nil {
		t.Fatal("monitor without a v2 stack")
	}
	store, receive := openSMSPathTestStore(t)
	stack := &v2Stack{Store: store}
	if googleSMSPathMonitor(stack, false) != nil {
		t.Fatal("monitor without v2 ingest, whose inbox would hold only stale frames")
	}
	now := time.Now()
	receive(now.Add(-time.Hour), "sms", 1)
	report, ok := googleSMSPathMonitor(stack, true).Report(context.Background(), now)
	// The inbox stores receipt in milliseconds, which caps the timestamp.
	if !ok || report.LastSMSMS != now.Add(-time.Hour).UnixMilli() || report.Frames != 1 {
		t.Fatalf("report = %+v ok = %v, want the inbox SMS", report, ok)
	}
}
