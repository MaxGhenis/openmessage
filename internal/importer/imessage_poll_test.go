package importer

import (
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
)

// Regression tests for the incremental iMessage importer. "r1 #n" and
// "r2 #n" name the findings of the two reviews of the design withdrawn in
// #231 (ROWID mark, quote-matched tapbacks, per-chat tapback rebuild); each
// is a requirement here.

var testNow = time.Unix(1_760_000_000, 0) // 2025-10-09

// at is a Unix time `ago` before testNow.
func at(ago time.Duration) int64 { return testNow.Add(-ago).Unix() }

func newPoller(path string) *IMessage {
	clock := &fixedClock{t: testNow}
	return &IMessage{DBPath: path, now: clock.now}
}

// requireMatchesFullImport checks the polled store against one full import
// of the same chat.db into an empty store.
func requireMatchesFullImport(t *testing.T, label, storePath string, chat *nativeChatDB) {
	t.Helper()
	requireSameStore(t, label, dumpStore(t, storePath), fullImportDump(t, chat.path, testNow))
}

func reactionsOf(t *testing.T, store *db.Store, id string) string {
	t.Helper()
	m, err := store.GetMessageByID(id)
	if err != nil || m == nil {
		t.Fatalf("%s missing: %v", id, err)
	}
	return m.Reactions
}

func TestIMessageNativeReactions(t *testing.T) {
	chat := newNativeChatDB(t)
	alice := chat.addHandle("+15550000001")
	bob := chat.addHandle("bob@example.com")
	c := chat.addChat("iMessage;+;g1", strPtr("Group"), true, alice, bob)
	chat.message(c, "t1", "hello", at(time.Hour), false, alice)
	chat.message(c, "t2", "second", at(50*time.Minute), true, 0)
	chat.react(c, "r1", 2000, "p:0/t1", at(40*time.Minute), true, alice) // from me (handle_id is the partner, as in real rows)
	chat.react(c, "r2", 2003, "bp:t1", at(39*time.Minute), false, alice)
	chat.react(c, "r3", 2001, "t2", at(38*time.Minute), false, bob) // a bare guid, as older rows store it
	chat.react(c, "r4", 2004, "p:1/t2", at(37*time.Minute), false, alice)
	chat.react(c, "r5", 3004, "p:1/t2", at(36*time.Minute), false, alice) // alice takes hers back
	chat.add(row{GUID: "r6", Body: "x", Attributed: true, Unix: at(35 * time.Minute), Handle: bob,
		AssocType: 2006, AssocGUID: "p:0/t1", Emoji: "🎉", Chats: []int64{c}})
	// A sticker reaction (2007) is not a native tapback; with a body it is a
	// row, as before.
	chat.add(row{GUID: "s1", Body: "sticker", Unix: at(34 * time.Minute), Handle: bob, AssocType: 2007, AssocGUID: "p:0/t1", Chats: []int64{c}})

	store, path := newIMessageStore(t)
	res := runImport(t, newPoller(chat.path), store)
	if res.MessagesImported != 3 {
		t.Fatalf("imported %d rows, want t1, t2 and the sticker row", res.MessagesImported)
	}
	if got, want := reactionsOf(t, store, "imessage:t1"), `[{"emoji":"❤️","count":1,"actors":["me"]},{"emoji":"😂","count":1,"actors":["+15550000001"]},{"emoji":"🎉","count":1,"actors":["bob@example.com"]}]`; got != want {
		t.Fatalf("t1 reactions = %s, want %s", got, want)
	}
	if got, want := reactionsOf(t, store, "imessage:t2"), `[{"emoji":"👍","count":1,"actors":["bob@example.com"]}]`; got != want {
		t.Fatalf("t2 reactions = %s, want %s", got, want)
	}
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5", "r6"} {
		if m, _ := store.GetMessageByID("imessage:" + id); m != nil {
			t.Fatalf("reaction row %s stored as a message", id)
		}
	}
	requireMatchesFullImport(t, "native reactions", path, chat)
}

