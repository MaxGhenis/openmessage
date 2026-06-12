package app

import (
	"fmt"
	"strings"

	"github.com/maxghenis/openmessage/internal/db"
)

// SyncGoogleContacts pulls the contact list from the linked Google Messages
// account and caches it locally (name + number).
//
// It is intentionally READ-ONLY: it only calls ListContacts and never
// GetOrCreateConversation, so it does NOT create threads on the phone. This is
// the safe way to populate contacts — unlike the deep-backfill contact
// discovery, which floods the inbox by creating a conversation per contact.
func (a *App) SyncGoogleContacts() (int, error) {
	cli := a.GetClient()
	if cli == nil {
		return 0, fmt.Errorf("not connected to Google Messages")
	}
	resp, err := cli.GM.ListContacts()
	if err != nil {
		return 0, fmt.Errorf("list contacts: %w", err)
	}

	count := 0
	for _, c := range resp.GetContacts() {
		number := ""
		if n := c.GetNumber(); n != nil {
			number = strings.TrimSpace(n.GetNumber())
		}
		name := strings.TrimSpace(c.GetName())
		if number == "" && name == "" {
			continue
		}
		id := c.GetContactID()
		if id == "" {
			id = "gm:" + number
		}
		if err := a.Store.UpsertContact(&db.Contact{ContactID: id, Name: name, Number: number}); err != nil {
			a.Logger.Warn().Err(err).Str("contact_id", id).Msg("Failed to cache Google contact")
			continue
		}
		count++
	}
	a.Logger.Info().Int("count", count).Msg("Synced Google Messages contacts")
	return count, nil
}
