package signaldesktop

// Reader tests over synthetic Signal Desktop archives (see ./fixture). They
// run the production queries through SQLRunner, the same SQL the Node helper
// runs against the real SQLCipher database.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/maxghenis/openmessage/internal/signaldesktop/fixture"
	"github.com/maxghenis/openmessage/internal/v2keys"
	_ "modernc.org/sqlite"
)

const (
	tSelf      = "+15555550100"
	tSelfACI   = "00000000-0000-4000-8000-000000000000"
	tPeerE164  = "+15555550101"
	tPeerACI   = "11111111-2222-4333-8444-555555555555"
	tOtherACI  = "66666666-7777-4888-9999-aaaaaaaaaaaa"
	tThirdE164 = "+15555550103"
	tGroupID   = "c3ludGhldGljLWdyb3VwLWlkLWZvci10ZXN0cy0wMQ=="
	tBaseMS    = int64(1_790_000_000_000)
	tAccount   = "signal-primary"
)

func openArchive(t *testing.T, archive fixture.Archive) Runner {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	if err := fixture.Write(path, archive); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return SQLRunner{DB: db}
}

func baseArchive() fixture.Archive {
	return fixture.Archive{
		SelfNumber: tSelf,
		SelfACI:    tSelfACI,
		Conversations: []fixture.Conversation{
			{ID: "conv-self", Type: "private", E164: tSelf, ServiceID: tSelfACI, ProfileFull: "Synthetic Self"},
			{ID: "conv-peer", Type: "private", E164: tPeerE164, ServiceID: tPeerACI, Name: "Synthetic Peer"},
			{ID: "conv-other", Type: "private", ServiceID: tOtherACI, ProfileFull: "Synthetic Other"},
			{ID: "conv-group", Type: "group", GroupID: tGroupID, Name: "Synthetic Group", MembersV2: []string{tSelfACI, tPeerACI, tOtherACI}},
		},
	}
}

func scanAll(t *testing.T, runner Runner, facts *Facts, options ScanOptions) ([]Chunk, Stats) {
	t.Helper()
	var chunks []Chunk
	stats, err := (&Reader{Runner: runner}).Scan(context.Background(), facts, options, func(chunk Chunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return chunks, stats
}

func emptyFacts() *Facts { return NewFacts(tAccount, tSelf, nil, nil, nil) }

func TestScanRefusesAnArchiveItCannotMatchToTheAccount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*fixture.Archive)
		facts   *Facts
		wantErr error
	}{
		{name: "different account", mutate: func(a *fixture.Archive) { a.SelfNumber = tThirdE164 }, facts: emptyFacts(), wantErr: ErrAccountMismatch},
		{name: "no own number", mutate: func(a *fixture.Archive) { a.SelfNumber = "" }, facts: emptyFacts(), wantErr: ErrAccountUnverified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := baseArchive()
			archive.Messages = []fixture.Message{{RowID: 1, ID: "m1", ConversationID: "conv-peer", Type: "incoming", Body: "synthetic", SentAt: tBaseMS, SourceServiceID: tPeerACI}}
			tc.mutate(&archive)
			emitted := 0
			stats, err := (&Reader{Runner: openArchive(t, archive)}).Scan(context.Background(), tc.facts, ScanOptions{}, func(Chunk) error {
				emitted++
				return nil
			})
			if !errors.Is(err, tc.wantErr) || emitted != 0 || stats.AccountVerified {
				t.Fatalf("Scan err=%v emitted=%d verified=%v, want %v and nothing emitted", err, emitted, stats.AccountVerified, tc.wantErr)
			}
		})
	}
	if _, err := (&Reader{Runner: openArchive(t, baseArchive())}).Scan(context.Background(), NewFacts(tAccount, "", nil, nil, nil), ScanOptions{}, nil); err == nil {
		t.Fatal("Scan without the v2 account number succeeded")
	}
}