// r2 #1: a typed message that reads like a tapback (associated_message_type
// 0, no associated guid) is text, never a reaction, whether or not it is
// already stored.
func TestIMessageTypedTapbackTextStaysText(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		chat := newNativeChatDB(t)
		alice := chat.addHandle("+15550000001")
		c := chat.addChat("iMessage;-;+15550000001", strPtr("Alice"), false, alice)
		chat.message(c, "hello", "hello", at(time.Hour), false, alice)
		chat.add(row{GUID: "typed", Body: "Loved “hello”", Attributed: true, Unix: at(50 * time.Minute), FromMe: true, Chats: []int64{c}})
		store, _ := newIMessageStore(t)
		if preexisting {
			if err := store.UpsertMessage(&db.Message{MessageID: "imessage:typed", ConversationID: "imessage:iMessage;-;+15550000001",
				SenderName: "Me", Body: "Loved “hello”", TimestampMS: at(50*time.Minute) * 1000, Status: "delivered", IsFromMe: true,
				SourcePlatform: "imessage", SourceID: "typed"}); err != nil {
				t.Fatal(err)
			}
		}
		runImport(t, newPoller(chat.path), store)
		typed, _ := store.GetMessageByID("imessage:typed")
		if typed == nil || typed.Body != "Loved “hello”" {
			t.Fatalf("preexisting=%v: typed text = %+v", preexisting, typed)
		}
		if r := reactionsOf(t, store, "imessage:hello"); r != "" {
			t.Fatalf("preexisting=%v: typed text became a reaction: %s", preexisting, r)
		}
	}
}

// r1 #2: a reaction synced late with an older date leaves the state a full
// import does. An add dated between a target and its already-read removal
// must not survive the removal.
func TestIMessageLateOlderReactionsMatchAFullImport(t *testing.T) {
	chat := newNativeChatDB(t)
	alice := chat.addHandle("+15550000001")
	c := chat.addChat("iMessage;-;+15550000001", nil, false, alice)
	chat.message(c, "hello", "hello", at(3*time.Hour), false, alice)
	chat.react(c, "remove", 3000, "p:0/hello", at(1*time.Hour), true, alice)
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)

	chat.react(c, "late-add", 2000, "p:0/hello", at(2*time.Hour), true, alice)
	runImport(t, poller, store)
	if poller.last.full {
		t.Fatalf("an appended reaction forced a full read (%s)", poller.last.fullReason)
	}
	if r := reactionsOf(t, store, "imessage:hello"); r != "" {
		t.Fatalf("late, older add escaped the newer removal: %s", r)
	}
	requireMatchesFullImport(t, "late add before a removal", path, chat)

	// And a late removal older than a read add leaves the add.
	chat.react(c, "add", 2001, "p:0/hello", at(30*time.Minute), false, alice)
	runImport(t, poller, store)
	chat.react(c, "late-remove", 3001, "p:0/hello", at(40*time.Minute), false, alice)
	runImport(t, poller, store)
	if r := reactionsOf(t, store, "imessage:hello"); !strings.Contains(r, "👍") {
		t.Fatalf("an older removal undid a newer add: %q", r)
	}
	requireMatchesFullImport(t, "late removal before an add", path, chat)
}

// r1 #2: two messages with the same body. A reaction names its target by
// guid, so a later message with that body takes nothing from it.
func TestIMessageReactionsFollowTheGUIDNotTheText(t *testing.T) {
	chat := newNativeChatDB(t)
	alice := chat.addHandle("+15550000001")
	c := chat.addChat("iMessage;-;+15550000001", nil, false, alice)
	chat.message(c, "first", "same words", at(3*time.Hour), false, alice)
	chat.react(c, "love", 2000, "p:0/first", at(1*time.Hour), true, alice)
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)
	chat.message(c, "second", "same words", at(2*time.Hour), false, alice)
	runImport(t, poller, store)
	if r := reactionsOf(t, store, "imessage:second"); r != "" {
		t.Fatalf("a message with the same text took the reaction: %s", r)
	}
	if r := reactionsOf(t, store, "imessage:first"); r == "" {
		t.Fatal("the target lost its reaction")
	}
	requireMatchesFullImport(t, "same text", path, chat)
}

// r1 #3: a reaction read before its target exists is applied when the
// target arrives later (here with an older date), as a full import would.
func TestIMessageReactionBeforeItsTarget(t *testing.T) {
	chat := newNativeChatDB(t)
	alice := chat.addHandle("+15550000001")
	c := chat.addChat("iMessage;-;+15550000001", nil, false, alice)
	chat.react(c, "early", 2005, "p:0/late-target", at(time.Hour), true, alice)
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)
	if m, _ := store.GetMessageByID("imessage:early"); m != nil {
		t.Fatal("an unmatched reaction was stored as text")
	}
	chat.add(row{GUID: "late-target", Body: "archive body", Attributed: true, Unix: at(2 * time.Hour), Handle: alice, Chats: []int64{c}})
	runImport(t, poller, store)
	if poller.last.full {
		t.Fatalf("a late target forced a full read (%s)", poller.last.fullReason)
	}
	if r := reactionsOf(t, store, "imessage:late-target"); r != `[{"emoji":"❓","count":1,"actors":["me"]}]` {
		t.Fatalf("late target reactions = %q", r)
	}
	requireMatchesFullImport(t, "late target", path, chat)
}

