package importer

import (
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// historyStats accumulates, over every seed, what the generator drew and
// what the importer did, so the test can prove it exercised each case.
type historyStats struct {
	kinds    map[string]int // operations drawn
	events   map[string]int // situations those operations produced
	paths    map[string]int // incremental, full, skipped polls
	reasons  map[string]int // why full polls were full
	appendOK int            // append-only seeds checked against an empty-store import
}

// propertySeeds is 160, or OPENMESSAGE_PROPERTY_SEEDS for a longer run.
func propertySeeds(t *testing.T) int {
	if v := os.Getenv("OPENMESSAGE_PROPERTY_SEEDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("OPENMESSAGE_PROPERTY_SEEDS: %v", err)
		}
		return n
	}
	if testing.Short() {
		return 24
	}
	return 160
}

// TestIMessagePollingMatchesFullReads (I2): for seeded random histories of
// chat.db changes, a store polled after every few changes by one reused
// importer equals, after every poll, a store given a full read by a fresh
// importer at the same points, on every column and search row. Append-only
// histories also equal one import of the final chat.db into an empty store.
// After the last step a full re-import of the polled store commits nothing.
func TestIMessagePollingMatchesFullReads(t *testing.T) {
	stats := &historyStats{kinds: map[string]int{}, events: map[string]int{}, paths: map[string]int{}, reasons: map[string]int{}}
	seeds := propertySeeds(t)
	for seed := 0; seed < seeds; seed++ {
		if !t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) { runHistory(t, uint64(seed), stats) }) {
			t.FailNow()
		}
	}
	report := func(name string, m map[string]int) string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%d", k, m[k])
		}
		return name + ":" + b.String()
	}
	t.Logf("%s", report("kinds", stats.kinds))
	t.Logf("%s", report("events", stats.events))
	t.Logf("%s", report("polls", stats.paths))
	t.Logf("%s", report("full reasons", stats.reasons))
	t.Logf("append-only seeds checked against an empty-store import: %d", stats.appendOK)

	// The generator must reach every case, or a pass proves little.
	scale := seeds / 8
	if scale < 1 {
		scale = 1
	}
	for _, k := range historyKinds {
		if stats.kinds[k.name] < scale {
			t.Errorf("kind %s drawn %d times, want at least %d", k.name, stats.kinds[k.name], scale)
		}
	}
	for _, e := range []string{
		"older-dated message", "shared message", "body in attributedBody", "no body",
		"reaction p:N", "reaction bp", "reaction bare", "reaction removal", "custom emoji", "sticker row",
		"reaction before its target", "reaction to an absent message", "late target with reactions",
		"first join", "lower-owner join", "typed tapback text", "in-window edit", "chat rename",
		"participants change", "handle change", "message deleted", "join deleted", "replaced (copy)",
		"replaced (new rows)", "user metadata",
	} {
		if stats.events[e] < scale {
			t.Errorf("event %q happened %d times, want at least %d", e, stats.events[e], scale)
		}
	}
	// Every seed's first poll is a full read; of the polls after it that
	// read anything, most must take the incremental path.
	later := stats.paths["incremental"] + stats.paths["full"] - stats.reasons["first read"]
	if stats.paths["incremental"]*10 < later*6 {
		t.Errorf("only %d of %d later reading polls were incremental; the property would mostly test full reads", stats.paths["incremental"], later)
	}
	if stats.paths["skipped"] < scale {
		t.Errorf("only %d polls were skipped on an unchanged data_version", stats.paths["skipped"])
	}
	for _, reason := range []string{"chat.db replaced", "message rows deleted", "a handle changed", "periodic"} {
		if stats.reasons[reason] < scale/2+1 {
			t.Errorf("full reason %q seen %d times", reason, stats.reasons[reason])
		}
	}
	if stats.reasons["chat_message_join rows deleted"]+stats.reasons["the chat_message_join row at the mark changed"] < scale/2+1 {
		t.Errorf("join deletions forced %d full reads", stats.reasons["chat_message_join rows deleted"]+stats.reasons["the chat_message_join row at the mark changed"])
	}
	if stats.appendOK < seeds/5 {
		t.Errorf("only %d append-only seeds", stats.appendOK)
	}
}

