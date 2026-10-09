package sqlitetest

import (
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// RandomStore is a migrated v2 store filled with seeded random rows, written
// straight through SQL (foreign keys on) so the generator controls what the
// repository write paths would normalize away: timestamp and created_at ties,
// blank and padded display names, inactive participants, attachments missing
// ordinal 0, several outbox rows per local message, outbox rows of another
// account naming the same local message, removed reactions.
type RandomStore struct {
	Path            string
	Store           *sqlite.Store
	Accounts        []RandomAccount
	ConversationIDs []string
	// RemoteIDs are the conversations' remote (legacy-shaped) IDs.
	RemoteIDs  []string
	MessageIDs []string
	Messages   []sqlite.Message
	// SearchTerms are substrings of stored titles, names, addresses and
	// bodies, plus case variants and LIKE metacharacters.
	SearchTerms []string
}

// RandomAccount is one generated account.
type RandomAccount struct {
	ID        string
	BridgeKey string
}

// bridgeKeys includes two accounts that map to one platform ("sms") and a
// padded key that platformForBridgeKey trims.
var randomBridgeKeys = []string{
	"google_messages", "whatsmeow", "signal_cli", "gchat", "imessage",
	"custom_bridge", " google_messages ",
}

var randomNames = []string{
	"", "", " ", "Alice", "alice", "Bob <b@x>", "Zoë", "ZOË", "李雷", "O'Brien \"OB\"",
	"  padded  ", "name", "number", "é", "Al", "x&y",
}

var randomBodies = []string{
	"", "", "hi", "  spaced   out\n body ", "Photo", "naïve café", "emoji 👍🏽",
	strings.Repeat("long ", 40), "tab\there", "Alice said hi",
}

var randomMIMEs = []string{"image/png", "IMAGE/JPEG", "video/mp4", "audio/ogg", "application/pdf"}

var outboxStates = []string{
	"queued", "dispatching", "not_dispatched", "uncertain",
	"confirmed", "store_failed", "rejected", "canceled",
}

// Shape bounds a random store: each account gets up to MaxIdentities
// identities and MaxConversations conversations, each with up to MaxMessages
// messages.
type Shape struct {
	MaxAccounts      int
	MaxIdentities    int
	MaxConversations int
	MaxMessages      int
}

// DenseShape is small enough to build dozens of stores per test and dense
// enough that every ordering column ties.
var DenseShape = Shape{MaxAccounts: 4, MaxIdentities: 12, MaxConversations: 14, MaxMessages: 18}

// BuildRandomStore creates and fills a store under t.TempDir() from seed.
func BuildRandomStore(t testing.TB, seed int64, shape Shape) *RandomStore {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	raw := OpenRaw(t, path)

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := raw.Exec(query, args...); err != nil {
			t.Fatalf("seed %d: %v\n%s %v", seed, err, query, args)
		}
	}
	pick := func(values []string) string { return values[rng.Intn(len(values))] }
	maybe := func(p float64) bool { return rng.Float64() < p }
	// Small value ranges force ties in every ordering column.
	tsChoice := func() int64 { return int64(1_000 * (1 + rng.Intn(6))) }

	rs := &RandomStore{Path: path, Store: store}

	accountCount := 1 + rng.Intn(shape.MaxAccounts)
	type identityRow struct{ id, account string }
	var identities []identityRow
	for a := 0; a < accountCount; a++ {
		account := RandomAccount{ID: fmt.Sprintf("acct-%d", a), BridgeKey: pick(randomBridgeKeys)}
		rs.Accounts = append(rs.Accounts, account)
		exec(`INSERT INTO accounts (account_id, bridge_key, display_name, mode, enabled, config_json, created_at_ms, updated_at_ms)
			VALUES (?, ?, ?, 'live', 1, '{}', 1, 1)`, account.ID, account.BridgeKey, account.ID)
		selfMade := false
		for i := 0; i < rng.Intn(shape.MaxIdentities+1); i++ {
			id := fmt.Sprintf("id-%d-%02d-%c", a, i, 'a'+rune(rng.Intn(26)))
			canonical := fmt.Sprintf("+1555%07d", rng.Intn(40))
			if maybe(0.2) {
				canonical = strings.ToLower(pick(randomNames[3:])) + "@example.com"
			}
			isSelf := !selfMade && maybe(0.25)
			selfMade = selfMade || isSelf
			exec(`INSERT OR IGNORE INTO identities (identity_id, account_id, kind, canonical_value, raw_value, display_name, is_self, metadata_json, created_at_ms, updated_at_ms)
				VALUES (?, ?, 'e164', ?, ?, ?, ?, '{}', 1, 1)`,
				id, account.ID, canonical, canonical, pick(randomNames), BoolInt(isSelf))
			identities = append(identities, identityRow{id: id, account: account.ID})
		}
	}
	// INSERT OR IGNORE may have skipped duplicate canonical values: keep only
	// the identities that exist.
	existing := map[string]bool{}
	rows, err := raw.Query(`SELECT identity_id FROM identities`)
	if err != nil {
		t.Fatalf("list identities: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan identity: %v", err)
		}
		existing[id] = true
	}
	_ = rows.Close()
	identitiesOf := map[string][]string{}
	var allIdentities []string
	for _, identity := range identities {
		if existing[identity.id] {
			identitiesOf[identity.account] = append(identitiesOf[identity.account], identity.id)
			allIdentities = append(allIdentities, identity.id)
		}
	}

	kinds := []string{"direct", "direct", "group", "broadcast", "system"}
	modes := []string{"all", "mentions", "muted"}
	outboxN := 0
	for _, account := range rs.Accounts {
		accountIdentities := identitiesOf[account.ID]
		for c := 0; c < rng.Intn(shape.MaxConversations+1); c++ {
			conversationID := fmt.Sprintf("%c%s-conv-%02d", 'a'+rune(rng.Intn(26)), account.ID, c)
			remoteID := fmt.Sprintf("remote-%s-%02d", account.ID, c)
			title := ""
			if maybe(0.4) {
				title = pick(randomNames)
			}
			var archived any
			if maybe(0.2) {
				archived = tsChoice()
			}
			exec(`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, title, notification_mode, is_favorite, archived_at_ms, last_message_at_ms, metadata_json, created_at_ms, updated_at_ms)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', 1, 1)`,
				conversationID, account.ID, remoteID, pick(kinds), title, pick(modes), BoolInt(maybe(0.3)), archived, tsChoice()-1_000)
			rs.ConversationIDs = append(rs.ConversationIDs, conversationID)
			rs.RemoteIDs = append(rs.RemoteIDs, remoteID)
			if title != "" {
				rs.SearchTerms = append(rs.SearchTerms, title)
			}

			for _, identityID := range accountIdentities {
				if !maybe(0.35) {
					continue
				}
				exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id, role, display_name, is_active)
					VALUES (?, ?, ?, ?, ?, ?)`,
					account.ID, conversationID, identityID, pick([]string{"member", "admin", "owner", "unknown"}),
					pick(randomNames), BoolInt(maybe(0.8)))
			}

			for m := 0; m < rng.Intn(shape.MaxMessages+1); m++ {
				messageID := fmt.Sprintf("%c-msg-%s-%03d", 'a'+rune(rng.Intn(26)), conversationID, m)
				direction := "incoming"
				if maybe(0.4) {
					direction = "outgoing"
				}
				var sender *string
				if len(accountIdentities) > 0 && maybe(0.7) {
					id := accountIdentities[rng.Intn(len(accountIdentities))]
					sender = &id
				}
				var replyTo *string
				if maybe(0.15) {
					reply := fmt.Sprintf("reply-%d", rng.Intn(5))
					replyTo = &reply
				}
				body := pick(randomBodies)
				occurred := tsChoice()
				message := sqlite.Message{
					MessageID: messageID, ConversationID: conversationID, AccountID: account.ID,
					RemoteMessageID: "r-" + messageID, SenderIdentityID: sender,
					Direction: sqlite.MessageDirection(direction), Body: body, ReplyToRemoteID: replyTo,
					State: sqlite.MessageState(pick([]string{"active", "active", "edited", "deleted"})), OccurredAtMS: occurred,
				}
				exec(`INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction, body, reply_to_remote_id, state, occurred_at_ms, created_at_ms, updated_at_ms)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1)`,
					message.MessageID, message.ConversationID, message.AccountID, message.RemoteMessageID,
					message.SenderIdentityID, direction, body, message.ReplyToRemoteID, string(message.State), occurred)
				rs.MessageIDs = append(rs.MessageIDs, messageID)
				rs.Messages = append(rs.Messages, message)
				if body != "" && maybe(0.3) {
					rs.SearchTerms = append(rs.SearchTerms, strings.Fields(body + " x")[0])
				}

				// Attachments: sometimes ordinal 0, sometimes only later ordinals.
				for _, ordinal := range []int{0, 1, 2} {
					if !maybe(0.25) {
						continue
					}
					exec(`INSERT INTO message_attachments (message_id, ordinal, remote_id, filename, mime, size_bytes, state, created_at_ms, updated_at_ms)
						VALUES (?, ?, ?, ?, ?, ?, 'pending', 1, 1)`,
						messageID, ordinal, fmt.Sprintf("media-%d", ordinal), "f.bin", pick(randomMIMEs), rng.Intn(1000))
				}

				// Outbox rows, including several per local message with
				// created_at ties (outbox_id breaks them) and rows on incoming
				// messages, which the mapping must ignore.
				if maybe(0.6) {
					for k := 0; k < 1+rng.Intn(3); k++ {
						insertRandomOutbox(exec, rng, &outboxN, account.ID, conversationID, messageID, tsChoice())
					}
				}
				// Another account's outbox row naming this message: never read.
				if len(rs.Accounts) > 1 && maybe(0.1) {
					other := rs.Accounts[rng.Intn(len(rs.Accounts))].ID
					insertRandomOutbox(exec, rng, &outboxN, other, conversationID, messageID, tsChoice())
				}

				// Reactions, some removed, some by identities of any account.
				for r := 0; r < rng.Intn(3); r++ {
					var reactor any
					if len(allIdentities) > 0 && maybe(0.6) {
						reactor = allIdentities[rng.Intn(len(allIdentities))]
					}
					state := "active"
					if maybe(0.2) {
						state = "removed"
					}
					exec(`INSERT OR IGNORE INTO reactions (message_id, reactor_key, account_id, conversation_id, reactor_identity_id, reactor_is_self, reactor_label, emoji, state, occurred_at_ms, created_at_ms, updated_at_ms)
						VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1)`,
						messageID, fmt.Sprintf("reactor-%d", rng.Intn(4)), account.ID, conversationID, reactor,
						BoolInt(maybe(0.2)), pick(randomNames), pick([]string{"👍", "❤️", "😂"}), state, tsChoice())
				}
			}
		}
	}
	// A conversation can be newer than its newest message or never have had
	// one; neither changes the reads, which order by the stored columns.
	for _, identity := range allIdentities {
		if maybe(0.1) {
			rs.SearchTerms = append(rs.SearchTerms, identity[len(identity)-1:])
		}
	}
	rs.SearchTerms = append(rs.SearchTerms, "alice", "ALICE", "zoë", "555", "@example", "%", "_", `\`)
	return rs
}

func insertRandomOutbox(
	exec func(string, ...any),
	rng *rand.Rand,
	counter *int,
	accountID, conversationID, localMessageID string,
	createdAtMS int64,
) {
	*counter++
	state := outboxStates[rng.Intn(len(outboxStates))]
	var leaseOwner, leaseToken, leaseExpires, nextAttempt any
	if state == "dispatching" {
		leaseOwner, leaseToken, leaseExpires = "owner", fmt.Sprintf("token-%d", *counter), int64(9_999)
	}
	if state == "not_dispatched" {
		nextAttempt = int64(9_999)
	}
	exec(`INSERT INTO outbox (outbox_id, account_id, conversation_id, kind, idempotency_key, payload_hash, operation, state, local_message_id, transport_request_id, lease_owner, lease_token, lease_expires_at_ms, scheduled_for_ms, next_attempt_at_ms, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, 'text', ?, 'hash', 'send_text', ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		fmt.Sprintf("%c-outbox-%04d", 'a'+rune(rng.Intn(26)), *counter), accountID, conversationID,
		fmt.Sprintf("key-%d", *counter), state, localMessageID, fmt.Sprintf("req-%d", *counter),
		leaseOwner, leaseToken, leaseExpires, nextAttempt, createdAtMS, createdAtMS)
}

// OpenRaw opens path as a plain database/sql handle with foreign keys on, for
// tests that write rows the repositories would not.
func OpenRaw(t testing.TB, path string) *sql.DB {
	t.Helper()
	query := url.Values{}
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", "busy_timeout(5000)")
	raw, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String())
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

// BoolInt is SQLite's integer boolean.
func BoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