// r1 #4: a message joined to several chats lives in the lowest one, however
// the joins and later polls arrive.
func TestIMessageSharedMessageHasOneOwner(t *testing.T) {
	chat := newNativeChatDB(t)
	h := chat.addHandle("+15550000001")
	a := chat.addChat("A", strPtr("A"), false, h)
	b := chat.addChat("B", strPtr("B"), false, h)
	cc := chat.addChat("C", strPtr("C"), false, h)
	chat.add(row{GUID: "shared", Body: "shared", Unix: at(3 * time.Hour), Handle: h, Chats: []int64{cc, b, a}})
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)
	for i, chatID := range []int64{cc, b, a} {
		chat.message(chatID, "new-"+string(rune('a'+i)), "new", at(time.Duration(2-i)*time.Hour), false, h)
		runImport(t, poller, store)
		m, _ := store.GetMessageByID("imessage:shared")
		if m.ConversationID != "imessage:A" {
			t.Fatalf("after a poll touching chat %d the owner is %s", chatID, m.ConversationID)
		}
	}
	requireMatchesFullImport(t, "three chats", path, chat)
}

// r1 #5 and I1: re-importing an unchanged chat.db, with matched and
// unmatched reactions, commits nothing and keeps unread counts; a reused
// importer does not even read the store.
func TestIMessageUnchangedReimportCommitsNothing(t *testing.T) {
	chat := newNativeChatDB(t)
	alice := chat.addHandle("+15550000001")
	c := chat.addChat("iMessage;-;+15550000001", strPtr("Alice"), false, alice)
	chat.message(c, "hello", "hello world", at(time.Hour), false, alice)
	chat.react(c, "love", 2000, "p:0/hello", at(50*time.Minute), false, alice)
	chat.react(c, "orphan", 2001, "p:0/nothing-there", at(40*time.Minute), false, alice)
	chat.add(row{GUID: "photo", Unix: at(30 * time.Minute), Handle: alice, Chats: []int64{c}}) // no body
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)

	obs := storeObserver(t, path)
	if _, err := obs.Exec(`UPDATE conversations SET unread_count = 37`); err != nil {
		t.Fatal(err)
	}
	before := dumpStore(t, path)
	version := storeDataVersion(t, obs)

	fresh := newPoller(chat.path)
	defer fresh.Close()
	res := runImport(t, fresh, store)
	if res.MessagesImported != 0 || res.ConversationsCreated != 0 || res.MessagesDuplicate != 1 || !fresh.last.full {
		t.Fatalf("unchanged full re-import = %+v (full=%v)", res, fresh.last.full)
	}
	if got := storeDataVersion(t, obs); got != version {
		t.Fatalf("unchanged re-import committed (data_version %d -> %d)", version, got)
	}
	requireSameStore(t, "unchanged re-import", dumpStore(t, path), before)

	// The reused importer returns on the unchanged data_version before
	// touching the store at all: a closed store would fail any read.
	closed, err := db.New(path)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if _, err := poller.ImportFromDB(closed); err != nil || !poller.last.skipped {
		t.Fatalf("unchanged poll: skipped=%v err=%v", poller.last.skipped, err)
	}
}

// r2 #2, input A: a message read while it had no join is stored once its
// join arrives, though no message row was added.
func TestIMessageLateJoinStoresTheMessage(t *testing.T) {
	chat := newNativeChatDB(t)
	h := chat.addHandle("+15550000001")
	c := chat.addChat("A", strPtr("A"), false, h)
	chat.message(c, "known", "known", at(3*time.Hour), false, h)
	late := chat.add(row{GUID: "late", Body: "late join", Unix: at(2 * time.Hour), Handle: h})
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)
	if m, _ := store.GetMessageByID("imessage:late"); m != nil {
		t.Fatal("an unjoined message was stored")
	}
	chat.join(c, late)
	runImport(t, poller, store)
	if poller.last.full {
		t.Fatalf("a late join forced a full read (%s)", poller.last.fullReason)
	}
	if m, _ := store.GetMessageByID("imessage:late"); m == nil {
		t.Fatal("late join lost the message")
	}
	requireMatchesFullImport(t, "late join", path, chat)
}

// r2 #2, input B: a late join to a lower chat moves an already-read message
// there, as a full import would.
func TestIMessageLateLowerOwnerJoinMovesTheMessage(t *testing.T) {
	chat := newNativeChatDB(t)
	h := chat.addHandle("+15550000001")
	a := chat.addChat("A", strPtr("A"), false, h)
	b := chat.addChat("B", strPtr("B"), false, h)
	shared := chat.message(b, "shared", "shared", at(3*time.Hour), false, h)
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)
	chat.join(a, shared)
	chat.message(a, "new-a", "new in A", at(2*time.Hour), false, h)
	runImport(t, poller, store)
	if m, _ := store.GetMessageByID("imessage:shared"); m.ConversationID != "imessage:A" {
		t.Fatalf("shared message owner = %s, want imessage:A", m.ConversationID)
	}
	requireMatchesFullImport(t, "late lower owner", path, chat)
}

