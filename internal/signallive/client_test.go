package signallive

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestQRCodeRendersDataURL(t *testing.T) {
	bridge := &Bridge{
		qr: QRSnapshot{
			URI: "sgnl://linkdevice?uuid=test",
		},
	}
	snap, err := bridge.QRCode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(snap.PNGDataURL, "data:image/png;base64,") {
		t.Fatalf("unexpected QR data url: %q", snap.PNGDataURL)
	}
}

func TestBridgeSendTextRunsSignalCLI(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		store:     nil,
		logger:    zerolog.Nop(),
		callbacks: Callbacks{},
	}

	originalRun := runSignalCLI
	originalNow := now
	defer func() {
		runSignalCLI = originalRun
		now = originalNow
	}()

	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{configDir}, args...)
		return []byte("ok"), nil
	}
	now = func() time.Time { return time.UnixMilli(1700000000123) }

	msg, err := bridge.SendText("signal:+15551234567", "hello from signal", "signal:quoted")
	if err != nil {
		t.Fatalf("SendText(): %v", err)
	}
	if got := strings.Join(captured[1:], " "); !strings.Contains(got, "-a +15551230000 send -m hello from signal +15551234567") {
		t.Fatalf("unexpected signal-cli args: %q", got)
	}
	if msg.ConversationID != "signal:+15551234567" {
		t.Fatalf("conversation id = %q", msg.ConversationID)
	}
	if msg.Status != "sent" || !msg.IsFromMe {
		t.Fatalf("unexpected outgoing message %+v", msg)
	}
	if msg.ReplyToID != "signal:quoted" {
		t.Fatalf("reply_to_id = %q", msg.ReplyToID)
	}
}

func TestSignalCLIExecutableFallsBackToHomebrewPath(t *testing.T) {
	originalOverride := os.Getenv("OPENMESSAGES_SIGNAL_CLI")
	originalLookPath := signalCLILookPath
	originalStat := signalCLIStat
	defer func() {
		if originalOverride == "" {
			_ = os.Unsetenv("OPENMESSAGES_SIGNAL_CLI")
		} else {
			_ = os.Setenv("OPENMESSAGES_SIGNAL_CLI", originalOverride)
		}
		signalCLILookPath = originalLookPath
		signalCLIStat = originalStat
	}()

	_ = os.Unsetenv("OPENMESSAGES_SIGNAL_CLI")
	signalCLILookPath = func(file string) (string, error) {
		return "", os.ErrNotExist
	}
	signalCLIStat = func(name string) (os.FileInfo, error) {
		if name == "/opt/homebrew/bin/signal-cli" {
			return fakeFileInfo{name: "signal-cli"}, nil
		}
		return nil, os.ErrNotExist
	}

	if got := signalCLIExecutable(); got != "/opt/homebrew/bin/signal-cli" {
		t.Fatalf("signalCLIExecutable() = %q, want /opt/homebrew/bin/signal-cli", got)
	}
}

func TestFirstSignalAccountParsesWrappedAccountsJSON(t *testing.T) {
	raw := []byte(`{"accounts":[{"number":"+16506303657","uuid":"abc"}],"version":2}`)
	if got := firstSignalAccount(raw); got != "+16506303657" {
		t.Fatalf("firstSignalAccount() = %q, want +16506303657", got)
	}
}

