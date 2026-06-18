package app

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

const (
	googleAvatarMaxBytes      = 512 * 1024
	googleAvatarSuccessTTL    = 7 * 24 * time.Hour
	googleAvatarMissingTTL    = 24 * time.Hour
	googleAvatarRequestDelay  = 500 * time.Millisecond
	googleAvatarQueueCapacity = 256
)

func googleAvatarSyncEnabled() bool {
	value := strings.TrimSpace(os.Getenv("OPENMESSAGE_GOOGLE_AVATAR_SYNC"))
	if value == "" {
		value = strings.TrimSpace(os.Getenv("OPENMESSAGES_GOOGLE_AVATAR_SYNC"))
	}
	switch strings.ToLower(value) {
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return true
	}
}

func (a *App) QueueGoogleAvatarCandidates(candidates []db.ContactAvatarCandidate) {
	if !googleAvatarSyncEnabled() || len(candidates) == 0 || a == nil {
		return
	}
	a.avatarSyncMu.Lock()
	if a.avatarSyncClosed {
		a.avatarSyncMu.Unlock()
		return
	}
	a.avatarSyncOnce.Do(func() {
		a.avatarSyncQueue = make(chan db.ContactAvatarCandidate, googleAvatarQueueCapacity)
		a.avatarSyncStop = make(chan struct{})
		a.avatarSyncWG.Add(1)
		go func() {
			defer a.avatarSyncWG.Done()
			a.googleAvatarSyncLoop()
		}()
	})
	queue := a.avatarSyncQueue
	stop := a.avatarSyncStop
	a.avatarSyncMu.Unlock()
	if queue == nil {
		return
	}
	for _, candidate := range candidates {
		if db.ContactAvatarID(candidate) == "" {
			continue
		}
		select {
		case <-stop:
			return
		default:
		}
		select {
		case queue <- candidate:
		case <-stop:
			return
		default:
			a.Logger.Debug().
				Str("avatar_source", avatarCandidateLogSource(candidate)).
				Str("avatar_key_type", avatarCandidateLogKeyType(candidate)).
				Msg("Google avatar sync queue full; dropping candidate")
		}
	}
}

func (a *App) StopGoogleAvatarSync() {
	if a == nil {
		return
	}
	a.avatarSyncMu.Lock()
	if !a.avatarSyncClosed {
		a.avatarSyncClosed = true
		if a.avatarSyncStop != nil {
			select {
			case <-a.avatarSyncStop:
			default:
				close(a.avatarSyncStop)
			}
		}
	}
	a.avatarSyncMu.Unlock()
	a.avatarSyncWG.Wait()
}

func (a *App) googleAvatarSyncLoop() {
	recent := map[string]time.Time{}
	for {
		select {
		case <-a.avatarSyncStop:
			return
		case candidate := <-a.avatarSyncQueue:
			key := db.ContactAvatarID(candidate)
			if key == "" {
				continue
			}
			if seenAt, ok := recent[key]; ok && time.Since(seenAt) < time.Hour {
				continue
			}
			recent[key] = time.Now()
			a.fetchGoogleAvatarCandidate(candidate)
			select {
			case <-a.avatarSyncStop:
				return
			case <-time.After(googleAvatarRequestDelay):
			}
		}
	}
}

func (a *App) fetchGoogleAvatarCandidate(candidate db.ContactAvatarCandidate) {
	now := time.Now().UnixMilli()
	if a.Store == nil {
		return
	}
	existing, err := a.Store.GetContactAvatar(candidate.SourcePlatform, candidate.ParticipantID, candidate.ContactID, candidate.PhoneNumber)
	if err != nil {
		a.Logger.Debug().Err(err).Msg("Google avatar lookup before fetch failed")
		return
	}
	if existing != nil {
		checkedAt := time.UnixMilli(existing.LastCheckedAtMS)
		if existing.ImageHash != "" && time.Since(checkedAt) < googleAvatarSuccessTTL {
			return
		}
		if existing.ImageHash == "" && time.Since(checkedAt) < googleAvatarMissingTTL {
			return
		}
	}

	gm := a.getGMClient()
	if gm == nil {
		return
	}
	identifier, useContact := googleAvatarIdentifier(candidate)
	if identifier == "" {
		_ = a.Store.MarkContactAvatarChecked(candidate, now)
		return
	}
	resp, err := func() (*gmproto.GetThumbnailResponse, error) {
		if useContact {
			return gm.GetContactThumbnail(identifier)
		}
		return gm.GetParticipantThumbnail(identifier)
	}()
	if err != nil {
		a.Logger.Debug().
			Err(err).
			Str("avatar_source", avatarCandidateLogSource(candidate)).
			Str("avatar_key_type", avatarCandidateLogKeyType(candidate)).
			Msg("Google avatar thumbnail fetch failed")
		_ = a.Store.MarkContactAvatarChecked(candidate, now)
		return
	}
	image := thumbnailImageForIdentifier(resp, identifier)
	if len(image) == 0 || len(image) > googleAvatarMaxBytes {
		_ = a.Store.MarkContactAvatarChecked(candidate, now)
		return
	}
	mimeType := http.DetectContentType(image)
	if mimeType == "application/octet-stream" || mimeType == "" {
		mimeType = "image/jpeg"
	}
	if !strings.HasPrefix(mimeType, "image/") {
		_ = a.Store.MarkContactAvatarChecked(candidate, now)
		return
	}
	sum := sha256.Sum256(image)
	if err := a.Store.UpsertContactAvatar(candidate, image, mimeType, hex.EncodeToString(sum[:]), now); err != nil {
		a.Logger.Debug().
			Err(err).
			Str("avatar_source", avatarCandidateLogSource(candidate)).
			Str("avatar_key_type", avatarCandidateLogKeyType(candidate)).
			Msg("Google avatar cache write failed")
	}
}

func avatarCandidateLogSource(candidate db.ContactAvatarCandidate) string {
	source := strings.ToLower(strings.TrimSpace(candidate.SourcePlatform))
	if source == "" {
		return "sms"
	}
	return source
}

func avatarCandidateLogKeyType(candidate db.ContactAvatarCandidate) string {
	switch {
	case strings.TrimSpace(candidate.ParticipantID) != "":
		return "participant"
	case strings.TrimSpace(candidate.ContactID) != "":
		return "contact"
	case db.NormalizeAvatarPhone(candidate.PhoneNumber) != "":
		return "phone"
	default:
		return "none"
	}
}

func googleAvatarIdentifier(candidate db.ContactAvatarCandidate) (string, bool) {
	if strings.EqualFold(candidate.Source, "contacts") && strings.TrimSpace(candidate.ContactID) != "" {
		return strings.TrimSpace(candidate.ContactID), true
	}
	if strings.TrimSpace(candidate.ParticipantID) != "" {
		return strings.TrimSpace(candidate.ParticipantID), false
	}
	if strings.TrimSpace(candidate.ContactID) != "" {
		return strings.TrimSpace(candidate.ContactID), true
	}
	return "", false
}

func thumbnailImageForIdentifier(resp *gmproto.GetThumbnailResponse, identifier string) []byte {
	if resp == nil {
		return nil
	}
	for _, thumbnail := range resp.GetThumbnail() {
		if thumbnail.GetIdentifier() != identifier {
			continue
		}
		if data := thumbnail.GetData(); data != nil {
			return data.GetImageBuffer()
		}
	}
	return nil
}