// r1 #1: a replaced chat.db is read in full, whether it has fewer rows, at
// least as many rows as the mark, or the same rows with an older one edited.
func TestIMessageReplacedChatDB(t *testing.T) {
	build := func(t *testing.T, path string, prefix string, n int) *nativeChatDB {
		g := newNativeChatDBAt(t, path)
		h := g.addHandle("+15550000001")
		c := g.addChat("A", strPtr("A"), false, h)
		for i := 0; i < n; i++ {
			g.message(c, prefix+string(rune('a'+i)), prefix+" body "+string(rune('a'+i)), at(time.Duration(n-i)*time.Hour), false, h)
		}
		return g
	}
	cases := []struct {
		name    string
		replace func(t *testing.T, chat *nativeChatDB)
	}{
		{"fewer rows", func(t *testing.T, chat *nativeChatDB) {
			chat.replaceWith(build(t, t.TempDir()+"/next.db", "new", 2))
		}},
		{"as many rows as the mark", func(t *testing.T, chat *nativeChatDB) {
			chat.replaceWith(build(t, t.TempDir()+"/next.db", "new", 8))
		}},
		{"same rows, an old one edited", func(t *testing.T, chat *nativeChatDB) {
			next := chat.copyTo()
			next.exec(`UPDATE message SET text = 'edited long ago' WHERE ROWID = 1`)
			chat.replaceWith(next)
		}},
		{"rewritten in place", func(t *testing.T, chat *nativeChatDB) {
			// AUTOINCREMENT keeps the new ROWIDs above the old mark.
			chat.exec(`DELETE FROM message`)
			c := chat.queryInt(`SELECT ROWID FROM chat`)
			h := chat.queryInt(`SELECT ROWID FROM handle`)
			for i := 0; i < 8; i++ {
				chat.message(c, "new"+string(rune('a'+i)), "new body", at(time.Duration(8-i)*time.Minute), false, h)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chat := build(t, t.TempDir()+"/chat.db", "old", 5)
			polled, polledPath := newIMessageStore(t)
			reference, referencePath := newIMessageStore(t)
			poller := newPoller(chat.path)
			defer poller.Close()
			runImport(t, poller, polled)
			runImport(t, newPoller(chat.path), reference)

			tc.replace(t, chat)
			runImport(t, poller, polled)
			if !poller.last.full {
				t.Fatal("a replaced chat.db was not read in full")
			}
			fresh := newPoller(chat.path)
			runImport(t, fresh, reference)
			fresh.Close()
			requireSameStore(t, tc.name, dumpStore(t, polledPath), dumpStore(t, referencePath))
		})
	}
}

// r2 #3 and I5: reactions and edits change only the owned columns, so a
// transcript, reply id or media on the row, and the chat's unread count,
// survive; a reaction change elsewhere in the chat touches no other row.
func TestIMessageMetadataSurvivesReactionsAndEdits(t *testing.T) {
	chat := newNativeChatDB(t)
	alice := chat.addHandle("+15550000001")
	c := chat.addChat("iMessage;-;+15550000001", strPtr("Alice"), false, alice)
	voice := chat.message(c, "voice", "voice note", at(2*time.Hour), false, alice)
	chat.message(c, "other", "other", at(90*time.Minute), false, alice)
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)
	model := "whisper"
	if err := store.SetMessageTranscript("imessage:voice", "the words", &model); err != nil {
		t.Fatal(err)
	}
	obs := storeObserver(t, path)
	if _, err := obs.Exec(`UPDATE messages SET media_id = 'm', mime_type = 'audio/amr', reply_to_id = 'imessage:other' WHERE message_id = 'imessage:voice'`); err != nil {
		t.Fatal(err)
	}
	if _, err := obs.Exec(`UPDATE conversations SET unread_count = 4`); err != nil {
		t.Fatal(err)
	}
	otherBefore, _ := store.GetMessageByID("imessage:other")

	chat.react(c, "love", 2000, "p:0/voice", at(time.Hour), true, alice)
	chat.exec(`UPDATE message SET text = 'voice note (edited)' WHERE ROWID = ?`, voice) // in the window
	runImport(t, poller, store)
	got, _ := store.GetMessageByID("imessage:voice")
	if got.Body != "voice note (edited)" || got.Reactions == "" {
		t.Fatalf("edit or reaction missing: %+v", got)
	}
	if got.Transcript != "the words" || got.TranscriptModel != model || got.MediaID != "m" || got.MimeType != "audio/amr" || got.ReplyToID != "imessage:other" {
		t.Fatalf("metadata lost: %+v", got)
	}
	if conv, _ := store.GetConversation("imessage:iMessage;-;+15550000001"); conv.UnreadCount != 4 {
		t.Fatalf("unread count = %d, want 4", conv.UnreadCount)
	}
	if other, _ := store.GetMessageByID("imessage:other"); *other != *otherBefore {
		t.Fatalf("an untouched row changed: %+v -> %+v", otherBefore, other)
	}
}