func TestParseSignalAccountsParsesWrappedAccountsJSON(t *testing.T) {
	raw := []byte(`{"accounts":[{"number":"+16506303657"},{"number":"+15551230000"}],"version":2}`)
	got := parseSignalAccounts(raw)
	want := []string{"+15551230000", "+16506303657"}
	if len(got) != len(want) {
		t.Fatalf("parseSignalAccounts() len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseSignalAccounts()[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestParseSignalGroupsParsesCLIOutput(t *testing.T) {
	raw := []byte("INFO  AccountHelper - The Signal protocol expects that incoming messages are regularly received.\nId: L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE= Name: Neighborhood Planning Group  Active: true Blocked: false\nId: tcbmDdkjub73sxbN39A0FW4CSnTFlz06Xh1Wk+kWrFQ= Name: Signal Sandbox  Active: true Blocked: false\n")
	got := parseSignalGroups(raw)
	if got["L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE="] != "Neighborhood Planning Group" {
		t.Fatalf("first parsed group = %q", got["L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE="])
	}
	if got["tcbmDdkjub73sxbN39A0FW4CSnTFlz06Xh1Wk+kWrFQ="] != "Signal Sandbox" {
		t.Fatalf("second parsed group = %q", got["tcbmDdkjub73sxbN39A0FW4CSnTFlz06Xh1Wk+kWrFQ="])
	}
}

func TestSignalHistorySyncStatusTracksProgressAndCompletion(t *testing.T) {
	bridge := &Bridge{
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	originalNow := now
	defer func() { now = originalNow }()

	current := time.UnixMilli(1700000000000)
	now = func() time.Time { return current }

	bridge.beginHistorySync()
	snap := bridge.Status()
	if snap.HistorySync == nil || !snap.HistorySync.Running {
		t.Fatalf("expected running history sync snapshot, got %+v", snap.HistorySync)
	}
	if snap.HistorySync.ImportedConversations != 0 || snap.HistorySync.ImportedMessages != 0 {
		t.Fatalf("unexpected initial history sync counts: %+v", snap.HistorySync)
	}

	bridge.recordHistorySyncProgress(true, true)
	snap = bridge.Status()
	if snap.HistorySync == nil {
		t.Fatal("expected history sync snapshot after progress")
	}
	if snap.HistorySync.ImportedConversations != 1 || snap.HistorySync.ImportedMessages != 1 {
		t.Fatalf("unexpected imported counts: %+v", snap.HistorySync)
	}

	current = current.Add(historySyncQuietAfter + time.Second)
	snap = bridge.Status()
	if snap.HistorySync == nil || snap.HistorySync.Running {
		t.Fatalf("expected completed history sync snapshot, got %+v", snap.HistorySync)
	}
	if snap.HistorySync.CompletedAt == 0 {
		t.Fatalf("expected completed_at on %+v", snap.HistorySync)
	}
}

func TestStartReceiveLoopDropsSignalConnectionAfterRepeatedReceiveFailures(t *testing.T) {
	bridge := &Bridge{
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	originalRun := runSignalCLI
	defer func() {
		_ = bridge.Close()
		bridge.commandMu.Lock()
		runSignalCLI = originalRun
		bridge.commandMu.Unlock()
	}()

	var receiveCalls atomic.Int32
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), " listAccounts") {
			return []byte(`[{"number":"+15551230000"}]`), nil
		}
		if strings.Contains(strings.Join(args, " "), " receive ") {
			receiveCalls.Add(1)
			return []byte("WARN  ReceiveHelper - Connection closed unexpectedly, reconnecting in 100 ms"), context.DeadlineExceeded
		}
		return []byte{}, nil
	}

	go bridge.startReceiveLoop("+15551230000", false)

	waitForCondition(t, 2*time.Second, func() bool {
		status := bridge.Status()
		return !status.Connected &&
			!status.Connecting &&
			status.Paired &&
			strings.Contains(status.LastError, "Connection closed unexpectedly")
	})
	if calls := receiveCalls.Load(); calls < int32(receiveFailureLimit) {
		t.Fatalf("receive called %d times, want at least %d", calls, receiveFailureLimit)
	}
}

func TestStartReceiveLoopIgnoresIdleReceiveTimeouts(t *testing.T) {
	bridge := &Bridge{
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	originalRun := runSignalCLI
	defer func() {
		_ = bridge.Close()
		bridge.commandMu.Lock()
		runSignalCLI = originalRun
		bridge.commandMu.Unlock()
	}()

	var receiveCalls atomic.Int32
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), " listAccounts") {
			return []byte(`[{"number":"+15551230000"}]`), nil
		}
		if strings.Contains(strings.Join(args, " "), " receive ") {
			calls := receiveCalls.Add(1)
			if calls >= int32(receiveFailureLimit+1) {
				go bridge.Close()
			}
			return []byte{}, context.DeadlineExceeded
		}
		return []byte{}, nil
	}

	go bridge.startReceiveLoop("+15551230000", false)

	waitForCondition(t, 2*time.Second, func() bool {
		return receiveCalls.Load() >= int32(receiveFailureLimit+1)
	})
	waitForCondition(t, 2*time.Second, func() bool {
		return !bridge.Status().Connected
	})

	status := bridge.Status()
	if status.LastError != "" {
		t.Fatalf("expected idle timeouts to avoid last_error, got %+v", status)
	}
	if calls := receiveCalls.Load(); calls < int32(receiveFailureLimit+1) {
		t.Fatalf("receive called %d times, want at least %d", calls, receiveFailureLimit+1)
	}
}