func TestScanKeysEachContactByTheFormV2Uses(t *testing.T) {
	archive := baseArchive()
	archive.Messages = []fixture.Message{
		{RowID: 1, ID: "m1", ConversationID: "conv-peer", Type: "incoming", Body: "synthetic", SentAt: tBaseMS + 1, SourceServiceID: tPeerACI},
	}
	for _, tc := range []struct {
		name  string
		facts *Facts
		want  string
	}{
		{name: "unknown to v2 uses the number, as live does", facts: emptyFacts(), want: tPeerE164},
		{name: "v2 knows only the ACI", facts: NewFacts(tAccount, tSelf, []string{tPeerACI}, nil, nil), want: tPeerACI},
		{name: "v2 knows only the number", facts: NewFacts(tAccount, tSelf, []string{tPeerE164}, nil, nil), want: tPeerE164},
		{name: "v2 knows both, thread bound by ACI", facts: NewFacts(tAccount, tSelf, []string{tPeerACI, tPeerE164}, []string{"signal:" + tPeerACI}, nil), want: tPeerACI},
		{name: "v2 knows both, no thread", facts: NewFacts(tAccount, tSelf, []string{tPeerACI, tPeerE164}, nil, nil), want: tPeerE164},
		{name: "number-bound thread wins", facts: NewFacts(tAccount, tSelf, []string{tPeerACI}, []string{"signal:" + tPeerE164}, nil), want: tPeerE164},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks, _ := scanAll(t, openArchive(t, archive), tc.facts, ScanOptions{})
			if len(chunks) != 1 || chunks[0].Chat.RemoteID != "signal:"+tc.want || chunks[0].Messages[0].AuthorAddress != tc.want ||
				chunks[0].Chat.Participants[0].Address != tc.want || chunks[0].Chat.Participants[0].Name != "Synthetic Peer" {
				t.Fatalf("chunks = %+v, want everything keyed by %s", chunks, tc.want)
			}
		})
	}
}

func TestScanSkipsWhatLiveNeverProjects(t *testing.T) {
	archive := baseArchive()
	archive.Conversations = append(archive.Conversations,
		fixture.Conversation{ID: "conv-pni-only", Type: "private", ServiceID: "PNI:" + tOtherACI},
		fixture.Conversation{ID: "conv-group-noid", Type: "group"},
	)
	m := func(row int64, conversation, kind string) fixture.Message {
		return fixture.Message{RowID: row, ID: fmt.Sprintf("m%d", row), ConversationID: conversation, Type: kind, Body: "synthetic", SentAt: tBaseMS + row, SourceServiceID: tPeerACI}
	}
	deleted := m(3, "conv-peer", "incoming")
	deleted.DeletedForEveryone = true
	noTime := m(4, "conv-peer", "incoming")
	noTime.SentAt = 0
	empty := m(5, "conv-peer", "incoming")
	empty.Body = "   "
	unsent := m(6, "conv-peer", "outgoing")
	unsent.SendState = map[string]string{"conv-peer": "Failed"}
	pending := m(7, "conv-peer", "outgoing")
	pending.SendState = map[string]string{"conv-peer": "Pending"}
	selfIncoming := m(8, "conv-group", "incoming")
	selfIncoming.SourceServiceID = tSelfACI
	notPeer := m(9, "conv-peer", "incoming")
	notPeer.SourceServiceID = tOtherACI
	unknown := m(10, "conv-group", "incoming")
	unknown.SourceServiceID = ""
	sent := m(11, "conv-peer", "outgoing")
	sent.SendState = map[string]string{"conv-peer": "Delivered"}
	archive.Messages = []fixture.Message{
		m(1, "conv-peer", "story"),
		m(2, "conv-peer", "group-v2-change"),
		deleted, noTime, empty, unsent, pending, selfIncoming, notPeer, unknown, sent,
		m(12, "conv-pni-only", "incoming"),
		m(13, "conv-group-noid", "incoming"),
		m(14, "conv-missing", "incoming"),
		m(15, "conv-peer", "profile-change"),
		m(16, "conv-peer", "some-future-type"),
		m(17, "conv-peer", "incoming"),
	}
	chunks, stats := scanAll(t, openArchive(t, archive), emptyFacts(), ScanOptions{})
	want := map[string]int64{
		"story": 1, "system": 2, "deleted": 1, "no_timestamp": 1, "empty": 1, "outgoing_unsent": 2,
		"self_incoming": 1, "author_not_peer": 1, "unknown_author": 1, "chat_unaddressable": 1,
		"chat_group_without_id": 1, "chat_missing": 1, "type_other": 1,
	}
	if !reflect.DeepEqual(stats.RowsSkippedByReason, want) {
		t.Fatalf("skips = %v, want %v", stats.RowsSkippedByReason, want)
	}
	if stats.RowsScanned != 17 || stats.RowsIncluded != 2 || stats.HighWaterRowID != 17 || len(chunks) != 1 || len(chunks[0].Messages) != 2 {
		t.Fatalf("stats=%+v chunks=%+v", stats, chunks)
	}
	if !chunks[0].Messages[0].Outgoing || chunks[0].Messages[1].Outgoing || chunks[0].Messages[1].AuthorAddress != tPeerE164 {
		t.Fatalf("included = %+v", chunks[0].Messages)
	}
	if stats.ChatsSkippedByReason["unaddressable"] != 1 || stats.ChatsSkippedByReason["group_without_id"] != 1 {
		t.Fatalf("chat skips = %v", stats.ChatsSkippedByReason)
	}
}