// r2 #5: another writer commits between this import's reads and its writes,
// including a change to a non-owned column of a row the import updates. The
// import neither fails on a stale snapshot nor undoes that change.
func TestIMessageConcurrentWriterDuringImport(t *testing.T) {
	chat := newNativeChatDB(t)
	alice := chat.addHandle("+15550000001")
	c := chat.addChat("iMessage;-;+15550000001", strPtr("Alice"), false, alice)
	target := chat.message(c, "target", "target", at(2*time.Hour), false, alice)
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)
	writer, err := db.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	chat.exec(`UPDATE message SET text = 'target, edited' WHERE ROWID = ?`, target)
	chat.react(c, "love", 2000, "p:0/target", at(time.Hour), true, alice)
	poller.beforeApply = func() {
		if err := writer.UpsertMessage(&db.Message{MessageID: "sms:live", ConversationID: "sms:c", Body: "live", SourcePlatform: "sms"}); err != nil {
			t.Fatal(err)
		}
		if err := writer.SetMessageTranscript("imessage:target", "set meanwhile", nil); err != nil {
			t.Fatal(err)
		}
	}
	res := runImport(t, poller, store)
	if res.MessagesImported != 1 {
		t.Fatalf("result = %+v", res)
	}
	got, _ := store.GetMessageByID("imessage:target")
	if got.Body != "target, edited" || got.Reactions == "" || got.Transcript != "set meanwhile" {
		t.Fatalf("target = %+v", got)
	}
}

