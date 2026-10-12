package cmd

import (
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/importer"
)

// Local desktop imports are the legacy-store importers that read other apps'
// databases on this Mac: WhatsApp Desktop (ChatStorage.sqlite), Signal Desktop
// (sql/db.sqlite) and Messages (chat.db). The daemon polls the configured ones
// every 30 s and rescans Signal Desktop in full every 30 min. They write only
// the legacy messages.db.

// Why a daemon runs none of the local desktop importers ("" = it runs them).
const (
	localImportsOffDemo       = "demo"
	localImportsOffTransports = "transports_off"
	localImportsOffAppSandbox = "app_sandbox"
	localImportsOffV2Primary  = "v2_primary"
)

const (
	localImportPollInterval       = 30 * time.Second
	localSignalFullRescanDelay    = 2 * time.Minute
	localSignalFullRescanInterval = 30 * time.Minute
)

// localImportsOffReason decides whether the daemon runs the local desktop
// importers at all; per-poll conditions (the WhatsApp live bridge in use,
// signal-cli paired, macOS) still apply on top.
//
// A v2-primary daemon runs none of them. It serves conversations, messages and
// search from the v2 store, so rows written to the legacy store don't appear
// there (only side surfaces such as /api/people still read legacy). They also
// corrupt /api/status: there the legacy store is the comparison copy of live traffic
// that `freshness.<platform>.projection_stalled` measures v2 against, and a
// desktop import advances `legacy_latest_ms` for whatsapp or signal past
// anything v2 holds. Seen live on 2026-10-11: signal `projection_stalled`
// with legacy_latest_ms on 09-27 and v2's newest Signal message on 08-18. The
// legacy rows in between are Signal Desktop imports (their attachments carry
// `signallocal:` refs), written by the 30-minute full rescan, which the
// sandbox flag never gated.
func localImportsOffReason(transports, demo, sandboxed, v2Primary bool) string {
	switch {
	case demo:
		return localImportsOffDemo
	case !transports:
		return localImportsOffTransports
	case sandboxed:
		return localImportsOffAppSandbox
	case v2Primary:
		return localImportsOffV2Primary
	default:
		return ""
	}
}

// localImportsStatus is the `local_imports` block of /api/status. The startup
// log line says the same thing, but the app keeps backend lines only in
// memory (os_log Info), so this is the durable place to read the decision.
// IMessage says whether the poll includes iMessage at all.
type localImportsStatus struct {
	Enabled    bool   `json:"enabled"`
	OffReason  string `json:"off_reason,omitempty"`
	AppSandbox bool   `json:"app_sandbox"`
	IMessage   bool   `json:"imessage"`
}

type localImportsConfig struct {
	Logger    zerolog.Logger
	Store     *db.Store
	OffReason string
	Sandboxed bool

	// Per-poll conditions, read live on every tick.
	WhatsAppLiveBridge func() bool
	SignalAccount      func() (account string, paired bool)
	IMessageSupported  bool

	// The importers. Signal's full flag forces a full rescan instead of the
	// incremental window. ImportIMessage is one importer.IMessage reused
	// across polls, so each poll reads only what chat.db changed.
	ImportWhatsApp func(store *db.Store) (*importer.ImportResult, error)
	ImportSignal   func(store *db.Store, account string, full bool) (*importer.ImportResult, error)
	ImportIMessage func(store *db.Store) (*importer.ImportResult, error)
}

// localImports runs the local desktop importers for one daemon. Its methods
// are called from one goroutine per schedule (poll, full rescan), never
// concurrently with themselves.
type localImports struct {
	cfg     localImportsConfig
	lastErr map[string]string
}

func newLocalImports(cfg localImportsConfig) *localImports {
	return &localImports{cfg: cfg, lastErr: map[string]string{}}
}

func (l *localImports) enabled() bool {
	return l.cfg.OffReason == ""
}

func (l *localImports) status() localImportsStatus {
	return localImportsStatus{
		Enabled:    l.enabled(),
		OffReason:  l.cfg.OffReason,
		AppSandbox: l.cfg.Sandboxed,
		IMessage:   l.iMessageEnabled(),
	}
}

func (l *localImports) iMessageEnabled() bool {
	return l.enabled() && l.cfg.ImportIMessage != nil && l.cfg.IMessageSupported
}

