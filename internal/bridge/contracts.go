package bridge

import (
	"context"
	"io"
	"time"
)

// StartRequest identifies one adapter connection generation.
type StartRequest struct {
	AccountID       string
	DeviceID        string
	RemoteAccountID string
	Generation      Generation
}

// Decoder projects a durable raw ingress record into normalized bridge events.
type Decoder interface {
	Decode(ctx context.Context, record RawIngressRecord) ([]Event, error)
}

type ConnectionSink interface {
	// AppendIngress rejects a stale generation, commits the raw record to the
	// inbox, and only then schedules Decoder projection.
	AppendIngress(ctx context.Context, record RawIngressRecord) error
	EmitEphemeral(ctx context.Context, event EphemeralEvent) error
	Beat(generation Generation, aliveAt time.Time, detail string)
}

// HistoryIngressSink is an optional ConnectionSink extension for frames a
// transport fetched on request (catch-up history) instead of receiving them on
// its live channel. AppendHistoryIngress keeps AppendIngress's generation
// fence and durable commit, but a history frame is not evidence that the live
// connection delivered anything: implementations must not record it as
// connection activity, and should count it apart from live ingress.
type HistoryIngressSink interface {
	AppendHistoryIngress(ctx context.Context, record RawIngressRecord) error
}

// Lifecycle owns transport connection generations.
type Lifecycle interface {
	// Start creates one owned connection generation. Run.Done must resolve when
	// every goroutine/process belonging to that generation has stopped.
	Start(ctx context.Context, req StartRequest, sink ConnectionSink) (Run, error)
}

type Run interface {
	Ready() <-chan struct{} // closes only when usable for sends
	Done() <-chan error     // one terminal result, then close
	Probe(ctx context.Context) (Liveness, error)
	Stop(ctx context.Context) error // cancel and join generation work
}

type Liveness struct {
	AliveAt time.Time
	Detail  string
}

type ConversationRef struct {
	RemoteID string
	// Kind is the locally stored conversation kind ("direct", "group",
	// "broadcast", "system") when the dispatcher knows it. Empty means
	// unknown; adapters must fail closed for any behaviour that depends on it.
	Kind string
	// DirectPeerNumber is the canonical E.164 number of a direct
	// conversation's sole active non-self participant, as recorded in the
	// local store. It is empty unless Kind is "direct" and exactly one such
	// E.164 peer is known. An adapter may use it to re-resolve a direct thread
	// whose RemoteID the transport no longer recognizes.
	DirectPeerNumber string
}

type MessageRef struct {
	RemoteID string
	AuthorID string
	SentAt   time.Time
	Text     string
}

type SendResult struct {
	RemoteMessageID string
	AcceptedAt      time.Time
	EchoExpected    bool
}

type RecipientRef struct {
	Kind  string // e164, jid, signal_aci, username, etc.
	Value string
	Name  string
}

type StartConversationRequest struct {
	AccountID   string
	Recipients  []RecipientRef
	Title       string
	InitialText string
	RequestID   string
}

type StartConversationResult struct {
	RemoteConversationID string
	RemoteRevision       string
	InitialSend          *SendResult
}

type ConversationStarter interface {
	StartConversation(
		ctx context.Context,
		req StartConversationRequest,
	) (StartConversationResult, error)
}

type TextRequest struct {
	AccountID    string
	Conversation ConversationRef
	Body         string
	ReplyTo      *MessageRef
	RequestID    string // stable outbox transport_request_id
}

type TextSender interface {
	SendText(ctx context.Context, req TextRequest) (SendResult, error)
}

type MediaRequest struct {
	AccountID    string
	Conversation ConversationRef
	Reader       io.Reader // fresh BlobStore reader for this attempt
	Size         int64
	Filename     string
	MIME         string
	Caption      string
	ReplyTo      *MessageRef
	RequestID    string
}

type MediaSender interface {
	SendMedia(ctx context.Context, req MediaRequest) (SendResult, error)
}

type ReactionRequest struct {
	AccountID    string
	Conversation ConversationRef
	Target       MessageRef
	Emoji        string
	Action       ReactionAction
	RequestID    string
}

type ReactionSender interface {
	SendReaction(ctx context.Context, req ReactionRequest) (SendResult, error)
}

type ReadReceiptRequest struct {
	AccountID    string
	Conversation ConversationRef
	Messages     []MessageRef
	ReadAt       time.Time
}

type ReadReceiptSender interface {
	MarkRead(ctx context.Context, req ReadReceiptRequest) error
}

type PairMethod string

const (
	PairQR        PairMethod = "qr"
	PairPhoneCode PairMethod = "phone_code"
)

type PairRequest struct {
	AccountID string
	Method    PairMethod
	Phone     string
}

type PairEvent struct {
	Kind      string // qr, code, instruction, blocked
	Payload   string // raw URI/code; presentation is outside the transport
	ExpiresAt time.Time
	Message   string
}

type PairSink interface {
	EmitPairEvent(ctx context.Context, event PairEvent) error
}

type PairResult struct {
	RemoteAccountID string
	RemoteDeviceID  string
}

type Pairer interface {
	Pair(ctx context.Context, req PairRequest, sink PairSink) (PairResult, error)
	Unpair(ctx context.Context, accountID string) error
}

type MediaRef struct {
	RemoteID string
	Opaque   []byte
	Filename string
	MIME     string
}

type MediaStream struct {
	io.ReadCloser
	Size     int64
	Filename string
	MIME     string
}

type MediaDownloader interface {
	DownloadMedia(ctx context.Context, accountID string, ref MediaRef) (MediaStream, error)
}

type Adapter interface {
	AccountID() string
	Platform() Platform
	Lifecycle
}