// r2 #7: rows the importer does not own stay as they are: an SMS row inside
// an iMessage conversation keeps its reactions, and a row with a reaction's
// id but another platform is not deleted as a stale copy.
func TestIMessageLeavesForeignRowsAlone(t *testing.T) {
	chat := newNativeChatDB(t)
	alice := chat.addHandle("+15550000001")
	c := chat.addChat("c1", strPtr("Alice"), false, alice)
	chat.message(c, "hello", "hello", at(2*time.Hour), false, alice)
	chat.react(c, "tap", 2000, "p:0/hello", at(time.Hour), false, alice)
	store, _ := newIMessageStore(t)
	foreign := []*db.Message{
		{MessageID: "sms:foreign", ConversationID: "imessage:c1", Body: "hello", TimestampMS: 1, SourcePlatform: "sms", Reactions: `[{"emoji":"🎉","count":1}]`},
		{MessageID: "imessage:tap", ConversationID: "signal:c2", Body: "important", TimestampMS: 1, SourcePlatform: "signal", SourceID: "x"},
	}
	for _, m := range foreign {
		if err := store.UpsertMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	runImport(t, newPoller(chat.path), store)
	for _, want := range foreign {
		got, _ := store.GetMessageByID(want.MessageID)
		if got == nil || got.Body != want.Body || got.Reactions != want.Reactions || got.ConversationID != want.ConversationID {
			t.Fatalf("foreign row changed: %+v", got)
		}
	}
}

// A store an older importer filled holds reactions as text rows and
// reactions RepairTapbacks guessed from quotes. A full import deletes the
// text copies (unless something was attached to them) and replaces guessed
// reactions with the native ones, and restores typed text the guess deleted.
func TestIMessageCleansUpOlderImports(t *testing.T) {
	chat := newNativeChatDB(t)
	alice := chat.addHandle("+15550000001")
	c := chat.addChat("c1", strPtr("Alice"), false, alice)
	chat.message(c, "hello", "hello", at(3*time.Hour), false, alice)
	chat.message(c, "bye", "bye", at(170*time.Minute), false, alice)
	chat.react(c, "tap", 2001, "p:0/hello", at(2*time.Hour), false, alice)
	chat.react(c, "tap-kept", 2000, "p:0/hello", at(100*time.Minute), true, alice)
	chat.add(row{GUID: "typed", Body: "Loved “bye”", Unix: at(time.Hour), FromMe: true, Chats: []int64{c}})
	store, _ := newIMessageStore(t)
	old := []*db.Message{
		{MessageID: "imessage:tap", ConversationID: "imessage:c1", Body: "Liked “hello”", SourcePlatform: "imessage", SourceID: "tap"},
		{MessageID: "imessage:tap-kept", ConversationID: "imessage:c1", Body: "Loved “hello”", SourcePlatform: "imessage", SourceID: "tap-kept"},
		{MessageID: "imessage:bye", ConversationID: "imessage:c1", Body: "bye", SourcePlatform: "imessage", SourceID: "bye",
			Reactions: `[{"emoji":"❤️","count":1,"actors":["me"]}]`}, // guessed from the typed row, which the guess deleted
	}
	for _, m := range old {
		if err := store.UpsertMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetMessageTranscript("imessage:tap-kept", "attached", nil); err != nil {
		t.Fatal(err)
	}
	runImport(t, newPoller(chat.path), store)
	if m, _ := store.GetMessageByID("imessage:tap"); m != nil {
		t.Fatal("stale reaction text row kept")
	}
	if m, _ := store.GetMessageByID("imessage:tap-kept"); m == nil {
		t.Fatal("a stale row with a transcript was deleted")
	}
	if r := reactionsOf(t, store, "imessage:hello"); r != `[{"emoji":"👍","count":1,"actors":["+15550000001"]},{"emoji":"❤️","count":1,"actors":["me"]}]` {
		t.Fatalf("hello reactions = %s", r)
	}
	if r := reactionsOf(t, store, "imessage:bye"); r != "" {
		t.Fatalf("guessed reaction kept: %s", r)
	}
	if m, _ := store.GetMessageByID("imessage:typed"); m == nil {
		t.Fatal("typed text not restored")
	}
}

// An edit to a row dated within Window is polled; one outside it waits for
// the periodic full read.
func TestIMessageEditsInAndOutOfTheWindow(t *testing.T) {
	chat := newNativeChatDB(t)
	h := chat.addHandle("+15550000001")
	c := chat.addChat("c1", strPtr("A"), false, h)
	recent := chat.message(c, "recent", "recent", at(time.Hour), false, h)
	old := chat.message(c, "old", "old", at(72*time.Hour), false, h)
	chat.message(c, "anchor", "anchor", at(30*time.Minute), false, h)
	store, path := newIMessageStore(t)
	clock := &fixedClock{t: testNow}
	poller := &IMessage{DBPath: chat.path, now: clock.now}
	runImport(t, poller, store)

	chat.exec(`UPDATE message SET text = 'recent, edited' WHERE ROWID = ?`, recent)
	chat.exec(`UPDATE message SET text = 'old, edited' WHERE ROWID = ?`, old)
	clock.t = testNow.Add(time.Minute)
	runImport(t, poller, store)
	if poller.last.full {
		t.Fatalf("an edit forced a full read (%s)", poller.last.fullReason)
	}
	if m, _ := store.GetMessageByID("imessage:recent"); m.Body != "recent, edited" {
		t.Fatalf("in-window edit not polled: %q", m.Body)
	}
	if m, _ := store.GetMessageByID("imessage:old"); m.Body != "old" {
		t.Fatalf("out-of-window edit was polled (the scope says it waits): %q", m.Body)
	}
	clock.t = testNow.Add(DefaultIMessageFullEvery + time.Minute)
	runImport(t, poller, store)
	if !poller.last.full || poller.last.fullReason != "periodic" {
		t.Fatalf("no periodic full read: %+v", poller.last)
	}
	requireMatchesFullImport(t, "after the periodic full read", path, chat)
}

// Chat renames, participant changes and new empty chats are polled; a
// changed handle forces a full read.
func TestIMessageChatChanges(t *testing.T) {
	chat := newNativeChatDB(t)
	alice := chat.addHandle("+15550000001")
	bob := chat.addHandle("+15550000002")
	g := chat.addChat("g", strPtr("Old name"), true, alice)
	chat.message(g, "m", "hi", at(time.Hour), false, alice)
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)

	chat.exec(`UPDATE chat SET display_name = NULL WHERE ROWID = ?`, g)
	chat.exec(`INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (?, ?)`, g, bob)
	chat.addChat("empty", nil, false, bob)
	runImport(t, poller, store)
	if poller.last.full {
		t.Fatalf("chat changes forced a full read (%s)", poller.last.fullReason)
	}
	conv, _ := store.GetConversation("imessage:g")
	if conv.Name != "+15550000001, +15550000002" {
		t.Fatalf("renamed chat = %+v", conv)
	}
	if conv, err := store.GetConversation("imessage:empty"); err != nil || conv.Name != "+15550000002" {
		t.Fatalf("new empty chat = %+v %v", conv, err)
	}
	requireMatchesFullImport(t, "chat changes", path, chat)

	chat.exec(`UPDATE handle SET uncanonicalized_id = '(555) 000-0001' WHERE ROWID = ?`, alice)
	runImport(t, poller, store)
	if !poller.last.full || poller.last.fullReason != "a handle changed" {
		t.Fatalf("changed handle: %+v", poller.last)
	}
	requireMatchesFullImport(t, "handle change", path, chat)
}

// Deleting rows at or below the marks forces a full read, including the
// case where the highest join is deleted and its rowid reused.
func TestIMessageDeletionsForceAFullRead(t *testing.T) {
	chat := newNativeChatDB(t)
	h := chat.addHandle("+15550000001")
	a := chat.addChat("A", strPtr("A"), false, h)
	b := chat.addChat("B", strPtr("B"), false, h)
	first := chat.message(a, "first", "first", at(3*time.Hour), false, h)
	chat.message(a, "second", "second", at(2*time.Hour), false, h)
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)

	chat.exec(`DELETE FROM message WHERE ROWID = ?`, first)
	chat.message(a, "third", "third", at(time.Hour), false, h) // the count balances, not the mark
	runImport(t, poller, store)
	if !poller.last.full || poller.last.fullReason != "message rows deleted" {
		t.Fatalf("deleted message: %+v", poller.last)
	}

	// Delete the highest join and add another: SQLite reuses its rowid.
	third := chat.queryInt(`SELECT ROWID FROM message WHERE guid = 'third'`)
	maxJoin := chat.queryInt(`SELECT MAX(rowid) FROM chat_message_join`)
	chat.exec(`DELETE FROM chat_message_join WHERE rowid = ?`, maxJoin)
	chat.join(b, third)
	if reused := chat.queryInt(`SELECT MAX(rowid) FROM chat_message_join`); reused != maxJoin {
		t.Fatalf("precondition: join rowid %d not reused (%d)", maxJoin, reused)
	}
	runImport(t, poller, store)
	if !poller.last.full || poller.last.fullReason != "the chat_message_join row at the mark changed" {
		t.Fatalf("reused join rowid: %+v", poller.last)
	}
	if m, _ := store.GetMessageByID("imessage:third"); m.ConversationID != "imessage:B" {
		t.Fatalf("moved message owner = %s", m.ConversationID)
	}
	_ = path // the deleted message stays stored, so no empty-store comparison
}

