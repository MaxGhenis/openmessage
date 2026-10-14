package v2read

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite/sqlitetest"
)

// randomStore is a sqlitetest random store read through both the batched
// Source and the per-row reference.
type randomStore struct {
	*sqlitetest.RandomStore
	source *Source
	ref    referenceSource
}

func buildRandomStore(t *testing.T, seed int64, shape sqlitetest.Shape) *randomStore {
	t.Helper()
	built := sqlitetest.BuildRandomStore(t, seed, shape)
	source := New(built.Store)
	return &randomStore{RandomStore: built, source: source, ref: referenceSource{s: source}}
}

// The batched mapping is a pure refactor of the per-row mapping: for every
// store and every read, Source returns DTOs byte-identical (as JSON, and
// reflect.DeepEqual) to referenceSource, the pre-batching code kept verbatim,
// and fails exactly when it fails. These properties run each read surface over
// random dense stores (see buildRandomStore) with every limit, cursor and ID
// shape the callers pass.

// readBatchSizeForTest mirrors sqlite.readBatchSize, the keys bound into one
// batched statement.
const readBatchSizeForTest = 500

func differentialSeeds(t *testing.T) *quick.Config {
	return &quick.Config{MaxCount: sqlitetest.PropertyRuns(30), Rand: rand.New(rand.NewSource(20261009))}
}

// sameResult reports whether two (value, error) results agree: both errors or
// neither, and byte-identical JSON plus deep equality for values.
func sameResult(t *testing.T, label string, got any, gotErr error, want any, wantErr error) bool {
	t.Helper()
	if (gotErr != nil) != (wantErr != nil) {
		t.Errorf("%s: error = %v, reference error = %v", label, gotErr, wantErr)
		return false
	}
	if gotErr != nil {
		return true
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s: marshal got: %v", label, err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("%s: marshal reference: %v", label, err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("%s: JSON differs\n got: %s\nwant: %s", label, gotJSON, wantJSON)
		return false
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: values differ\n got: %#v\nwant: %#v", label, got, want)
		return false
	}
	return true
}

func TestConversationMappingMatchesPerRowReferenceProperty(t *testing.T) {
	property := func(seed int64) bool {
		rs := buildRandomStore(t, seed, sqlitetest.DenseShape)
		ok := true
		total := len(rs.ConversationIDs)
		for _, limit := range []int{-1, 0, 1, 2, 3, total / 2, total, total + 3, math.MaxInt} {
			got, gotErr := rs.source.ListConversations(limit)
			want, wantErr := rs.ref.ListConversations(limit)
			ok = sameResult(t, fmt.Sprintf("seed %d ListConversations(%d)", seed, limit), got, gotErr, want, wantErr) && ok
		}

		// The platform oracle filters one per-row ListConversations(MaxInt),
		// exactly as referenceSource.ListPlatformConversations does per call.
		everything, err := rs.ref.ListConversations(math.MaxInt)
		if err != nil {
			t.Fatal(err)
		}
		platforms := []string{"sms", "whatsapp", "signal", "gchat", "imessage", "custom_bridge", "", " sms", "nope"}
		for _, platform := range platforms {
			for _, limit := range []int{0, 1, 2, total, math.MaxInt} {
				want := []*db.Conversation{}
				for _, conversation := range everything {
					if limit > 0 && len(want) < limit && conversation.SourcePlatform == platform {
						want = append(want, conversation)
					}
				}
				got, gotErr := rs.source.ListPlatformConversations(platform, limit)
				ok = sameResult(t, fmt.Sprintf("seed %d ListPlatformConversations(%q, %d)", seed, platform, limit), got, gotErr, want, nil) && ok
			}
		}
		// And the oracle itself once, against the reference method.
		gotRef, gotRefErr := rs.ref.ListPlatformConversations("sms", 2)
		wantRef := []*db.Conversation{}
		for _, conversation := range everything {
			if len(wantRef) < 2 && conversation.SourcePlatform == "sms" {
				wantRef = append(wantRef, conversation)
			}
		}
		ok = sameResult(t, fmt.Sprintf("seed %d reference ListPlatformConversations", seed), gotRef, gotRefErr, wantRef, nil) && ok

		for _, term := range rs.SearchTerms {
			for _, limit := range []int{0, 1, 5, 500} {
				got, gotErr := rs.source.SearchConversationsByMetadata(term, limit)
				want, wantErr := rs.ref.SearchConversationsByMetadata(term, limit)
				ok = sameResult(t, fmt.Sprintf("seed %d SearchConversationsByMetadata(%q, %d)", seed, term, limit), got, gotErr, want, wantErr) && ok
			}
		}

		ids := conversationIDProbes(rs)
		for _, id := range ids {
			got, gotErr := rs.source.GetConversation(id)
			want, wantErr := rs.ref.GetConversation(id)
			ok = sameResult(t, fmt.Sprintf("seed %d GetConversation(%q)", seed, id), got, gotErr, want, wantErr) && ok
		}
		got, gotErr := rs.source.GetConversationsByID(ids)
		want, wantErr := rs.ref.GetConversationsByID(ids)
		ok = sameResult(t, fmt.Sprintf("seed %d GetConversationsByID", seed), got, gotErr, want, wantErr) && ok
		return ok
	}
	if err := quick.Check(property, differentialSeeds(t)); err != nil {
		t.Fatal(err)
	}
}

