package cmd

import (
	"context"
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
	return store, receive
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

func TestSMSPathStatusLineCoversEveryReason(t *testing.T) {
	cfg := freshness.DefaultSMSPathConfig
	base := freshness.NewSMSPathReport(freshness.SMSPathVerdict{
		LastSMS:          time.Date(2026, 10, 3, 16, 16, 0, 0, time.Local),
		Silence:          61 * time.Hour,
		ArrivalsPerDay:   5.8,
		ExpectedArrivals: 14.7,
		ActiveDays:       19,
		BaselineArrivals: 40,
	}, cfg)
	cases := map[string]string{
		freshness.SMSPathStalled:           "try restarting the phone",
		freshness.SMSPathUsualPace:         "within this phone's usual pace (5.8 arrivals a day)",
		freshness.SMSPathRCSQuiet:          "not an SMS-only failure",
		freshness.SMSPathHistoryStale:      "could not be reloaded",
		freshness.SMSPathFlowing:           "last incoming SMS 2026-10-03 16:16 (2d)",
		freshness.SMSPathThinBaseline:      "SMS arrived on 19 of the 28 days before the last one, in 40 separate arrivals (needs 21 days and 10 arrivals)",
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
