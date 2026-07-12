package bridge

import (
	"fmt"
	"time"
)

type Platform string

type Generation uint64

type RawIngressRecord struct {
	AccountID    string
	Generation   Generation
	DedupeKey    string // stable remote event/envelope ID when available
	Codec        string // e.g. google.protobuf, whatsapp.event, signal.jsonrpc
	CodecVersion uint32
	ReceivedAt   time.Time
	Payload      []byte // untouched serializable transport frame/event
}

type IdentityRef struct {
	Kind      string
	Canonical string
	Raw       string
	Name      string
	IsSelf    bool
}

type Participant struct {
	Identity IdentityRef
	Role     string
	Active   bool
}

type Attachment struct {
	RemoteID  string
	RemoteRef []byte
	Filename  string
	MIME      string
	Size      int64
}

type ConversationEvent struct {
	RemoteConversationID string
	Kind                 string
	Title                string
	Participants         []Participant
	RemoteRevision       string
}

type MessageEvent struct {
	RemoteConversationID string
	RemoteMessageID      string
	ClientRequestID      string // self-echo/outbox correlation
	Sender               IdentityRef
	Direction            string
	Body                 string
	Attachments          []Attachment
	ReplyToRemoteID      string
	OccurredAt           time.Time
}

type MessageMutationEvent struct {
	RemoteConversationID string
	RemoteMessageID      string
	Kind                 string // edit or delete
	Body                 string
	OccurredAt           time.Time
}

type ReactionAction string

const (
	ReactionAdd    ReactionAction = "add"
	ReactionRemove ReactionAction = "remove"
	ReactionSwitch ReactionAction = "switch"
)

type ReactionEvent struct {
	RemoteConversationID  string
	TargetRemoteMessageID string
	Actor                 IdentityRef
	Emoji                 string
	Action                ReactionAction
	OccurredAt            time.Time
}

type ReceiptEvent struct {
	RemoteConversationID string
	RemoteMessageIDs     []string
	Actor                IdentityRef
	Status               string // delivered or read
	OccurredAt           time.Time
}

type TypingEvent struct {
	RemoteConversationID string
	Actor                IdentityRef
	Typing               bool
	ExpiresAt            time.Time
}

type EventKind string

const (
	EventConversation    EventKind = "conversation"
	EventMessage         EventKind = "message"
	EventMessageMutation EventKind = "message_mutation"
	EventReaction        EventKind = "reaction"
	EventReceipt         EventKind = "receipt"
)

// Event is the closed normalized durable projection contract: exactly one
// payload matching Kind must be non-nil. No transport or db type can cross it.
type Event struct {
	AccountID       string
	SourceInboxID   string
	Kind            EventKind
	Conversation    *ConversationEvent
	Message         *MessageEvent
	MessageMutation *MessageMutationEvent
	Reaction        *ReactionEvent
	Receipt         *ReceiptEvent
}

type EphemeralEvent struct {
	AccountID  string
	Generation Generation
	Typing     *TypingEvent
}

type FailureClass string

const (
	FailureTransient          FailureClass = "transient"
	FailureRateLimited        FailureClass = "rate_limited"
	FailureCredentialsExpired FailureClass = "credentials_expired"
	FailureReauthRequired     FailureClass = "reauth_required"
	FailureUpgradeRequired    FailureClass = "upgrade_required"
	FailureMisconfigured      FailureClass = "misconfigured"
	FailureUnsupported        FailureClass = "unsupported"
)

type DispatchCertainty string

const (
	DispatchNotCalled DispatchCertainty = "not_dispatched"
	DispatchUncertain DispatchCertainty = "uncertain"
)

type OpError struct {
	Class       FailureClass
	Operation   string
	RetryAt     time.Time
	Fingerprint string
	Dispatch    DispatchCertainty
	Cause       error
}

func (e OpError) Error() string {
	if e.Cause == nil {
		return fmt.Sprintf("%s: %s", e.Operation, e.Class)
	}

	return fmt.Sprintf("%s: %s: %v", e.Operation, e.Class, e.Cause)
}

type Capability string

const (
	CapabilityStartConversation Capability = "start_conversation"
	CapabilityTextSend          Capability = "text_send"
	CapabilityMediaSend         Capability = "media_send"
	CapabilityReactions         Capability = "reactions"
	CapabilityReadReceipts      Capability = "read_receipts"
	CapabilityMediaDownload     Capability = "media_download"
)

type CapabilitySet struct {
	StartConversation bool `json:"start_conversation"`
	TextSend          bool `json:"text_send"`
	MediaSend         bool `json:"media_send"`
	Reactions         bool `json:"reactions"`
	ReadReceipts      bool `json:"read_receipts"`
	PairQR            bool `json:"pair_qr"`
	PairPhone         bool `json:"pair_phone"`
	MediaDownload     bool `json:"media_download"`
}