func TestScanReadsOnlyTheMessagesOwnAttachments(t *testing.T) {
	one := 1
	attachments := []fixture.Attachment{
		{Type: "quote", Order: 0, ContentType: "image/jpeg"},
		{Type: "preview", Order: 0, ContentType: "image/png"},
		{Type: "attachment", Order: 1, ContentType: "application/pdf", FileName: "synthetic.pdf"},
		{Type: "attachment", Order: 0, ContentType: "video/mp4", FileName: "synthetic.mp4"},
		{Type: "attachment", Order: 0, ContentType: "image/gif", EditHistoryIndex: &one},
	}
	archive := baseArchive()
	archive.Messages = []fixture.Message{
		{RowID: 1, ID: "m1", ConversationID: "conv-peer", Type: "incoming", SentAt: tBaseMS + 1, SourceServiceID: tPeerACI, Attachments: attachments},
		{RowID: 2, ID: "m2", ConversationID: "conv-peer", Type: "incoming", SentAt: tBaseMS + 2, SourceServiceID: tPeerACI, JSONAttachments: []fixture.Attachment{{ContentType: "audio/aac"}}},
	}
	chunks, stats := scanAll(t, openArchive(t, archive), emptyFacts(), ScanOptions{})
	if stats.AttachmentSource != "typed" || len(chunks) != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	got := chunks[0].Messages[0].Attachments
	if len(got) != 2 || got[0].ContentType != "video/mp4" || got[1].ContentType != "application/pdf" {
		t.Fatalf("typed attachments = %+v, want the message's own two, in order", got)
	}
	if got := chunks[0].Messages[1].Attachments; len(got) != 1 || got[0].ContentType != "audio/aac" {
		t.Fatalf("json attachments = %+v", got)
	}

	archive.LegacyAttachmentsTable = true
	archive.Messages[0].Attachments = []fixture.Attachment{{ContentType: "image/png", FileName: "synthetic.png"}}
	chunks, stats = scanAll(t, openArchive(t, archive), emptyFacts(), ScanOptions{})
	if stats.AttachmentSource != "basic" || chunks[0].Messages[0].Attachments[0].ContentType != "image/png" {
		t.Fatalf("legacy attachments: stats=%+v chunks=%+v", stats, chunks)
	}
}

func TestScanCapturesGroupsReactionsAndContentFlags(t *testing.T) {
	archive := baseArchive()
	archive.Messages = []fixture.Message{
		{RowID: 1, ID: "m1", ConversationID: "conv-group", Type: "incoming", Body: "synthetic hello", SentAt: tBaseMS + 1, SourceServiceID: tPeerACI,
			Reactions: []fixture.Reaction{
				{Emoji: "👍", FromID: "conv-other", Timestamp: tBaseMS + 5},
				{Emoji: "❤️", FromID: "conv-self", Timestamp: tBaseMS + 6},
				{Emoji: "😂", FromID: "conv-unknown"},
				{Emoji: "", FromID: "conv-peer"},
			}},
		{RowID: 2, ID: "m2", ConversationID: "conv-group", Type: "outgoing", SentAt: tBaseMS + 2, Sticker: true},
		{RowID: 3, ID: "m3", ConversationID: "conv-peer", Type: "incoming", SentAt: tBaseMS + 3, SourceServiceID: tPeerACI, Contact: true, Poll: true, Payment: true, Preview: true, ViewOnce: true, StoryID: "story-synthetic",
			Reactions: []fixture.Reaction{{Emoji: "👍", FromID: "conv-other"}, {Emoji: "🙏", FromID: "conv-peer"}}},
	}
	chunks, stats := scanAll(t, openArchive(t, archive), emptyFacts(), ScanOptions{})
	if len(chunks) != 2 {
		t.Fatalf("chunks = %+v", chunks)
	}
	var group, direct Chunk
	for _, chunk := range chunks {
		if chunk.Chat.Group {
			group = chunk
		} else {
			direct = chunk
		}
	}
	if group.Chat.RemoteID != "signal-group:"+tGroupID || group.Chat.Title != "Synthetic Group" {
		t.Fatalf("group chat = %+v", group.Chat)
	}
	wantRoster := []Participant{{Address: tPeerE164, Name: "Synthetic Peer"}, {Address: tOtherACI, Name: "Synthetic Other"}}
	if !reflect.DeepEqual(group.Chat.Participants, wantRoster) {
		t.Fatalf("group roster = %+v, want %+v (self excluded)", group.Chat.Participants, wantRoster)
	}
	reactions := group.Messages[0].Reactions
	if len(reactions) != 2 || !reactions[0].IsSelf || reactions[0].Emoji != "❤️" || reactions[1].ActorAddress != tOtherACI || reactions[1].TimestampMS != tBaseMS+5 {
		t.Fatalf("group reactions = %+v", reactions)
	}
	if !group.Messages[1].Outgoing || !group.Messages[1].Sticker {
		t.Fatalf("group outgoing = %+v", group.Messages[1])
	}
	m := direct.Messages[0]
	if !m.Contact || !m.Poll || !m.Payment || !m.Preview || !m.ViewOnce || !m.StoryReply || m.AuthorAddress != tPeerE164 {
		t.Fatalf("direct flags = %+v", m)
	}
	if len(m.Reactions) != 1 || m.Reactions[0].ActorAddress != tPeerE164 {
		t.Fatalf("a 1:1 reaction from a third party must be skipped: %+v", m.Reactions)
	}
	if stats.ReactionsIncluded != 3 || stats.ReactionsSkipped != 3 {
		t.Fatalf("reaction stats = %+v", stats)
	}
}

