package tools

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// routeBenchSizes are the live stores' sizes on 2026-11-04, from aggregate
// counts read without names, numbers or text: the legacy store's threads,
// participant entries, messages, incoming messages and distinct
// (thread, sender) pairs, and v2's conversations, participant rows and
// identities other than the owner's. The address book holds 3,000 cards at
// the live size.
const (
	routeBenchThreads, routeBenchEntries                    = 2180, 8267
	routeBenchMessages, routeBenchIncoming, routeBenchPairs = 79793, 63730, 3667
	routeBenchCards                                         = 3000
	routeBenchV2Conversations, routeBenchV2Participants     = 1479, 5362
	routeBenchV2Identities                                  = 3374
)

// routeBenchPerson names person index and gives the value their card and
// threads hold. One person in twenty shares the first name Alex, so Alex
// finds far more cards and thread entries than the route tool's 80, as a
// common first name does; their values mix phone numbers with email
// addresses, Signal UUIDs and SMS short codes, which the tool looks up as
// whole addresses. Person 7 is the one Zelda.
func routeBenchPerson(index int) (name, value string) {
	switch {
	case index == 7:
		return "Zelda Quill", "+12025550007"
	case index%20 == 0:
		name = fmt.Sprintf("Alex %d", index)
		switch index / 20 % 7 {
		case 0, 1, 2:
			return name, fmt.Sprintf("+1303%07d", index)
		case 3:
			return name, fmt.Sprintf("alex.%d@example.com", index)
		case 4:
			return name, fmt.Sprintf("abcdefab-abcd-abcd-abcd-%012x", index)
		default:
			return name, fmt.Sprintf("%05d", 70000+index%30000)
		}
	}
	return fmt.Sprintf("Person %d", index), fmt.Sprintf("+1202%07d", index)
}

// routeBenchStores builds a legacy store and a v2 store scale times the live
// size in dir, writing rows straight into each file, and opens them.
func routeBenchStores(b *testing.B, dir string, scale int) (*db.Store, *sqlite.Store) {
	b.Helper()
	legacyPath := filepath.Join(dir, fmt.Sprintf("legacy-%d.db", scale))
	legacy, err := db.New(legacyPath)
	if err != nil {
		b.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		b.Fatal(err)
	}
	fill := func(path string, write func(tx *sql.Tx)) {
		conn, err := sql.Open("sqlite", path)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		tx, err := conn.Begin()
		if err != nil {
			b.Fatal(err)
		}
		write(tx)
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
		if _, err := conn.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			b.Fatal(err)
		}
	}
	prepare := func(tx *sql.Tx, query string) *sql.Stmt {
		stmt, err := tx.Prepare(query)
		if err != nil {
			b.Fatal(err)
		}
		return stmt
	}
	exec := func(stmt *sql.Stmt, args ...any) {
		if _, err := stmt.Exec(args...); err != nil {
			b.Fatal(err)
		}
	}

	threads, entries := routeBenchThreads*scale, routeBenchEntries*scale
	messages, incoming, pairs := routeBenchMessages*scale, routeBenchIncoming*scale, routeBenchPairs*scale
	ownerOnly := 33 * scale
	fill(legacyPath, func(tx *sql.Tx) {
		conversation := prepare(tx, `INSERT INTO conversations (conversation_id, name, participants, last_message_ts, source_platform) VALUES (?, ?, ?, ?, 'sms')`)
		written := 0
		for thread := 0; thread < threads; thread++ {
			list := []map[string]any{{"name": "Me", "number": "+15550009999", "is_me": true}}
			written++
			if thread < threads-ownerOnly {
				for person := thread; person < entries-threads; person += threads - ownerOnly {
					name, value := routeBenchPerson(person)
					list = append(list, map[string]any{"name": name, "number": value})
					written++
				}
			}
			participants, err := json.Marshal(list)
			if err != nil {
				b.Fatal(err)
			}
			exec(conversation, fmt.Sprintf("c%d", thread), fmt.Sprintf("Conversation %d", thread), string(participants), thread+1)
		}
		if written != entries {
			b.Fatalf("wrote %d participant entries, want %d", written, entries)
		}
		message := prepare(tx, `INSERT INTO messages (message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, is_from_me, source_platform) VALUES (?, ?, ?, ?, ?, ?, ?, 'sms')`)
		for index := 0; index < messages; index++ {
			pair := index % pairs
			name, value := routeBenchPerson(pair)
			fromMe := index >= incoming
			if fromMe {
				name, value = "Me", "+15550009999"
			}
			exec(message, fmt.Sprintf("m%d", index), fmt.Sprintf("c%d", pair%(threads-ownerOnly)), name, value, "Message body.", index+1, fromMe)
		}
		card := prepare(tx, `INSERT INTO contacts (contact_id, name, number) VALUES (?, ?, ?)`)
		for index := 0; index < routeBenchCards*scale; index++ {
			name, value := routeBenchPerson(index)
			exec(card, fmt.Sprintf("p%d", index), name, value)
		}
	})
	legacy, err = db.New(legacyPath)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = legacy.Close() })

	v2Path := filepath.Join(dir, fmt.Sprintf("v2-%d.sqlite3", scale))
	store, err := sqlite.Open(v2Path)
	if err != nil {
		b.Fatal(err)
	}
	if err := store.Close(); err != nil {
		b.Fatal(err)
	}
	conversations, participants, identities := routeBenchV2Conversations*scale, routeBenchV2Participants*scale, routeBenchV2Identities*scale
	fill(v2Path, func(tx *sql.Tx) {
		exec(prepare(tx, `INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES ('account', 'google_messages', 1, 1)`))
		identity := prepare(tx, `INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, display_name, is_self, created_at_ms, updated_at_ms) VALUES (?, 'account', ?, ?, ?, ?, ?, 1, 1)`)
		exec(identity, "self", "e164", "+15550009999", "+15550009999", "Me", true)
		for index := 0; index < identities; index++ {
			name, value := routeBenchPerson(index)
			exec(identity, fmt.Sprintf("i%d", index), "e164", value, value, name, false)
		}
		conversation := prepare(tx, `INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, last_message_at_ms, created_at_ms, updated_at_ms) VALUES (?, 'account', ?, 'group', ?, 1, 1)`)
		member := prepare(tx, `INSERT INTO conversation_participants (account_id, conversation_id, identity_id) VALUES ('account', ?, ?)`)
		rows := 0
		for index := 0; index < conversations; index++ {
			id := fmt.Sprintf("v%d", index)
			exec(conversation, id, "remote-"+id, index+1)
			exec(member, id, "self")
			rows++
		}
		for index := 0; rows < participants; index++ {
			exec(member, fmt.Sprintf("v%d", index%conversations), fmt.Sprintf("i%d", index%identities))
			rows++
		}
	})
	store, err = sqlite.Open(v2Path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return legacy, store
}

