// Package v2keys owns deterministic keys shared by v2 migration and live
// ingestion.
package v2keys

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Identity is the natural key for an account-scoped identity.
type Identity struct {
	AccountID string
	Kind      string
	Canonical string
}

// DeriveID deterministically mints a v2 primary key from an entity, account,
// and natural key.
func DeriveID(entity, accountID, naturalKey string) string {
	sum := sha256.Sum256([]byte(entity + "\x1f" + accountID + "\x1f" + naturalKey))
	return hex.EncodeToString(sum[:])[:32]
}

var signalACI = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IdentityKey classifies and canonicalizes a platform identity.
func IdentityKey(accountID, platform, raw string) (Identity, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return Identity{}, fmt.Errorf("identity value is empty")
	}
	kind := "username"
	canonical := value
	switch {
	case strings.HasPrefix(value, "+"):
		var digits strings.Builder
		for _, character := range value[1:] {
			if character >= '0' && character <= '9' {
				digits.WriteRune(character)
			}
		}
		if digits.Len() == 0 {
			return Identity{}, fmt.Errorf("invalid E.164 identity %q", value)
		}
		kind = "e164"
		canonical = "+" + digits.String()
	case platform == "signal" && signalACI.MatchString(value):
		kind = "signal_aci"
		canonical = strings.ToLower(value)
	case strings.Contains(value, "@") && platform == "whatsapp":
		kind = "jid"
		canonical = strings.ToLower(value)
	case strings.Contains(value, "@"):
		kind = "email"
		canonical = strings.ToLower(value)
	case strings.HasPrefix(value, "legacy-participant:"):
		kind = "legacy_participant"
	}
	return Identity{AccountID: accountID, Kind: kind, Canonical: canonical}, nil
}

// NormalizeRemoteConversationID preserves the legacy remote conversation form
// while removing whitespace around Signal address payloads.
func NormalizeRemoteConversationID(platform, legacyID string) string {
	value := strings.TrimSpace(legacyID)
	if platform != "signal" {
		return value
	}
	if strings.HasPrefix(value, "signal-group:") {
		return "signal-group:" + strings.TrimSpace(strings.TrimPrefix(value, "signal-group:"))
	}
	if strings.HasPrefix(value, "signal:") {
		return "signal:" + strings.TrimSpace(strings.TrimPrefix(value, "signal:"))
	}
	return value
}

// SignalIncomingSourceID computes the body-independent SHA-1 message id used
// by the Signal live bridge for incoming envelopes.
func SignalIncomingSourceID(conversationID, source string, timestamp int64) string {
	sum := sha1.Sum([]byte(strings.Join([]string{
		strings.TrimSpace(conversationID),
		strings.TrimSpace(source),
		strconv.FormatInt(timestamp, 10),
	}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

// SignalLocalAlias returns the migration remote-message key for a fabricated
// local Signal message.
func SignalLocalAlias(conversationID string, timestamp int64) string {
	return "local:" + SignalIncomingSourceID(conversationID, "me", timestamp)
}
