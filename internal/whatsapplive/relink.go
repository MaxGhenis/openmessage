package whatsapplive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	wastore "go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
)

// loggedOutPurgeTimeout bounds the lookup and delete that remove an identity
// WhatsApp logged out. It is independent of any connection context on purpose.
const loggedOutPurgeTimeout = 15 * time.Second

// whatsappRuntimeStateColumns were added to openmessage_whatsapp_runtime_state
// after the table first shipped. CREATE TABLE IF NOT EXISTS leaves an existing
// table as it was, so ensureRuntimeStateColumns adds each one that is missing.
// A binary that predates them reads only blocked_until_ns and ignores them.
var whatsappRuntimeStateColumns = []struct{ name, definition string }{
	{"logged_out_at_ns", "INTEGER NOT NULL DEFAULT 0"},
	{"logged_out_reason", "TEXT NOT NULL DEFAULT ''"},
	{"logged_out_jid", "TEXT NOT NULL DEFAULT ''"},
	{"logged_out_key", "TEXT NOT NULL DEFAULT ''"},
}

// linkedIdentity is a linked device as the bridge recorded it: its JID and a
// fingerprint of its identity key. A new link always generates a new identity
// key (whatsmeow's NewDevice), so the pair names one link even if WhatsApp
// hands the same JID out again.
type linkedIdentity struct {
	JID watypes.JID
	Key string
}

func (id linkedIdentity) empty() bool { return id.JID.IsEmpty() }

// identityOf reads a device's identity. The caller must hold the only
// reference to device or know that nothing writes its ID concurrently:
// whatsmeow's Store.Delete sets ID to nil from its own goroutine.
func identityOf(device *wastore.Device) linkedIdentity {
	if device == nil || device.ID == nil {
		return linkedIdentity{}
	}
	return linkedIdentity{JID: *device.ID, Key: identityKeyFingerprint(device)}
}

// identityKeyFingerprint names a device's identity key, which whatsmeow fixes
// when it creates the device and never rewrites.
func identityKeyFingerprint(device *wastore.Device) string {
	if device == nil || device.IdentityKey == nil {
		return ""
	}
	sum := sha256.Sum256(device.IdentityKey.Pub[:])
	return hex.EncodeToString(sum[:16])
}

// storedIdentity is one device row of the session store, read into a device
// struct that nothing else holds.
type storedIdentity struct {
	identity    linkedIdentity
	pushName    string
	identityKey []byte // the stored private identity key, for an exact delete
}

// loggedOutState is the durable record that WhatsApp logged this install's
// linked device out. JID and Key name the identity it revoked; an empty JID
// means no logout is outstanding.
type loggedOutState struct {
	At     time.Time
	Reason string
	JID    string
	Key    string
}

func (s loggedOutState) active() bool { return s.JID != "" }

// revokes reports whether identity is the one this logout revoked.
func (s loggedOutState) revokes(identity linkedIdentity) bool {
	return s.active() && !identity.empty() && s.JID == identity.JID.String() && s.Key == identity.Key
}

// same reports whether two records name the same episode.
func (s loggedOutState) same(other loggedOutState) bool {
	return s.JID == other.JID && s.Key == other.Key && s.Reason == other.Reason &&
		s.At.UnixNano() == other.At.UnixNano()
}

// Test seams.
var (
	// writeLoggedOutRecord stores an episode unconditionally.
	writeLoggedOutRecord = func(ctx context.Context, database *sql.DB, state loggedOutState) error {
		_, err := database.ExecContext(
			ctx,
			`UPDATE openmessage_whatsapp_runtime_state
			 SET logged_out_at_ns = ?, logged_out_reason = ?, logged_out_jid = ?, logged_out_key = ?
			 WHERE singleton = 1`,
			state.At.UnixNano(), state.Reason, state.JID, state.Key,
		)
		return err
	}
	// clearLoggedOutRecord clears the stored record only if it still holds
	// episode, and reports whether it did.
	clearLoggedOutRecord = func(ctx context.Context, database *sql.DB, episode loggedOutState) (bool, error) {
		result, err := database.ExecContext(
			ctx,
			`UPDATE openmessage_whatsapp_runtime_state
			 SET logged_out_at_ns = 0, logged_out_reason = '', logged_out_jid = '', logged_out_key = ''
			 WHERE singleton = 1 AND logged_out_jid = ? AND logged_out_key = ? AND logged_out_at_ns = ?`,
			episode.JID, episode.Key, episode.At.UnixNano(),
		)
		if err != nil {
			return false, err
		}
		rows, err := result.RowsAffected()
		return rows > 0, err
	}
	// deleteRevokedDevice deletes a device row only if it still holds the
	// identity key that was checked, so a row another process replaced in the
	// meantime survives. Foreign keys cascade the device's sessions and keys,
	// as whatsmeow's own Container.DeleteDevice does.
	deleteRevokedDevice = func(ctx context.Context, database *sql.DB, jid string, identityKey []byte) (int64, error) {
		result, err := database.ExecContext(
			ctx,
			`DELETE FROM whatsmeow_device WHERE jid = ? AND identity_key = ?`,
			jid, identityKey,
		)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}
	// testHookAfterAdmission runs after handleClientEvent's first generation
	// check, before the event changes any state.
	testHookAfterAdmission func(raw any)
	// testHookBeforeRevokedDelete runs between the purge's check of the stored
	// row and its delete.
	testHookBeforeRevokedDelete func()
)

