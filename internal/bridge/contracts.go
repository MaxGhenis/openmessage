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
}

// MessageRef names a stored message an operation refers to: a reaction or
// read-receipt target, or the message a reply quotes. RemoteID is the
// transport's identity for it. The dispatcher fills the other fields from the
// stored message. AuthorID is the author's canonical identity. It is empty
// when the stored message names no sender, which is always the case for a
// message this account sent, and adapters read empty as this account. SentAt
// is the message's occurred time. A zero
// SentAt means the dispatcher had no stored message to describe (it does not
// hold one under RemoteID, or the message is an outgoing one still waiting
// for its transport ID), so only RemoteID is meaningful.
type MessageRef struct {
	RemoteID string
	AuthorID string
	SentAt   time.Time
	// Text, HasAttachment and AttachmentMIME are filled for reply targets
	// only. Text is the stored body. HasAttachment reports whether the message
	// carries an attachment, and AttachmentMIME is the first attachment's MIME
	// type, so an adapter can describe a body-less message in its quote.
	Text           string
	HasAttachment  bool
	AttachmentMIME string
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
