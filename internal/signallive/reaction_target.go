package signallive

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/v2keys"
)

// ReactionTarget is the message a durable Signal reaction targets, as the v2
// dispatcher describes it (bridge.MessageRef). RemoteID is its v2 remote ID,
// AuthorID its sender's canonical identity (empty when the store names none),
// Outgoing whether this account sent it, and SentAt its stored occurred time.
type ReactionTarget struct {
	RemoteID string
	AuthorID string
	Outgoing bool
	SentAt   time.Time
}

// ReactionTargetArgs builds signal-cli sendReaction's target arguments,
// "-a <author> -t <sent timestamp>", for a reaction sent from account.
//
// Signal names the message a reaction targets by its author and sent
// timestamp, never by an ID OpenMessage stores. A reaction that names a
// message nobody has is still a well-formed command, so this fails, before
// signal-cli runs, unless the store vouches for both:
//
//   - An incoming message is named by its stored sender and its occurred
//     time, and only when its remote ID has the form a Signal receiver gives a
//     message it keyed by sender and sent timestamp (a SHA-1,
//     v2keys.SignalIncomingSourceID): the v2 decoder, the legacy receiver, and
//     a Signal Desktop import of a row with a sent time all store that
//     timestamp as the occurred time. The ID itself is not sent. Any other ID
//     is refused: a Desktop row with no sent time is stored under its received
//     time and marked (v2keys.SignalReceivedSourceID), and the legacy-primary
//     mirror keeps a legacy ID. A message with no stored sender has no author
//     to name.
//   - A message this account sent is named by this account and by its remote
//     ID, and only when that ID is a decimal timestamp. That is how a send
//     Signal accepted is stored when its timestamp was kept: an outbox
//     confirmation, a sync message from the phone, the legacy SendMedia and
//     the legacy-primary projector all key it by that timestamp. Its occurred
//     time is not used, because for every other ID it is not known to be Signal's:
//     an outbox send still on its request ID carries its submit time, a
//     migrated scheduled send its creation time, and a migrated "local:" row
//     may carry the wall clock the legacy SendText read after signal-cli
//     returned.
//
// Quotes (QuoteArgs) are more lenient about a sent message's time: a quote
// carries the quoted text with it.
func ReactionTargetArgs(target ReactionTarget, account string) ([]string, error) {
	author := normalizeSignalAddress(target.AuthorID)
	var timestampMS int64
	if target.Outgoing {
		author = normalizeSignalAddress(account)
		timestampMS, _ = signalTimestampID(target.RemoteID)
	} else if v2keys.IsSignalIncomingSourceID(strings.TrimSpace(target.RemoteID)) && !target.SentAt.IsZero() {
		timestampMS = target.SentAt.UnixMilli()
	}
	if author == "" {
		return nil, errors.New("signal reaction target author is unavailable")
	}
	if timestampMS <= 0 {
		return nil, errors.New("signal reaction target timestamp is unavailable")
	}
	return []string{"-a", author, "-t", strconv.FormatInt(timestampMS, 10)}, nil
}
