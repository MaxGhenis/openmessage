package app

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"

	"github.com/maxghenis/openmessage/internal/client"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/importer"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// BackfillPhase represents the current phase of a deep backfill.
type BackfillPhase string

const (
	BackfillPhaseIdle     BackfillPhase = ""
	BackfillPhaseFolders  BackfillPhase = "folders"
	BackfillPhaseMessages BackfillPhase = "messages"
	BackfillPhaseContacts BackfillPhase = "contacts"
	BackfillPhaseDone     BackfillPhase = "done"
)

const maxErrorDetails = 100

// BackfillSnapshot is a point-in-time copy of backfill progress, safe to
// pass and marshal by value.
type BackfillSnapshot struct {
	Running            bool          `json:"running"`
	Phase              BackfillPhase `json:"phase"`
	FoldersScanned     int           `json:"folders_scanned"`
	ConversationsFound int           `json:"conversations_found"`
	MessagesFound      int           `json:"messages_found"`
	ContactsChecked    int           `json:"contacts_checked"`
	Errors             int           `json:"errors"`
	ErrorDetails       []string      `json:"error_details,omitempty"`
	// HistoryTeed counts fetched conversations and messages handed to v2
	// ingest; HistoryTeeFailed counts hand-offs v2 refused. A refused item is
	// still written to the legacy store unless the refusal was the generation
	// closing, which stops the catch-up. Both stay zero when v2 ingest is not
	// running.
	HistoryTeed      int `json:"history_teed"`
	HistoryTeeFailed int `json:"history_tee_failed"`
}

// BackfillProgress tracks the current state of a deep backfill operation.
type BackfillProgress struct {
	mu sync.Mutex
	BackfillSnapshot
}

// reset clears all fields for a fresh backfill run.
func (p *BackfillProgress) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Running = true
	p.Phase = BackfillPhaseFolders
	p.FoldersScanned = 0
	p.ConversationsFound = 0
	p.MessagesFound = 0
	p.ContactsChecked = 0
	p.Errors = 0
	p.ErrorDetails = nil
	p.HistoryTeed = 0
	p.HistoryTeeFailed = 0
}

// setPhase updates the current phase.
func (p *BackfillProgress) setPhase(phase BackfillPhase) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Phase = phase
}

// finish marks the backfill as complete.
func (p *BackfillProgress) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Running = false
	p.Phase = BackfillPhaseDone
}

// addError increments the error count and optionally records a detail string.
func (p *BackfillProgress) addError(detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Errors++
	if detail != "" && len(p.ErrorDetails) < maxErrorDetails {
		p.ErrorDetails = append(p.ErrorDetails, detail)
	}
}

// addHistory counts hand-offs of fetched history to v2 ingest.
func (p *BackfillProgress) addHistory(teed, failed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.HistoryTeed += teed
	p.HistoryTeeFailed += failed
}

// add increments the given counters atomically.
func (p *BackfillProgress) add(conversations, messages, contacts, folders int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ConversationsFound += conversations
	p.MessagesFound += messages
	p.ContactsChecked += contacts
	p.FoldersScanned += folders
}

func (p *BackfillProgress) snapshot() BackfillSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := p.BackfillSnapshot
	if len(p.ErrorDetails) > 0 {
		cp.ErrorDetails = append([]string(nil), p.ErrorDetails...)
	}
	return cp
}

type App struct {
	clientMu            sync.RWMutex
	Client              *client.Client
	googleGeneration    *GoogleGeneration
	Store               *db.Store
	EventHandler        *client.EventHandler
	Logger              zerolog.Logger
	DataDir             string
	SessionPath         string
	WhatsAppSessionPath string
	SignalConfigPath    string
	// sendTextOverride lets tests substitute the scheduler's send. Nil in prod.
	sendTextOverride func(conversationID, body, replyToID string) (*db.Message, error)
	// sendMediaOverride lets tests substitute the scheduler's media send. Nil in prod.
	sendMediaOverride      func(conversationID string, data []byte, filename, mime, caption, replyToID string) (*db.Message, error)
	Connected              atomic.Bool
	OnConversationsChange  func()
	OnIncomingMessage      func(*db.Message)
	OnMessagesChange       func(string)
	OnStatusChange         func(bool)
	OnTypingChange         func(conversationID, senderName, senderNumber string, typing bool)
	OnWhatsAppStatusChange func()
	OnSignalStatusChange   func()

	// gmClient is used by backfill methods. If nil, it's derived from Client.GM.
	// Set this field directly in tests to inject a mock.
	gmClient GMClient
	// gmHistory pairs with gmClient in tests: the history ingress catch-ups
	// hand fetched history to while the mock client is installed.
	gmHistory                 GoogleHistoryIngress
	BackfillProgress          BackfillProgress
	backfillRunning           atomic.Bool
	reconcileRunning          atomic.Bool
	avatarSyncMu              sync.Mutex
	avatarSyncOnce            sync.Once
	avatarSyncWG              sync.WaitGroup
	avatarSyncClosed          bool
	avatarSyncQueue           chan db.ContactAvatarCandidate
	avatarSyncStop            chan struct{}
	whatsAppMu                sync.Mutex
	WhatsApp                  *whatsapplive.Bridge
	whatsAppLifecycleMu       sync.RWMutex
	whatsAppLifecycleNotifier WhatsAppLifecycleNotifier
	signalMu                  sync.Mutex
	Signal                    *signallive.Bridge
	statusMu                  sync.Mutex
	googleLastError           string
	// A lapsed Google Messages linked-device session keeps reporting
	// Connected=true while every send comes back UNKNOWN (the phone has
	// silently unlinked us). Count consecutive non-SUCCESS Google sends so
	// the UI can surface a re-pair affordance even while "connected".
	googleSendFailures        atomic.Int32
	googleNeedsRepair         atomic.Bool
	googleAuthExpired         atomic.Bool
	googlePhoneResponding     atomic.Bool
	googlePhoneRespondingSeen atomic.Bool
	googleLifecycleMu         sync.RWMutex
	googleLifecycleNotifier   GoogleLifecycleNotifier
	googleRepairPaceMu        sync.RWMutex
	googlePull                googlePullHealth
	googleRepairPaceCount     func() uint64
	signalLifecycleMu         sync.RWMutex
	signalLifecycleNotifier   SignalLifecycleNotifier
	tempDataDir               string
	pendingMediaMu            sync.Mutex
	pendingMedia              map[string]struct{}
	// googleAccountSwitch is the single source of truth for
	// GoogleStatusSnapshot.AccountSwitched: the phone said Google Messages now
	// uses Google-account pairing, so it refuses this QR-paired session's
	// requests while still pushing inbound updates.
	googleAccountSwitchMu sync.Mutex
	googleAccountSwitch   googleAccountSwitchState
}

