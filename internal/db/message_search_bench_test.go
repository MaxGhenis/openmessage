package db

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A searchBenchmark is a query and a filter, named for what they find.
type searchBenchmark struct {
	name   string
	query  string
	filter SearchFilter
}

// uniformSearches search benchmarkStore, where message i reads "message number
// i about dinner plans, the weekend and a shared document", sits in
// conversation i mod 200, comes from sender i mod 50 and has timestamp i.
var uniformSearches = []searchBenchmark{
	{"every message", "dinner", SearchFilter{}},
	{"one message", "number 4242 about", SearchFilter{}},
	{"no message", "no such phrase", SearchFilter{}},
	{"two letters", "ab", SearchFilter{}},
	{"every message in a conversation", "dinner", SearchFilter{ConversationID: "c42"}},
	{"one message from a sender", "number 4242 about", SearchFilter{Phone: "+155500042"}},
	{"one message in a window", "number 4242 about", SearchFilter{SinceMS: 4000, UntilMS: 5000}},
}

const (
	chatStartMS = 1704067200000 // 2024-01-01 UTC
	chatStepMS  = 20 * 60 * 1000
)

// plantedWord is a word chatBenchmarkStore adds to count messages, spread
// evenly over time unless it goes in the newest or the oldest.
type plantedWord struct {
	word, place string
	count       int
}

var plantedWords = []plantedWord{
	{"birthday", "spread", 4000},
	{"airport", "spread", 800},
	{"passport", "spread", 200},
	{"dentist", "spread", 50},
	{"zeppelin", "spread", 10},
	{"quokka", "spread", 1},
	{"reunion", "newest", 200},
	{"lasagna", "oldest", 1000},
}

// chatAbsent are in no message of a chat store: a rare word, and a phrase of
// the commonest words.
var chatAbsent = []string{"xylophone", "dinner tomorrow and the meeting with you"}

func (p plantedWord) in(i, n int) bool {
	switch p.place {
	case "newest":
		return i > n-p.count
	case "oldest":
		return i <= p.count
	}
	step := n / p.count
	return i%step == step/2
}

// chatSearches search chatBenchmarkStore.
var chatSearches = []searchBenchmark{
	{"the", "the", SearchFilter{}},
	{"birthday in 4000", "birthday", SearchFilter{}},
	{"airport in 800", "airport", SearchFilter{}},
	{"passport in 200", "passport", SearchFilter{}},
	{"dentist in 50", "dentist", SearchFilter{}},
	{"zeppelin in 10", "zeppelin", SearchFilter{}},
	{"quokka in 1", "quokka", SearchFilter{}},
	{"xylophone in none", chatAbsent[0], SearchFilter{}},
	{"a phrase of common words in none", chatAbsent[1], SearchFilter{}},
	{"64 runs of passport", strings.Repeat("passport%", 64), SearchFilter{}},
	{"reunion in the newest 200", "reunion", SearchFilter{}},
	{"lasagna in the oldest 1000", "lasagna", SearchFilter{}},
	{"two letters", "ok", SearchFilter{}},
	{"the in the largest conversation", "the", SearchFilter{ConversationID: "c0"}},
	{"passport in the largest conversation", "passport", SearchFilter{ConversationID: "c0"}},
	{"passport in a small conversation", "passport", SearchFilter{ConversationID: "c150"}},
	{"passport in a month", "passport", SearchFilter{
		SinceMS: chatStartMS + 40_000*chatStepMS,
		UntilMS: chatStartMS + 40_000*chatStepMS + (30 * 24 * time.Hour).Milliseconds(),
	}},
}