// cpuTime is the CPU time, user and system, that this process has used.
func cpuTime(b *testing.B) time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// BenchmarkResolveContactRoutesSearches times findRouteConversations, at the
// tool's default limit of 10 (80 inside), for an absent name, a rare name, a
// common first name and the owner's name, on each serving store, at the
// live size and at ten times it, and ListContactsFromConversations for the
// same names and for a one-letter autocomplete. It reports CPU time as well
// as wall time.
//
//	go test ./internal/tools -run '^$' -bench BenchmarkResolveContactRoutesSearches -benchtime 3x
func BenchmarkResolveContactRoutesSearches(b *testing.B) {
	dir := b.TempDir()
	for _, scale := range []int{1, 10} {
		b.Run(fmt.Sprintf("scale=%d", scale), func(b *testing.B) {
			legacy, store := routeBenchStores(b, dir, scale)
			modes := map[string]readsource.ReadSource{"legacy": legacy, "v2": v2read.New(store)}
			for _, mode := range []string{"legacy", "v2"} {
				for _, query := range []string{"Nobody", "Zelda", "Alex", "Me"} {
					b.Run(fmt.Sprintf("routes/%s/%s", mode, query), func(b *testing.B) {
						found := 0
						started := cpuTime(b)
						b.ResetTimer()
						for range b.N {
							routes, err := findRouteConversations(modes[mode], legacy, query, 80)
							if err != nil {
								b.Fatal(err)
							}
							found = len(routes)
						}
						b.StopTimer()
						b.ReportMetric(float64(cpuTime(b)-started)/float64(b.N)/1e6, "cpu-ms/op")
						b.ReportMetric(float64(found), "routes")
					})
				}
			}
			for _, query := range []string{"Nobody", "Zelda", "Alex", "Me", "M"} {
				b.Run(fmt.Sprintf("contacts/%s", query), func(b *testing.B) {
					found := 0
					started := cpuTime(b)
					b.ResetTimer()
					for range b.N {
						contacts, err := legacy.ListContactsFromConversations(query, 80)
						if err != nil {
							b.Fatal(err)
						}
						found = len(contacts)
					}
					b.StopTimer()
					b.ReportMetric(float64(cpuTime(b)-started)/float64(b.N)/1e6, "cpu-ms/op")
					b.ReportMetric(float64(found), "contacts")
				})
			}
		})
	}
}
