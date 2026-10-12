package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newFileStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "messages.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// observer opens a second connection to the store file; its data_version
// moves exactly when another connection commits.
func observer(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func dataVersionOf(t *testing.T, conn *sql.DB) int64 {
	t.Helper()
	var v int64
	if err := conn.QueryRow(`PRAGMA data_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func ftsRows(t *testing.T, s *Store, id string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT body FROM messages_fts WHERE message_id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func sampleIMessageChanges() IMessageChanges {
	return IMessageChanges{
		Conversations: []IMessageConversationChange{{
			IMessageConversation: IMessageConversation{ConversationID: "imessage:c1", Name: "Alice", Participants: `[{"name":"a","number":"+1"}]`, LastMessageTS: 2000},
			SetLastMessageTS:     true,
		}},
		Messages: []IMessageRow{
			{MessageID: "imessage:m1", ConversationID: "imessage:c1", SenderName: "a", SenderNumber: "+1", Body: "hello", TimestampMS: 1000, SourceID: "m1", Reactions: `[{"emoji":"❤️","count":1,"actors":["me"]}]`},
			{MessageID: "imessage:m2", ConversationID: "imessage:c1", SenderName: "Me", Body: "hi back", TimestampMS: 2000, IsFromMe: true, SourceID: "m2"},
		},
	}
}

func TestApplyIMessageChangesWritesOnlyDifferences(t *testing.T) {
	s, path := newFileStore(t)
	obs := observer(t, path)
	res, err := s.ApplyIMessageChanges(sampleIMessageChanges())
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("apply: %+v %v", res, err)
	}
	if res.ConversationsCreated != 1 || res.MessagesInserted != 2 || res.Written() != 2 {
		t.Fatalf("first apply = %+v", res)
	}
	m1, _ := s.GetMessageByID("imessage:m1")
	if m1 == nil || m1.Reactions == "" || m1.Status != "delivered" || m1.SourcePlatform != "imessage" {
		t.Fatalf("m1 = %+v", m1)
	}
	if got := ftsRows(t, s, "imessage:m1"); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("search entry = %v", got)
	}

	before := dataVersionOf(t, obs)
	res, err = s.ApplyIMessageChanges(sampleIMessageChanges())
	if err != nil || res.Written() != 0 || res.ConversationsCreated+res.ConversationsUpdated != 0 {
		t.Fatalf("re-apply wrote: %+v %v", res, err)
	}
	if after := dataVersionOf(t, obs); after != before {
		t.Fatalf("re-applying the same state committed (data_version %d -> %d)", before, after)
	}
}

// TestApplyIMessageChangesKeepsMetadata (I5): changing every owned column
// leaves transcripts, media, keys, reply ids, mentions, unread counts,
// favorites, tabs, notification modes and display protocols alone.
func TestApplyIMessageChangesKeepsMetadata(t *testing.T) {
	s, _ := newFileStore(t)
	if _, err := s.ApplyIMessageChanges(sampleIMessageChanges()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE messages SET media_id = 'media', mime_type = 'audio/mp4', decryption_key = 'key',
		reply_to_id = 'imessage:m0', mentions_me = 1 WHERE message_id = 'imessage:m1'`); err != nil {
		t.Fatal(err)
	}
	model := "model-x"
	if err := s.SetMessageTranscript("imessage:m1", "a transcript", &model); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE conversations SET unread_count = 7, is_favorite = 1, tab = 'archive',
		notification_mode = 'muted', display_protocol = 'RCS' WHERE conversation_id = 'imessage:c1'`); err != nil {
		t.Fatal(err)
	}
	ch := sampleIMessageChanges()
	ch.Conversations[0].Name, ch.Conversations[0].Participants, ch.Conversations[0].LastMessageTS = "Alice B", `[]`, 3000
	ch.Conversations[0].IsGroup = true
	m := &ch.Messages[0]
	m.ConversationID, m.SenderName, m.SenderNumber, m.Body, m.TimestampMS, m.Reactions = "imessage:c2", "b", "+2", "hello, edited", 1500, ""
	ch.Conversations = append(ch.Conversations, IMessageConversationChange{
		IMessageConversation: IMessageConversation{ConversationID: "imessage:c2", Name: "B"}, SetLastMessageTS: true,
	})
	res, err := s.ApplyIMessageChanges(ch)
	if err != nil || len(res.Failed) != 0 || res.MessagesUpdated != 1 || res.ConversationsUpdated != 1 || res.ConversationsCreated != 1 {
		t.Fatalf("apply: %+v %v", res, err)
	}
	got, _ := s.GetMessageByID("imessage:m1")
	if got.ConversationID != "imessage:c2" || got.SenderName != "b" || got.SenderNumber != "+2" || got.Body != "hello, edited" ||
		got.TimestampMS != 1500 || got.Reactions != "" {
		t.Fatalf("owned columns not written: %+v", got)
	}
	if got.MediaID != "media" || got.MimeType != "audio/mp4" || got.DecryptionKey != "key" || got.ReplyToID != "imessage:m0" ||
		!got.MentionsMe || got.Transcript != "a transcript" || got.TranscriptModel != model || got.TranscribedAtMS == 0 {
		t.Fatalf("metadata lost: %+v", got)
	}
	if fts := ftsRows(t, s, "imessage:m1"); len(fts) != 1 || fts[0] != "hello, edited" {
		t.Fatalf("search entry after an edit = %v", fts)
	}
	conv, err := s.GetConversation("imessage:c1")
	if err != nil {
		t.Fatal(err)
	}
	if conv.Name != "Alice B" || !conv.IsGroup || conv.Participants != `[]` || conv.LastMessageTS != 3000 {
		t.Fatalf("owned conversation columns not written: %+v", conv)
	}
	if conv.UnreadCount != 7 || !conv.IsFavorite || conv.Tab != "archive" || conv.NotificationMode != "muted" || conv.DisplayProtocol != "RCS" {
		t.Fatalf("conversation metadata lost: %+v", conv)
	}
}

func TestApplyIMessageChangesLeavesLastTSUnlessSet(t *testing.T) {
	s, _ := newFileStore(t)
	if _, err := s.ApplyIMessageChanges(sampleIMessageChanges()); err != nil {
		t.Fatal(err)
	}
	res, err := s.ApplyIMessageChanges(IMessageChanges{Conversations: []IMessageConversationChange{{
		IMessageConversation: IMessageConversation{ConversationID: "imessage:c1", Name: "Renamed", Participants: `[{"name":"a","number":"+1"}]`, LastMessageTS: 99},
	}}})
	if err != nil || res.ConversationsUpdated != 1 {
		t.Fatalf("rename: %+v %v", res, err)
	}
	conv, _ := s.GetConversation("imessage:c1")
	if conv.Name != "Renamed" || conv.LastMessageTS != 2000 {
		t.Fatalf("conversation = %+v, want the rename and last_message_ts 2000 kept", conv)
	}
	// Without a computed last_message_ts, a vanished conversation is not
	// re-created with 0; it fails for a retry.
	res, err = s.ApplyIMessageChanges(IMessageChanges{Conversations: []IMessageConversationChange{{
		IMessageConversation: IMessageConversation{ConversationID: "imessage:gone", Name: "Gone"},
	}}})
	if err != nil || !errors.Is(res.Failed["imessage:gone"], ErrIMessageConversationVanished) {
		t.Fatalf("vanished conversation: %+v %v", res, err)
	}
	if c, _ := s.GetConversation("imessage:gone"); c != nil {
		t.Fatalf("vanished conversation inserted: %+v", c)
	}
}

func TestApplyIMessageChangesStaleRows(t *testing.T) {
	s, _ := newFileStore(t)
	for _, m := range []*Message{
		{MessageID: "imessage:stale", ConversationID: "imessage:c1", Body: "Loved “hello”", SourcePlatform: "imessage", SourceID: "stale"},
		{MessageID: "imessage:kept-transcript", ConversationID: "imessage:c1", Body: "Liked “x”", SourcePlatform: "imessage", SourceID: "kt"},
		{MessageID: "imessage:kept-media", ConversationID: "imessage:c1", Body: "Liked “y”", SourcePlatform: "imessage", SourceID: "km", MediaID: "m"},
		{MessageID: "imessage:other-platform", ConversationID: "signal:c2", Body: "text", SourcePlatform: "signal", SourceID: "op"},
	} {
		if err := s.UpsertMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetMessageTranscript("imessage:kept-transcript", "words", nil); err != nil {
		t.Fatal(err)
	}
	res, err := s.ApplyIMessageChanges(IMessageChanges{StaleIDs: []string{
		"imessage:stale", "imessage:kept-transcript", "imessage:kept-media", "imessage:other-platform", "imessage:absent", "sms:x",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.StaleDeleted != 1 || len(res.Failed) != 1 || res.Failed["sms:x"] == nil {
		t.Fatalf("result = %+v", res)
	}
	if m, _ := s.GetMessageByID("imessage:stale"); m != nil {
		t.Fatal("stale reaction text row kept")
	}
	if fts := ftsRows(t, s, "imessage:stale"); len(fts) != 0 {
		t.Fatalf("stale row's search entry kept: %v", fts)
	}
	for _, id := range []string{"imessage:kept-transcript", "imessage:kept-media", "imessage:other-platform"} {
		if m, _ := s.GetMessageByID(id); m == nil {
			t.Fatalf("%s deleted", id)
		}
	}
}

// DeleteConversation removes messages without their search entries, so an
// id the importer inserts again may already have one; it must end with one.
func TestApplyIMessageChangesReplacesLeftoverSearchEntries(t *testing.T) {
	s, _ := newFileStore(t)
	if _, err := s.ApplyIMessageChanges(sampleIMessageChanges()); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConversation("imessage:c1"); err != nil {
		t.Fatal(err)
	}
	if fts := ftsRows(t, s, "imessage:m1"); len(fts) != 1 {
		t.Fatalf("precondition: DeleteConversation left %d search entries", len(fts))
	}
	if _, err := s.ApplyIMessageChanges(sampleIMessageChanges()); err != nil {
		t.Fatal(err)
	}
	if fts := ftsRows(t, s, "imessage:m1"); len(fts) != 1 {
		t.Fatalf("re-inserted row has %d search entries, want 1", len(fts))
	}
}

// TestApplyIMessageChangesComparesFieldsNotStrings: r2 finding 6 compared
// delimiter-joined snapshots, so ("Alice|suffix", "+1") equalled
// ("Alice", "suffix|+1").
func TestApplyIMessageChangesComparesFieldsNotStrings(t *testing.T) {
	s, _ := newFileStore(t)
	ch := sampleIMessageChanges()
	ch.Messages[0].SenderName, ch.Messages[0].SenderNumber = "Alice|suffix", "+1"
	if _, err := s.ApplyIMessageChanges(ch); err != nil {
		t.Fatal(err)
	}
	ch.Messages[0].SenderName, ch.Messages[0].SenderNumber = "Alice", "suffix|+1"
	res, err := s.ApplyIMessageChanges(ch)
	if err != nil || res.MessagesUpdated != 1 {
		t.Fatalf("apply: %+v %v", res, err)
	}
	got, _ := s.GetMessageByID("imessage:m1")
	if got.SenderName != "Alice" || got.SenderNumber != "suffix|+1" {
		t.Fatalf("sender = %q / %q", got.SenderName, got.SenderNumber)
	}
}

func TestApplyIMessageChangesFailedRowDoesNotBlockOthers(t *testing.T) {
	s, _ := newFileStore(t)
	// Another row already claims (imessage, m1) under a different id, so
	// inserting imessage:m1 violates idx_messages_source.
	if err := s.UpsertMessage(&Message{MessageID: "legacy-m1", ConversationID: "imessage:c1", Body: "x", SourcePlatform: "imessage", SourceID: "m1"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.ApplyIMessageChanges(sampleIMessageChanges())
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed["imessage:m1"] == nil || len(res.Failed) != 1 || res.MessagesInserted != 1 {
		t.Fatalf("result = %+v", res)
	}
	if m, _ := s.GetMessageByID("imessage:m2"); m == nil {
		t.Fatal("the other row was not written")
	}
	if fts := ftsRows(t, s, "imessage:m1"); len(fts) != 0 {
		t.Fatalf("failed row got a search entry: %v", fts)
	}
}

// TestApplyIMessageChangesAfterAnotherWriterCommits: r2 finding 5. The
// withdrawn rebuild read in a deferred transaction and then wrote, so a
// commit by another connection in between failed it with
// SQLITE_BUSY_SNAPSHOT (517). Reads here happen before the write
// transaction, which starts with the write lock.
func TestApplyIMessageChangesAfterAnotherWriterCommits(t *testing.T) {
	s, path := newFileStore(t)
	writer, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := s.ApplyIMessageChanges(sampleIMessageChanges()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IMessageRowsByID([]string{"imessage:m1", "imessage:m2"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.UpsertMessage(&Message{MessageID: "sms:live", ConversationID: "sms:c", Body: "live", SourcePlatform: "sms"}); err != nil {
		t.Fatal(err)
	}
	ch := sampleIMessageChanges()
	ch.Messages[1].Body = "changed after the other commit"
	res, err := s.ApplyIMessageChanges(ch)
	if err != nil || res.MessagesUpdated != 1 {
		t.Fatalf("apply after another commit: %+v %v", res, err)
	}
}

// TestApplyIMessageChangesWaitsForAHeldWriter: with another connection
// holding the write lock, the apply waits (busy_timeout) and succeeds once
// it is released, instead of failing at once.
func TestApplyIMessageChangesWaitsForAHeldWriter(t *testing.T) {
	s, path := newFileStore(t)
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO messages (message_id, conversation_id, body) VALUES ('sms:held', 'sms:c', 'held')`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var res IMessageApplyResult
	var applyErr error
	go func() {
		defer wg.Done()
		res, applyErr = s.ApplyIMessageChanges(sampleIMessageChanges())
	}()
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if applyErr != nil || res.MessagesInserted != 2 {
		t.Fatalf("apply behind a held writer: %+v %v", res, applyErr)
	}
}

// TestApplyIMessageChangesBatches: more rows than one transaction holds all
// land, each with one search entry.
func TestApplyIMessageChangesBatches(t *testing.T) {
	s, _ := newFileStore(t)
	n := iMessageApplyBatch*2 + 17
	var ch IMessageChanges
	ch.Conversations = sampleIMessageChanges().Conversations
	for i := 0; i < n; i++ {
		ch.Messages = append(ch.Messages, IMessageRow{
			MessageID: fmt.Sprintf("imessage:b%04d", i), ConversationID: "imessage:c1", Body: fmt.Sprintf("body %d", i),
			TimestampMS: int64(i), SourceID: fmt.Sprintf("b%04d", i),
		})
	}
	res, err := s.ApplyIMessageChanges(ch)
	if err != nil || res.MessagesInserted != n {
		t.Fatalf("apply: inserted %d of %d, %v", res.MessagesInserted, n, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE message_id >= 'imessage:b' AND message_id < 'imessage:c'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("search entries = %d, want %d", count, n)
	}
}

// TestRepairTapbacksLeavesIMessageRows (I8): the SMS/RCS quote heuristic
// never converts an iMessage row (a typed "Loved “hello”" there is text) and
// never puts a reaction on one; SMS rows still convert.
func TestRepairTapbacksLeavesIMessageRows(t *testing.T) {
	s := newTestStore(t)
	for _, m := range []*Message{
		{MessageID: "imessage:hello", ConversationID: "imessage:c", Body: "hello", TimestampMS: 10, SourcePlatform: "imessage", SourceID: "hello"},
		{MessageID: "imessage:typed", ConversationID: "imessage:c", Body: "Loved “hello”", TimestampMS: 20, IsFromMe: true, SourcePlatform: "imessage", SourceID: "typed"},
		{MessageID: "sms:hello", ConversationID: "sms:c", Body: "hello", TimestampMS: 10, SourcePlatform: "sms"},
		{MessageID: "sms:tap", ConversationID: "sms:c", Body: "Loved “hello”", TimestampMS: 20, SenderNumber: "+1", SourcePlatform: "sms"},
		// An SMS tapback inside an iMessage conversation must not react to
		// the iMessage row either.
		{MessageID: "sms:mixed", ConversationID: "imessage:c", Body: "Liked “hello”", TimestampMS: 30, SenderNumber: "+2", SourcePlatform: "sms"},
		// Each guard alone: an iMessage-platform row without the id prefix,
		// and a pre-migration iMessage row (platform defaulted to sms).
		{MessageID: "x-target", ConversationID: "imessage:d", Body: "yo", TimestampMS: 10, SourcePlatform: "imessage"},
		{MessageID: "x-typed", ConversationID: "imessage:d", Body: "Loved “yo”", TimestampMS: 20, IsFromMe: true, SourcePlatform: "imessage"},
		{MessageID: "imessage:old-target", ConversationID: "imessage:e", Body: "hey", TimestampMS: 10, SourcePlatform: "sms"},
		{MessageID: "imessage:old-typed", ConversationID: "imessage:e", Body: "Loved “hey”", TimestampMS: 20, IsFromMe: true, SourcePlatform: "sms"},
		// The candidate guard alone: an iMessage row quoting an SMS row,
		// which the target guard would allow.
		{MessageID: "sms:quoted", ConversationID: "imessage:f", Body: "sup", TimestampMS: 10, SourcePlatform: "sms"},
		{MessageID: "x-quoting", ConversationID: "imessage:f", Body: "Loved “sup”", TimestampMS: 20, IsFromMe: true, SourcePlatform: "imessage"},
	} {
		if err := s.UpsertMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.RepairTapbacks()
	if err != nil || n != 1 {
		t.Fatalf("RepairTapbacks = %d, %v; want only the SMS tapback converted", n, err)
	}
	typed, _ := s.GetMessageByID("imessage:typed")
	hello, _ := s.GetMessageByID("imessage:hello")
	if typed == nil || hello.Reactions != "" {
		t.Fatalf("iMessage rows changed: typed=%+v hello reactions=%q", typed, hello.Reactions)
	}
	smsHello, _ := s.GetMessageByID("sms:hello")
	if !strings.Contains(smsHello.Reactions, "❤️") {
		t.Fatalf("SMS tapback not applied: %q", smsHello.Reactions)
	}
	if m, _ := s.GetMessageByID("sms:mixed"); m == nil {
		t.Fatal("unmatched SMS tapback deleted")
	}
	if quoted, _ := s.GetMessageByID("sms:quoted"); quoted.Reactions != "" {
		t.Fatalf("an iMessage row reacted to an SMS row: %q", quoted.Reactions)
	}
	for _, id := range []string{"x-typed", "imessage:old-typed", "x-quoting"} {
		if m, _ := s.GetMessageByID(id); m == nil {
			t.Fatalf("%s converted", id)
		}
	}
}
