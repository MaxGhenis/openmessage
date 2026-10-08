package app

import (
	"fmt"
	"math/rand"
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/sim"
)

// SIMs remembers the SIM cards the phone reports in its Settings event so send
// paths and labels can name carriers. One process serves one Google account,
// so a package-level registry is enough; the Google event handler feeds it.
var SIMs = &sim.Registry{}

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
// payload from a conversation. On a dual-SIM phone every thread has two self
// participants; the one named by Conversation.DefaultOutgoingID is the SIM the
// phone itself would send from, so that is the default here too (the first
// self participant when no default is reported). Falls back to the
// conversation's SIM card when there is no self participant at all.
func ExtractSIMAndParticipant(conv *gmproto.Conversation) (participantID string, payload *gmproto.SIMPayload) {
	participantID, payload, _, _ = SelectSIM(conv, "")
	return
}

// SelectSIM is ExtractSIMAndParticipant with an explicit SIM choice: a slot
// number ("1", "2"), the SIM's phone number, a self participant ID, or a
// carrier name. An empty selector picks the thread's default. The returned
// slot (nil when the thread reports no self participants) tells the caller
// which SIM was chosen so it can say so in its reply.
func SelectSIM(conv *gmproto.Conversation, selector string) (participantID string, payload *gmproto.SIMPayload, chosen *sim.Slot, err error) {
	return sim.Select(conv, selector, SIMs)
}

// ConversationSIMs lists the SIMs a conversation can send from, default first.
func ConversationSIMs(conv *gmproto.Conversation) []sim.Slot {
	return sim.FromConversation(conv, SIMs)
}

// BuildSendPayload constructs a SendMessageRequest matching the format used by
// the mautrix bridge: MessageInfo array (not MessagePayloadContent), TmpID in 3
// places, SIMPayload, and ParticipantID.
func BuildSendPayload(conversationID, message, replyToID, participantID string, sim *gmproto.SIMPayload) *gmproto.SendMessageRequest {
	return BuildSendPayloadWithTmpID(conversationID, message, replyToID, participantID, sim, "")
}

func newSendTmpID(preferred string) string {
	if preferred = strings.TrimSpace(preferred); preferred != "" {
		return preferred
	}
	return fmt.Sprintf("tmp_%012d", rand.Int63n(1e12))
}

// BuildSendPayloadWithTmpID is BuildSendPayload with an optional caller-owned
// temporary ID. Queued sends use this to make retries idempotent server-side.
func BuildSendPayloadWithTmpID(conversationID, message, replyToID, participantID string, sim *gmproto.SIMPayload, tmpID string) *gmproto.SendMessageRequest {
	tmpID = newSendTmpID(tmpID)
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
	return BuildSendMediaPayloadWithTmpID(conversationID, media, participantID, sim, "")
}

// BuildSendMediaPayloadWithTmpID is BuildSendMediaPayload with an optional
// caller-owned temporary ID for idempotent queued media retries.
func BuildSendMediaPayloadWithTmpID(conversationID string, media *gmproto.MediaContent, participantID string, sim *gmproto.SIMPayload, tmpID string) *gmproto.SendMessageRequest {
	tmpID = newSendTmpID(tmpID)
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