func TestHandleReceiveOutputStoresIncomingSignalMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hi from signal","mentions":[{"number":"+15551230000"}]}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	convo, err := store.GetConversation("signal:+15551234567")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo.Name != "Taylor" || convo.SourcePlatform != "signal" {
		t.Fatalf("unexpected conversation %+v", convo)
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "hi from signal" {
		t.Fatalf("body = %q", msgs[0].Body)
	}
	if !msgs[0].MentionsMe {
		t.Fatalf("expected mentions_me on %+v", msgs[0])
	}
}

func TestHandleReceiveOutputAdvancesSignalHistorySyncCounts(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}
	bridge.beginHistorySync()

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hi from signal"}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	snap := bridge.Status()
	if snap.HistorySync == nil {
		t.Fatal("expected history sync snapshot")
	}
	if snap.HistorySync.ImportedConversations != 1 || snap.HistorySync.ImportedMessages != 1 {
		t.Fatalf("unexpected history sync counts: %+v", snap.HistorySync)
	}
}

func TestHandleReceiveOutputStoresIncomingSignalAttachmentID(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"attachments":[{"contentType":"image/png","id":"att-123"}]}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].MediaID != "signalatt:att-123" {
		t.Fatalf("media_id = %q, want signalatt:att-123", msgs[0].MediaID)
	}
	if msgs[0].MimeType != "image/png" {
		t.Fatalf("mime_type = %q, want image/png", msgs[0].MimeType)
	}
}