func TestMessageMappingMatchesPerRowReferenceProperty(t *testing.T) {
	property := func(seed int64) bool {
		rs := buildRandomStore(t, seed, sqlitetest.DenseShape)
		rng := rand.New(rand.NewSource(seed))
		ok := true
		for _, conversationID := range conversationIDProbes(rs) {
			for _, limit := range []int{0, 1, 3, 100} {
				got, gotErr := rs.source.GetMessagesByConversation(conversationID, limit)
				want, wantErr := rs.ref.GetMessagesByConversation(conversationID, limit)
				ok = sameResult(t, fmt.Sprintf("seed %d GetMessagesByConversation(%q, %d)", seed, conversationID, limit), got, gotErr, want, wantErr) && ok
			}
		}
		for i := 0; i < 12 && len(rs.Messages) > 0; i++ {
			anchor := rs.Messages[rng.Intn(len(rs.Messages))]
			limit := 1 + rng.Intn(6)
			cursorID := anchor.MessageID
			if i%3 == 0 {
				cursorID = ""
			}
			got, gotErr := rs.source.GetMessagesByConversationBefore(anchor.ConversationID, anchor.OccurredAtMS, cursorID, limit)
			want, wantErr := rs.ref.GetMessagesByConversationBefore(anchor.ConversationID, anchor.OccurredAtMS, cursorID, limit)
			ok = sameResult(t, fmt.Sprintf("seed %d Before(%q)", seed, anchor.MessageID), got, gotErr, want, wantErr) && ok

			got, gotErr = rs.source.GetMessagesByConversationAfter(anchor.ConversationID, anchor.OccurredAtMS, cursorID, limit)
			want, wantErr = rs.ref.GetMessagesByConversationAfter(anchor.ConversationID, anchor.OccurredAtMS, cursorID, limit)
			ok = sameResult(t, fmt.Sprintf("seed %d After(%q)", seed, anchor.MessageID), got, gotErr, want, wantErr) && ok

			before, after := 1+i%3, 2-i%3
			got, gotErr = rs.source.GetMessagesAroundMessage(anchor.ConversationID, anchor.MessageID, before, after)
			want, wantErr = rs.ref.GetMessagesAroundMessage(anchor.ConversationID, anchor.MessageID, before, after)
			ok = sameResult(t, fmt.Sprintf("seed %d Around(%q, %d, %d)", seed, anchor.MessageID, before, after), got, gotErr, want, wantErr) && ok
		}
		if _, err := rs.source.GetMessagesAroundMessage("missing", "missing", 1, 1); !errors.Is(err, db.ErrMessageNotFound) {
			t.Errorf("seed %d Around(missing) error = %v, want ErrMessageNotFound", seed, err)
			ok = false
		}

		for _, term := range append([]string{""}, rs.SearchTerms...) {
			for _, filter := range []db.SearchFilter{
				{Limit: 5}, {Limit: 500}, {SinceMS: 2_000, UntilMS: 4_000, Limit: 50},
			} {
				got, gotErr := rs.source.SearchMessagesFiltered(term, filter)
				want, wantErr := rs.ref.SearchMessagesFiltered(term, filter)
				ok = sameResult(t, fmt.Sprintf("seed %d SearchMessagesFiltered(%q, %+v)", seed, term, filter), got, gotErr, want, wantErr) && ok
			}
		}

		ids := conversationIDProbes(rs)
		for _, window := range [][2]int64{{0, 0}, {2_000, 0}, {0, 4_000}, {2_000, 4_000}} {
			for _, limit := range []int{1, 7, 2_000} {
				got, gotErr := rs.source.GetMessagesByConversationsRange(ids, window[0], window[1], limit)
				want, wantErr := rs.ref.GetMessagesByConversationsRange(ids, window[0], window[1], limit)
				ok = sameResult(t, fmt.Sprintf("seed %d Range(%v, %d)", seed, window, limit), got, gotErr, want, wantErr) && ok
			}
		}

		gotLatest, gotErr := rs.source.LatestMessagesByConversation(ids)
		wantLatest, wantErr := rs.ref.LatestMessagesByConversation(ids)
		ok = sameResult(t, fmt.Sprintf("seed %d LatestMessagesByConversation", seed), gotLatest, gotErr, wantLatest, wantErr) && ok

		previewIDs := append(append([]string{}, ids...), "", "  ", ids[0]+" ", " "+ids[len(ids)-1])
		gotPreviews, gotErr := rs.source.LatestConversationPreviews(previewIDs)
		wantPreviews, wantErr := rs.ref.LatestConversationPreviews(previewIDs)
		ok = sameResult(t, fmt.Sprintf("seed %d LatestConversationPreviews", seed), gotPreviews, gotErr, wantPreviews, wantErr) && ok
		return ok
	}
	if err := quick.Check(property, differentialSeeds(t)); err != nil {
		t.Fatal(err)
	}
}

