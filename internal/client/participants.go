package client

import (
	"encoding/json"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

// storedParticipant is the per-participant record kept in the conversations
// table's participants JSON. Both the live event handler and the backfill
// path write it, so it lives in one place.
type storedParticipant struct {
	Name      string `json:"name"`
	Number    string `json:"number"`
	IsMe      bool   `json:"is_me,omitempty"`
	ID        string `json:"id,omitempty"` // participant ID, used to resolve reaction actors to names
	ContactID string `json:"contact_id,omitempty"`
	// SIM slot and thread default for self participants, so reads can say
	// which SIM a message left from or which one a thread uses without the
	// live proto (see internal/sim).
	SIM        int  `json:"sim,omitempty"`
	SIMDefault bool `json:"sim_default,omitempty"`
}

// BuildParticipantsJSON renders a conversation's participants as the JSON the
// store keeps, and lists the non-self participants as contact avatar
// candidates tagged with source ("live" or "backfill"). Returns "[]" for a
// conversation without participants.
func BuildParticipantsJSON(conv *gmproto.Conversation, source string) (string, []db.ContactAvatarCandidate) {
	ps := conv.GetParticipants()
	if len(ps) == 0 {
		return "[]", nil
	}
	defaultOutgoingID := conv.GetDefaultOutgoingID()
	infos := make([]storedParticipant, 0, len(ps))
	var avatarCandidates []db.ContactAvatarCandidate
	for _, p := range ps {
		info := storedParticipant{
			Name:      p.GetFullName(),
			IsMe:      p.GetIsMe(),
			ContactID: p.GetContactID(),
		}
		if id := p.GetID(); id != nil {
			info.Number = id.GetNumber()
			info.ID = id.GetParticipantID()
		}
		if info.Number == "" {
			info.Number = p.GetFormattedNumber()
		}
		if info.IsMe {
			info.SIM = int(p.GetSimPayload().GetSIMNumber())
			info.SIMDefault = defaultOutgoingID != "" && info.ID == defaultOutgoingID
		} else {
			avatarCandidates = append(avatarCandidates, db.ContactAvatarCandidate{
				SourcePlatform: "sms",
				ParticipantID:  info.ID,
				ContactID:      info.ContactID,
				PhoneNumber:    info.Number,
				DisplayName:    info.Name,
				Source:         source,
			})
		}
		infos = append(infos, info)
	}
	b, err := json.Marshal(infos)
	if err != nil {
		return "[]", avatarCandidates
	}
	return string(b), avatarCandidates
}