func ensureRuntimeStateColumns(ctx context.Context, database *sql.DB) error {
	existing, err := runtimeStateColumnNames(ctx, database)
	if err != nil {
		return err
	}
	for _, column := range whatsappRuntimeStateColumns {
		if existing[column.name] {
			continue
		}
		statement := "ALTER TABLE openmessage_whatsapp_runtime_state ADD COLUMN " + column.name + " " + column.definition
		if _, err := database.ExecContext(ctx, statement); err != nil {
			// Another process opening the same session store may have added the
			// column between the read above and this ALTER.
			again, readErr := runtimeStateColumnNames(ctx, database)
			if readErr == nil && again[column.name] {
				continue
			}
			return fmt.Errorf("add WhatsApp runtime state column %s: %w", column.name, err)
		}
	}
	return nil
}

func runtimeStateColumnNames(ctx context.Context, database *sql.DB) (map[string]bool, error) {
	rows, err := database.QueryContext(ctx, `SELECT name FROM pragma_table_info('openmessage_whatsapp_runtime_state')`)
	if err != nil {
		return nil, fmt.Errorf("read WhatsApp runtime state columns: %w", err)
	}
	defer rows.Close()
	names := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("read WhatsApp runtime state columns: %w", err)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read WhatsApp runtime state columns: %w", err)
	}
	return names, nil
}

func readLoggedOutRecord(ctx context.Context, database *sql.DB) (loggedOutState, error) {
	var (
		atNS   int64
		reason string
		jid    string
		key    string
	)
	if err := database.QueryRowContext(
		ctx,
		`SELECT logged_out_at_ns, logged_out_reason, logged_out_jid, logged_out_key
		 FROM openmessage_whatsapp_runtime_state WHERE singleton = 1`,
	).Scan(&atNS, &reason, &jid, &key); err != nil {
		return loggedOutState{}, fmt.Errorf("read WhatsApp logout record: %w", err)
	}
	return loadLoggedOutState(atNS, reason, jid, key), nil
}

func loadLoggedOutState(atNS int64, reason, jid, key string) loggedOutState {
	if jid == "" {
		return loggedOutState{}
	}
	state := loggedOutState{Reason: reason, JID: jid, Key: key}
	if atNS > 0 {
		state.At = time.Unix(0, atNS)
	}
	return state
}

// loggedOutEventReason reports whether raw is WhatsApp revoking the linked
// device, and how whatsmeow describes it. These are the events for which
// whatsmeow itself deletes the session (ConnectFailureReason.IsLoggedOut: 401
// logged out from another device, including the device_removed stream error,
// 403 primary device logged out, 406 unknown logout). whatsmeow emits
// ConnectFailure only for reasons it does not recognize, so the ConnectFailure
// case is defensive; it matches the adapter's reauthentication classification.
// Temporary bans, an outdated client and a replaced stream are not logouts:
// relinking would not help them.
func loggedOutEventReason(raw any) (string, bool) {
	switch evt := raw.(type) {
	case *waevents.LoggedOut:
		if evt == nil {
			return "", false
		}
		return evt.PermanentDisconnectDescription(), true
	case *waevents.ConnectFailure:
		if evt == nil || !evt.Reason.IsLoggedOut() {
			return "", false
		}
		return evt.PermanentDisconnectDescription(), true
	}
	return "", false
}

func (b *Bridge) loggedOutSnapshot() loggedOutState {
	b.runtimeMu.Lock()
	defer b.runtimeMu.Unlock()
	return b.loggedOut
}

// recordLoggedOut latches the first logout of identity. A repeat for the same
// identity keeps the original time and reason, so the episode keeps one
// identity (relink_since_ms) until a new link or an unpair ends it. The record
// is written before it is published. If the write fails it still holds in
// memory, and the next record, clear or purge retries the write. Callers hold
// b.mu, so it is ordered against Unpair and client replacement.
func (b *Bridge) recordLoggedOut(at time.Time, reason string, identity linkedIdentity) {
	b.runtimeMu.Lock()
	defer b.runtimeMu.Unlock()
	if !b.loggedOut.revokes(identity) {
		b.loggedOut = loggedOutState{At: at, Reason: reason, JID: identity.JID.String(), Key: identity.Key}
		b.logger.Warn().Str("reason", reason).Msg("WhatsApp logged this linked device out; relink required")
	}
	_ = b.flushLoggedOutLocked()
}