// Batches split at readBatchSize keys: reads over more conversations and
// messages than one batch holds must still match the reference.
func TestMappingAcrossReadBatchBoundariesMatchesReference(t *testing.T) {
	shape := sqlitetest.Shape{MaxAccounts: 1, MaxIdentities: 12}
	rs := buildRandomStore(t, 7, shape)
	raw := sqlitetest.OpenRaw(t, rs.Path)
	accountID := rs.Accounts[0].ID
	identities := []string{}
	rows, err := raw.Query(`SELECT identity_id FROM identities WHERE account_id = ?`, accountID)
	if err != nil {
		t.Fatalf("list identities: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		identities = append(identities, id)
	}
	_ = rows.Close()

	const conversations = readBatchSizeForTest*2 + 37
	const bigThread = readBatchSizeForTest*2 + 11
	var ids []string
	tx, err := raw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for c := 0; c < conversations; c++ {
		id := fmt.Sprintf("conv-%04d", c)
		ids = append(ids, id)
		if _, err := tx.Exec(`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, title, last_message_at_ms, metadata_json, created_at_ms, updated_at_ms)
			VALUES (?, ?, ?, 'direct', '', ?, '{}', 1, 1)`, id, accountID, "remote-"+id, c%17); err != nil {
			t.Fatal(err)
		}
		for i, identityID := range identities {
			if (c+i)%3 != 0 {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id, display_name)
				VALUES (?, ?, ?, '')`, accountID, id, identityID); err != nil {
				t.Fatal(err)
			}
		}
		messages := 1
		if c == 0 {
			messages = bigThread
		}
		for m := 0; m < messages; m++ {
			messageID := fmt.Sprintf("msg-%04d-%04d", c, m)
			direction := "incoming"
			if m%2 == 1 {
				direction = "outgoing"
			}
			if _, err := tx.Exec(`INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, direction, body, occurred_at_ms, created_at_ms, updated_at_ms)
				VALUES (?, ?, ?, ?, ?, ?, ?, 1, 1)`, messageID, id, accountID, "r-"+messageID, direction, fmt.Sprintf("body %d", m), 1+m%29); err != nil {
				t.Fatal(err)
			}
			if m%3 == 0 {
				if _, err := tx.Exec(`INSERT INTO message_attachments (message_id, ordinal, mime, created_at_ms, updated_at_ms)
					VALUES (?, 0, 'image/png', 1, 1)`, messageID); err != nil {
					t.Fatal(err)
				}
			}
			if direction == "outgoing" && m%5 != 0 {
				if _, err := tx.Exec(`INSERT INTO outbox (outbox_id, account_id, conversation_id, kind, idempotency_key, payload_hash, operation, state, local_message_id, transport_request_id, scheduled_for_ms, created_at_ms, updated_at_ms)
					VALUES (?, ?, ?, 'text', ?, 'h', 'send_text', 'confirmed', ?, ?, 1, 1, 1)`,
					"ob-"+messageID, accountID, id, "k-"+messageID, messageID, "t-"+messageID); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got, gotErr := rs.source.ListConversations(conversations)
	want, wantErr := rs.ref.ListConversations(conversations)
	sameResult(t, "ListConversations across batches", got, gotErr, want, wantErr)

	gotPreviews, gotErr := rs.source.LatestConversationPreviews(ids)
	wantPreviews, wantErr := rs.ref.LatestConversationPreviews(ids)
	sameResult(t, "LatestConversationPreviews across batches", gotPreviews, gotErr, wantPreviews, wantErr)

	gotMessages, gotErr := rs.source.GetMessagesByConversation("conv-0000", bigThread)
	wantMessages, wantErr := rs.ref.GetMessagesByConversation("conv-0000", bigThread)
	sameResult(t, "GetMessagesByConversation across batches", gotMessages, gotErr, wantMessages, wantErr)
	if len(gotMessages) != bigThread {
		t.Fatalf("big thread page = %d messages, want %d", len(gotMessages), bigThread)
	}

	gotByID, gotErr := rs.source.GetConversationsByID(ids)
	wantByID, wantErr := rs.ref.GetConversationsByID(ids)
	sameResult(t, "GetConversationsByID across batches", gotByID, gotErr, wantByID, wantErr)

	gotLatest, gotErr := rs.source.LatestMessagesByConversation(ids)
	wantLatest, wantErr := rs.ref.LatestMessagesByConversation(ids)
	sameResult(t, "LatestMessagesByConversation across batches", gotLatest, gotErr, wantLatest, wantErr)
}

// conversationIDProbes is every conversation ID plus the ID shapes callers
// pass that are not v2 IDs: legacy remote IDs (resolved through the alias
// fallback), padded IDs, blank and unknown IDs, and a repeat.
func conversationIDProbes(rs *randomStore) []string {
	ids := append([]string{}, rs.ConversationIDs...)
	ids = append(ids, "missing", "", " ", "remote-missing")
	if len(rs.RemoteIDs) > 0 {
		ids = append(ids, rs.RemoteIDs[0], " "+rs.RemoteIDs[len(rs.RemoteIDs)-1])
	}
	if len(rs.ConversationIDs) > 0 {
		ids = append(ids, " "+rs.ConversationIDs[0], rs.ConversationIDs[0])
	}
	return ids
}

// The differential properties are only as strong as the stores they run on:
// across the seeds they use, the generator must produce every mapped field
// and every edge the batching could get wrong.
func TestRandomStoresCoverEveryMappedEdge(t *testing.T) {
	seen := map[string]int{}
	config := differentialSeeds(t)
	for i := 0; i < config.MaxCount; i++ {
		rs := buildRandomStore(t, config.Rand.Int63(), sqlitetest.DenseShape)
		conversations, err := rs.source.ListConversations(math.MaxInt)
		if err != nil {
			t.Fatal(err)
		}
		for _, conversation := range conversations {
			var participants []participantDTO
			if err := json.Unmarshal([]byte(conversation.Participants), &participants); err != nil {
				t.Fatal(err)
			}
			switch {
			case len(participants) == 0:
				seen["conversation without participants"]++
			case len(participants) > 1:
				seen["conversation with several participants"]++
			}
			for _, participant := range participants {
				if participant.IsMe {
					seen["self participant"]++
				}
			}
			if !conversation.IsGroup && conversation.Name != "" {
				seen["named direct conversation"]++
			}
			messages, err := rs.source.GetMessagesByConversation(conversation.ConversationID, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range messages {
				if message.Status != "" {
					seen["status "+message.Status]++
				} else if message.IsFromMe {
					seen["outgoing without status"]++
				}
				if message.MediaID != "" {
					seen["media"]++
				}
				if message.SenderName != "" || message.SenderNumber != "" {
					seen["attributed sender"]++
				}
				if message.Reactions != "" {
					seen["reactions"]++
				}
				if message.ReplyToID != "" {
					seen["reply"]++
				}
			}
		}
		raw := sqlitetest.OpenRaw(t, rs.Path)
		for label, query := range map[string]string{
			"outbox rows tied on created_at":  `SELECT COUNT(*) FROM (SELECT 1 FROM outbox GROUP BY account_id, local_message_id, created_at_ms HAVING COUNT(*) > 1)`,
			"outbox row of another account":   `SELECT COUNT(*) FROM outbox o JOIN messages m ON m.message_id = o.local_message_id WHERE o.account_id <> m.account_id`,
			"attachment without ordinal 0":    `SELECT COUNT(DISTINCT message_id) FROM message_attachments a WHERE NOT EXISTS (SELECT 1 FROM message_attachments z WHERE z.message_id = a.message_id AND z.ordinal = 0)`,
			"inactive participant":            `SELECT COUNT(*) FROM conversation_participants WHERE is_active = 0`,
			"padded participant display name": `SELECT COUNT(*) FROM conversation_participants WHERE display_name <> trim(display_name) AND trim(display_name) <> ''`,
			"messages tied on occurred_at":    `SELECT COUNT(*) FROM (SELECT 1 FROM messages GROUP BY conversation_id, occurred_at_ms HAVING COUNT(*) > 1)`,
			"conversations tied on recency":   `SELECT COUNT(*) FROM (SELECT 1 FROM conversations GROUP BY last_message_at_ms HAVING COUNT(*) > 1)`,
			"removed reaction":                `SELECT COUNT(*) FROM reactions WHERE state = 'removed'`,
			"two accounts on one platform":    `SELECT COUNT(*) FROM (SELECT 1 FROM accounts GROUP BY trim(bridge_key) HAVING COUNT(*) > 1)`,
			"padded stored conversation ID":   `SELECT COUNT(*) FROM conversations WHERE conversation_id <> trim(conversation_id)`,
		} {
			var count int
			if err := raw.QueryRow(query).Scan(&count); err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			seen[label] += count
		}
	}
	for _, want := range []string{
		"conversation without participants", "conversation with several participants", "self participant",
		"named direct conversation", "status sending", "status sent", "status failed", "outgoing without status",
		"media", "attributed sender", "reactions", "reply",
		"outbox rows tied on created_at", "outbox row of another account", "attachment without ordinal 0",
		"inactive participant", "padded participant display name", "messages tied on occurred_at",
		"conversations tied on recency", "removed reaction", "two accounts on one platform",
		"padded stored conversation ID",
	} {
		if seen[want] == 0 {
			t.Errorf("random stores never produced %q (seen: %v)", want, seen)
		}
	}
}