func TestHandleReceiveOutputUsesCachedSignalGroupNameWhenTitleMissing(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:      store,
		logger:     zerolog.Nop(),
		configDir:  t.TempDir(),
		groupNames: map[string]string{"L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE=": "Neighborhood Planning Group"},
	}

	payload := `{"account":"+15551230000","envelope":{"sourceName":"Michael Thorning","sourceUuid":"d91b5024-f3db-4c82-98f8-2691974d6a9b","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"hello group","groupInfo":{"groupId":"L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE="}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	convo, err := store.GetConversation("signal-group:L3uFclL9x2vlGMKnNURIUnN06p31Y+rY3s9QocxIJnE=")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if !convo.IsGroup {
		t.Fatalf("expected group conversation, got %+v", convo)
	}
	if convo.Name != "Neighborhood Planning Group" {
		t.Fatalf("group name = %q", convo.Name)
	}
}

func TestHandleReceiveOutputAppliesIncomingSignalReactionToTargetMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal:+15551234567"
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Taylor",
		LastMessageTS:  1700000000123,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:target-msg",
		ConversationID: conversationID,
		SenderName:     "Taylor",
		SenderNumber:   "+15551234567",
		Body:           "hello",
		TimestampMS:    1700000000123,
		SourcePlatform: "signal",
		SourceID:       "target-msg",
	}); err != nil {
		t.Fatalf("seed target message: %v", err)
	}

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceName":"Taylor","timestamp":1700000001123,"dataMessage":{"timestamp":1700000001123,"reaction":{"emoji":"😂","targetAuthor":"+15551234567","targetSentTimestamp":1700000000123,"isRemove":false}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msg, err := store.GetMessageByID("signal:target-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(target): %v", err)
	}
	if msg == nil {
		t.Fatal("expected target message to exist")
	}
	var reactions []storedReaction
	if err := json.Unmarshal([]byte(msg.Reactions), &reactions); err != nil {
		t.Fatalf("json.Unmarshal(reactions): %v", err)
	}
	if len(reactions) != 1 || reactions[0].Emoji != "😂" || reactions[0].Count != 1 {
		t.Fatalf("unexpected reactions after add: %+v", reactions)
	}

	msgs, err := store.GetMessagesByConversation(conversationID, 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
}

func TestBridgeSendReactionRunsSignalCLIAndUpdatesLocalState(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal:+15551234567"
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Taylor",
		LastMessageTS:  1700000000123,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:target-msg",
		ConversationID: conversationID,
		SenderName:     "Taylor",
		SenderNumber:   "+15551234567",
		Body:           "hello",
		TimestampMS:    1700000000123,
		SourcePlatform: "signal",
		SourceID:       "target-msg",
	}); err != nil {
		t.Fatalf("seed target message: %v", err)
	}

	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		store:     store,
		logger:    zerolog.Nop(),
		callbacks: Callbacks{},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{configDir}, args...)
		return []byte("ok"), nil
	}

	if err := bridge.SendReaction(conversationID, "signal:target-msg", "😂", "add"); err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}
	if got := strings.Join(captured[1:], " "); !strings.Contains(got, "-a +15551230000 sendReaction -e 😂 -a +15551234567 -t 1700000000123 +15551234567") {
		t.Fatalf("unexpected signal-cli args: %q", got)
	}

	msg, err := store.GetMessageByID("signal:target-msg")
	if err != nil {
		t.Fatalf("GetMessageByID(target): %v", err)
	}
	var reactions []storedReaction
	if err := json.Unmarshal([]byte(msg.Reactions), &reactions); err != nil {
		t.Fatalf("json.Unmarshal(reactions): %v", err)
	}
	if len(reactions) != 1 || reactions[0].Emoji != "😂" || reactions[0].Count != 1 {
		t.Fatalf("unexpected reactions after send: %+v", reactions)
	}
}

func TestBridgeSendReactionResolvesGroupTargetAuthorACIToNumber(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal-group:test-group"
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Group",
		IsGroup:        true,
		LastMessageTS:  1700000000123,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:target-msg",
		ConversationID: conversationID,
		SenderName:     "Michael Thorning",
		SenderNumber:   "d91b5024-f3db-4c82-98f8-2691974d6a9b",
		Body:           "hello",
		TimestampMS:    1700000000123,
		SourcePlatform: "signal",
		SourceID:       "target-msg",
	}); err != nil {
		t.Fatalf("seed target message: %v", err)
	}

	bridge := &Bridge{
		account:      "+15551230000",
		connected:    true,
		configDir:    t.TempDir(),
		store:        store,
		logger:       zerolog.Nop(),
		callbacks:    Callbacks{},
		contactByACI: map[string]string{},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	var calls [][]string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) > 0 && args[0] == "listContacts" {
			return []byte("Number: +15551234567 ACI: d91b5024-f3db-4c82-98f8-2691974d6a9b Name:  Profile name: Michael Thorning Username:  Color:  Blocked: false Message expiration: disabled\n"), nil
		}
		return []byte("ok"), nil
	}

	if err := bridge.SendReaction(conversationID, "signal:target-msg", "😂", "add"); err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}
	if len(calls) < 2 {
		t.Fatalf("expected listContacts and sendReaction calls, got %d", len(calls))
	}
	got := strings.Join(calls[len(calls)-1], " ")
	if !strings.Contains(got, "sendReaction -e 😂 -a +15551234567 -t 1700000000123 --group-id test-group") {
		t.Fatalf("unexpected signal-cli args: %q", got)
	}
}

func TestBridgeSendTextIncludesSignalQuoteArguments(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "signal:+15551234567"
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "signal:reply-1",
		ConversationID: conversationID,
		SenderName:     "Taylor",
		SenderNumber:   "+15551234567",
		Body:           "quoted body",
		TimestampMS:    1700000000123,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("seed reply target: %v", err)
	}

	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		store:     store,
		logger:    zerolog.Nop(),
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{configDir}, args...)
		return []byte("ok"), nil
	}

	if _, err := bridge.SendText(conversationID, "replying", "signal:reply-1"); err != nil {
		t.Fatalf("SendText(): %v", err)
	}
	got := strings.Join(captured[1:], " ")
	if !strings.Contains(got, "--quote-timestamp 1700000000123 --quote-author +15551234567 --quote-message quoted body") {
		t.Fatalf("unexpected quote args: %q", got)
	}
}

func TestBridgeSendMediaRunsSignalCLIAndReturnsLocalAttachmentMessage(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		logger:    zerolog.Nop(),
		callbacks: Callbacks{},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{configDir}, args...)
		return []byte("ok"), nil
	}

	msg, err := bridge.SendMedia("signal:+15551234567", []byte("png-bytes"), "photo.png", "image/png", "signal photo", "")
	if err != nil {
		t.Fatalf("SendMedia(): %v", err)
	}
	got := strings.Join(captured[1:], " ")
	if !strings.Contains(got, "-a +15551230000 send -m signal photo -a ") || !strings.Contains(got, " +15551234567") {
		t.Fatalf("unexpected signal-cli args: %q", got)
	}
	if msg.SourcePlatform != "signal" {
		t.Fatalf("source platform = %q, want signal", msg.SourcePlatform)
	}
	if msg.MimeType != "image/png" {
		t.Fatalf("mime = %q, want image/png", msg.MimeType)
	}
	if !strings.HasPrefix(msg.MediaID, signalLocalAttachmentPrefix) {
		t.Fatalf("media id = %q, want local signal attachment ref", msg.MediaID)
	}
	data, mimeType, err := bridge.DownloadMedia(msg)
	if err != nil {
		t.Fatalf("DownloadMedia(local): %v", err)
	}
	if string(data) != "png-bytes" {
		t.Fatalf("data = %q, want png-bytes", string(data))
	}
	if mimeType != "image/png" {
		t.Fatalf("mimeType = %q, want image/png", mimeType)
	}
}

func TestBridgeDownloadMediaRunsSignalCLIAndDecodesAttachment(t *testing.T) {
	bridge := &Bridge{
		account:   "+15551230000",
		connected: true,
		configDir: t.TempDir(),
		store:     nil,
		logger:    zerolog.Nop(),
		callbacks: Callbacks{},
	}

	originalRun := runSignalCLI
	defer func() { runSignalCLI = originalRun }()

	var captured []string
	runSignalCLI = func(ctx context.Context, configDir string, args ...string) ([]byte, error) {
		captured = append([]string{configDir}, args...)
		return []byte("aGVsbG8="), nil
	}

	data, mimeType, err := bridge.DownloadMedia(&db.Message{
		MessageID:      "signal:m1",
		ConversationID: "signal:+15551234567",
		SenderNumber:   "+15551234567",
		MimeType:       "image/png",
		MediaID:        "signalatt:att-123",
		SourcePlatform: "signal",
	})
	if err != nil {
		t.Fatalf("DownloadMedia(): %v", err)
	}
	if got := strings.Join(captured[1:], " "); !strings.Contains(got, "-a +15551230000 getAttachment --id att-123 --recipient +15551234567") {
		t.Fatalf("unexpected signal-cli args: %q", got)
	}
	if string(data) != "hello" {
		t.Fatalf("data = %q, want hello", string(data))
	}
	if mimeType != "image/png" {
		t.Fatalf("mimeType = %q, want image/png", mimeType)
	}
}

type fakeFileInfo struct {
	name string
}

func (f fakeFileInfo) Name() string     { return f.name }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

func TestHandleReceiveOutputStoresSentSyncMessageFromAnotherClient(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"timestamp":1700000000222,"syncMessage":{"sentMessage":{"timestamp":1700000000222,"message":"sent from phone","destinationNumber":"+15551234567"}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	convo, err := store.GetConversation("signal:+15551234567")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo.SourcePlatform != "signal" {
		t.Fatalf("unexpected conversation %+v", convo)
	}

	msgs, err := store.GetMessagesByConversation("signal:+15551234567", 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if !msgs[0].IsFromMe || msgs[0].Status != "sent" {
		t.Fatalf("unexpected outgoing sync message %+v", msgs[0])
	}
	if msgs[0].Body != "sent from phone" {
		t.Fatalf("body = %q", msgs[0].Body)
	}
}

func TestHandleReceiveOutputDedupesMatchingLocalOutgoingSignalMessage(t *testing.T) {
	dataDir := t.TempDir()
	store, err := db.New(filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	conversationID := "signal:+15551234567"
	timestamp := int64(1700000000222)
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: conversationID,
		Name:           "Taylor",
		LastMessageTS:  timestamp - 1000,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("UpsertConversation(): %v", err)
	}
	local := &db.Message{
		MessageID:      localOutgoingMessageID(conversationID, timestamp-500, "sent from openmessage"),
		ConversationID: conversationID,
		SenderName:     "Me",
		SenderNumber:   "+15551230000",
		Body:           "sent from openmessage",
		TimestampMS:    timestamp - 500,
		Status:         "sent",
		IsFromMe:       true,
		SourcePlatform: "signal",
	}
	if err := store.UpsertMessage(local); err != nil {
		t.Fatalf("UpsertMessage(): %v", err)
	}

	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		configDir: t.TempDir(),
	}

	payload := `{"account":"+15551230000","envelope":{"timestamp":1700000000222,"syncMessage":{"sentMessage":{"timestamp":1700000000222,"message":"sent from openmessage","destinationNumber":"+15551234567"}}}}`
	if err := bridge.handleReceiveOutput("+15551230000", []byte(payload+"\n")); err != nil {
		t.Fatalf("handleReceiveOutput(): %v", err)
	}

	msgs, err := store.GetMessagesByConversation(conversationID, 10)
	if err != nil {
		t.Fatalf("GetMessagesByConversation(): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].MessageID != local.MessageID {
		t.Fatalf("message id = %q, want %q", msgs[0].MessageID, local.MessageID)
	}
}

func TestConnectEmitsSignalQRCodeAndStoresPairedAccount(t *testing.T) {
	configDir := t.TempDir()
	bridge, err := New(configDir, nil, zerolog.Nop(), Callbacks{})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	originalStartLink := startSignalLink
	originalRun := runSignalCLI
	defer func() {
		_ = bridge.Close()
		bridge.commandMu.Lock()
		runSignalCLI = originalRun
		bridge.commandMu.Unlock()
		startSignalLink = originalStartLink
	}()

	releaseWait := make(chan struct{})
	var callsMu sync.Mutex
	var calls [][]string
	startSignalLink = func(ctx context.Context, cfg string) (io.ReadCloser, func() error, error) {
		reader := io.NopCloser(strings.NewReader("sgnl://linkdevice?uuid=test\n"))
		wait := func() error {
			if err := os.MkdirAll(filepath.Join(cfg, "data"), 0700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(cfg, "data", "accounts.json"), []byte(`[{"number":"+15551230000"}]`), 0600); err != nil {
				return err
			}
			<-releaseWait
			return nil
		}
		return reader, wait, nil
	}
	runSignalCLI = func(ctx context.Context, cfg string, args ...string) ([]byte, error) {
		callsMu.Lock()
		calls = append(calls, append([]string(nil), args...))
		callsMu.Unlock()
		if len(args) >= 3 && args[0] == "--output" && args[2] == "listAccounts" {
			return []byte(`[{"number":"+15551230000"}]`), nil
		}
		return []byte{}, nil
	}

	if err := bridge.Connect(); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool {
		return bridge.Status().QRAvailable
	})
	close(releaseWait)
	waitForCondition(t, 2*time.Second, func() bool {
		status := bridge.Status()
		return status.Paired && status.Account == "+15551230000"
	})
	waitForCondition(t, 2*time.Second, func() bool {
		callsMu.Lock()
		defer callsMu.Unlock()
		for _, args := range calls {
			if len(args) == 3 && args[0] == "-a" && args[1] == "+15551230000" && args[2] == "sendSyncRequest" {
				return true
			}
		}
		return false
	})

}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