// clearLoggedOut ends an outstanding logout: a new link succeeded, a newer
// link superseded it, or the user unpaired on purpose. It reports whether the
// clear is durable.
func (b *Bridge) clearLoggedOut() error {
	b.runtimeMu.Lock()
	defer b.runtimeMu.Unlock()
	b.loggedOut = loggedOutState{}
	return b.flushLoggedOutLocked()
}

// flushLoggedOutLocked makes the stored record match the in-memory one. Only
// the process that owns the live session writes it. A record is written
// outright; a clear is a compare-and-swap against the episode last known to
// be stored, so it can never erase a different episode another writer stored
// meanwhile, which is reloaded instead. A failed write leaves the difference
// for the next call to retry. Caller holds runtimeMu.
func (b *Bridge) flushLoggedOutLocked() error {
	if !b.liveSessionOwner || b.runtimeDB == nil || b.loggedOut.same(b.storedLoggedOut) {
		return nil
	}
	ctx := context.Background()
	if b.loggedOut.active() {
		if err := writeLoggedOutRecord(ctx, b.runtimeDB, b.loggedOut); err != nil {
			b.logger.Warn().Err(err).Msg("Failed to persist the WhatsApp logout record; retrying on the next change")
			return fmt.Errorf("persist WhatsApp logout record: %w", err)
		}
		b.storedLoggedOut = b.loggedOut
		return nil
	}
	if !b.storedLoggedOut.active() {
		return nil
	}
	cleared, err := clearLoggedOutRecord(ctx, b.runtimeDB, b.storedLoggedOut)
	if err != nil {
		b.logger.Warn().Err(err).Msg("Failed to clear the persisted WhatsApp logout record; retrying on the next change")
		return fmt.Errorf("clear WhatsApp logout record: %w", err)
	}
	if cleared {
		b.storedLoggedOut = loggedOutState{}
		return nil
	}
	// The stored record is no longer the episode this clear was for.
	current, err := readLoggedOutRecord(ctx, b.runtimeDB)
	if err != nil {
		return err
	}
	b.logger.Warn().Msg("The stored WhatsApp logout record changed under this process; adopting it")
	b.loggedOut, b.storedLoggedOut = current, current
	return errors.New("the stored WhatsApp logout record changed; adopted the newer one")
}

// refreshLoggedOutLocked rereads the record in a process that does not own
// the live session, so its status follows the owner's writes. Caller holds
// b.mu.
func (b *Bridge) refreshLoggedOutLocked() {
	b.runtimeMu.Lock()
	defer b.runtimeMu.Unlock()
	if b.liveSessionOwner || b.runtimeDB == nil {
		return
	}
	if current, err := readLoggedOutRecord(context.Background(), b.runtimeDB); err == nil {
		b.loggedOut, b.storedLoggedOut = current, current
	}
}

// storedIdentitiesLocked reads every device row of the session store into
// device structs that nothing else holds. Caller holds b.mu.
func (b *Bridge) storedIdentitiesLocked() []storedIdentity {
	if b.container == nil {
		return nil
	}
	devices, err := b.container.GetAllDevices(context.Background())
	if err != nil {
		b.logger.Debug().Err(err).Msg("Failed to inspect persisted WhatsApp session state")
		return nil
	}
	stored := make([]storedIdentity, 0, len(devices))
	for _, device := range devices {
		identity := identityOf(device)
		if identity.empty() {
			continue
		}
		entry := storedIdentity{identity: identity, pushName: device.PushName}
		if device.IdentityKey != nil && device.IdentityKey.Priv != nil {
			entry.identityKey = append([]byte(nil), device.IdentityKey.Priv[:]...)
		}
		stored = append(stored, entry)
	}
	return stored
}

// storesIdentityLocked reports whether the session store holds identity.
// Caller holds b.mu.
func (b *Bridge) storesIdentityLocked(identity linkedIdentity) bool {
	for _, stored := range b.storedIdentitiesLocked() {
		if stored.identity == identity {
			return true
		}
	}
	return false
}

// storedLinkLocked returns the first stored identity the logout record does
// not revoke: a link WhatsApp still accepts. Caller holds b.mu.
func (b *Bridge) storedLinkLocked(loggedOut loggedOutState) (storedIdentity, bool) {
	for _, stored := range b.storedIdentitiesLocked() {
		if !loggedOut.revokes(stored.identity) {
			return stored, true
		}
	}
	return storedIdentity{}, false
}