// Deleting a join below the mark, not the highest, is seen by the count
// alone. Here it moves a shared message to its other chat and lowers the
// first chat's newest message.
func TestIMessageDeletedJoinBelowTheMark(t *testing.T) {
	chat := newNativeChatDB(t)
	h := chat.addHandle("+15550000001")
	a := chat.addChat("A", strPtr("A"), false, h)
	b := chat.addChat("B", strPtr("B"), false, h)
	chat.message(a, "old", "old", at(5*time.Hour), false, h)
	chat.add(row{GUID: "shared", Body: "shared", Unix: at(time.Hour), Handle: h, Chats: []int64{a, b}})
	chat.message(b, "tail", "tail", at(3*time.Hour), false, h) // the highest join, left alone
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)

	shared := chat.queryInt(`SELECT ROWID FROM message WHERE guid = 'shared'`)
	chat.exec(`DELETE FROM chat_message_join WHERE chat_id = ? AND message_id = ?`, a, shared)
	runImport(t, poller, store)
	if !poller.last.full || poller.last.fullReason != "chat_message_join rows deleted" {
		t.Fatalf("deleted join below the mark: %+v", poller.last)
	}
	if m, _ := store.GetMessageByID("imessage:shared"); m.ConversationID != "imessage:B" {
		t.Fatalf("shared message owner = %s, want imessage:B", m.ConversationID)
	}
	if conv, _ := store.GetConversation("imessage:A"); conv.LastMessageTS != at(5*time.Hour)*1000 {
		t.Fatalf("chat A's newest message = %d, want the old one", conv.LastMessageTS)
	}
	requireMatchesFullImport(t, "deleted join below the mark", path, chat)
}