func TestScanGroupMembersFromTheOlderMembersColumn(t *testing.T) {
	archive := baseArchive()
	archive.Conversations[3].MembersV2 = nil
	archive.Conversations[3].Members = []string{"conv-peer", tOtherACI, "conv-self"}
	archive.Messages = []fixture.Message{{RowID: 1, ID: "m1", ConversationID: "conv-group", Type: "outgoing", Body: "synthetic", SentAt: tBaseMS}}
	chunks, _ := scanAll(t, openArchive(t, archive), emptyFacts(), ScanOptions{})
	want := []Participant{{Address: tPeerE164, Name: "Synthetic Peer"}, {Address: tOtherACI, Name: "Synthetic Other"}}
	if len(chunks) != 1 || !reflect.DeepEqual(chunks[0].Chat.Participants, want) {
		t.Fatalf("roster = %+v, want %+v", chunks, want)
	}
}

func TestScanSkipsUnboundGroupsThatShareARoster(t *testing.T) {
	archive := baseArchive()
	twin := "dHdpbi1zeW50aGV0aWMtZ3JvdXAtaWQtZm9yLXRlc3RzLTI="
	archive.Conversations = append(archive.Conversations, fixture.Conversation{ID: "conv-twin", Type: "group", GroupID: twin, MembersV2: []string{tPeerACI, tOtherACI}})
	archive.Messages = []fixture.Message{
		{RowID: 1, ID: "m1", ConversationID: "conv-group", Type: "outgoing", Body: "synthetic", SentAt: tBaseMS + 1},
		{RowID: 2, ID: "m2", ConversationID: "conv-twin", Type: "outgoing", Body: "synthetic", SentAt: tBaseMS + 2},
	}
	chunks, stats := scanAll(t, openArchive(t, archive), emptyFacts(), ScanOptions{})
	if len(chunks) != 0 || stats.RowsSkippedByReason["chat_ambiguous_roster"] != 2 || stats.ChatsSkippedByReason["ambiguous_roster"] != 2 {
		t.Fatalf("chunks=%+v stats=%+v", chunks, stats)
	}
	// Once v2 has one of them, it is not ambiguous here; the worker's roster
	// rule then decides the other one, the same way in any frame order.
	chunks, _ = scanAll(t, openArchive(t, archive), NewFacts(tAccount, tSelf, nil, []string{"signal-group:" + tGroupID}, nil), ScanOptions{})
	if len(chunks) != 2 {
		t.Fatalf("with one group bound: chunks=%+v", chunks)
	}
}