// chatBenchmarkStore opens a store of n messages whose words vary as chat's
// do, indexed as an open rebuilds them. Each body holds 2 to 21 words drawn
// so that a few, such as "the", turn up in many messages and most in few, and
// one body in ten starts with a capital. A message comes every 20 minutes, so
// 80,000 span three years, and the largest of 200 conversations holds about a
// sixth of them. plantedWords adds each of its words to a set number of
// messages.
func chatBenchmarkStore(b *testing.B, n int) *Store {
	b.Helper()
	path := filepath.Join(b.TempDir(), "messages.db")
	store, err := New(path)
	if err != nil {
		b.Fatalf("New(): %v", err)
	}
	r := rand.New(rand.NewSource(7))
	vocabulary := []string{
		"the", "and", "you", "for", "that", "with", "have", "this", "are", "but", "not",
		"what", "all", "were", "when", "can", "dinner", "tomorrow", "meeting", "love", "call",
	}
	for len(vocabulary) < 6000 {
		word := make([]byte, 3+r.Intn(8))
		for i := range word {
			word[i] = byte('a' + r.Intn(26))
		}
		vocabulary = append(vocabulary, string(word))
	}
	skewed := func(k int) int {
		u := r.Float64()
		return int(float64(k) * u * u * u)
	}

	tx, err := store.db.Begin()
	if err != nil {
		b.Fatalf("Begin(): %v", err)
	}
	insert, err := tx.Prepare(`INSERT INTO messages (message_id, conversation_id, sender_number, body, timestamp_ms, source_platform) VALUES (?, ?, ?, ?, ?, 'sms')`)
	if err != nil {
		b.Fatalf("prepare: %v", err)
	}
	for i := 1; i <= n; i++ {
		words := make([]string, 2+r.Intn(20))
		for j := range words {
			words[j] = vocabulary[skewed(len(vocabulary))]
		}
		for _, p := range plantedWords {
			if p.in(i, n) {
				words = append(words, p.word)
			}
		}
		body := strings.Join(words, " ")
		if r.Intn(10) == 0 {
			body = strings.ToUpper(body[:1]) + body[1:]
		}
		conversation := skewed(200)
		if _, err := insert.Exec(
			fmt.Sprintf("m%d", i), fmt.Sprintf("c%d", conversation), fmt.Sprintf("+1555%07d", conversation),
			body, chatStartMS+int64(i)*chatStepMS,
		); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("Commit(): %v", err)
	}
	if err := store.Close(); err != nil {
		b.Fatalf("Close(): %v", err)
	}
	if store, err = New(path); err != nil {
		b.Fatalf("reopen: %v", err)
	}
	b.Cleanup(func() { store.Close() })

	counts := map[string]int{}
	for _, absent := range chatAbsent {
		counts[absent] = 0
	}
	for _, p := range plantedWords {
		counts[p.word] = p.count
	}
	for word, want := range counts {
		var got int
		if err := store.db.QueryRow(`SELECT count(*) FROM messages WHERE body LIKE ?`, "%"+word+"%").Scan(&got); err != nil {
			b.Fatalf("count %s: %v", word, err)
		}
		if got != want {
			b.Fatalf("%d messages hold %q, want %d", got, word, want)
		}
	}
	return store
}

// searchBenchmarkLimit is how many results each benchmark search asks for, as
// the HTTP API does by default.
const searchBenchmarkLimit = 50

// runSearchBenchmarks opens each benchmark store of 80,000 messages once and
// runs search over it for each of its searches.
func runSearchBenchmarks(b *testing.B, search func(b *testing.B, store *Store, query string, f SearchFilter)) {
	for _, set := range []struct {
		name     string
		open     func(*testing.B, int) *Store
		searches []searchBenchmark
	}{
		{"uniform", benchmarkStore, uniformSearches},
		{"chat", chatBenchmarkStore, chatSearches},
	} {
		b.Run(set.name, func(b *testing.B) {
			store := set.open(b, 80_000)
			for _, s := range set.searches {
				f := s.filter
				f.Limit = searchBenchmarkLimit
				b.Run(s.name, func(b *testing.B) { search(b, store, s.query, f) })
			}
		})
	}
}

// BenchmarkSearchLargeStore searches stores of 80,000 messages with
// SearchMessagesFiltered, as the CLI, the MCP tool and the HTTP API do.
func BenchmarkSearchLargeStore(b *testing.B) {
	runSearchBenchmarks(b, func(b *testing.B, store *Store, query string, f SearchFilter) {
		for i := 0; i < b.N; i++ {
			if _, err := store.SearchMessagesFiltered(query, f); err != nil {
				b.Fatal(err)
			}
		}
	})
}
