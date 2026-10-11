package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow/types"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/whatsappdesktop"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

const (
	whatsappDesktopInitialDelay = 20 * time.Second
	whatsappDesktopScanInterval = 30 * time.Second
	whatsappDesktopFullInterval = 6 * time.Hour
	whatsappDesktopOfflineDelay = 5 * time.Minute
	whatsappDesktopChunkSize    = 100
)

// whatsappDesktopFeedStatus describes the history feeder only. It never
// changes the meaning of WhatsApp's paired/connected or live-ingest counters.
type whatsappDesktopFeedStatus struct {
	Enabled             bool             `json:"enabled"`
	Available           bool             `json:"available"`
	Active              bool             `json:"active"`
	DBPath              string           `json:"db_path"`
	LastScanAtMS        int64            `json:"last_scan_at_ms"`
	LastFullScanAtMS    int64            `json:"last_full_scan_at_ms"`
	HighWaterPK         int64            `json:"high_water_pk"`
	FramesAppended      uint64           `json:"frames_appended"`
	RowsSkippedByReason map[string]int64 `json:"rows_skipped_by_reason"`
	LastError           string           `json:"last_error"`
}

// whatsappDesktopGate observes each live status transition as well as scan
// ticks. A brief reconnect therefore restarts the continuous offline window.
type whatsappDesktopGate struct {
	mu             sync.Mutex
	disconnectedAt time.Time
}

func (g *whatsappDesktopGate) allowed(status whatsapplive.StatusSnapshot, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if status.Connected || !status.Paired {
		g.disconnectedAt = time.Time{}
		return !status.Connected
	}
	if g.disconnectedAt.IsZero() {
		g.disconnectedAt = now
	} else if status.LastConnectedAt.After(g.disconnectedAt) {
		// SendPresence can delay the live status callback past a short
		// reconnect. Its retained connection timestamp still breaks the
		// continuous offline window; start conservatively at this observation.
		g.disconnectedAt = now
	}
	return now.Sub(g.disconnectedAt) >= whatsappDesktopOfflineDelay
}

type whatsappDesktopScan func(context.Context, whatsappdesktop.ScanOptions, func(whatsappdesktop.Chunk) error) (whatsappdesktop.Stats, error)

type whatsappDesktopFeed struct {
	mu      sync.RWMutex
	scanMu  sync.Mutex
	status  whatsappDesktopFeedStatus
	gate    *whatsappDesktopGate
	live    func() whatsapplive.StatusSnapshot
	scan    whatsappDesktopScan
	sink    bridge.HistoryIngressSink
	logger  zerolog.Logger
	logged  map[string]bool
	initErr error
	selfJID string
}

func whatsappDesktopFeedEnabled(stack *v2Stack, transports, demo bool) bool {
	return stack != nil && transports && !demo && !v2FlagExplicitlyDisabled("OPENMESSAGES_WHATSAPP_DESKTOP_FEED")
}

func newWhatsAppDesktopFeed(stack *v2Stack, dataDir string, transports, demo bool, gate *whatsappDesktopGate, live func() whatsapplive.StatusSnapshot, logger zerolog.Logger) *whatsappDesktopFeed {
	if gate == nil {
		gate = &whatsappDesktopGate{}
	}
	f := &whatsappDesktopFeed{
		status: whatsappDesktopFeedStatus{
			Enabled:             whatsappDesktopFeedEnabled(stack, transports, demo),
			DBPath:              whatsappdesktop.DefaultDBPath(),
			RowsSkippedByReason: map[string]int64{},
		},
		gate: gate, live: live, logger: logger, logged: map[string]bool{},
	}
	if !f.status.Enabled {
		return f
	}
	// The launcher sets OPENMESSAGES_APP_SANDBOX=1 even for its unsandboxed
	// hardened-runtime build. Actual read access to the Desktop DB is the
	// permission check; app.Sandboxed() would disable a working history source.
	if err := ensureWhatsAppDesktopAccount(stack.Store); err != nil {
		f.initErr = err
		f.recordError(err)
		return f
	}
	selfJID, err := whatsappDesktopSelfJID(stack.Store)
	if err != nil {
		f.initErr = err
		f.recordError(err)
		return f
	}
	reader := whatsappdesktop.New(whatsappdesktop.Options{
		DBPath:        f.status.DBPath,
		SessionDBPath: filepath.Join(dataDir, "whatsapp-session.db"),
		SelfJID:       selfJID,
	})
	f.scan = reader.Scan
	f.sink = stack.Sink
	f.selfJID = selfJID
	if err := f.checkAvailable(); err != nil {
		f.recordError(err)
	}
	return f
}