// logDecision writes one line at startup saying whether the importers run.
func (l *localImports) logDecision() {
	logger := l.cfg.Logger
	switch l.cfg.OffReason {
	case "":
		msg := "Local desktop imports on (legacy store): WhatsApp Desktop while its live bridge is unused, Signal Desktop while signal-cli is paired"
		switch {
		case l.iMessageEnabled():
			msg += ", iMessage (chat.db changes since the last poll)"
		case !l.cfg.IMessageSupported:
			msg += "; no iMessage (macOS only)"
		default:
			msg += "; no iMessage importer configured"
		}
		logger.Info().
			Bool("app_sandbox", l.cfg.Sandboxed).
			Bool("imessage", l.iMessageEnabled()).
			Msg(msg)
	case localImportsOffV2Primary:
		logger.Info().
			Str("off_reason", l.cfg.OffReason).
			Msg("Local desktop imports off: this daemon reads the v2 store, and the WhatsApp Desktop, Signal Desktop and iMessage importers write only the legacy store, where their rows would also make /api/status report projection_stalled")
	case localImportsOffAppSandbox:
		logger.Info().
			Str("off_reason", l.cfg.OffReason).
			Msg("Local desktop imports off: the app runs in the macOS App Sandbox, which blocks other apps' data")
	default:
		logger.Info().
			Str("off_reason", l.cfg.OffReason).
			Msg("Local desktop imports off")
	}
}

// syncOnce runs one poll and reports whether any importer stored a message.
func (l *localImports) syncOnce() bool {
	if !l.enabled() {
		return false
	}
	changed := false
	syncPlatform := func(platform, successMsg string, importFromDB func(*db.Store) (*importer.ImportResult, error)) {
		result, err := importFromDB(l.cfg.Store)
		if err != nil {
			logSyncError(l.cfg.Logger, l.lastErr, platform, err)
			return
		}
		if len(result.Errors) > 0 {
			// Rows the importer could not write (it retries them); the rest
			// of the poll still landed.
			logSyncError(l.cfg.Logger, l.lastErr, platform,
				fmt.Errorf("%d rows not written, first: %s", len(result.Errors), result.Errors[0]))
		}
		if result.MessagesImported == 0 {
			return
		}

		l.lastErr[platform] = ""
		changed = true
		l.cfg.Logger.Info().
			Int("messages", result.MessagesImported).
			Int("conversations", result.ConversationsCreated).
			Msg(successMsg)
	}

	if l.cfg.ImportWhatsApp != nil && l.cfg.WhatsAppLiveBridge != nil && !l.cfg.WhatsAppLiveBridge() {
		syncPlatform("whatsapp", "WhatsApp sync complete", l.cfg.ImportWhatsApp)
	}
	if l.cfg.ImportSignal != nil && l.cfg.SignalAccount != nil {
		if account, paired := l.cfg.SignalAccount(); paired {
			syncPlatform("signal", "Signal desktop sync complete", func(store *db.Store) (*importer.ImportResult, error) {
				return l.cfg.ImportSignal(store, account, false)
			})
		}
	}
	if l.iMessageEnabled() {
		syncPlatform("imessage", "iMessage sync complete", l.cfg.ImportIMessage)
	}
	return changed
}

// fullSignalRescanOnce forces a full Signal Desktop rescan, so drift that
// slipped past both the live signal-cli socket and the 30-second incremental
// window is eventually recovered, and reports how many messages it stored.
func (l *localImports) fullSignalRescanOnce() int {
	if !l.enabled() || l.cfg.ImportSignal == nil || l.cfg.SignalAccount == nil {
		return 0
	}
	account, paired := l.cfg.SignalAccount()
	if !paired {
		return 0
	}
	result, err := l.cfg.ImportSignal(l.cfg.Store, account, true)
	if err != nil {
		l.cfg.Logger.Debug().Err(err).Msg("Periodic Signal full rescan failed")
		return 0
	}
	if result.MessagesImported > 0 {
		l.cfg.Logger.Info().
			Int("recovered", result.MessagesImported).
			Msg("Periodic Signal full rescan recovered drifted messages")
	}
	return result.MessagesImported
}
