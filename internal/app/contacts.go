package app

import (
	"fmt"
	"strings"

	"github.com/maxghenis/openmessage/internal/db"
)

func (a *App) StartGoogleContactSync() {
	if a == nil || !googleAvatarSyncEnabled() {
		return
	}
	a.avatarSyncMu.Lock()
	if a.avatarSyncClosed {
		a.avatarSyncMu.Unlock()
		return
	}
	a.avatarSyncWG.Add(1)
	a.avatarSyncMu.Unlock()
	go func() {
		defer a.avatarSyncWG.Done()
		count, err := a.SyncGoogleContacts()
		if err != nil {
			a.Logger.Debug().Err(err).Msg("Google contact sync skipped")
			return
		}
		a.Logger.Debug().Int("contacts", count).Msg("Queued Google contact avatars")
	}()
}

func (a *App) SyncGoogleContacts() (int, error) {
	gm := a.getGMClient()
	if gm == nil {
		return 0, fmt.Errorf("not connected to Google Messages")
	}
	resp, err := gm.ListContacts()
	if err != nil {
		return 0, fmt.Errorf("list contacts: %w", err)
	}
	count := 0
	var avatarCandidates []db.ContactAvatarCandidate
	for _, c := range resp.GetContacts() {
		number := ""
		if n := c.GetNumber(); n != nil {
			number = strings.TrimSpace(n.GetNumber())
		}
		name := strings.TrimSpace(c.GetName())
		if number == "" && name == "" {
			continue
		}
		contactID := strings.TrimSpace(c.GetContactID())
		if contactID == "" {
			contactID = "gm:" + number
		}
		if err := a.Store.UpsertContact(&db.Contact{ContactID: contactID, Name: name, Number: number}); err != nil {
			a.Logger.Warn().Err(err).Msg("Failed to cache Google contact")
			continue
		}
		count++
		avatarCandidates = append(avatarCandidates, db.ContactAvatarCandidate{
			SourcePlatform: "sms",
			ParticipantID:  strings.TrimSpace(c.GetParticipantID()),
			ContactID:      strings.TrimSpace(c.GetContactID()),
			PhoneNumber:    number,
			DisplayName:    name,
			Source:         "contacts",
		})
	}
	a.QueueGoogleAvatarCandidates(avatarCandidates)
	return count, nil
}