type historyKind struct {
	name   string
	weight int
	// appendOnly: allowed in an append-only seed (no row is deleted, no file
	// replaced, no store row edited by anyone but the importer).
	appendOnly bool
}

var historyKinds = []historyKind{
	{"message", 20, true},
	{"typed", 3, true},
	{"react", 18, true},
	{"late target", 5, true},
	{"orphan", 3, true},
	{"join", 7, true},
	{"chat", 3, true},
	{"rename", 3, true},
	{"participants", 3, true},
	{"handle", 2, true},
	{"edit", 5, true},
	{"jump", 1, true},
	{"poll", 22, true},
	{"user metadata", 3, false},
	{"delete message", 2, false},
	{"delete join", 2, false},
	{"replace", 1, false},
}

// history is one seed's generator state. Everything about chat.db is read
// back from it at each step; only names not yet used live here.
type history struct {
	t          *testing.T
	r          *rand.Rand
	stats      *historyStats
	clock      *fixedClock
	chat       *nativeChatDB
	nextName   int
	futures    []string // guids reactions named before any row had them
	appendOnly bool
}

func (h *history) name(prefix string) string {
	h.nextName++
	return fmt.Sprintf("%s-%d", prefix, h.nextName)
}

func (h *history) ids(query string, args ...any) []int64 {
	h.t.Helper()
	rows, err := h.chat.db.Query(query, args...)
	if err != nil {
		h.t.Fatalf("chat.db: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func (h *history) strs(query string, args ...any) []string {
	h.t.Helper()
	rows, err := h.chat.db.Query(query, args...)
	if err != nil {
		h.t.Fatalf("chat.db: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (h *history) pick(ids []int64) int64 {
	if len(ids) == 0 {
		return 0
	}
	return ids[h.r.IntN(len(ids))]
}

func (h *history) event(name string) { h.stats.events[name]++ }

// date draws a message time: mostly within the last day and a half, a fifth
// of the time up to a year back, so late rows often predate what was read.
func (h *history) date() int64 {
	now := h.clock.t.Unix()
	if h.r.IntN(5) == 0 {
		return now - 86400 - h.r.Int64N(365*86400)
	}
	return now - h.r.Int64N(36*3600)
}

func (h *history) handleOrMe() (fromMe bool, handle int64) {
	handles := h.ids(`SELECT ROWID FROM handle`)
	if h.r.IntN(3) == 0 || len(handles) == 0 {
		return true, h.pick(handles) // from-me rows carry the partner's handle, as real ones do
	}
	return false, h.pick(handles)
}

func (h *history) addMessage(guid string, chats []int64) {
	fromMe, handle := h.handleOrMe()
	r := row{GUID: guid, Unix: h.date(), FromMe: fromMe, Handle: handle, Chats: chats}
	if latest := h.chat.queryInt(`SELECT MAX(date) FROM message`); latest > coreDate(r.Unix) {
		h.event("older-dated message")
	}
	switch h.r.IntN(10) {
	case 0:
		h.event("no body")
	case 1, 2, 3, 4, 5:
		r.Body, r.Attributed = "body "+guid, true
		h.event("body in attributedBody")
	default:
		r.Body = "body " + guid
	}
	h.chat.add(r)
}

func (h *history) step(kind string) {
	chat := h.chat
	chats := h.ids(`SELECT ROWID FROM chat`)
	switch kind {
	case "message":
		joined := []int64{h.pick(chats)}
		if h.r.IntN(8) == 0 && len(chats) > 1 {
			joined = append(joined, h.pick(chats))
			if joined[0] != joined[1] {
				h.event("shared message")
			} else {
				joined = joined[:1]
			}
		}
		h.addMessage(h.name("m"), joined)
	case "orphan":
		h.addMessage(h.name("o"), nil)
	case "typed":
		bodies := h.strs(`SELECT COALESCE(text, 'x') FROM message WHERE associated_message_type = 0`)
		quoted := "x"
		if len(bodies) > 0 {
			quoted = bodies[h.r.IntN(len(bodies))]
		}
		texts := []string{"Loved “" + quoted + "”", "Liked \"" + quoted + "\"", "Reacted 🎉 to “" + quoted + "”", "Removed a heart from “" + quoted + "”"}
		fromMe, handle := h.handleOrMe()
		chat.add(row{GUID: h.name("typed"), Body: texts[h.r.IntN(len(texts))], Attributed: h.r.IntN(2) == 0,
			Unix: h.date(), FromMe: fromMe, Handle: handle, Chats: []int64{h.pick(chats)}})
		h.event("typed tapback text")
	case "react":
		var target string
		switch n := h.r.IntN(20); {
		case n < 12:
			guids := h.strs(`SELECT guid FROM message`)
			if len(guids) == 0 {
				target = h.name("ghost")
			} else {
				target = guids[h.r.IntN(len(guids))]
			}
		case n < 17:
			target = h.name("future")
			h.futures = append(h.futures, target)
			h.event("reaction before its target")
		default:
			target = h.name("ghost")
			h.event("reaction to an absent message")
		}
		var assoc string
		switch h.r.IntN(3) {
		case 0:
			assoc = fmt.Sprintf("p:%d/%s", h.r.IntN(3), target)
			h.event("reaction p:N")
		case 1:
			assoc = "bp:" + target
			h.event("reaction bp")
		default:
			assoc = target
			h.event("reaction bare")
		}
		kind := int64(h.r.IntN(7))
		assocType := 2000 + kind
		if h.r.IntN(3) == 0 {
			assocType = 3000 + kind
			h.event("reaction removal")
		}
		emoji := ""
		if kind == 6 {
			emoji = []string{"🎉", "🔥", ""}[h.r.IntN(3)]
			h.event("custom emoji")
		}
		body := "reaction text"
		if h.r.IntN(15) == 0 {
			assocType, body = 2007, "sticker "+target // a sticker reaction: a row, not a tapback
			h.event("sticker row")
		}
		fromMe, handle := h.handleOrMe()
		var joined []int64
		if h.r.IntN(10) != 0 {
			joined = []int64{h.pick(chats)}
		}
		chat.add(row{GUID: h.name("r"), Body: body, Attributed: true, Unix: h.date(), FromMe: fromMe, Handle: handle,
			AssocType: assocType, AssocGUID: assoc, Emoji: emoji, Chats: joined})
	case "late target":
		var open []string
		for _, g := range h.futures {
			if chat.queryInt(`SELECT COUNT(*) FROM message WHERE guid = ?`, g) == 0 {
				open = append(open, g)
			}
		}
		if len(open) == 0 {
			h.addMessage(h.name("m"), []int64{h.pick(chats)})
			return
		}
		g := open[h.r.IntN(len(open))]
		h.addMessage(g, []int64{h.pick(chats)})
		h.event("late target with reactions")
	case "join":
		msgs := h.ids(`SELECT ROWID FROM message`)
		m, c := h.pick(msgs), h.pick(chats)
		if m == 0 || c == 0 || chat.queryInt(`SELECT COUNT(*) FROM chat_message_join WHERE chat_id = ? AND message_id = ?`, c, m) > 0 {
			return
		}
		owner := chat.queryInt(`SELECT MIN(chat_id) FROM chat_message_join WHERE message_id = ?`, m)
		switch {
		case owner == 0:
			h.event("first join")
		case c < owner:
			h.event("lower-owner join")
		}
		chat.join(c, m)
	case "chat":
		handles := h.ids(`SELECT ROWID FROM handle`)
		var name *string
		if h.r.IntN(2) == 0 {
			name = strPtr(h.name("Chat"))
		}
		var members []int64
		for i := 0; i < 1+h.r.IntN(3); i++ {
			if m := h.pick(handles); m != 0 && !containsID(members, m) {
				members = append(members, m)
			}
		}
		chat.addChat(h.name("chat"), name, len(members) > 1, members...)
	case "rename":
		var name any
		if h.r.IntN(3) != 0 {
			name = h.name("Renamed")
		}
		chat.exec(`UPDATE chat SET display_name = ? WHERE ROWID = ?`, name, h.pick(chats))
		h.event("chat rename")
	case "participants":
		c, handle := h.pick(chats), h.pick(h.ids(`SELECT ROWID FROM handle`))
		if chat.queryInt(`SELECT COUNT(*) FROM chat_handle_join WHERE chat_id = ? AND handle_id = ?`, c, handle) > 0 {
			chat.exec(`DELETE FROM chat_handle_join WHERE chat_id = ? AND handle_id = ?`, c, handle)
		} else {
			chat.exec(`INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (?, ?)`, c, handle)
		}
		h.event("participants change")
	case "handle":
		if h.r.IntN(2) == 0 {
			chat.addHandle(fmt.Sprintf("+1555%07d", h.r.IntN(10_000_000)))
			return
		}
		chat.exec(`UPDATE handle SET uncanonicalized_id = ? WHERE ROWID = ?`, h.name("display"), h.pick(h.ids(`SELECT ROWID FROM handle`)))
		h.event("handle change")
	case "edit":
		// Rows well inside the window, so the clock cannot carry them out of
		// it before the next poll.
		recent := h.ids(`SELECT ROWID FROM message WHERE associated_message_type = 0 AND date >= ?`, coreDate(h.clock.t.Unix()-20*3600))
		m := h.pick(recent)
		if m == 0 {
			return
		}
		switch h.r.IntN(3) {
		case 0:
			chat.exec(`UPDATE message SET text = ?, attributedBody = NULL WHERE ROWID = ?`, h.name("edited"), m)
		case 1:
			chat.exec(`UPDATE message SET attributedBody = ?, text = NULL WHERE ROWID = ?`, makeAttributedBody(h.name("edited")), m)
		default:
			chat.exec(`UPDATE message SET date = date + ? WHERE ROWID = ?`, int64(1+h.r.IntN(5))*1_000_000_000, m)
		}
		h.event("in-window edit")
	case "jump":
		h.clock.t = h.clock.t.Add(DefaultIMessageFullEvery + time.Minute)
	case "delete message":
		chat.exec(`DELETE FROM message WHERE ROWID = ?`, h.pick(h.ids(`SELECT ROWID FROM message`)))
		h.event("message deleted")
	case "delete join":
		joins := h.ids(`SELECT rowid FROM chat_message_join`)
		target := h.pick(joins)
		if h.r.IntN(2) == 0 && len(joins) > 0 {
			target = joins[len(joins)-1] // the highest: its rowid is reused by the next join
		}
		chat.exec(`DELETE FROM chat_message_join WHERE rowid = ?`, target)
		h.event("join deleted")
	case "replace":
		if h.r.IntN(2) == 0 {
			next := chat.copyTo()
			switch h.r.IntN(3) {
			case 0:
				next.exec(`UPDATE message SET text = 'changed in the copy', attributedBody = NULL WHERE ROWID = (SELECT MIN(ROWID) FROM message WHERE associated_message_type = 0)`)
			case 1:
				next.exec(`DELETE FROM message WHERE ROWID = (SELECT MIN(ROWID) FROM message)`)
			default:
				next.exec(`UPDATE chat SET display_name = 'renamed in the copy'`)
			}
			chat.replaceWith(next)
			h.event("replaced (copy)")
			return
		}
		// A different chat.db with at least as many rows as the old mark.
		next := newNativeChatDBAt(h.t, h.t.TempDir()+"/next.db")
		handle := next.addHandle("+15559990000")
		c := next.addChat(h.name("newchat"), nil, false, handle)
		for n := chat.queryInt(`SELECT MAX(ROWID) FROM message`) + 2; n > 0; n-- {
			next.message(c, h.name("n"), "replacement body", h.date(), h.r.IntN(2) == 0, handle)
		}
		chat.replaceWith(next)
		h.futures = nil
		h.event("replaced (new rows)")
	}
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func runHistory(t *testing.T, seed uint64, stats *historyStats) {
	r := rand.New(rand.NewPCG(seed, 0x0f1e2d3c))
	h := &history{t: t, r: r, stats: stats, clock: &fixedClock{t: testNow}, chat: newNativeChatDB(t), appendOnly: seed%4 == 0}
	alice, bob := h.chat.addHandle("+15550000001"), h.chat.addHandle("bob@example.com")
	h.chat.addChat("chat-a", strPtr("A"), false, alice)
	h.chat.addChat("chat-b", nil, true, alice, bob)
	for i := 0; i < 3; i++ {
		h.step("message")
	}

	polled, polledPath := newIMessageStore(t)
	reference, referencePath := newIMessageStore(t)
	poller := &IMessage{DBPath: h.chat.path, now: h.clock.now}
	defer poller.Close()

	poll := func(label string) {
		t.Helper()
		runImport(t, poller, polled)
		switch {
		case poller.last.skipped:
			stats.paths["skipped"]++
		case poller.last.full:
			stats.paths["full"]++
			stats.reasons[poller.last.fullReason]++
		default:
			stats.paths["incremental"]++
		}
		fresh := &IMessage{DBPath: h.chat.path, now: h.clock.now}
		runImport(t, fresh, reference)
		fresh.Close()
		requireSameStore(t, label, dumpStore(t, polledPath), dumpStore(t, referencePath))
	}

	total := 0
	for _, k := range historyKinds {
		if h.appendOnly && !k.appendOnly {
			continue
		}
		total += k.weight
	}
	const steps = 40
	for i := 0; i < steps; i++ {
		h.clock.t = h.clock.t.Add(time.Duration(1+r.IntN(120)) * time.Second)
		n := r.IntN(total)
		var kind string
		for _, k := range historyKinds {
			if h.appendOnly && !k.appendOnly {
				continue
			}
			if n < k.weight {
				kind = k.name
				break
			}
			n -= k.weight
		}
		stats.kinds[kind]++
		switch kind {
		case "poll":
			poll(fmt.Sprintf("seed %d step %d", seed, i))
		case "user metadata":
			// Someone else edits rows the importer shares: the same edit in
			// both stores, which every later poll must keep.
			for _, path := range []string{polledPath, referencePath} {
				obs := storeObserver(t, path)
				if _, err := obs.Exec(`UPDATE conversations SET unread_count = unread_count + 3 WHERE conversation_id = (SELECT MIN(conversation_id) FROM conversations)`); err != nil {
					t.Fatal(err)
				}
				if _, err := obs.Exec(`UPDATE messages SET transcript = 'user transcript', reply_to_id = 'imessage:x' WHERE message_id = (SELECT MAX(message_id) FROM messages)`); err != nil {
					t.Fatal(err)
				}
				obs.Close()
			}
			h.event("user metadata")
		default:
			h.step(kind)
		}
	}
	poll(fmt.Sprintf("seed %d final", seed))
	// A second poll with nothing changed returns on data_version.
	poll(fmt.Sprintf("seed %d unchanged", seed))

	obs := storeObserver(t, polledPath)
	version := storeDataVersion(t, obs)
	fresh := &IMessage{DBPath: h.chat.path, now: h.clock.now}
	res := runImport(t, fresh, polled)
	fresh.Close()
	if res.MessagesImported != 0 || res.ConversationsCreated != 0 {
		t.Fatalf("seed %d: a full re-import of the polled store wrote %+v", seed, res)
	}
	if got := storeDataVersion(t, obs); got != version {
		t.Fatalf("seed %d: a full re-import of the polled store committed", seed)
	}
	if h.appendOnly {
		requireSameStore(t, fmt.Sprintf("seed %d against an empty-store import", seed), dumpStore(t, polledPath), fullImportDump(t, h.chat.path, h.clock.t))
		stats.appendOK++
	}
}
