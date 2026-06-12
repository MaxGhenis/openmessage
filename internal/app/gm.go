package app

import (
	"fmt"
	"math/rand"
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

var (
	getGoogleConversationForSend = func(a *App, conversationID string) (*gmproto.Conversation, error) {
		cli := a.GetClient()
		if cli == nil {
			return nil, fmt.Errorf(ErrNotConnected)
		}
		return cli.GM.GetConversation(conversationID)
	}
	sendGoogleTextPayload = func(a *App, payload *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
		cli := a.GetClient()
		if cli == nil {
			return nil, fmt.Errorf(ErrNotConnected)
		}
		return cli.GM.SendMessage(payload)
	}
)

// ErrNotConnected is the error message returned when an operation requires
// a Google Messages connection but one is not established.
const ErrNotConnected = "not connected to Google Messages"

// ContactNumberMysteriousInt is the default value for the MysteriousInt field
// in ContactNumber structs used for conversation lookups and message sending.
const ContactNumberMysteriousInt = 7

// NewContactNumbers builds a ContactNumber slice from phone number strings,
// suitable for GetOrCreateConversation requests.
func NewContactNumbers(phones []string) []*gmproto.ContactNumber {
	numbers := make([]*gmproto.ContactNumber, len(phones))
	for i, phone := range phones {
		numbers[i] = &gmproto.ContactNumber{
			MysteriousInt: ContactNumberMysteriousInt,
			Number:        phone,
			Number2:       phone,
		}
	}
	return numbers
}

// ExtractSIMAndParticipant finds the current user's participant ID and SIM
// payload from a conversation, falling back to the conversation's SIM card.
func ExtractSIMAndParticipant(conv *gmproto.Conversation) (participantID string, sim *gmproto.SIMPayload) {
	for _, p := range conv.GetParticipants() {
		if p.GetIsMe() {
			if id := p.GetID(); id != nil {
				participantID = id.GetNumber()
			}
			sim = p.GetSimPayload()
			break
		}
	}
	if sim == nil {
		if sc := conv.GetSimCard(); sc != nil {
			sim = sc.GetSIMData().GetSIMPayload()
		}
	}
	return
}

// BuildSendPayload constructs a SendMessageRequest matching the format used by
// the mautrix bridge: MessageInfo array (not MessagePayloadContent), TmpID in 3
// places, SIMPayload, and ParticipantID.
func BuildSendPayload(conversationID, message, replyToID, participantID string, sim *gmproto.SIMPayload) *gmproto.SendMessageRequest {
	tmpID := fmt.Sprintf("tmp_%012d", rand.Int63n(1e12))
	req := &gmproto.SendMessageRequest{
		ConversationID: conversationID,
		MessagePayload: &gmproto.MessagePayload{
			TmpID:                 tmpID,
			MessagePayloadContent: nil,
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{MessageContent: &gmproto.MessageContent{
					Content: message,
				}},
			}},
			ConversationID: conversationID,
			ParticipantID:  participantID,
			TmpID2:         tmpID,
		},
		SIMPayload: sim,
		TmpID:      tmpID,
	}
	if replyToID != "" {
		req.Reply = &gmproto.ReplyPayload{
			MessageID: replyToID,
		}
	}
	return req
}

// BuildSendMediaPayload constructs a SendMessageRequest with a MediaContent attachment
// instead of text. Uses the same MessageInfo array format as BuildSendPayload.
func BuildSendMediaPayload(conversationID string, media *gmproto.MediaContent, participantID string, sim *gmproto.SIMPayload) *gmproto.SendMessageRequest {
	tmpID := fmt.Sprintf("tmp_%012d", rand.Int63n(1e12))
	return &gmproto.SendMessageRequest{
		ConversationID: conversationID,
		MessagePayload: &gmproto.MessagePayload{
			TmpID:                 tmpID,
			MessagePayloadContent: nil,
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MediaContent{MediaContent: media},
			}},
			ConversationID: conversationID,
			ParticipantID:  participantID,
			TmpID2:         tmpID,
		},
		SIMPayload: sim,
		TmpID:      tmpID,
	}
}

// BuildReactionPayload constructs a SendReactionRequest using
// gmproto.MakeReactionData for proper emoji type mapping.
func BuildReactionPayload(messageID, emoji, action string, sim *gmproto.SIMPayload) *gmproto.SendReactionRequest {
	var a gmproto.SendReactionRequest_Action
	switch strings.ToLower(action) {
	case "remove":
		a = gmproto.SendReactionRequest_REMOVE
	case "switch":
		a = gmproto.SendReactionRequest_SWITCH
	default:
		a = gmproto.SendReactionRequest_ADD
	}
	return &gmproto.SendReactionRequest{
		MessageID:    messageID,
		ReactionData: gmproto.MakeReactionData(emoji),
		Action:       a,
		SIMPayload:   sim,
	}
}