// ClaimLiveSession marks this bridge as the one process that owns the live
// WhatsApp session: the daemon with transports on. Only the owner repairs
// session state other processes share: it writes and clears the logout
// record and deletes a revoked identity. Any other process that opens the
// same session store (serve --no-transports, for one) only reads it, rereading
// the record on every status. The owner reconciles an outstanding logout at
// once.
func (b *Bridge) ClaimLiveSession() {
	b.mu.Lock()
	b.runtimeMu.Lock()
	b.liveSessionOwner = true
	b.runtimeMu.Unlock()
	_, err := b.purgeLoggedOutIdentityLocked()
	b.mu.Unlock()
	if err != nil {
		b.logger.Warn().Err(err).Msg("Failed to remove the logged-out WhatsApp identity; the next connect or pair attempt retries")
	}
	b.emitStatusChange()
}

// purgeLoggedOutIdentity removes the identity an outstanding logout revoked
// and publishes the resulting status.
func (b *Bridge) purgeLoggedOutIdentity() {
	b.mu.Lock()
	_, err := b.purgeLoggedOutIdentityLocked()
	b.mu.Unlock()
	if err != nil {
		b.logger.Warn().Err(err).Msg("Failed to remove the logged-out WhatsApp identity; the next connect or pair attempt retries")
	}
	b.emitStatusChange()
}

// purgeLoggedOutIdentityLocked reconciles an outstanding logout with the
// session store, in the process that owns the live session only. It deletes
// the revoked identity's device row if it is still there, replaces a client
// that still holds that identity with a fresh one from the store, and drops
// the record once a newer link is stored. It is a no-op when no logout is
// outstanding, and idempotent otherwise. Caller holds b.mu. It runs after the
// logout event, when the owner claims the session, and at every connect and
// pair entry point.
//
// whatsmeow deletes the session itself after a logout (connectionevents.go:
// handleConnectFailure and the device_removed branch of handleStreamError),
// but it first dispatches LoggedOut on a goroutine and then deletes with the
// connection's event context, which is the context ConnectPaired was given.
// Once LoggedOut reaches the lifecycle adapter its coordinator cancels that
// context, and the SQLite driver interrupts a statement whose context ends, so
// the delete can lose that race and leave the revoked identity in place. The
// revoked identity then reads as paired, the supervisor refuses to reconnect
// it, and QR pairing refuses a store that still holds an identity: the
// 2026-08-20 state, cleared only by an unpair eight days later. This purge
// runs with its own bounded context, and its delete matches the JID and the
// identity key checked a moment before in one statement, so a newer link is
// never deleted, even one stored under the same JID meanwhile.
func (b *Bridge) purgeLoggedOutIdentityLocked() (bool, error) {
	// A record write that failed earlier is retried here; its failure is
	// logged and must not block connecting or pairing.
	b.runtimeMu.Lock()
	owner := b.liveSessionOwner
	_ = b.flushLoggedOutLocked()
	state := b.loggedOut
	b.runtimeMu.Unlock()
	if !owner || !state.active() || b.runtimeDB == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), loggedOutPurgeTimeout)
	defer cancel()
	removed := false
	for _, stored := range b.storedIdentitiesLocked() {
		if !state.revokes(stored.identity) || len(stored.identityKey) == 0 {
			continue
		}
		if testHookBeforeRevokedDelete != nil {
			testHookBeforeRevokedDelete()
		}
		deleted, err := deleteRevokedDevice(ctx, b.runtimeDB, state.JID, stored.identityKey)
		if err != nil {
			return false, fmt.Errorf("delete logged-out WhatsApp identity: %w", err)
		}
		if deleted > 0 {
			removed = true
			b.logger.Warn().Msg("Removed the logged-out WhatsApp identity that whatsmeow's own delete left behind")
		}
	}
	if state.revokes(b.identity) || b.loggedOutGeneration == b.clientGeneration {
		// The current client still holds the revoked identity, in memory if not
		// on disk, or was logged out before the bridge recorded what it paired.
		// Install a fresh client from the store (the reset Unpair uses).
		if err := b.resetClientLocked(); err != nil {
			return removed, fmt.Errorf("reset WhatsApp client after logout: %w", err)
		}
	}
	// A link stored since the logout supersedes the record: a pairing whose
	// PairSuccess never ran (the process died between whatsmeow saving it and
	// dispatching the event), a relink by a binary that predates the record,
	// or a clear whose write failed.
	if _, ok := b.storedLinkLocked(state); ok {
		_ = b.clearLoggedOut()
	}
	return removed, nil
}