// The feeder uses the same account spec as live ingress, including when live
// adapter initialization failed before it could bootstrap that account.
func ensureWhatsAppDesktopAccount(store *sqlite.Store) error {
	if _, err := store.GetAccount(whatsappAccountID); err == nil {
		return nil
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return fmt.Errorf("WhatsApp desktop feed: read account: %w", err)
	}
	spec, err := liveAccountSpec(bridge.PlatformWhatsApp)
	if err != nil {
		return err
	}
	nowMS := time.Now().UnixMilli()
	return store.UpsertAccount(sqlite.Account{
		AccountID: whatsappAccountID, BridgeKey: spec.bridgeKey, DisplayName: spec.displayName,
		Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: "{}",
		CreatedAtMS: nowMS, UpdatedAtMS: nowMS,
	})
}

// Capture uses the existing WhatsApp self identity's PN JID, matching live
// canonicalJID(...).String(). If no PN self identity exists, the empty sender
// is intentional: IsFromMe/IsSelf still identifies outgoing messages and the
// worker never resolves an outgoing sender identity. No self identity is minted.
func whatsappDesktopSelfJID(store *sqlite.Store) (string, error) {
	identityID, found, err := store.SelfIdentityID(whatsappAccountID)
	if err != nil || !found {
		return "", err
	}
	identity, err := store.GetIdentity(identityID)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(identity.CanonicalValue)
	if identity.Kind == "e164" {
		value = strings.TrimPrefix(value, "+") + "@s.whatsapp.net"
	}
	jid, err := types.ParseJID(value)
	if err != nil || jid.Server != types.DefaultUserServer || jid.User == "" || jid.User == "0" {
		return "", nil
	}
	for _, digit := range jid.User {
		if digit < '0' || digit > '9' {
			return "", nil
		}
	}
	return jid.ToNonAD().String(), nil
}

func (f *whatsappDesktopFeed) Snapshot() whatsappDesktopFeedStatus {
	f.mu.RLock()
	snapshot := f.status
	snapshot.RowsSkippedByReason = make(map[string]int64, len(f.status.RowsSkippedByReason))
	for reason, count := range f.status.RowsSkippedByReason {
		snapshot.RowsSkippedByReason[reason] = count
	}
	f.mu.RUnlock()
	if snapshot.Active && f.live != nil {
		snapshot.Active = f.gate.allowed(f.live(), time.Now())
	}
	return snapshot
}

func (f *whatsappDesktopFeed) checkAvailable() error {
	file, err := os.Open(f.status.DBPath)
	if err == nil {
		info, statErr := file.Stat()
		err = errors.Join(statErr, file.Close())
		if err == nil && !info.Mode().IsRegular() {
			err = errors.New("database path is not a regular file")
		}
	}
	f.mu.Lock()
	f.status.Available = err == nil
	f.mu.Unlock()
	if err != nil {
		return fmt.Errorf("WhatsApp desktop database unavailable: %w", err)
	}
	return nil
}

func (f *whatsappDesktopFeed) recordError(err error) {
	if err == nil {
		return
	}
	message := err.Error()
	f.mu.Lock()
	f.status.LastError = message
	f.status.Active = false
	logged := f.logged[message]
	f.logged[message] = true
	f.mu.Unlock()
	if !logged {
		f.logger.Warn().Err(err).Msg("WhatsApp desktop history feed unavailable")
	}
}

var errWhatsAppDesktopLiveServing = errors.New("WhatsApp live bridge resumed during desktop scan")