func TestScanMergesDuplicateDesktopRowsForOnePerson(t *testing.T) {
	archive := baseArchive()
	archive.Conversations = append(archive.Conversations, fixture.Conversation{ID: "conv-peer-copy", Type: "private", E164: tPeerE164, Name: "Synthetic Copy"})
	archive.Messages = []fixture.Message{
		{RowID: 1, ID: "m1", ConversationID: "conv-peer", Type: "incoming", Body: "synthetic one", SentAt: tBaseMS + 1, SourceServiceID: tPeerACI},
		{RowID: 2, ID: "m2", ConversationID: "conv-peer-copy", Type: "incoming", Body: "synthetic two", SentAt: tBaseMS + 2, Source: tPeerE164},
	}
	chunks, stats := scanAll(t, openArchive(t, archive), emptyFacts(), ScanOptions{})
	if len(chunks) != 1 || len(chunks[0].Messages) != 2 || stats.ChatsSkippedByReason["merged_duplicate"] != 1 {
		t.Fatalf("chunks=%+v stats=%+v", chunks, stats)
	}
}

func TestScanLeavesOutMessagesV2HoldsUnderAnyForm(t *testing.T) {
	archive := baseArchive()
	archive.Messages = []fixture.Message{
		{RowID: 1, ID: "m1", ConversationID: "conv-peer", Type: "incoming", Body: "synthetic", SentAt: tBaseMS + 1, SourceServiceID: tPeerACI},
		{RowID: 2, ID: "m2", ConversationID: "conv-peer", Type: "outgoing", Body: "synthetic", SentAt: tBaseMS + 2},
		{RowID: 3, ID: "m3", ConversationID: "conv-group", Type: "incoming", Body: "synthetic", SentAt: tBaseMS + 3, SourceServiceID: tPeerACI},
		{RowID: 4, ID: "m4", ConversationID: "conv-peer", Type: "outgoing", Body: "synthetic", SentAt: tBaseMS + 4},
		{RowID: 5, ID: "m5", ConversationID: "conv-peer", Type: "incoming", Body: "synthetic new", SentAt: tBaseMS + 5, SourceServiceID: tPeerACI},
	}
	aciConversation := "signal:" + tPeerACI
	groupConversation := "signal-group:" + tGroupID
	stored := []MessageKey{
		// The 1:1 incoming under the ACI thread, the group incoming under the
		// ACI author, the outgoing under the legacy alias, and one outgoing
		// found only by its remote key in a re-keyed row.
		{MessageID: v2keys.DeriveID("message", tAccount, aciConversation+"\x1f"+v2keys.SignalIncomingSourceID(aciConversation, tPeerACI, tBaseMS+1))},
		{MessageID: v2keys.DeriveID("message", tAccount, groupConversation+"\x1f"+v2keys.SignalIncomingSourceID(groupConversation, tPeerACI, tBaseMS+3))},
		{MessageID: "migrated", RemoteConversationID: "signal:" + tPeerE164, RemoteMessageID: v2keys.SignalLocalAlias("signal:"+tPeerE164, tBaseMS+2)},
		{MessageID: "rekeyed", RemoteConversationID: aciConversation, RemoteMessageID: strconv.FormatInt(tBaseMS+4, 10)},
	}
	// v2 knows both forms, so this contact is keyed by the number, and every
	// stored copy is under the other form or an alias.
	facts := NewFacts(tAccount, tSelf, []string{tPeerE164, tPeerACI}, nil, stored)
	chunks, stats := scanAll(t, openArchive(t, archive), facts, ScanOptions{SinceMS: tBaseMS + 3})
	if stats.RowsInV2 != 4 || stats.RowsIncluded != 1 || len(chunks) != 1 || chunks[0].Messages[0].SentAtMS != tBaseMS+5 {
		t.Fatalf("stats=%+v chunks=%+v", stats, chunks)
	}
	if stats.RowsSince != 3 || stats.RowsSinceInV2 != 2 || stats.RowsSinceIncluded != 1 {
		t.Fatalf("since tallies = %+v", stats)
	}
}

func TestScanChunksDeterministically(t *testing.T) {
	archive := baseArchive()
	random := rand.New(rand.NewSource(7))
	rows := random.Perm(250)
	for index, row := range rows {
		archive.Messages = append(archive.Messages, fixture.Message{
			RowID: int64(row + 1), ID: fmt.Sprintf("m%03d", row), ConversationID: "conv-peer", Type: "incoming",
			Body: "synthetic", SentAt: tBaseMS + int64(index), SourceServiceID: tPeerACI,
		})
	}
	runner := openArchive(t, archive)
	first, _ := scanAll(t, runner, emptyFacts(), ScanOptions{})
	second, _ := scanAll(t, runner, emptyFacts(), ScanOptions{})
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two scans of one archive emitted different chunks")
	}
	if len(first) != 3 || len(first[0].Messages) != 100 || len(first[1].Messages) != 100 || len(first[2].Messages) != 50 {
		t.Fatalf("chunk sizes = %d", len(first))
	}
	var last int64
	for _, chunk := range first {
		for _, message := range chunk.Messages {
			if message.SentAtMS < last {
				t.Fatal("messages not in sent order")
			}
			last = message.SentAtMS
		}
	}
	small, _ := scanAll(t, runner, emptyFacts(), ScanOptions{ChunkSize: 40})
	if len(small) != 7 {
		t.Fatalf("chunk size 40 gave %d chunks", len(small))
	}
}

