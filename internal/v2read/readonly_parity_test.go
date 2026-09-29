package v2read

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// parityCorpus is a generated v2 store: a schema version the client accepts
// plus random conversations and messages.
type parityCorpus struct {
	version       int
	accounts      int
	conversations int
	messages      []parityMessage
}

type parityMessage struct {
	conversation int
	body         string
	occurredAtMS int64
	outgoing     bool
	attachment   bool
	reaction     bool
}

func (c parityCorpus) String() string {
	return fmt.Sprintf("version=%d accounts=%d conversations=%d messages=%d", c.version, c.accounts, c.conversations, len(c.messages))
}

var parityWords = []string{"hello", "Héllo", "dinner", "at", "7", "8", "你好", "👍", "ok", "OK", "tomorrow", "person", "e", "%", "_"}

func (parityCorpus) Generate(r *rand.Rand, _ int) reflect.Value {
	c := parityCorpus{
		version:       sqlite.MinClientReadSchemaVersion + r.Intn(sqlite.LatestSchemaVersion()-sqlite.MinClientReadSchemaVersion+1),
		accounts:      1 + r.Intn(3),
		conversations: 1 + r.Intn(5),
	}
	for m := r.Intn(20); m > 0; m-- {
		words := make([]string, 1+r.Intn(6))
		for i := range words {
			words[i] = parityWords[r.Intn(len(parityWords))]
		}
		c.messages = append(c.messages, parityMessage{
			conversation: r.Intn(c.conversations),
			body:         strings.Join(words, " "),
			// A narrow window forces timestamp ties, which exercise the
			// message_id tie-breakers.
			occurredAtMS: sourceTestTimeMS - int64(r.Intn(50)),
			outgoing:     r.Intn(3) == 0,
			attachment:   r.Intn(4) == 0,
			reaction:     r.Intn(4) == 0,
		})
	}
	return reflect.ValueOf(c)
}

var parityBridgeKeys = []string{"google_messages", "whatsmeow", "signal"}

