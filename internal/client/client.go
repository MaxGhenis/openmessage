package client

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// RequestTimeout is how long a libgm request waits for the phone's answer
// before failing with libgm.ErrPhoneNotResponding. libgm's own default is
// 60 s. The wait starts once the request has been sent, and the fork's HTTP
// client allows 20 s for the server's response headers (ResponseHeaderTimeout),
// so 35 s keeps a lookup the phone never answers inside the outbox's 60 s bound
// on a transport call (internal/messaging). The Google adapter, which knows
// whether anything was sent, then classifies it, rather than the dispatcher,
// which must assume it was. A send that runs past the bound anyway (a fresh
// connection's dial and TLS on top, or a slow lookup followed by an unanswered
// send) is abandoned as uncertain, which never sends it twice. A phone that
// answers after the timeout costs a retried lookup, or an uncertain send that
// its echo later confirms.
const RequestTimeout = 35 * time.Second

type Client struct {
	GM     *libgm.Client
	Logger zerolog.Logger
}

func newGMClient(authData *libgm.AuthData, pushKeys *libgm.PushKeys, logger zerolog.Logger) *libgm.Client {
	cli := libgm.NewClient(authData, pushKeys, logger)
	cli.SetRequestTimeout(RequestTimeout)
	return cli
}

func NewFromSession(sessionData *SessionData, logger zerolog.Logger) (*Client, error) {
	authData := libgm.NewAuthData()
	if err := json.Unmarshal(sessionData.AuthDataJSON, authData); err != nil {
		return nil, fmt.Errorf("unmarshal auth data: %w", err)
	}

	var pushKeys *libgm.PushKeys
	if len(sessionData.PushKeysJSON) > 0 {
		pushKeys = &libgm.PushKeys{}
		if err := json.Unmarshal(sessionData.PushKeysJSON, pushKeys); err != nil {
			return nil, fmt.Errorf("unmarshal push keys: %w", err)
		}
	}

	cli := newGMClient(authData, pushKeys, logger)
	return &Client{GM: cli, Logger: logger}, nil
}

func NewForPairing(logger zerolog.Logger) *Client {
	authData := libgm.NewAuthData()
	cli := newGMClient(authData, nil, logger)
	return &Client{GM: cli, Logger: logger}
}

func (c *Client) SessionData() (*SessionData, error) {
	authJSON, err := json.Marshal(c.GM.AuthData)
	if err != nil {
		return nil, fmt.Errorf("marshal auth data: %w", err)
	}
	var pushJSON json.RawMessage
	if c.GM.PushKeys != nil {
		pushJSON, err = json.Marshal(c.GM.PushKeys)
		if err != nil {
			return nil, fmt.Errorf("marshal push keys: %w", err)
		}
	}
	return &SessionData{
		AuthDataJSON: authJSON,
		PushKeysJSON: pushJSON,
	}, nil
}

// ExtractMessageBody extracts text content from a protobuf Message.
func ExtractMessageBody(msg *gmproto.Message) string {
	for _, info := range msg.GetMessageInfo() {
		if mc := info.GetMessageContent(); mc != nil {
			return mc.GetContent()
		}
	}
	return ""
}

// MediaInfo holds extracted media metadata from a protobuf Message.
type MediaInfo struct {
	MediaID                string
	MimeType               string
	MediaName              string
	DecryptionKey          []byte
	Size                   int64
	ThumbnailMediaID       string
	ThumbnailDecryptionKey []byte
	InlineData             []byte // Inline thumbnail bytes from mediaData field
}

// ExtractMediaInfo extracts media content from a protobuf Message.
// Returns nil if the message has no media attachment.
// Falls back to thumbnail or inline data when full-size MediaID is unavailable.
func ExtractMediaInfo(msg *gmproto.Message) *MediaInfo {
	for _, info := range msg.GetMessageInfo() {
		if mc := info.GetMediaContent(); mc != nil {
			mime := mc.GetMimeType()
			if mime == "" {
				switch {
				case mc.GetFormat() >= 1 && mc.GetFormat() <= 7:
					mime = "image/jpeg"
				default:
					mime = "application/octet-stream"
				}
			}

			mi := &MediaInfo{
				MediaID:                mc.GetMediaID(),
				MimeType:               mime,
				MediaName:              mc.GetMediaName(),
				DecryptionKey:          mc.GetDecryptionKey(),
				Size:                   mc.GetSize(),
				ThumbnailMediaID:       mc.GetThumbnailMediaID(),
				ThumbnailDecryptionKey: mc.GetThumbnailDecryptionKey(),
				InlineData:             mc.GetMediaData(),
			}

			// If no full-size MediaID, fall back to thumbnail
			if mi.MediaID == "" && mi.ThumbnailMediaID != "" {
				mi.MediaID = mi.ThumbnailMediaID
				mi.DecryptionKey = mi.ThumbnailDecryptionKey
			}

			return mi
		}
	}
	return nil
}

// Reaction holds an emoji, how many people reacted with it, and the participant
// IDs of those reactors. Actors are Google Messages participant IDs that resolve
// to names via the conversation's participant list (see SmallInfo.ParticipantID).
type Reaction struct {
	Emoji  string   `json:"emoji"`
	Count  int      `json:"count"`
	Actors []string `json:"actors,omitempty"`
}

// ExtractReactions extracts reaction data from a protobuf Message.
// Returns nil if there are no reactions.
func ExtractReactions(msg *gmproto.Message) []Reaction {
	entries := msg.GetReactions()
	if len(entries) == 0 {
		return nil
	}
	var reactions []Reaction
	for _, entry := range entries {
		if data := entry.GetData(); data != nil {
			emoji := data.GetUnicode()
			if emoji == "" {
				continue
			}
			participantIDs := entry.GetParticipantIDs()
			reactions = append(reactions, Reaction{
				Emoji:  emoji,
				Count:  len(participantIDs),
				Actors: participantIDs,
			})
		}
	}
	if len(reactions) == 0 {
		return nil
	}
	return reactions
}

// ExtractReplyToID extracts the replied-to message ID, if any.
func ExtractReplyToID(msg *gmproto.Message) string {
	if rm := msg.GetReplyMessage(); rm != nil {
		return rm.GetMessageID()
	}
	return ""
}

// ExtractSenderInfo gets the sender name and number from a Message.
func ExtractSenderInfo(msg *gmproto.Message) (name, number string) {
	if p := msg.GetSenderParticipant(); p != nil {
		name = p.GetFullName()
		if name == "" {
			name = p.GetFirstName()
		}
		if id := p.GetID(); id != nil {
			number = id.GetNumber()
		}
		if number == "" {
			number = p.GetFormattedNumber()
		}
	}
	return
}

// MessageIsFromMe infers whether a Google Messages/RCS message was sent by the
// local account. Historical backfill messages sometimes omit sender participant
// metadata even when the message status is clearly outgoing.
func MessageIsFromMe(msg *gmproto.Message) bool {
	if msg == nil {
		return false
	}
	if p := msg.GetSenderParticipant(); p != nil && p.GetIsMe() {
		return true
	}
	if ms := msg.GetMessageStatus(); ms != nil {
		return strings.HasPrefix(ms.GetStatus().String(), "OUTGOING")
	}
	return false
}