// Sync order is not insertion order: a message can name a handle, and a join
// a chat, whose row arrives later. The poll that first sees the handle or
// chat re-reads the messages naming it, so the sender and the owner end as
// a full import would set them.
func TestIMessageHandleAndChatRowsArrivingLate(t *testing.T) {
	chat := newNativeChatDB(t)
	h := chat.addHandle("+15550000001")
	c := chat.exec(`INSERT INTO chat (ROWID, guid, style, display_name) VALUES (2, 'c2', 45, 'Two')`)
	chat.exec(`INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (?, ?)`, c, h)
	chat.message(c, "m1", "from a handle not there yet", at(3*time.Hour), false, 99)
	orphan := chat.add(row{GUID: "m2", Body: "joined to a chat not there yet", Unix: at(2 * time.Hour), Handle: h})
	chat.exec(`INSERT INTO chat_message_join (chat_id, message_id, message_date) VALUES (50, ?, 0)`, orphan)
	shared := chat.message(c, "m3", "will move to a lower chat", at(time.Hour), false, h)
	chat.exec(`INSERT INTO chat_message_join (chat_id, message_id, message_date) VALUES (1, ?, 0)`, shared)
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)

	chat.exec(`INSERT INTO handle (ROWID, id, uncanonicalized_id) VALUES (99, '+15550000099', '+1 (555) 000-0099')`)
	chat.exec(`INSERT INTO chat (ROWID, guid, style, display_name) VALUES (50, 'c50', 45, 'Fifty')`)
	chat.exec(`INSERT INTO chat (ROWID, guid, style, display_name) VALUES (1, 'c1', 45, 'One')`)
	runImport(t, poller, store)
	if poller.last.full {
		t.Fatalf("late handle and chat rows forced a full read (%s)", poller.last.fullReason)
	}
	if m, _ := store.GetMessageByID("imessage:m1"); m.SenderNumber != "+15550000099" {
		t.Fatalf("sender after the late handle = %q", m.SenderNumber)
	}
	if m, _ := store.GetMessageByID("imessage:m2"); m == nil || m.ConversationID != "imessage:c50" {
		t.Fatalf("message after the late chat = %+v", m)
	}
	if m, _ := store.GetMessageByID("imessage:m3"); m.ConversationID != "imessage:c1" {
		t.Fatalf("owner after a late lower chat = %s", m.ConversationID)
	}
	requireMatchesFullImport(t, "late handle and chat rows", path, chat)
}

// A row whose write fails is retried by the next poll even when chat.db has
// not changed since; the marks still advance past it.
func TestIMessageRetriesFailedRows(t *testing.T) {
	chat := newNativeChatDB(t)
	h := chat.addHandle("+15550000001")
	c := chat.addChat("c1", strPtr("A"), false, h)
	chat.message(c, "m1", "one", at(2*time.Hour), false, h)
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)
	// Another row claims (imessage, m2), so inserting imessage:m2 fails.
	if err := store.UpsertMessage(&db.Message{MessageID: "blocker", ConversationID: "x", Body: "x", SourcePlatform: "imessage", SourceID: "m2"}); err != nil {
		t.Fatal(err)
	}
	chat.message(c, "m2", "two", at(time.Hour), false, h)
	chat.message(c, "m3", "three", at(50*time.Minute), false, h)
	res, err := poller.ImportFromDB(store)
	if err != nil || len(res.Errors) != 1 || !strings.HasPrefix(res.Errors[0], "imessage:m2:") || res.MessagesImported != 1 {
		t.Fatalf("poll with a failing row: %+v %v", res, err)
	}
	if err := store.DeleteMessageByID("blocker"); err != nil {
		t.Fatal(err)
	}
	res = runImport(t, poller, store)
	if poller.last.skipped || poller.last.full || res.MessagesImported != 1 {
		t.Fatalf("retry poll = %+v (%+v)", res, poller.last)
	}
	requireMatchesFullImport(t, "after the retry", path, chat)
}

// A chat.db read error leaves the marks where they were, so the next poll
// catches up on everything since.
func TestIMessageReadErrorKeepsTheMarks(t *testing.T) {
	chat := newNativeChatDB(t)
	h := chat.addHandle("+15550000001")
	c := chat.addChat("c1", strPtr("A"), false, h)
	chat.message(c, "m1", "one", at(3*time.Hour), false, h)
	store, path := newIMessageStore(t)
	poller := newPoller(chat.path)
	runImport(t, poller, store)
	chat.message(c, "m2", "two", at(2*time.Hour), false, h)
	chat.exec(`ALTER TABLE handle RENAME TO handle_moved`)
	if _, err := poller.ImportFromDB(store); err == nil {
		t.Fatal("expected a read error")
	}
	chat.exec(`ALTER TABLE handle_moved RENAME TO handle`)
	chat.message(c, "m3", "three", at(time.Hour), false, h)
	res := runImport(t, poller, store)
	if poller.last.full || res.MessagesImported != 2 {
		t.Fatalf("catch-up poll = %+v (%+v)", res, poller.last)
	}
	requireMatchesFullImport(t, "after a read error", path, chat)
}

// The importer still reads a chat.db without the association columns (very
// old Messages); every row is a message there.
func TestIMessageOldSchemaHasNoReactions(t *testing.T) {
	chat := newFakeChatDB(t)
	c := chat.addChat("c1", strPtr("A"), false, "+15550000001")
	chat.addMessage(c, "m1", "Loved “x”", false, 1_700_000_000, true)
	store, _ := newIMessageStore(t)
	res := runImport(t, &IMessage{DBPath: chat.path}, store)
	if res.MessagesImported != 1 {
		t.Fatalf("result = %+v", res)
	}
}