func (f *whatsappDesktopFeed) scanOnce(ctx context.Context, now time.Time) {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	snapshot := f.Snapshot()
	if !snapshot.Enabled || f.initErr != nil {
		return
	}
	if !f.gate.allowed(f.live(), now) {
		f.mu.Lock()
		f.status.Active = false
		f.mu.Unlock()
		return
	}
	if err := f.checkAvailable(); err != nil {
		f.recordError(err)
		return
	}
	full := snapshot.LastFullScanAtMS == 0 || now.Sub(time.UnixMilli(snapshot.LastFullScanAtMS)) >= whatsappDesktopFullInterval
	afterPK := snapshot.HighWaterPK
	if full {
		afterPK = 0
	}
	f.mu.Lock()
	f.status.Active = true
	f.status.LastScanAtMS = now.UnixMilli()
	f.mu.Unlock()
	var callbackErr error
	stats, err := f.scan(ctx, whatsappdesktop.ScanOptions{AfterPK: afterPK, ChunkSize: whatsappDesktopChunkSize}, func(chunk whatsappdesktop.Chunk) (emitErr error) {
		defer func() {
			if emitErr != nil {
				callbackErr = emitErr
			}
		}()
		if err := ctx.Err(); err != nil {
			return err
		}
		// Recheck at each durable append, so reconnecting during a long full
		// scan stops desktop history before the next frame reaches the inbox.
		if !f.gate.allowed(f.live(), time.Now()) {
			return errWhatsAppDesktopLiveServing
		}
		frame := ingest.WhatsAppDesktopHistorySnapshot{ChatJID: chunk.Chat.JID, Title: chunk.Chat.Title}
		for _, participant := range chunk.Chat.Participants {
			frame.Participants = append(frame.Participants, ingest.WhatsAppDesktopHistoryParticipant{JID: participant.JID, Name: participant.Name, IsSelf: f.selfJID != "" && participant.JID == f.selfJID})
		}
		for _, message := range chunk.Messages {
			frame.Messages = append(frame.Messages, ingest.WhatsAppDesktopHistoryMessage{
				StanzaID: message.StanzaID, SenderJID: message.SenderJID, TimestampMS: message.TimestampMS,
				IsFromMe: message.IsFromMe, Type: message.Type, Text: message.Text, Caption: message.Caption,
				FileName: message.FileName, MIMEType: message.MIMEType, LocalMediaRef: message.LocalMediaRef,
				VoiceNote: message.VoiceNote, Size: message.Size,
			})
		}
		// Generation 0 means daemon-owned capture outside a supervisor
		// generation. validateIngress and inbox CHECK(generation >= 0) accept
		// it. Direct history append bypasses generationSink because this reader
		// owns no live transport; it records no supervisor activity or heartbeat.
		record, err := ingest.WhatsAppDesktopHistoryRecord(whatsappAccountID, 0, frame, now)
		if err != nil {
			return err
		}
		if err := f.sink.AppendHistoryIngress(ctx, record); err != nil {
			return err
		}
		f.mu.Lock()
		f.status.FramesAppended++
		f.mu.Unlock()
		return nil
	})
	f.mu.Lock()
	// Skip counters describe the most recent scan, rather than multiplying
	// the same skipped rows on each six-hour full rescan.
	f.status.RowsSkippedByReason = stats.RowsSkippedByReason
	if err != nil && callbackErr == nil && !errors.Is(err, context.Canceled) {
		// A readable file can still be a corrupt or unsupported Desktop
		// database. Projection/append failures leave source availability alone.
		f.status.Available = false
	}
	if errors.Is(err, errWhatsAppDesktopLiveServing) {
		f.status.Active = false
	}
	if err == nil {
		f.status.HighWaterPK = stats.HighWaterPK
		if full {
			f.status.LastFullScanAtMS = now.UnixMilli()
		}
		f.status.LastError = ""
	}
	f.mu.Unlock()
	if err != nil && !errors.Is(err, errWhatsAppDesktopLiveServing) && !errors.Is(err, context.Canceled) {
		f.recordError(err)
	}
}

func (f *whatsappDesktopFeed) Start(ctx context.Context) func() {
	if !f.Snapshot().Enabled || f.initErr != nil {
		return func() {}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		initial := time.NewTimer(whatsappDesktopInitialDelay)
		defer initial.Stop()
		select {
		case <-runCtx.Done():
			return
		case now := <-initial.C:
			f.scanOnce(runCtx, now)
		}
		ticker := time.NewTicker(whatsappDesktopScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case now := <-ticker.C:
				f.scanOnce(runCtx, now)
			}
		}
	}()
	return func() { cancel(); <-done }
}