func seedParityCorpus(store *sqlite.Store, corpus parityCorpus) error {
	now := func() time.Time { return time.UnixMilli(sourceTestTimeMS) }
	for a := 0; a < corpus.accounts; a++ {
		accountID := fmt.Sprintf("account-%d", a)
		if err := store.UpsertAccount(sqlite.Account{
			AccountID: accountID, BridgeKey: parityBridgeKeys[a%len(parityBridgeKeys)], DisplayName: accountID,
			Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: `{}`,
			CreatedAtMS: sourceTestTimeMS, UpdatedAtMS: sourceTestTimeMS,
		}); err != nil {
			return err
		}
		if err := store.UpsertIdentity(sqlite.Identity{
			IdentityID: fmt.Sprintf("identity-%d", a), AccountID: accountID, Kind: sqlite.IdentityKind("e164"),
			CanonicalValue: fmt.Sprintf("+1555000000%d", a), RawValue: fmt.Sprintf("+1 555 000 000%d", a),
			DisplayName: fmt.Sprintf("Person %d", a), MetadataJSON: `{}`,
			CreatedAtMS: sourceTestTimeMS, UpdatedAtMS: sourceTestTimeMS,
		}); err != nil {
			return err
		}
	}
	for c := 0; c < corpus.conversations; c++ {
		account := c % corpus.accounts
		conversation := sqlite.Conversation{
			ConversationID: fmt.Sprintf("conversation-%d", c), AccountID: fmt.Sprintf("account-%d", account),
			RemoteConversationID: fmt.Sprintf("remote-conversation-%d", c), Kind: sqlite.ConversationKindDirect,
			Title: fmt.Sprintf("Person %d chat %d", account, c), NotificationMode: sqlite.NotificationModeAll,
			LastMessageAtMS: sourceTestTimeMS - int64(c), MetadataJSON: `{}`,
			CreatedAtMS: sourceTestTimeMS, UpdatedAtMS: sourceTestTimeMS,
		}
		if err := store.UpsertConversation(conversation); err != nil {
			return err
		}
		if err := store.ReplaceConversationParticipants(conversation.ConversationID, []sqlite.ConversationParticipant{{
			AccountID: conversation.AccountID, ConversationID: conversation.ConversationID,
			IdentityID: fmt.Sprintf("identity-%d", account), Role: sqlite.ParticipantRoleMember,
			DisplayName: fmt.Sprintf("Person %d", account), IsActive: true,
		}}); err != nil {
			return err
		}
	}
	messages, err := sqlite.NewMessageRepository(store, now)
	if err != nil {
		return err
	}
	reactions, err := sqlite.NewReactionRepository(store, now)
	if err != nil {
		return err
	}
	for m, spec := range corpus.messages {
		account := spec.conversation % corpus.accounts
		message := sqlite.Message{
			MessageID: fmt.Sprintf("message-%02d", m), ConversationID: fmt.Sprintf("conversation-%d", spec.conversation),
			AccountID: fmt.Sprintf("account-%d", account), RemoteMessageID: fmt.Sprintf("remote-message-%d", m),
			Direction: sqlite.MessageDirectionIncoming, Body: spec.body, State: sqlite.MessageStateActive,
			OccurredAtMS: spec.occurredAtMS,
		}
		if spec.outgoing {
			message.Direction = sqlite.MessageDirectionOutgoing
		} else {
			message.SenderIdentityID = stringPointer(fmt.Sprintf("identity-%d", account))
		}
		var attachments []sqlite.MessageAttachment
		if spec.attachment {
			size := int64(100 + m)
			attachments = append(attachments, sqlite.MessageAttachment{
				Ordinal: 0, RemoteID: fmt.Sprintf("media-%d", m), RemoteRef: []byte("opaque"),
				Filename: "file.bin", MIME: "application/octet-stream", SizeBytes: &size,
			})
		}
		if err := messages.ImportMessage(context.Background(), sqlite.MessageProjection{Message: message, Attachments: attachments}); err != nil {
			return err
		}
		if spec.reaction {
			if _, err := reactions.ApplyReaction(context.Background(), sqlite.ReactionApply{
				AccountID: message.AccountID, ConversationID: message.ConversationID, MessageID: message.MessageID,
				ReactorKey: "self", ReactorIsSelf: true, ReactorLabel: "me", Emoji: "❤️",
				Action: bridge.ReactionAdd, OccurredAtMS: spec.occurredAtMS + 1,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// readSourceCall runs one ReadSource method with arguments drawn from the
// corpus and returns its results as comparable values.
type readSourceCall struct {
	method string
	call   func(source readsource.ReadSource, corpus parityCorpus) []any
}

func parityConversationIDs(corpus parityCorpus) []string {
	ids := make([]string, 0, corpus.conversations+1)
	for c := 0; c < corpus.conversations; c++ {
		ids = append(ids, fmt.Sprintf("conversation-%d", c))
	}
	return append(ids, "conversation-missing")
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func withErr(value any, err error) []any { return []any{value, errText(err)} }

var readSourceCalls = []readSourceCall{
	{method: "ListConversations", call: func(s readsource.ReadSource, _ parityCorpus) []any {
		return withErr(s.ListConversations(50))
	}},
	{method: "SearchConversationsByMetadata", call: func(s readsource.ReadSource, _ parityCorpus) []any {
		return withErr(s.SearchConversationsByMetadata("person", 50))
	}},
	{method: "GetConversation", call: func(s readsource.ReadSource, c parityCorpus) []any {
		var out []any
		for _, id := range parityConversationIDs(c) {
			out = append(out, withErr(s.GetConversation(id))...)
		}
		return out
	}},
	{method: "GetMessagesByConversation", call: func(s readsource.ReadSource, c parityCorpus) []any {
		var out []any
		for _, id := range parityConversationIDs(c) {
			out = append(out, withErr(s.GetMessagesByConversation(id, 100))...)
		}
		return out
	}},
	{method: "GetMessagesByConversationBefore", call: func(s readsource.ReadSource, c parityCorpus) []any {
		return withErr(s.GetMessagesByConversationBefore("conversation-0", sourceTestTimeMS-10, "message-05", 100))
	}},
	{method: "GetMessagesByConversationAfter", call: func(s readsource.ReadSource, c parityCorpus) []any {
		return withErr(s.GetMessagesByConversationAfter("conversation-0", sourceTestTimeMS-40, "message-03", 100))
	}},
	{method: "GetMessagesAroundMessage", call: func(s readsource.ReadSource, c parityCorpus) []any {
		var out []any
		for m, spec := range c.messages[:min(len(c.messages), 6)] {
			out = append(out, withErr(s.GetMessagesAroundMessage(fmt.Sprintf("conversation-%d", spec.conversation), fmt.Sprintf("message-%02d", m), 3, 3))...)
		}
		return append(out, withErr(s.GetMessagesAroundMessage("conversation-0", "message-missing", 3, 3))...)
	}},
	{method: "GetMessagesByConversations", call: func(s readsource.ReadSource, c parityCorpus) []any {
		return withErr(s.GetMessagesByConversations(parityConversationIDs(c), 100))
	}},
	{method: "GetMessagesByConversationsRange", call: func(s readsource.ReadSource, c parityCorpus) []any {
		return withErr(s.GetMessagesByConversationsRange(parityConversationIDs(c), sourceTestTimeMS-30, sourceTestTimeMS, 100))
	}},
	{method: "SearchMessagesFiltered", call: func(s readsource.ReadSource, _ parityCorpus) []any {
		var out []any
		for _, filter := range []struct {
			query  string
			filter db.SearchFilter
		}{
			{query: "hello", filter: db.SearchFilter{Limit: 100}},
			{query: "e", filter: db.SearchFilter{Limit: 5}},
			{query: "%", filter: db.SearchFilter{Limit: 100}},
			{query: "o", filter: db.SearchFilter{ConversationID: "conversation-0", Limit: 100}},
			{query: "o", filter: db.SearchFilter{Phone: "+15550000000", Limit: 100}},
			{query: "o", filter: db.SearchFilter{SinceMS: sourceTestTimeMS - 20, UntilMS: sourceTestTimeMS - 5, Limit: 100}},
		} {
			out = append(out, withErr(s.SearchMessagesFiltered(filter.query, filter.filter))...)
		}
		return out
	}},
	{method: "PlatformStats", call: func(s readsource.ReadSource, _ parityCorpus) []any {
		return withErr(s.PlatformStats())
	}},
	{method: "MessageCount", call: func(s readsource.ReadSource, _ parityCorpus) []any {
		var out []any
		for _, platform := range []string{"", "sms", "whatsapp", "signal"} {
			out = append(out, withErr(s.MessageCount(platform))...)
		}
		return out
	}},
	{method: "ConversationCount", call: func(s readsource.ReadSource, _ parityCorpus) []any {
		var out []any
		for _, platform := range []string{"", "sms", "whatsapp", "signal"} {
			out = append(out, withErr(s.ConversationCount(platform))...)
		}
		return out
	}},
	{method: "LatestTimestamp", call: func(s readsource.ReadSource, _ parityCorpus) []any {
		var out []any
		for _, platform := range []string{"", "sms", "whatsapp", "signal"} {
			out = append(out, withErr(s.LatestTimestamp(platform))...)
		}
		return out
	}},
	{method: "LatestConversationPreviews", call: func(s readsource.ReadSource, c parityCorpus) []any {
		return withErr(s.LatestConversationPreviews(parityConversationIDs(c)))
	}},
}

// TestReadSourceCallsCoverInterface keeps readSourceCalls covering every
// ReadSource method, so the parity differential cannot silently skip one.
func TestReadSourceCallsCoverInterface(t *testing.T) {
	interfaceType := reflect.TypeOf((*readsource.ReadSource)(nil)).Elem()
	var want, got []string
	for i := 0; i < interfaceType.NumMethod(); i++ {
		want = append(want, interfaceType.Method(i).Name)
	}
	for _, call := range readSourceCalls {
		got = append(got, call.method)
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readSourceCalls cover %v, ReadSource has %v", got, want)
	}
}

// TestReadOnlyAttachMatchesWritableReads is the I7 differential for the v2
// read path: on generated corpora at every schema version the client accepts,
// every ReadSource method returns the same results through a read-only attach
// of the store as through the owner's handle (sqlite.Open) on a byte copy of
// the same file. For a store older than the build, the owner's handle has
// migrated its copy first, so this also shows that serving an older store
// read-only answers exactly what the migrated store would.
func TestReadOnlyAttachMatchesWritableReads(t *testing.T) {
	versionsSeen := map[int]int{}
	largestCorpus := 0
	property := func(corpus parityCorpus) bool {
		largestCorpus = max(largestCorpus, len(corpus.messages))
		dir := t.TempDir()
		path := filepath.Join(dir, "store.sqlite3")
		if err := sqlite.BuildStoreAtVersion(path, corpus.version, func(store *sqlite.Store) error {
			return seedParityCorpus(store, corpus)
		}); err != nil {
			t.Logf("%s: build: %v", corpus, err)
			return false
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Logf("read fixture: %v", err)
			return false
		}
		ownerPath := filepath.Join(dir, "owner.sqlite3")
		if err := os.WriteFile(ownerPath, content, 0o600); err != nil {
			t.Logf("copy fixture: %v", err)
			return false
		}

		readOnlyStore, info, err := sqlite.OpenReadOnly(path)
		if err != nil {
			t.Logf("%s: OpenReadOnly: %v", corpus, err)
			return false
		}
		defer readOnlyStore.Close()
		if info.SchemaVersion != corpus.version {
			t.Logf("%s: attached at schema %d", corpus, info.SchemaVersion)
			return false
		}
		ownerStore, err := sqlite.Open(ownerPath)
		if err != nil {
			t.Logf("%s: Open(owner copy): %v", corpus, err)
			return false
		}
		defer ownerStore.Close()

		readOnlySource, ownerSource := New(readOnlyStore), New(ownerStore)
		for _, call := range readSourceCalls {
			got, want := call.call(readOnlySource, corpus), call.call(ownerSource, corpus)
			if !reflect.DeepEqual(got, want) {
				t.Logf("%s: %s differs:\nread-only: %#v\nowner:     %#v", corpus, call.method, got, want)
				return false
			}
		}
		versionsSeen[corpus.version]++
		return true
	}
	config := &quick.Config{MaxCount: 8, Rand: rand.New(rand.NewSource(7011))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
	if largestCorpus < 10 {
		t.Fatalf("largest generated corpus had %d messages; the differential is close to vacuous", largestCorpus)
	}
	for version := sqlite.MinClientReadSchemaVersion; version <= sqlite.LatestSchemaVersion(); version++ {
		if versionsSeen[version] == 0 {
			t.Fatalf("no corpus at schema %d (seen %v); widen the generator", version, versionsSeen)
		}
	}
}