type GoogleStatusSnapshot struct {
	Connected    bool `json:"connected"`
	Paired       bool `json:"paired"`
	NeedsPairing bool `json:"needs_pairing"`
	// NeedsRepair is set when the session reports connected but sends keep
	// failing — the phone has likely unlinked the device. The UI should
	// offer re-pairing even though Connected is true.
	NeedsRepair bool   `json:"needs_repair,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	// AuthExpired is set when Google rejects the web session cookies. Unlike a
	// true unpair, a cookie refresh plus reconnect can recover it.
	AuthExpired bool `json:"auth_expired,omitempty"`
	// PhoneResponding is false after libgm reports PhoneNotResponding. Before
	// such an event is observed, unknown is treated as healthy.
	PhoneResponding bool `json:"phone_responding"`
	// AccountSwitched is set when the phone reports that Google Messages
	// switched to Google-account pairing while this session is QR-paired.
	// The phone then keeps pushing inbound updates but answers this
	// session's requests (conversation lookups, sends) without data, so
	// SMS/RCS sends cannot succeed until the session is re-linked with
	// Google-account pairing or the phone is switched back.
	AccountSwitched bool `json:"account_pairing_switched,omitempty"`
	// SwitchedAccount is the Google account the phone reported, when known.
	SwitchedAccount string `json:"switched_account,omitempty"`
	// AccountSwitchedAtMS is when the switch was first observed (Unix ms).
	AccountSwitchedAtMS int64 `json:"account_pairing_switched_at_ms,omitempty"`
	// RepairsPaced counts automatic credential repairs the supervisor delayed
	// to honour the minimum repair interval. 0 through the healthy ~15-minute
	// heal cycle; a climbing count means cookies are being revoked within
	// minutes, which the pacing floor is throttling rather than hiding.
	RepairsPaced uint64 `json:"repairs_paced,omitempty"`
	// PullHealth reports recent catch-up pulls (conversation listings and
	// targeted lookups). Its empty_with_local_history flag is set when pulls
	// return no data while the store holds this account's conversations, the
	// state push-only health checks can't see. Absent before the first pull.
	PullHealth *GooglePullHealthSnapshot `json:"pull_health,omitempty"`
}

// googleRepairThreshold is how many consecutive failed Google sends (with no
// success in between) flip the session into the "needs re-pair" state.
const googleRepairThreshold = 3

// RecordGoogleSendOutcome tracks Google Messages send results so a silently
// unlinked session (Connected=true, every send UNKNOWN) becomes visible. A
// single success clears the flag.
func (a *App) RecordGoogleSendOutcome(success bool) {
	a.RecordGoogleSendOutcomeWithPhone(success, true)
}

func (a *App) RecordGoogleSendOutcomeWithPhone(success bool, phoneResponding bool) {
	if success {
		a.RecordGooglePhoneResponding(true)
		a.googleSendFailures.Store(0)
		a.googleNeedsRepair.Store(false)
		return
	}
	if !phoneResponding {
		return
	}
	if a.googleSendFailures.Add(1) >= googleRepairThreshold {
		a.markGoogleNeedsRepairAndPark(errGoogleSendsRepeatedlyFailed)
	}
}

// RecordGoogleSendError handles send failures that happen before Google
// returns a SendMessageResponse status. Only auth/dead-session failures mark
// the session for re-pair; transient network errors should remain recoverable.
func (a *App) RecordGoogleSendError(err error) {
	// An account-switch answer (IsGoogleAccountSwitchError) is deliberately not
	// counted here: needs_repair parks the transport, and in that state push
	// is the only delivery still working. pull_health.account_switch reports it.
	if isGoogleAuthInvalid(err) {
		a.markGoogleNeedsRepairAndPark(err)
	}
}

// ClearGoogleRepairFlag resets the stuck-session state, e.g. after a fresh
// (re)connect or pairing where sends haven't been attempted yet.
func (a *App) ClearGoogleRepairFlag() {
	a.googleSendFailures.Store(0)
	a.googleNeedsRepair.Store(false)
}

// FlagGoogleNeedsRepair marks the Google Messages session as needing a manual
// re-pair (cookies expired and no automated refresh is available), so the
// reconnect watchdog stops retrying and the UI surfaces a "Re-pair" banner. The
// status change is emitted only on the false->true transition to avoid
// re-arming the watchdog in a loop.
func (a *App) FlagGoogleNeedsRepair() {
	if a.googleNeedsRepair.CompareAndSwap(false, true) {
		a.emitStatusChange(a.Connected.Load())
	}
}

// RecordGooglePhoneResponding tracks whether the paired Android phone is
// currently answering Google Messages requests. This is distinct from
// NeedsRepair: a non-responding phone may simply be off or offline.
func (a *App) RecordGooglePhoneResponding(responding bool) {
	a.googlePhoneResponding.Store(responding)
	a.googlePhoneRespondingSeen.Store(true)
}

// GooglePhoneResponding reports true until libgm explicitly says otherwise.
func (a *App) GooglePhoneResponding() bool {
	if !a.GooglePaired() || !a.googlePhoneRespondingSeen.Load() {
		return true
	}
	return a.googlePhoneResponding.Load()
}

// googleAccountSwitchState records the phone's report that Google Messages
// switched to Google-account pairing. sessionKey names the QR-paired session
// the report was about, so a new pairing never inherits it.
type googleAccountSwitchState struct {
	switched   bool
	account    string
	at         time.Time
	sessionKey string
}

// googleQRSessionKey identifies a QR-paired session by its browser device ID.
// ok is false for a missing client or auth data and for a Google-account
// session: the account-switch signal is only interpreted for QR sessions,
// because whether Google-account sessions also receive it is unverified and a
// false positive there would refuse every send on a correctly paired install.
func googleQRSessionKey(cli *client.Client) (string, bool) {
	if cli == nil || cli.GM == nil || cli.GM.AuthData == nil {
		return "", false
	}
	auth := cli.GM.AuthData
	if auth.IsGoogleAccount() {
		return "", false
	}
	return auth.Browser.GetSourceID(), true
}

// applyGoogleAccountChange records one libgm AccountChange for the session
// cli. Following the mautrix-gmessages connector (Enabled || IsFake), the
// phone has switched when the event is enabled or was synthesized from an
// account-container frame (IsFake); a real event with Enabled=false switches
// it back. libgm calls it synchronously on its receive goroutine, before it
// hands the response that carried the container to the waiting request, so
// it only takes the flag's own short lock.
func (a *App) applyGoogleAccountChange(cli *client.Client, evt *events.AccountChange) {
	if evt == nil {
		return
	}
	switched := evt.GetEnabled() || evt.IsFake
	if !switched {
		a.clearGoogleAccountSwitch("phone_reported_pairing_switched_back")
		return
	}
	a.recordGoogleAccountSwitch(cli, evt.GetAccount(), "account_change_event")
}

// NoteGoogleAccountSwitch records that a send response from the session cli
// carried the phone's Google-account switch. It is a no-op for a
// Google-account session.
func (a *App) NoteGoogleAccountSwitch(cli *client.Client, account string) {
	a.recordGoogleAccountSwitch(cli, account, "send_response")
}

func (a *App) recordGoogleAccountSwitch(cli *client.Client, account, source string) {
	key, ok := googleQRSessionKey(cli)
	if !ok {
		return
	}
	// A send that started on a replaced session can answer after another
	// session was installed; its report must not overwrite that session's.
	if installed := a.GetClient(); installed != nil && installed != cli {
		if installedKey, qr := googleQRSessionKey(installed); !qr || installedKey != key {
			return
		}
	}
	account = strings.TrimSpace(account)
	a.googleAccountSwitchMu.Lock()
	previous := a.googleAccountSwitch
	rising := !previous.switched || previous.sessionKey != key
	changed := rising
	if rising {
		a.googleAccountSwitch = googleAccountSwitchState{
			switched:   true,
			account:    account,
			at:         time.Now(),
			sessionKey: key,
		}
	} else if account != "" && account != previous.account {
		a.googleAccountSwitch.account = account
		changed = true
	}
	a.googleAccountSwitchMu.Unlock()
	if !changed {
		return
	}
	if rising {
		a.Logger.Warn().
			Str("account", account).
			Str("source", source).
			Msg("Phone switched Google Messages to Google-account pairing; this QR-paired session still receives messages but cannot send")
	}
	a.emitStatusChange(a.Connected.Load())
}

// ClearGoogleAccountSwitch forgets the phone's account-switch report
// unconditionally, for when the session itself goes away (unpaired or
// invalidated). Proof that the phone serves a session clears it through
// ClearGoogleAccountSwitchFor instead.
func (a *App) ClearGoogleAccountSwitch() {
	a.clearGoogleAccountSwitch("session_removed")
}

// ClearGoogleAccountSwitchFor forgets the account-switch report because the
// session cli just proved the phone serves it (a real conversation or a
// successful send). A result from a replaced client, or from a session other
// than the one the report is about, proves nothing about the installed
// session and changes nothing.
func (a *App) ClearGoogleAccountSwitchFor(cli *client.Client) {
	key, ok := googleQRSessionKey(cli)
	if !ok || a.GetClient() != cli {
		return
	}
	a.googleAccountSwitchMu.Lock()
	previous := a.googleAccountSwitch
	if !previous.switched || previous.sessionKey != key {
		a.googleAccountSwitchMu.Unlock()
		return
	}
	a.googleAccountSwitch = googleAccountSwitchState{}
	a.googleAccountSwitchMu.Unlock()
	a.Logger.Info().
		Str("reason", "phone_served_session").
		Msg("Google-account pairing switch cleared for this session")
	a.emitStatusChange(a.Connected.Load())
}

func (a *App) clearGoogleAccountSwitch(reason string) {
	a.googleAccountSwitchMu.Lock()
	previous := a.googleAccountSwitch
	a.googleAccountSwitch = googleAccountSwitchState{}
	a.googleAccountSwitchMu.Unlock()
	if !previous.switched {
		return
	}
	a.Logger.Info().
		Str("reason", reason).
		Msg("Google-account pairing switch cleared for this session")
	a.emitStatusChange(a.Connected.Load())
}

// forgetGoogleAccountSwitchForNewSession clears a report about another
// session before cli is installed: a new pairing (another browser ID, or a
// Google-account session) never inherits it. Reinstalling the same session
// keeps it, because the phone may not resend its account container on
// reconnect; proof that the phone serves the session clears it instead.
func (a *App) forgetGoogleAccountSwitchForNewSession(cli *client.Client) {
	a.googleAccountSwitchMu.Lock()
	previous := a.googleAccountSwitch
	key, ok := googleQRSessionKey(cli)
	stale := previous.switched && (!ok || key != previous.sessionKey)
	if stale {
		a.googleAccountSwitch = googleAccountSwitchState{}
	}
	a.googleAccountSwitchMu.Unlock()
	if stale {
		a.Logger.Info().Msg("Google-account pairing switch cleared: a different Google Messages session is being installed")
		a.emitStatusChange(a.Connected.Load())
	}
}

// GoogleAccountSwitch reports whether the phone said it switched Google
// Messages to Google-account pairing for the installed QR-paired session,
// and the Google account it named (empty when unknown).
func (a *App) GoogleAccountSwitch() (bool, string) {
	state := a.currentGoogleAccountSwitch()
	return state.switched, state.account
}

func (a *App) currentGoogleAccountSwitch() googleAccountSwitchState {
	a.googleAccountSwitchMu.Lock()
	state := a.googleAccountSwitch
	a.googleAccountSwitchMu.Unlock()
	if !state.switched {
		return googleAccountSwitchState{}
	}
	if cli := a.GetClient(); cli != nil {
		if key, ok := googleQRSessionKey(cli); !ok || key != state.sessionKey {
			return googleAccountSwitchState{}
		}
	}
	return state
}

func DefaultDataDir() string {
	if dir := os.Getenv("OPENMESSAGES_DATA_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "openmessage")
}

func DemoMode() bool {
	value := strings.TrimSpace(os.Getenv("OPENMESSAGES_DEMO"))
	if value == "" {
		return false
	}
	switch strings.ToLower(value) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// New opens the app state for store-owning entrypoints — the daemon and
// one-shot commands that may write (send, import) — and runs the startup
// repair sweeps that clean legacy artifacts out of messages.db.
func New(logger zerolog.Logger) (*App, error) {
	return newApp(logger, true)
}

// NewClient opens the app state exactly like New — same data-dir resolution
// and permission hardening, same store open (including schema migration) —
// but skips the startup repair sweeps, the only write-y startup step beyond
// the SQLite open itself. Per-session client processes use it: MCP hosts
// spawn one `serve --mcp-stdio` per session, so with many sessions open the
// daemon's live messages.db would otherwise absorb N concurrent repair-sweep
// write bursts at every session start. The daemon still repairs on each of
// its own startups via New, so the sweeps run once per daemon lifecycle
// instead of once per client process. Demo mode still seeds its isolated
// per-process temp store.
func NewClient(logger zerolog.Logger) (*App, error) {
	return newApp(logger, false)
}

func newApp(logger zerolog.Logger, runRepairSweeps bool) (*App, error) {
	dataDir := DefaultDataDir()
	tempDataDir := ""
	if DemoMode() {
		tmpDir, err := os.MkdirTemp("", "openmessage-demo-*")
		if err != nil {
			return nil, fmt.Errorf("create temp dir: %w", err)
		}
		dataDir = tmpDir
		tempDataDir = tmpDir
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		if tempDataDir != "" {
			_ = os.RemoveAll(tempDataDir)
		}
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(dataDir, 0700); err != nil {
		if tempDataDir != "" {
			_ = os.RemoveAll(tempDataDir)
		}
		return nil, fmt.Errorf("secure data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "messages.db")
	sessionPath := filepath.Join(dataDir, "session.json")
	whatsAppSessionPath := filepath.Join(dataDir, "whatsapp-session.db")
	signalConfigPath := filepath.Join(dataDir, "signal-cli")
	for _, path := range []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
		dbPath + "-journal",
		sessionPath,
		sessionPath + ".tmp",
		whatsAppSessionPath,
		whatsAppSessionPath + "-wal",
		whatsAppSessionPath + "-shm",
		whatsAppSessionPath + "-journal",
	} {
		if err := chmodIfExists(path, 0600); err != nil {
			if tempDataDir != "" {
				_ = os.RemoveAll(tempDataDir)
			}
			return nil, fmt.Errorf("secure state file %q: %w", path, err)
		}
	}
	store, err := db.New(dbPath)
	if err != nil {
		if tempDataDir != "" {
			_ = os.RemoveAll(tempDataDir)
		}
		return nil, fmt.Errorf("open db: %w", err)
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := chmodIfExists(path, 0600); err != nil {
			store.Close()
			if tempDataDir != "" {
				_ = os.RemoveAll(tempDataDir)
			}
			return nil, fmt.Errorf("secure database file %q: %w", path, err)
		}
	}
	if runRepairSweeps {
		repairStartupArtifacts(logger, store)
	}

	// Seed demo data
	if DemoMode() {
		if err := store.SeedDemo(); err != nil {
			store.Close()
			if tempDataDir != "" {
				_ = os.RemoveAll(tempDataDir)
			}
			return nil, fmt.Errorf("seed demo data: %w", err)
		}
		logger.Info().
			Str("data_dir", dataDir).
			Str("db", dbPath).
			Msg("Demo mode — using isolated fake data")
	}

	app := &App{
		Store:               store,
		Logger:              logger,
		DataDir:             dataDir,
		SessionPath:         sessionPath,
		WhatsAppSessionPath: whatsAppSessionPath,
		SignalConfigPath:    signalConfigPath,
		tempDataDir:         tempDataDir,
	}
	return app, nil
}

// repairStartupArtifacts runs the write-y startup repair sweeps that clean
// legacy artifacts out of the store. Every sweep is idempotent and
// best-effort: failures are logged, never fatal. Only store-owning
// entrypoints (New) run them; per-session clients (NewClient) must not.
func repairStartupArtifacts(logger zerolog.Logger, store *db.Store) {
	if report, err := store.RepairLegacyArtifacts(); err != nil {
		logger.Warn().Err(err).Msg("Failed to repair legacy message artifacts")
	} else {
		if report.DeletedWhatsAppReactionPlaceholders > 0 {
			logger.Info().
				Int("deleted", report.DeletedWhatsAppReactionPlaceholders).
				Msg("Removed legacy WhatsApp reaction placeholder rows")
		}
		if report.DeletedWhatsAppUnsupportedRows > 0 {
			logger.Info().
				Int("deleted", report.DeletedWhatsAppUnsupportedRows).
				Msg("Removed legacy WhatsApp unsupported placeholder rows")
		}
		if report.DeletedSignalReactionPlaceholders > 0 {
			logger.Info().
				Int("deleted", report.DeletedSignalReactionPlaceholders).
				Msg("Removed legacy Signal reaction placeholder rows")
		}
		if report.FixedSignalBlankMessages > 0 {
			logger.Info().
				Int("fixed", report.FixedSignalBlankMessages).
				Msg("Repaired blank legacy Signal message rows")
		}
		if report.RemainingWhatsAppMediaPlaceholders > 0 {
			logger.Info().
				Int("count", report.RemainingWhatsAppMediaPlaceholders).
				Msg("Legacy WhatsApp media placeholders remain without downloadable metadata")
		}
		if report.FixedGoogleOutgoingAttributionRows > 0 {
			logger.Info().
				Int("fixed", report.FixedGoogleOutgoingAttributionRows).
				Msg("Repaired legacy Google Messages outgoing attribution rows")
		}
	}
	// Drop conversations that a contentless stub (e.g. a group reaction arriving
	// as an empty message in a 1:1 thread) wrongly floated to the top of recents.
	if fixed, err := store.RepairContentlessRecency(); err != nil {
		logger.Warn().Err(err).Msg("Failed to repair contentless conversation recency")
	} else if fixed > 0 {
		logger.Info().Int("fixed", fixed).Msg("Repaired conversations floated up by contentless messages")
	}
	if converted, err := store.RepairTapbacks(); err != nil {
		logger.Warn().Err(err).Msg("Failed to convert legacy iMessage tapbacks to reactions")
	} else if converted > 0 {
		logger.Info().Int("converted", converted).Msg("Converted iMessage tapback texts into reactions")
	}
	if removed, err := store.RepairEmptyStubMessages(); err != nil {
		logger.Warn().Err(err).Msg("Failed to remove empty stub messages")
	} else if removed > 0 {
		logger.Info().Int("removed", removed).Msg("Removed empty stub messages")
	}
	if !Sandboxed() {
		if mediaRepair, err := (&importer.WhatsAppNative{}).RepairLegacyMediaPlaceholders(store); err != nil {
			logger.Warn().Err(err).Msg("Failed to repair legacy WhatsApp media placeholders")
		} else if mediaRepair.MessagesRepaired > 0 {
			logger.Info().
				Int("repaired", mediaRepair.MessagesRepaired).
				Int("skipped", mediaRepair.MessagesSkipped).
				Msg("Repaired legacy WhatsApp media placeholders from local desktop store")
		}
	}
}

func chmodIfExists(path string, mode os.FileMode) error {
	if err := os.Chmod(path, mode); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func LocalIdentityName() string {
	if name := os.Getenv("OPENMESSAGES_MY_NAME"); name != "" {
		return name
	}
	if currentUser, err := user.Current(); err == nil {
		if currentUser.Name != "" {
			return currentUser.Name
		}
		if currentUser.Username != "" {
			return currentUser.Username
		}
	}
	return "Me"
}

func Sandboxed() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("OPENMESSAGES_APP_SANDBOX")), "1") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("OPENMESSAGES_APP_SANDBOX")), "true")
}

func (a *App) GetClient() *client.Client {
	a.clientMu.RLock()
	defer a.clientMu.RUnlock()
	return a.Client
}

func (a *App) setClient(cli *client.Client) {
	a.clientMu.Lock()
	defer a.clientMu.Unlock()
	a.Client = cli
}

func (a *App) LoadAndConnect() error {
	sessionData, err := client.LoadSession(a.SessionPath)
	if err != nil {
		a.setGoogleLastError(err.Error())
		return fmt.Errorf("load session (run 'gmessages-mcp pair' first): %w", err)
	}

	cli, err := client.NewFromSession(sessionData, a.Logger)
	if err != nil {
		a.setGoogleLastError(err.Error())
		return fmt.Errorf("create client: %w", err)
	}
	a.forgetGoogleAccountSwitchForNewSession(cli)
	a.setClient(cli)

	a.EventHandler = &client.EventHandler{
		Store:       a.Store,
		Logger:      a.Logger,
		SessionPath: a.SessionPath,
		Client:      cli,
		OnConversationsChange: func() {
			a.emitConversationsChange()
		},
		OnIncomingMessage: a.OnIncomingMessage,
		OnPendingMedia: func(conversationID, messageID string) {
			a.StartPendingMediaRefresh(conversationID, messageID)
		},
		OnMessagesChange: func(conversationID string) {
			a.emitMessagesChange(conversationID)
		},
		OnTypingChange:           a.OnTypingChange,
		OnGoogleAvatarCandidates: a.QueueGoogleAvatarCandidates,
		OnRealtimeGapRecovered: func(reason string) {
			a.StartRecentReconcile(reason)
		},
		OnPhoneRespondingChange: func(responding bool) {
			a.RecordGooglePhoneResponding(responding)
			if !responding {
				a.setGoogleLastError("Your phone isn't responding to OpenMessage right now; make sure it's on and online.")
			} else {
				a.clearGoogleLastErrorIf("Your phone isn't responding to OpenMessage right now; make sure it's on and online.")
			}
			a.emitStatusChange(a.Connected.Load())
		},
		OnConnectionLost: func() {
			// Transient: keep the session so the reconnect watchdog can
			// recover without a manual re-pair.
			a.Connected.Store(false)
			a.setGoogleLastError("Google Messages connection lost; reconnecting…")
			a.emitStatusChange(false)
			a.Logger.Warn().Msg("Google Messages connection lost; will attempt to reconnect")
		},
		OnAccountChange: func(evt *events.AccountChange) {
			// A replaced client's late event must not describe the new one.
			if a.GetClient() != cli {
				return
			}
			a.applyGoogleAccountChange(cli, evt)
		},
		OnSessionInvalid: func() {
			a.Connected.Store(false)
			a.ClearGoogleAccountSwitch()
			a.setClient(nil)
			if err := os.Remove(a.SessionPath); err != nil && !os.IsNotExist(err) {
				a.Logger.Warn().Err(err).Msg("Failed to remove invalidated Google Messages session")
			}
			a.setGoogleLastError("Google Messages session invalidated; pair again")
			a.emitStatusChange(false)
			a.Logger.Warn().Msg("Disconnected from Google Messages")
		},
	}
	// Wrap the handler so a panic on a malformed event can't kill libgm's
	// single long-poll goroutine (it has no recover() of its own). A dead
	// goroutine would freeze SMS while Connected stayed true — the zombie. On
	// panic, mark disconnected so the reconnect watchdog re-establishes the
	// long-poll.
	cli.GM.SetEventHandler(func(evt any) {
		defer func() {
			if r := recover(); r != nil {
				a.Logger.Error().
					Interface("panic", r).
					Bytes("stack", debug.Stack()).
					Msg("Recovered from panic in Google Messages event handler")
				a.Connected.Store(false)
				a.setGoogleLastError("Google Messages sync interrupted; reconnecting…")
				a.emitStatusChange(false)
			}
		}()
		a.EventHandler.Handle(evt)
	})

	if err := cli.GM.Connect(); err != nil {
		a.setGoogleLastError(err.Error())
		// A 401/UNAUTHENTICATED on connect or token refresh means the session
		// is genuinely dead — the phone unlinked this device. Retrying can't
		// recover it, so flag it for re-pair (the reconnect watchdog backs off
		// on this, and the UI surfaces a "Re-pair" banner) instead of looping
		// "reconnecting…" forever with a bare 401 the user can't act on.
		if isGoogleAuthInvalid(err) {
			a.googleNeedsRepair.Store(true)
		}
		return fmt.Errorf("connect: %w", err)
	}
	// A clean connect proves the session is alive; clear any prior stuck/dead
	// state (also covers the path right after re-pairing).
	a.ClearGoogleRepairFlag()
	a.googleAuthExpired.Store(false)
	a.RecordGooglePhoneResponding(true)
	a.Connected.Store(true)
	a.setGoogleLastError("")
	a.emitStatusChange(true)
	a.Logger.Info().Msg("Connected to Google Messages")
	a.StartGoogleContactSync()
	return nil
}

// isGoogleAuthInvalid reports whether a Google Messages connect/refresh error
// means the stored credentials are dead (re-pair required) rather than a
// transient network failure (reconnect will recover).
func isGoogleAuthInvalid(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "invalid authentication credentials") ||
		strings.Contains(m, "unauthenticated") ||
		strings.Contains(m, "http 401") ||
		(strings.Contains(m, "refresh auth token") && strings.Contains(m, "401"))
}

// Unpair deletes the session file so the app can re-pair.
func (a *App) Unpair() error {
	a.Connected.Store(false)
	a.ClearGoogleAccountSwitch()
	a.setGoogleLastError("")
	a.emitStatusChange(false)
	if cli := a.GetClient(); cli != nil {
		cli.GM.Disconnect()
		a.setClient(nil)
	}
	if err := os.Remove(a.SessionPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove session: %w", err)
	}
	a.Logger.Info().Msg("Unpaired — session deleted")
	return nil
}

// getGMClient returns the GMClient for backfill operations.
// Uses the injected mock if set, otherwise wraps the real libgm client.
func (a *App) getGMClient() GMClient {
	if a.gmClient != nil {
		return a.gmClient
	}
	if cli := a.GetClient(); cli != nil {
		return newRealGMClient(cli.GM)
	}
	return nil
}

func (a *App) backfillClientStillCurrent(token any) bool {
	if token == nil {
		return false
	}
	if a.gmClient != nil {
		return a.gmClient == token
	}
	if cli := a.GetClient(); cli != nil {
		return cli.GM == token
	}
	return false
}

func (a *App) StartDeepBackfill() bool {
	if !a.beginBackfill() {
		return false
	}
	go a.deepBackfill()
	return true
}

func (a *App) StartRecentReconcile(reason string) bool {
	if a.backfillRunning.Load() || !a.reconcileRunning.CompareAndSwap(false, true) {
		return false
	}
	go a.reconcileRecentConversations(reason)
	return true
}

var pendingMediaRefreshSchedule = []time.Duration{
	0,
	2 * time.Second,
	6 * time.Second,
	15 * time.Second,
}

func (a *App) StartPendingMediaRefresh(conversationID, messageID string) bool {
	conversationID = strings.TrimSpace(conversationID)
	messageID = strings.TrimSpace(messageID)
	if conversationID == "" || messageID == "" || a.backfillRunning.Load() {
		return false
	}

	key := conversationID + "|" + messageID
	a.pendingMediaMu.Lock()
	if a.pendingMedia == nil {
		a.pendingMedia = make(map[string]struct{})
	}
	if _, exists := a.pendingMedia[key]; exists {
		a.pendingMediaMu.Unlock()
		return false
	}
	a.pendingMedia[key] = struct{}{}
	a.pendingMediaMu.Unlock()

	go func() {
		defer func() {
			a.pendingMediaMu.Lock()
			delete(a.pendingMedia, key)
			a.pendingMediaMu.Unlock()
		}()
		a.refreshPendingMediaMessageWithSchedule(conversationID, messageID, pendingMediaRefreshSchedule)
	}()
	return true
}

func (a *App) GooglePaired() bool {
	_, err := os.Stat(a.SessionPath)
	return err == nil
}

func (a *App) GoogleStatus() GoogleStatusSnapshot {
	a.statusMu.Lock()
	lastError := a.googleLastError
	a.statusMu.Unlock()
	connected := a.Connected.Load()
	paired := a.GooglePaired()
	snapshot := GoogleStatusSnapshot{
		Connected:    connected,
		Paired:       paired,
		NeedsPairing: !connected && !paired,
		// needs_repair surfaces whenever the session is known-bad and a session
		// file still exists — whether that's a zombie (connected, sends fail)
		// or a dead-credentials disconnect (auth 401). Either way the fix is
		// re-pair, not reconnect; gating on `connected` alone would hide the
		// 401 case (which is disconnected).
		NeedsRepair:     paired && a.googleNeedsRepair.Load(),
		LastError:       lastError,
		AuthExpired:     a.googleAuthExpired.Load(),
		PhoneResponding: a.GooglePhoneResponding(),
		RepairsPaced:    a.GoogleRepairsPaced(),
		PullHealth:      a.GooglePullHealth(),
	}
	// The account switch is reported independently of needs_repair: the
	// session is not broken (inbound sync keeps working), the phone just
	// refuses its requests until it is re-linked or switched back.
	if switchState := a.currentGoogleAccountSwitch(); paired && switchState.switched {
		snapshot.AccountSwitched = true
		snapshot.SwitchedAccount = switchState.account
		snapshot.AccountSwitchedAtMS = switchState.at.UnixMilli()
	}
	return snapshot
}

// SetGoogleRepairPaceCounter installs the supervisor's paced-repair counter so
// status surfaces can report it. Only the transport-owning daemon builds a
// credential repairer; everyone else (client mode, demo) reports 0.
func (a *App) SetGoogleRepairPaceCounter(count func() uint64) {
	a.googleRepairPaceMu.Lock()
	a.googleRepairPaceCount = count
	a.googleRepairPaceMu.Unlock()
}

// GoogleRepairsPaced reports how many automatic Google credential repairs have
// been delayed by the minimum-repair-interval floor. See
// GoogleStatusSnapshot.RepairsPaced.
func (a *App) GoogleRepairsPaced() uint64 {
	a.googleRepairPaceMu.RLock()
	count := a.googleRepairPaceCount
	a.googleRepairPaceMu.RUnlock()
	if count == nil {
		return 0
	}
	return count()
}

func (a *App) AnyConnected() bool {
	if a.Connected.Load() {
		return true
	}
	if a.WhatsAppStatus().Connected {
		return true
	}
	if a.SignalStatus().Connected {
		return true
	}
	return false
}

func (a *App) ReconnectGoogleMessages() error {
	if a.Connected.Load() && a.GetClient() != nil {
		a.googleAuthExpired.Store(false)
		a.setGoogleLastError("")
		return nil
	}
	if cli := a.GetClient(); cli != nil {
		cli.GM.Disconnect()
		a.setClient(nil)
	}
	return a.LoadAndConnect()
}

func (a *App) setGoogleLastError(message string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	a.googleLastError = strings.TrimSpace(message)
}

func (a *App) clearGoogleLastErrorIf(message string) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if a.googleLastError == strings.TrimSpace(message) {
		a.googleLastError = ""
	}
}

func (a *App) beginBackfill() bool {
	return a.backfillRunning.CompareAndSwap(false, true)
}

func (a *App) endBackfill() {
	a.backfillRunning.Store(false)
}

func (a *App) emitConversationsChange() {
	if a.OnConversationsChange != nil {
		a.OnConversationsChange()
	}
}

func (a *App) emitMessagesChange(conversationID string) {
	if a.OnMessagesChange != nil {
		a.OnMessagesChange(conversationID)
	}
}

func (a *App) emitStatusChange(connected bool) {
	if a.OnStatusChange != nil {
		a.OnStatusChange(connected)
	}
}

func (a *App) IsDeepBackfillRunning() bool {
	return a.backfillRunning.Load()
}

// GetBackfillProgress returns a snapshot of the current backfill progress.
func (a *App) GetBackfillProgress() BackfillSnapshot {
	snap := a.BackfillProgress.snapshot()
	// A shallow Backfill (startup catch-up) holds the same mutual-exclusion
	// guard (backfillRunning) without populating the deep-backfill progress
	// struct. Reflect the guard here so status never reports "idle" while a
	// sync is actually running — otherwise a concurrent deep-backfill request
	// is rejected as "already running" while this status shows nothing going
	// on, which looks like a phantom/zombie state.
	if a.backfillRunning.Load() {
		snap.Running = true
	}
	return snap
}

func (a *App) Close() {
	a.StopGoogleAvatarSync()
	if cli := a.GetClient(); cli != nil {
		cli.GM.Disconnect()
	}
	if signal := a.GetSignal(); signal != nil {
		if err := signal.Close(); err != nil {
			a.Logger.Warn().Err(err).Msg("Failed to close Signal bridge")
		}
	}
	if wa := a.GetWhatsApp(); wa != nil {
		if err := wa.Close(); err != nil {
			a.Logger.Warn().Err(err).Msg("Failed to close WhatsApp bridge")
		}
	}
	if a.Store != nil {
		a.Store.Close()
	}
	if a.tempDataDir != "" {
		if err := os.RemoveAll(a.tempDataDir); err != nil {
			a.Logger.Warn().Err(err).Str("dir", a.tempDataDir).Msg("Failed to remove demo temp data dir")
		}
	}
}
