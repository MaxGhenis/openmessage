package whatsapplive

import (
	"context"
	"database/sql"
	"fmt"
	"time"

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
}

// loggedOutState is the durable record that WhatsApp logged this install's
// linked device out. JID is the identity it revoked; an empty JID means no
// logout is outstanding.
type loggedOutState struct {
	At     time.Time
	Reason string
	JID    string
}

func (s loggedOutState) active() bool { return s.JID != "" }

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

// isLoggedOutIdentity reports whether jid is the identity an outstanding logout
// revoked.
func (b *Bridge) isLoggedOutIdentity(jid watypes.JID) bool {
	if jid.IsEmpty() {
		return false
	}
	state := b.loggedOutSnapshot()
	return state.active() && state.JID == jid.String()
}

// recordLoggedOut latches the first logout of identity jid. Repeats for the
// same identity keep the original time and reason, so the episode keeps one
// identity (relink_since_ms) until a new link or an unpair clears it. The
// record is written before it is published; if the write fails it still holds
// in memory for this process, and the failure is only logged because the
// lifecycle is already parked for reauthentication either way.
func (b *Bridge) recordLoggedOut(at time.Time, reason string, jid watypes.JID) {
	b.runtimeMu.Lock()
	defer b.runtimeMu.Unlock()
	if b.loggedOut.active() && b.loggedOut.JID == jid.String() {
		return
	}
	b.loggedOut = loggedOutState{At: at, Reason: reason, JID: jid.String()}
	if err := b.persistLoggedOutLocked(); err != nil {
		b.logger.Warn().Err(err).Msg("Failed to persist the WhatsApp logout; it holds until this process exits")
	}
	b.logger.Warn().Str("reason", reason).Msg("WhatsApp logged this linked device out; relink required")
}

// clearLoggedOut ends an outstanding logout: a new link succeeded, or the user
// unpaired on purpose.
func (b *Bridge) clearLoggedOut() {
	b.runtimeMu.Lock()
	defer b.runtimeMu.Unlock()
	if !b.loggedOut.active() {
		return
	}
	b.loggedOut = loggedOutState{}
	if err := b.persistLoggedOutLocked(); err != nil {
		b.logger.Warn().Err(err).Msg("Failed to clear the persisted WhatsApp logout")
	}
}

func (b *Bridge) persistLoggedOutLocked() error {
	if b.runtimeDB == nil {
		return nil
	}
	var atNS int64
	if b.loggedOut.active() {
		atNS = b.loggedOut.At.UnixNano()
	}
	if _, err := b.runtimeDB.ExecContext(
		context.Background(),
		`UPDATE openmessage_whatsapp_runtime_state
		 SET logged_out_at_ns = ?, logged_out_reason = ?, logged_out_jid = ?
		 WHERE singleton = 1`,
		atNS,
		b.loggedOut.Reason,
		b.loggedOut.JID,
	); err != nil {
		return fmt.Errorf("persist WhatsApp logout: %w", err)
	}
	return nil
}

func loadLoggedOutState(atNS int64, reason, jid string) loggedOutState {
	if jid == "" {
		return loggedOutState{}
	}
	state := loggedOutState{Reason: reason, JID: jid}
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

// purgeLoggedOutIdentityLocked deletes the session store row of the identity
// an outstanding logout revoked, if it is still there, and replaces a client
// that still holds that identity with a fresh unpaired one. It is a no-op when
// no logout is outstanding, and idempotent otherwise. Caller holds b.mu.
//
// whatsmeow deletes the session itself after a logout (connectionevents.go:
// handleConnectFailure and the device_removed branch of handleStreamError),
// but it first dispatches LoggedOut on a goroutine and then deletes with the
// connection's event context, which is the context ConnectPaired was given.
// The lifecycle adapter cancels that context as soon as LoggedOut reaches it,
// and the SQLite driver interrupts a statement whose context ends, so the
// delete can lose that race and leave the revoked identity in place. The
// revoked identity then reads as paired, the supervisor refuses to reconnect
// it, and QR pairing refuses a store that still holds an identity: the
// 2026-08-20 state, cleared only by an unpair eight days later. This purge
// runs with its own bounded context and deletes only the revoked JID, so a
// newer link is never touched.
func (b *Bridge) purgeLoggedOutIdentityLocked() (bool, error) {
	state := b.loggedOutSnapshot()
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
	if device != nil {
		if err := b.container.DeleteDevice(ctx, device); err != nil {
			return false, fmt.Errorf("delete logged-out WhatsApp identity: %w", err)
		}
		removed = true
		b.logger.Warn().Msg("Removed the logged-out WhatsApp identity that whatsmeow's own delete left behind")
	}
	if b.identityJID.String() == state.JID {
		// The current client still holds the revoked identity, in memory if not
		// on disk. Install a fresh unpaired client (the same reset Unpair uses).
		if err := b.resetClientLocked(); err != nil {
			return removed, fmt.Errorf("reset WhatsApp client after logout: %w", err)
		}
	}
	return removed, nil
}
