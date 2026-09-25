package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/maxghenis/openmessage/internal/contactsync"
)

type ContactSyncStatus struct {
	Source                 string `json:"source"`
	Running                bool   `json:"running"`
	Complete               bool   `json:"complete"`
	LastAttemptMS          int64  `json:"last_attempt_ms"`
	LastSuccessMS          int64  `json:"last_success_ms"`
	People                 int    `json:"people"`
	PhoneEntries           int    `json:"phone_entries"`
	ThreadsUpdated         int    `json:"threads_updated"`
	RefreshIntervalSeconds int    `json:"refresh_interval_seconds"`
	LastError              string `json:"last_error,omitempty"`
}
type contactDirectoryState struct {
	mu     sync.Mutex
	workMu sync.Mutex
	wg     sync.WaitGroup
	cancel context.CancelFunc
	ctx    context.Context
	closed bool
	status ContactSyncStatus
}

func googleContactsMCPURL() string {
	return strings.TrimSpace(os.Getenv("OPENMESSAGES_GOOGLE_CONTACTS_MCP_URL"))
}
func contactRefreshInterval() time.Duration {
	if v, err := time.ParseDuration(strings.TrimSpace(os.Getenv("OPENMESSAGES_CONTACTS_REFRESH_INTERVAL"))); err == nil && v >= time.Minute && v <= 24*time.Hour {
		return v
	}
	return 5 * time.Minute
}
func (a *App) GetContactSyncStatus() ContactSyncStatus {
	d := &a.contactDirectory
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.status
	if a.ContactDirectoryDisabled {
		s.Source = "disabled"
		return s
	}
	if s.Source == "" {
		s.Source = "google_messages_suggestions"
		if googleContactsMCPURL() != "" {
			s.Source = "google_contacts_mcp"
		}
	}
	if s.Source == "google_contacts_mcp" {
		s.RefreshIntervalSeconds = int(contactRefreshInterval() / time.Second)
	}
	return s
}

// StartContactDirectorySync is daemon-only and independent of phone connectivity.
// Reconnects cannot create additional workers. Close cancels and joins requests.
func (a *App) StartContactDirectorySync() {
	if a == nil || a.ContactDirectoryDisabled || googleContactsMCPURL() == "" {
		return
	}
	d := &a.contactDirectory
	d.mu.Lock()
	if d.closed || d.cancel != nil {
		d.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.ctx = ctx
	d.cancel = cancel
	d.wg.Add(1)
	d.mu.Unlock()
	go func() {
		defer d.wg.Done()
		runContactRefreshLoop(ctx, contactRefreshInterval(), func() {
			if _, err := a.syncContactDirectory(ctx); err != nil && ctx.Err() == nil {
				a.Logger.Warn().Msg("Google Contacts directory refresh failed; check contact_sync status")
			}
		})
	}()
}
func runContactRefreshLoop(ctx context.Context, interval time.Duration, refresh func()) {
	refresh()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}
func (a *App) stopContactDirectorySync() {
	d := &a.contactDirectory
	d.mu.Lock()
	d.closed = true
	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Unlock()
	d.wg.Wait()
	// A manual refresh can be in flight even before the periodic worker starts.
	d.workMu.Lock()
	d.workMu.Unlock()
}
func (a *App) syncContactDirectory(parent context.Context) (int, error) {
	if a.ContactDirectoryDisabled {
		return 0, errors.New("contact directory sync requires a legacy daemon")
	}
	d := &a.contactDirectory
	d.workMu.Lock()
	defer d.workMu.Unlock()
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return 0, errors.New("contact sync is stopped")
	}
	// Tie manual requests to daemon shutdown as well as their timeout.
	if d.ctx != nil {
		parent = d.ctx
	}
	d.status.Source = "google_contacts_mcp"
	d.status.Running = true
	d.status.LastAttemptMS = time.Now().UnixMilli()
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	people, err := contactsync.Fetch(ctx, googleContactsMCPURL(), strings.TrimSpace(os.Getenv("OPENMESSAGES_GOOGLE_CONTACTS_MCP_TOKEN_FILE")))
	phones, changed := 0, 0
	if err == nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			phones, changed, err = a.Store.ReplaceContactDirectory(people)
		}
	}
	d.mu.Lock()
	d.status.Running = false
	if err != nil {
		d.status.LastError = "Contact directory refresh failed; verify connector availability and authorization, then retry"
	} else {
		d.status.LastSuccessMS = time.Now().UnixMilli()
		d.status.Complete = true
		d.status.People = len(people)
		d.status.PhoneEntries = phones
		d.status.ThreadsUpdated = changed
		d.status.LastError = ""
	}
	d.mu.Unlock()
	if err != nil {
		return 0, err
	}
	a.Logger.Info().Int("people", len(people)).Int("phone_entries", phones).Int("threads_updated", changed).Msg("Google Contacts directory refreshed")
	a.emitConversationsChange()
	return phones, nil
}
