package signaldesktop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// V2Store is what the feed reads from v2. It only reads.
type V2Store interface {
	GetAccount(accountID string) (sqlite.Account, error)
	ListIdentities(accountID string) ([]sqlite.Identity, error)
	ListConversationsByRecency(accountID string) ([]sqlite.Conversation, error)
	ListMessageRemoteKeys(ctx context.Context, accountID string) ([]sqlite.MessageRemoteKey, error)
}

// ErrNoAccountNumber means v2 does not say which Signal number is the
// account's own, so Desktop's archive cannot be matched to it.
var ErrNoAccountNumber = errors.New("v2 has no own number for the Signal account")

// AccountNumber is the account's own E.164 number as v2 records it: the
// account's remote_account_id when that is a phone number, else its one
// self phone identity. It never comes from signal-cli's pairing state, which
// is exactly what is missing when this feed matters.
func AccountNumber(account sqlite.Account, identities []sqlite.Identity) (string, error) {
	if account.RemoteAccountID != nil {
		if number := NormalizeE164(*account.RemoteAccountID); number != "" {
			return number, nil
		}
	}
	found := ""
	for _, identity := range identities {
		if !identity.IsSelf || identity.Kind != "e164" {
			continue
		}
		number := NormalizeE164(identity.CanonicalValue)
		if number == "" {
			continue
		}
		if found != "" && found != number {
			return "", errors.New("v2 records more than one own Signal number")
		}
		found = number
	}
	if found == "" {
		return "", ErrNoAccountNumber
	}
	return found, nil
}

// LoadFacts reads, once per scan, what v2 holds for the account.
func LoadFacts(ctx context.Context, store V2Store, accountID string) (*Facts, error) {
	account, err := store.GetAccount(accountID)
	if err != nil {
		return nil, fmt.Errorf("read the v2 Signal account: %w", err)
	}
	identities, err := store.ListIdentities(accountID)
	if err != nil {
		return nil, err
	}
	number, err := AccountNumber(account, identities)
	if err != nil {
		return nil, err
	}
	canonical := make([]string, 0, len(identities))
	for _, identity := range identities {
		if identity.Kind == "e164" || identity.Kind == "signal_aci" {
			canonical = append(canonical, identity.CanonicalValue)
		}
	}
	conversations, err := store.ListConversationsByRecency(accountID)
	if err != nil {
		return nil, err
	}
	bound := make([]string, 0, len(conversations))
	for _, conversation := range conversations {
		if remoteID := strings.TrimSpace(conversation.RemoteConversationID); remoteID != "" {
			bound = append(bound, remoteID)
		}
	}
	stored, err := store.ListMessageRemoteKeys(ctx, accountID)
	if err != nil {
		return nil, err
	}
	keys := make([]MessageKey, 0, len(stored))
	for _, key := range stored {
		keys = append(keys, MessageKey{MessageID: key.MessageID, RemoteConversationID: key.RemoteConversationID, RemoteMessageID: key.RemoteMessageID})
	}
	return NewFacts(accountID, number, canonical, bound, keys), nil
}
