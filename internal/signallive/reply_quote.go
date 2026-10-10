package signallive

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
)

// ReplyTarget is the message a durable Signal send quotes. RemoteID names it.
// The other fields describe the stored message; the v2 dispatcher fills them
// from the v2 store (bridge.MessageRef). A zero SentAt means they are absent,
// and the quote is resolved from the legacy store by RemoteID, as it always
// was (signalQuoteArgs). Outgoing reports that this account sent the message;
// QuoteArgs does not read it and still takes an empty AuthorID as this
// account.
type ReplyTarget struct {
	RemoteID       string
	AuthorID       string
	Outgoing       bool
	SentAt         time.Time
	Text           string
	HasAttachment  bool
	AttachmentMIME string
}

// QuoteArgs builds signal-cli's --quote-timestamp, --quote-author and
// --quote-message arguments for a reply to target sent from account. Signal
// identifies the quoted message by its author and sent timestamp.
//
// An empty AuthorID, or one matching account, quotes this account; any other
// author goes through resolveAuthor, which maps an ACI to its number when the
// contact is known.
//
// The timestamp is SentAt, the stored occurred time: the v2 decoder and the
// migration both set it to the Signal sent timestamp. One case differs. A
// message this account sent through the v2 outbox keeps its submit time as
// its occurred time, while confirming the send set its remote ID to the
// timestamp signal-cli reported. So for a message this account wrote, a
// RemoteID that is a decimal timestamp wins over SentAt. Every v2 writer gives
// such messages either that timestamp or a non-numeric ID ("local:<sha1>"
// aliases, unconfirmed request IDs). Incoming messages carry SHA-1 IDs and
// always use SentAt.
//
// The quoted text is the trimmed body, else a placeholder naming the first
// attachment's kind, else "Attachment".
func QuoteArgs(target ReplyTarget, account string, resolveAuthor func(string) string) ([]string, error) {
	author := normalizeSignalAddress(target.AuthorID)
	self := author == "" || addressesMatch(author, account)
	if self {
		author = account
	} else if resolveAuthor != nil {
		author = resolveAuthor(author)
	}
	if author == "" {
		return nil, errors.New("signal reply target author is unavailable")
	}

	var timestampMS int64
	if remoteTimestamp, ok := signalTimestampID(target.RemoteID); ok && self {
		timestampMS = remoteTimestamp
	} else if !target.SentAt.IsZero() {
		timestampMS = target.SentAt.UnixMilli()
	}
	if timestampMS <= 0 {
		return nil, errors.New("signal reply target timestamp is unavailable")
	}

	quoteBody := strings.TrimSpace(target.Text)
	if quoteBody == "" && target.HasAttachment {
		quoteBody = signalAttachmentPlaceholder([]signalAttachment{{ContentType: target.AttachmentMIME}})
	}
	if quoteBody == "" {
		quoteBody = "Attachment"
	}
	return []string{
		"--quote-timestamp", strconv.FormatInt(timestampMS, 10),
		"--quote-author", author,
		"--quote-message", quoteBody,
	}, nil
}

// replyQuoteArgs builds the quote for a durable send. A target the dispatcher
// described is quoted from that description and needs no legacy row; any
// other is looked up in the legacy store by RemoteID.
//
// One described target still quotes from its legacy row: a legacy message ID
// ("signal:" prefix) the legacy store holds. Of the v2 writers only the
// legacy-primary mirror (v2wire.MirrorReplyTarget) keys a message that way,
// and its copy carries no sender or attachment, so the legacy row stays the
// better source there, exactly as before.
func (b *Bridge) replyQuoteArgs(reply ReplyTarget, account string) ([]string, error) {
	if reply.SentAt.IsZero() || b.holdsLegacyReplyTarget(reply.RemoteID) {
		return b.signalQuoteArgs(reply.RemoteID, account)
	}
	return QuoteArgs(reply, account, b.resolveContactAddress)
}

// holdsLegacyReplyTarget reports whether remoteID is a legacy Signal message
// ID whose row the legacy lookup can quote.
func (b *Bridge) holdsLegacyReplyTarget(remoteID string) bool {
	remoteID = strings.TrimSpace(remoteID)
	if b == nil || b.store == nil || !strings.HasPrefix(remoteID, "signal:") {
		return false
	}
	target, err := b.store.GetMessageByID(remoteID)
	return err == nil && target != nil && target.SourcePlatform == "signal" && target.TimestampMS != 0
}

// legacyReplyTarget describes a legacy Signal row for QuoteArgs exactly as the
// legacy lookup always read it: its own timestamp, and this account as author
// of a message it sent. RemoteID stays empty so the timestamp is the row's.
func legacyReplyTarget(target *db.Message) ReplyTarget {
	authorID := target.SenderNumber
	if target.IsFromMe {
		authorID = ""
	}
	var sentAt time.Time
	if target.TimestampMS != 0 {
		sentAt = time.UnixMilli(target.TimestampMS)
	}
	return ReplyTarget{
		AuthorID:       authorID,
		SentAt:         sentAt,
		Text:           target.Body,
		HasAttachment:  target.MediaID != "",
		AttachmentMIME: target.MimeType,
	}
}

// signalTimestampID reports whether remoteID is a positive base-10 timestamp.
func signalTimestampID(remoteID string) (int64, bool) {
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		return 0, false
	}
	for _, r := range remoteID {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	timestamp, err := strconv.ParseInt(remoteID, 10, 64)
	if err != nil || timestamp <= 0 {
		return 0, false
	}
	return timestamp, true
}