// Property: an incremental scan after any high-water mark, together with the
// scan up to it, captures exactly what one full scan captures.
func TestScanIncrementalScansCoverTheFullScanProperty(t *testing.T) {
	random := rand.New(rand.NewSource(20261011))
	for trial := 0; trial < 30; trial++ {
		archive := baseArchive()
		count := 5 + random.Intn(40)
		conversations := []string{"conv-peer", "conv-group", "conv-other", "conv-self"}
		for row := 1; row <= count; row++ {
			conversation := conversations[random.Intn(len(conversations))]
			kind := "incoming"
			if conversation == "conv-self" || random.Intn(3) == 0 {
				kind = "outgoing"
			}
			author := tPeerACI
			if conversation == "conv-other" || (conversation == "conv-group" && random.Intn(2) == 0) {
				author = tOtherACI
			}
			archive.Messages = append(archive.Messages, fixture.Message{
				RowID: int64(row), ID: fmt.Sprintf("m%d", row), ConversationID: conversation, Type: kind,
				Body: fmt.Sprintf("synthetic %d", row), SentAt: tBaseMS + int64(random.Intn(1000)), SourceServiceID: author,
			})
		}
		runner := openArchive(t, archive)
		key := func(chunks []Chunk) []string {
			var keys []string
			for _, chunk := range chunks {
				for _, message := range chunk.Messages {
					keys = append(keys, fmt.Sprintf("%s|%d|%s|%t", chunk.Chat.RemoteID, message.RowID, message.AuthorAddress, message.Outgoing))
				}
			}
			sort.Strings(keys)
			return keys
		}
		full, fullStats := scanAll(t, runner, emptyFacts(), ScanOptions{})
		split := int64(random.Intn(count + 1))
		head, headStats := scanAll(t, openArchive(t, truncate(archive, split)), emptyFacts(), ScanOptions{})
		tail, tailStats := scanAll(t, runner, emptyFacts(), ScanOptions{AfterRowID: headStats.HighWaterRowID})
		if got, want := append(key(head), key(tail)...), key(full); !reflect.DeepEqual(sorted(got), want) {
			t.Fatalf("trial %d split %d: head+tail=%v full=%v", trial, split, got, want)
		}
		if tailStats.HighWaterRowID != fullStats.HighWaterRowID {
			t.Fatalf("trial %d: tail high water %d != full %d", trial, tailStats.HighWaterRowID, fullStats.HighWaterRowID)
		}
	}
}

func truncate(archive fixture.Archive, lastRow int64) fixture.Archive {
	out := archive
	out.Messages = nil
	for _, message := range archive.Messages {
		if message.RowID <= lastRow {
			out.Messages = append(out.Messages, message)
		}
	}
	return out
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func TestNormalizers(t *testing.T) {
	for value, want := range map[string]string{"+15555550100": "+15555550100", " +15555550100 ": "+15555550100", "15555550100": "", "+1": "", "+1555555010a": ""} {
		if got := NormalizeE164(value); got != want {
			t.Errorf("NormalizeE164(%q) = %q, want %q", value, got, want)
		}
	}
	for value, want := range map[string]string{tPeerACI: tPeerACI, "11111111-2222-4333-8444-55555555555A": "11111111-2222-4333-8444-55555555555a", "PNI:" + tPeerACI: "", "": ""} {
		if got := NormalizeACI(value); got != want {
			t.Errorf("NormalizeACI(%q) = %q, want %q", value, got, want)
		}
	}
	if got := stripDeviceSuffix("+15555550100.3"); got != "+15555550100" {
		t.Errorf("stripDeviceSuffix = %q", got)
	}
	if got := stripDeviceSuffix(tPeerACI + ".12"); got != tPeerACI {
		t.Errorf("stripDeviceSuffix = %q", got)
	}
}

func openRaw(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?mode=ro")
}
