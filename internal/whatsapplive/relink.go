package whatsapplive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
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

// Test seams: the purge's device delete and the record's persistence.
var (
	deleteLoggedOutDevice = func(ctx context.Context, container *sqlstore.Container, device *wastore.Device) error {
		return container.DeleteDevice(ctx, device)
	}
	writeLoggedOutState = func(ctx context.Context, database *sql.DB, state loggedOutState) error {
		var atNS int64
		if state.active() {
			atNS = state.At.UnixNano()
		}
		_, err := database.ExecContext(
			ctx,
			`UPDATE openmessage_whatsapp_runtime_state
			 SET logged_out_at_ns = ?, logged_out_reason = ?, logged_out_jid = ?, logged_out_key = ?
			 WHERE singleton = 1`,
			atNS, state.Reason, state.JID, state.Key,
		)
		return err
	}
	// testHookBeforeLogoutRecord runs between handleClientEvent's first
	// generation check and the locked one that records a logout.
	testHookBeforeLogoutRecord func()
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
// memory, and the next record, clear or purge retries the write.
// handleClientEvent calls this with b.mu held, so it is ordered against
// Unpair and client replacement.
func (b *Bridge) recordLoggedOut(at time.Time, reason string, identity linkedIdentity) {
	b.runtimeMu.Lock()
	defer b.runtimeMu.Unlock()
	if b.loggedOut.revokes(identity) {
		b.flushLoggedOutLocked()
		return
	}
	b.loggedOut = loggedOutState{At: at, Reason: reason, JID: identity.JID.String(), Key: identity.Key}
	b.loggedOutDirty = true
	b.flushLoggedOutLocked()
	b.logger.Warn().Str("reason", reason).Msg("WhatsApp logged this linked device out; relink required")
}

// clearLoggedOut ends an outstanding logout: a new link succeeded, a newer
// link superseded it, or the user unpaired on purpose. It reports whether the
// clear is durable.
func (b *Bridge) clearLoggedOut() error {
	b.runtimeMu.Lock()
	defer b.runtimeMu.Unlock()
	if b.loggedOut.active() {
		b.loggedOut = loggedOutState{}
		b.loggedOutDirty = true
	}
	return b.flushLoggedOutLocked()
}

// flushLoggedOutLocked writes the in-memory record if an earlier write did not
// land. Caller holds runtimeMu.
func (b *Bridge) flushLoggedOutLocked() error {
	if !b.loggedOutDirty || b.runtimeDB == nil {
		return nil
	}
	if err := writeLoggedOutState(context.Background(), b.runtimeDB, b.loggedOut); err != nil {
		b.logger.Warn().Err(err).Bool("logged_out", b.loggedOut.active()).Msg("Failed to persist the WhatsApp logout record; retrying on the next change")
		return fmt.Errorf("persist WhatsApp logout record: %w", err)
	}
	b.loggedOutDirty = false
	return nil
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
// session store. It deletes the revoked identity's device row if it is still
// there, replaces a client that still holds that identity with a fresh
// unpaired one, and drops the record once a newer link is stored. It is a
// no-op when no logout is outstanding, and idempotent otherwise. Caller holds
// b.mu. It runs after the logout event, at every connect and pair entry point,
// and when the bridge is built.
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
// runs with its own bounded context and deletes a row only if both its JID
// and its identity key match the revoked identity, so a newer link is never
// deleted, even one WhatsApp gave the same JID.
func (b *Bridge) purgeLoggedOutIdentityLocked() (bool, error) {
	// A record write that failed earlier is retried here; its failure is
	// logged by the flush and must not block connecting or pairing.
	b.runtimeMu.Lock()
	_ = b.flushLoggedOutLocked()
	state := b.loggedOut
	b.runtimeMu.Unlock()
	if !state.active() || b.container == nil {
		return false, nil
	}
	jid, err := watypes.ParseJID(state.JID)
	if err != nil {
		return false, fmt.Errorf("parse logged-out WhatsApp identity: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), loggedOutPurgeTimeout)
	defer cancel()
	device, err := b.container.GetDevice(ctx, jid)
	if err != nil {
		return false, fmt.Errorf("look up logged-out WhatsApp identity: %w", err)
	}
	removed := false
	if device != nil && state.revokes(identityOf(device)) {
		if err := deleteLoggedOutDevice(ctx, b.container, device); err != nil {
			return false, fmt.Errorf("delete logged-out WhatsApp identity: %w", err)
		}
		removed = true
		b.logger.Warn().Msg("Removed the logged-out WhatsApp identity that whatsmeow's own delete left behind")
	}
	if state.revokes(b.identity) {
		// The current client still holds the revoked identity, in memory if not
		// on disk. Install a fresh client from the store (the reset Unpair uses).
		if err := b.resetClientLocked(); err != nil {
			return removed, fmt.Errorf("reset WhatsApp client after logout: %w", err)
		}
	}
	// A link stored since the logout supersedes the record: a pairing whose
	// PairSuccess never ran (the process died between whatsmeow saving it and
	// dispatching the event), a relink by a binary that predates the record,
	// or a clear whose write failed.
	if linked, _ := b.linkedIdentityLocked(); !linked.empty() && !state.revokes(linked) {
		_ = b.clearLoggedOut()
	}
	return removed, nil
}
