package sqlite

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/quick"
	"unicode/utf8"

	moderncsqlite "modernc.org/sqlite"

	"github.com/maxghenis/openmessage/internal/addressmatch"
	"github.com/maxghenis/openmessage/internal/phoneline"
)

// batchPhoneDigits and batchAddressValues are what identities hold in a
// random store. The phones include numbers that hold another's last ten
// digits inside them, share only its last seven, or are too short to name a
// line. The addresses include values that hold another inside them, differ
// from it only in a non-ASCII letter's case or only after a NUL, hold a byte
// that isn't UTF-8, or have white space around them, which another writer may
// keep. The owner holds the first phone and the first address under self
// identities, and other identities hold them too.
var (
	batchPhoneDigits   = []string{"15550009999", "12025550123", "13035550147", "15555550199", "447555012399", "4412025550123", "13035550123", "5550123", "550123", "12025550124"}
	batchAddressValues = []string{"me@example.com", "support@example.com", "other-support@example.com", "support@example.com.au", "\u00a0support@example.com", "support@example.com\u3000", "a&b@example.com", "élise@example.com", "Élise@example.com", "users/123", "users/1234", "abcdefab-abcd-abcd-abcd-abcdefabcdef", "72975", "nul\x00a@example.com", "nul\x00b@example.com", "bad\xff@example.com", "15555550199@lid", "alice12025550123@example.com", "\t"}
)

// padBatchAddress puts white space that strings.TrimSpace trims around value
// some of the time.
func padBatchAddress(random *rand.Rand, value string) string {
	if random.Intn(4) != 0 {
		return value
	}
	spaces := []string{"", " ", "\t", "\u0085", "\u00a0", "\u2028", "\u3000"}
	return spaces[random.Intn(len(spaces))] + value + spaces[random.Intn(len(spaces))]
}

// flipBatchCase writes value with the case of some of its ASCII letters
// flipped.
func flipBatchCase(random *rand.Rand, value string) string {
	b := []byte(value)
	for index, c := range b {
		if random.Intn(3) != 0 {
			continue
		}
		switch {
		case 'a' <= c && c <= 'z':
			b[index] = c - 'a' + 'A'
		case 'A' <= c && c <= 'Z':
			b[index] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// batchPhoneForm writes digits as v2 or a transport might: E.164, a WhatsApp
// JID, national with separators, or with a NUL and more after it.
func batchPhoneForm(random *rand.Rand, digits string) string {
	switch random.Intn(5) {
	case 0:
		return digits + "@s.whatsapp.net"
	case 1:
		var b strings.Builder
		for index, r := range digits {
			if index > 0 && index%3 == 0 {
				b.WriteString([]string{" ", "-", "."}[random.Intn(3)])
			}
			b.WriteRune(r)
		}
		return b.String()
	case 2:
		return "+" + digits + "\x00x"
	}
	return "+" + digits
}

// seedBatchV2Store writes random identities and conversations into store in
// two accounts. Each account has a self identity on the owner's phone and
// one on the owner's address, and other identities hold those values too,
// sometimes in another ASCII case. Conversations list the owner most of the
// time, some participants are inactive, some conversations are archived, and
// conversations often share a time.
func seedBatchV2Store(t *testing.T, random *rand.Rand, store *Store) {
	t.Helper()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(query, args...); err != nil {
			t.Fatalf("seed: %v\n%s %q", err, query, args)
		}
	}
	type identity struct{ id, account string }
	identities := map[string][]identity{}
	selves := map[string][]string{}
	taken := map[string]bool{}
	number := 0
	add := func(account, kind, value string, self bool) {
		if taken[account+"|"+kind+"|"+value] {
			return
		}
		taken[account+"|"+kind+"|"+value] = true
		id := fmt.Sprintf("identity-%03d", number)
		number++
		exec(`INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, is_self, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?, 1, 1)`,
			id, account, kind, value, value, self)
		if self {
			selves[account] = append(selves[account], id)
			return
		}
		identities[account] = append(identities[account], identity{id: id, account: account})
	}
	accounts := []string{"account-a", "account-b"}
	for index, account := range accounts {
		exec(`INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES (?, ?, 1, 1)`,
			account, []string{"google_messages", "signal_cli"}[index])
		add(account, "e164", "+"+batchPhoneDigits[0], true)
		add(account, "email", batchAddressValues[0], true)
		for count := 3 + random.Intn(8); count > 0; count-- {
			if random.Intn(2) == 0 {
				add(account, []string{"e164", "whatsapp_jid", "phone"}[random.Intn(3)], batchPhoneForm(random, batchPhoneDigits[random.Intn(len(batchPhoneDigits))]), false)
				continue
			}
			add(account, []string{"email", "username", "signal_aci"}[random.Intn(3)], flipBatchCase(random, batchAddressValues[random.Intn(len(batchAddressValues))]), false)
		}
	}
	for index := 0; index < 3+random.Intn(12); index++ {
		account := accounts[random.Intn(len(accounts))]
		id := fmt.Sprintf("conversation-%02d", index)
		var archived any
		if random.Intn(5) == 0 {
			archived = int64(1)
		}
		exec(`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, archived_at_ms, last_message_at_ms, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?, 1, 1)`,
			id, account, "remote-"+id, []string{"direct", "group"}[random.Intn(2)], archived, int64(1000+10*random.Intn(6)))
		members := identities[account]
		for _, member := range random.Perm(len(members))[:random.Intn(min(4, len(members))+1)] {
			exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id, is_active) VALUES (?, ?, ?, ?)`,
				account, id, members[member].id, random.Intn(4) != 0)
		}
		if random.Intn(4) != 0 {
			self := selves[account][random.Intn(len(selves[account]))]
			exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id, is_active) VALUES (?, ?, ?, 1)`, account, id, self)
		}
	}
}

// newestOfV2 is the reference for a batch: the conversations newest first,
// then in conversation ID order, each once, and at most limit.
func newestOfV2(conversations []Conversation, limit int) []Conversation {
	sorted := slices.Clone(conversations)
	slices.SortStableFunc(sorted, func(a, b Conversation) int {
		switch {
		case a.LastMessageAtMS > b.LastMessageAtMS:
			return -1
		case a.LastMessageAtMS < b.LastMessageAtMS:
			return 1
		}
		return strings.Compare(a.ConversationID, b.ConversationID)
	})
	newest := []Conversation{}
	seen := map[string]bool{}
	for _, conversation := range sorted {
		if len(newest) >= limit {
			break
		}
		if !seen[conversation.ConversationID] {
			seen[conversation.ConversationID] = true
			newest = append(newest, conversation)
		}
	}
	return newest
}

// sameV2Conversations reports whether a and b hold equal conversations in
// the same order; nil and empty count as equal.
func sameV2Conversations(a, b []Conversation) bool {
	return len(a) == len(b) && (len(a) == 0 || reflect.DeepEqual(a, b))
}

// Differential, over random v2 stores: SearchConversationsByPhoneLines and
// SearchConversationsByAddresses return exactly the newest of what
// SearchConversationsByPhoneLine or SearchConversationsByAddress returns for
// each value: the same conversations, newest first, then in conversation ID
// order, and at most the limit, which each reads as its search for one value
// does. So, as invariants, the batch matches each value as its own search
// does, under phoneline's rules for a line and addressmatch's key for a whole
// address, skips self identities, applies the limit after matching, and keeps
// conversations that share a time in ID order. Three more hold for every
// batch: the order of the values and repeats among them change nothing, and
// neither does how many addresses each query binds.
func TestBatchedSearchesReturnTheNewestOfEachValuesSearch(t *testing.T) {
	exercised := map[string]int{}
	property := func(seed int64) bool {
		random := rand.New(rand.NewSource(seed))
		store := openRepositoryTestStore(t)
		seedBatchV2Store(t, random, store)
		note := func(kinds string, values []string, each [][]Conversation, limit int) {
			withHits := 0
			var all []Conversation
			for index, hits := range each {
				if len(hits) > 0 {
					withHits++
					if strings.Contains(values[index], "\x00") {
						exercised["a value with a NUL among "+kinds+" that finds conversations"]++
					}
					if strings.TrimSpace(values[index]) != values[index] {
						exercised["a value with white space around it among "+kinds+" that finds conversations"]++
					}
					if !utf8.ValidString(values[index]) {
						exercised["a value with a byte that isn't UTF-8 among "+kinds+" that finds conversations"]++
					}
				}
				all = append(all, hits...)
			}
			all = newestOfV2(all, len(all))
			if withHits >= 2 {
				exercised["a batch of "+kinds+" whose hits come from two or more of them"]++
			}
			if limit >= 1 && len(all) > limit {
				exercised["a limit that cuts a batch of "+kinds]++
				if all[limit].LastMessageAtMS == all[limit-1].LastMessageAtMS {
					exercised["conversations that share a time across the limit of a batch of "+kinds]++
				}
			}
		}
		describe := func(conversations []Conversation) []string {
			ids := []string{}
			for _, conversation := range conversations {
				ids = append(ids, conversation.ConversationID)
			}
			return ids
		}

		for round := 0; round < 4; round++ {
			limit := []int{-1, 0, 1, 2, 3, 5, 40}[random.Intn(7)]

			var lines []string
			for count := random.Intn(7); count > 0; count-- {
				if random.Intn(4) == 0 {
					lines = append(lines, []string{"", "5550123", "550123", "2025550123\x00junk", "\x002025550123", "abc5550123", "99912025550123"}[random.Intn(7)])
					continue
				}
				lines = append(lines, phoneline.Line(batchPhoneDigits[random.Intn(len(batchPhoneDigits))]))
			}
			var each [][]Conversation
			var union []Conversation
			for _, line := range lines {
				hits, err := store.SearchConversationsByPhoneLine(line, limit)
				if err != nil {
					t.Fatalf("SearchConversationsByPhoneLine(%q, %d): %v", line, limit, err)
				}
				each = append(each, hits)
				union = append(union, hits...)
			}
			want := newestOfV2(union, limit)
			got, err := store.SearchConversationsByPhoneLines(lines, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByPhoneLines(%q, %d): %v", lines, limit, err)
			}
			if !sameV2Conversations(got, want) {
				t.Logf("seed %d: lines %q (limit %d) = %v, each line's search gives %v", seed, lines, limit, describe(got), describe(want))
				return false
			}
			note("lines", lines, each, limit)
			shuffled := slices.Clone(lines)
			for _, line := range lines {
				if random.Intn(2) == 0 {
					shuffled = append(shuffled, line)
				}
			}
			random.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			if again, err := store.SearchConversationsByPhoneLines(shuffled, limit); err != nil || !sameV2Conversations(again, got) {
				t.Logf("seed %d: lines %q (limit %d) = %v, but reordered and repeated as %q = %v (%v)", seed, lines, limit, describe(got), shuffled, describe(again), err)
				return false
			}

			var addresses []string
			for count := random.Intn(8); count > 0; count-- {
				switch random.Intn(8) {
				case 0:
					addresses = append(addresses, []string{"", "  "}[random.Intn(2)])
				case 1:
					addresses = append(addresses, batchPhoneForm(random, batchPhoneDigits[random.Intn(len(batchPhoneDigits))]))
				default:
					addresses = append(addresses, padBatchAddress(random, flipBatchCase(random, batchAddressValues[random.Intn(len(batchAddressValues))])))
				}
			}
			each, union = nil, nil
			for _, address := range addresses {
				hits, err := store.SearchConversationsByAddress(address, limit)
				if err != nil {
					t.Fatalf("SearchConversationsByAddress(%q, %d): %v", address, limit, err)
				}
				each = append(each, hits)
				union = append(union, hits...)
			}
			want = newestOfV2(union, limit)
			got, err = store.SearchConversationsByAddresses(addresses, limit)
			if err != nil {
				t.Fatalf("SearchConversationsByAddresses(%q, %d): %v", addresses, limit, err)
			}
			if !sameV2Conversations(got, want) {
				t.Logf("seed %d: addresses %q (limit %d) = %v, each address's search gives %v", seed, addresses, limit, describe(got), describe(want))
				return false
			}
			note("addresses", addresses, each, limit)
			for _, perQuery := range []int{1, 2, 3} {
				again, err := store.searchConversationsByAddresses(addresses, limit, perQuery)
				if err != nil || !sameV2Conversations(again, got) {
					t.Logf("seed %d: addresses %q (limit %d) = %v, but %d to a query gives %v (%v)", seed, addresses, limit, describe(got), perQuery, describe(again), err)
					return false
				}
				if len(addressmatch.Keys(addresses)) > perQuery && len(got) > 0 {
					exercised["addresses split across queries"]++
				}
			}
			shuffled = slices.Clone(addresses)
			for _, address := range addresses {
				if random.Intn(2) == 0 {
					shuffled = append(shuffled, address)
				}
			}
			random.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			if again, err := store.SearchConversationsByAddresses(shuffled, limit); err != nil || !sameV2Conversations(again, got) {
				t.Logf("seed %d: addresses %q (limit %d) = %v, but reordered and repeated as %q = %v (%v)", seed, addresses, limit, describe(got), shuffled, describe(again), err)
				return false
			}
		}
		return true
	}
	cases := 150
	if raceDetectorEnabled {
		cases = 30
	}
	if err := quick.Check(property, &quick.Config{MaxCount: cases, Rand: rand.New(rand.NewSource(20261104))}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"a batch of lines whose hits come from two or more of them",
		"a batch of addresses whose hits come from two or more of them",
		"a limit that cuts a batch of lines",
		"a limit that cuts a batch of addresses",
		"conversations that share a time across the limit of a batch of lines",
		"conversations that share a time across the limit of a batch of addresses",
		"a value with a NUL among lines that finds conversations",
		"a value with a NUL among addresses that finds conversations",
		"a value with white space around it among addresses that finds conversations",
		"a value with a byte that isn't UTF-8 among addresses that finds conversations",
		"addresses split across queries",
	} {
		if exercised[path] == 0 {
			t.Errorf("the property never drew %s (drew %v)", path, exercised)
		}
	}
}

// countedPhoneLineKeys is phoneline.SQLKeys under another name, and
// countedPhoneLineKeysCalls counts its calls.
const countedPhoneLineKeys = "om_test_v2_counted_phone_line_keys"

var countedPhoneLineKeysCalls atomic.Int64

// countedPhoneLineKeysSQL writes a call of countedPhoneLineKeys, casting each
// argument to BLOB as phoneline.KeysSQL does.
func countedPhoneLineKeysSQL(args ...string) string {
	cast := make([]string, len(args))
	for index, arg := range args {
		cast[index] = "CAST(" + arg + " AS BLOB)"
	}
	return countedPhoneLineKeys + "(" + strings.Join(cast, ", ") + ")"
}

func init() {
	moderncsqlite.MustRegisterDeterministicScalarFunction(countedPhoneLineKeys, -1, func(_ *moderncsqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		countedPhoneLineKeysCalls.Add(1)
		for _, arg := range args {
			var value string
			switch arg := arg.(type) {
			case string:
				value = arg
			case []byte:
				value = string(arg)
			}
			if strings.TrimSpace(value) == "" {
				continue
			}
			keys := phoneline.ValueKeys(value)
			if len(keys) == 0 {
				return nil, nil
			}
			encoded, err := json.Marshal(keys)
			return string(encoded), err
		}
		return nil, nil
	})
}

// The batched line search calls the Go keys function once for each identity
// at most, however many lines it holds and however many conversations list
// each identity: a name can find 160 numbers, and a search for each read
// every identity again.
func TestSearchConversationsByPhoneLinesReadsEachIdentityOnce(t *testing.T) {
	store := openRepositoryTestStore(t)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(query, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES ('account', 'google_messages', 1, 1)`)
	values := []string{"+15550009999", "+12025550123", "+13035550147", "bob@example.com"}
	for index, value := range values {
		exec(`INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, is_self, created_at_ms, updated_at_ms) VALUES (?, 'account', 'e164', ?, ?, ?, 1, 1)`,
			fmt.Sprintf("identity-%d", index), value, value, index == 0)
	}
	for index := 0; index < 300; index++ {
		id := fmt.Sprintf("conversation-%03d", index)
		exec(`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, last_message_at_ms, created_at_ms, updated_at_ms) VALUES (?, 'account', ?, 'group', ?, 1, 1)`,
			id, "remote-"+id, int64(index))
		for member := range values {
			if member == 0 || index%len(values) != member {
				exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id) VALUES ('account', ?, ?)`, id, fmt.Sprintf("identity-%d", member))
			}
		}
	}
	lines := []string{"2025550123"}
	for index := 0; len(lines) < 160; index++ {
		lines = append(lines, phoneline.Line(fmt.Sprintf("1%d", 4000000000+index)))
	}
	countedPhoneLineKeysCalls.Store(0)
	rows, err := store.db.Query(phoneLinesSearchSQL(countedPhoneLineKeysSQL), phoneline.KeysOfLines(lines), 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	found, err := collectRows(rows, scanConversation)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if calls := countedPhoneLineKeysCalls.Load(); calls > int64(len(values)) {
		t.Errorf("the search called the keys function %d times for %d lines and %d identities in 300 conversations", calls, len(lines), len(values))
	}
	real, err := store.SearchConversationsByPhoneLines(lines, 10)
	if err != nil || !sameV2Conversations(real, found) || len(found) != 10 {
		t.Errorf("SearchConversationsByPhoneLines = %d conversations (%v), but the counted query found %d", len(real), err, len(found))
	}
}

// countedAddressKey is addressmatch.SQLKey under another name, and
// countedAddressKeyCalls counts its calls.
const countedAddressKey = "om_test_v2_counted_address_key"

var countedAddressKeyCalls atomic.Int64

// countedAddressKeySQL writes a call of countedAddressKey, casting its
// argument to BLOB as addressmatch.KeySQL does.
func countedAddressKeySQL(value string) string {
	return countedAddressKey + "(CAST(" + value + " AS BLOB))"
}

func init() {
	moderncsqlite.MustRegisterDeterministicScalarFunction(countedAddressKey, 1, func(_ *moderncsqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		countedAddressKeyCalls.Add(1)
		value, ok := args[0].([]byte)
		if !ok {
			return nil, nil
		}
		key := addressmatch.Key(string(value))
		if key == "" {
			return nil, nil
		}
		return []byte(key), nil
	})
}

// The batched address search calls the Go key function once for each identity
// at most, however many addresses it holds and however many conversations list
// each identity: a name can find 160 values, and a search for each read every
// identity again.
func TestSearchConversationsByAddressesReadsEachIdentityOnce(t *testing.T) {
	store := openRepositoryTestStore(t)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(query, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES ('account', 'google_messages', 1, 1)`)
	values := []string{"me@example.com", "bob@example.com", "dee@example.com", "+12025550123"}
	for index, value := range values {
		exec(`INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, is_self, created_at_ms, updated_at_ms) VALUES (?, 'account', 'email', ?, ?, ?, 1, 1)`,
			fmt.Sprintf("identity-%d", index), value, value, index == 0)
	}
	for index := 0; index < 300; index++ {
		id := fmt.Sprintf("conversation-%03d", index)
		exec(`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, last_message_at_ms, created_at_ms, updated_at_ms) VALUES (?, 'account', ?, 'group', ?, 1, 1)`,
			id, "remote-"+id, int64(index))
		for member := range values {
			if member == 0 || index%len(values) != member {
				exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id) VALUES ('account', ?, ?)`, id, fmt.Sprintf("identity-%d", member))
			}
		}
	}
	addresses := []string{"BOB@example.com "}
	for index := 0; len(addresses) < 160; index++ {
		addresses = append(addresses, fmt.Sprintf("person%d@example.com", index))
	}
	keys := addressmatch.Keys(addresses)
	args := make([]any, 0, len(keys)+1)
	for _, key := range keys {
		args = append(args, []byte(key))
	}
	countedAddressKeyCalls.Store(0)
	rows, err := store.db.Query(addressesSearchSQL(len(keys), countedAddressKeySQL), append(args, 10)...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	found, err := collectRows(rows, scanConversation)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if calls := countedAddressKeyCalls.Load(); calls > int64(len(values)) {
		t.Errorf("the search called the key function %d times for %d addresses and %d identities in 300 conversations", calls, len(addresses), len(values))
	}
	real, err := store.SearchConversationsByAddresses(addresses, 10)
	if err != nil || !sameV2Conversations(real, found) || len(found) != 10 {
		t.Errorf("SearchConversationsByAddresses = %d conversations (%v), but the counted query found %d", len(real), err, len(found))
	}
}

// A batch compares what the search for one address compares: every byte of a
// canonical value's key. Each row names the conversations that its addresses
// must find: white space around a value or an address and its ASCII letters'
// case don't matter, but a non-ASCII letter's case, a byte after a NUL and a
// byte that isn't UTF-8 do, a value that only holds an address matches nothing,
// and neither does the owner's.
func TestSearchConversationsByAddressesComparesEveryByteOfTheKey(t *testing.T) {
	store := openRepositoryTestStore(t)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(query, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES ('account', 'google_messages', 1, 1)`)
	for index, row := range []struct {
		id, value string
		self      bool
	}{
		{"padded", "\u00a0Support@Example.COM\t", false},
		{"holds-it", "other-support@example.com", false},
		{"nul-a", "NUL\x00a@example.com", false},
		{"nul-b", "nul\x00b@example.com", false},
		{"raw", " RAW\xff@example.com", false},
		{"accent", "Élise@example.com", false},
		{"owner", "me@example.com", true},
	} {
		exec(`INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, is_self, created_at_ms, updated_at_ms) VALUES (?, 'account', 'email', ?, ?, ?, 1, 1)`,
			"identity-"+row.id, row.value, row.value, row.self)
		exec(`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, last_message_at_ms, created_at_ms, updated_at_ms) VALUES (?, 'account', ?, 'direct', ?, 1, 1)`,
			row.id, "remote-"+row.id, int64(1000-index))
		exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id) VALUES ('account', ?, ?)`, row.id, "identity-"+row.id)
	}
	ids := func(conversations []Conversation) []string {
		found := []string{}
		for _, conversation := range conversations {
			found = append(found, conversation.ConversationID)
		}
		return found
	}
	for _, c := range []struct {
		addresses []string
		want      []string
	}{
		{[]string{"support@example.com", "nobody@example.com"}, []string{"padded"}},
		{[]string{"\tSUPPORT@example.com\u2003", " support@example.com"}, []string{"padded"}},
		{[]string{"example.com", "support", "nobody@example.com"}, []string{}},
		{[]string{"nul\x00a@example.com", "nobody@example.com"}, []string{"nul-a"}},
		{[]string{"NUL\x00B@example.com", "nobody@example.com"}, []string{"nul-b"}},
		{[]string{"nul\x00a@example.com", "nul\x00b@example.com"}, []string{"nul-a", "nul-b"}},
		{[]string{"nul", "nul\x00", "nul\x00c@example.com"}, []string{}},
		{[]string{"raw\xff@example.com", "nobody@example.com"}, []string{"raw"}},
		{[]string{"raw\xfe@example.com", "raw\ufffd@example.com"}, []string{}},
		{[]string{"ÉLISE@EXAMPLE.com", "nobody@example.com"}, []string{"accent"}},
		{[]string{"élise@example.com", "éLISE@example.com"}, []string{}},
		{[]string{"me@example.com", "ME@example.com "}, []string{}},
		{[]string{"", " ", "\u00a0"}, []string{}},
	} {
		var union []Conversation
		for _, address := range c.addresses {
			hits, err := store.SearchConversationsByAddress(address, 10)
			if err != nil {
				t.Fatalf("SearchConversationsByAddress(%q): %v", address, err)
			}
			union = append(union, hits...)
		}
		if found := ids(newestOfV2(union, 10)); !slices.Equal(found, c.want) {
			t.Errorf("the searches for each of %q find %v, want %v", c.addresses, found, c.want)
		}
		got, err := store.SearchConversationsByAddresses(c.addresses, 10)
		if err != nil {
			t.Fatalf("SearchConversationsByAddresses(%q): %v", c.addresses, err)
		}
		if found := ids(got); !slices.Equal(found, c.want) {
			t.Errorf("SearchConversationsByAddresses(%q) = %v, want %v", c.addresses, found, c.want)
		}
	}
}

// The batches take as many values as a caller gives them. Address keys bind one
// value each, so a query holds the cap's worth at most, and past the cap each
// group runs as its own query; line keys bind as one JSON array. This runs the
// real cap through real bound values, in one query and across groups: the tool
// sends far fewer, but a store's search shouldn't hold a limit of its own.
func TestBatchesTakeThousandsOfValues(t *testing.T) {
	store := openRepositoryTestStore(t)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.db.Exec(query, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	total := maxAddressesPerQuery*5/2 + 1
	addresses, lines := make([]string, total), make([]string, total)
	for index := range addresses {
		addresses[index] = fmt.Sprintf("person%d@example.com", index)
		lines[index] = fmt.Sprintf("20255%d", 10000+index)
	}
	exec(`INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES ('account', 'google_messages', 1, 1)`)
	participant := func(conversation, value string) {
		exec(`INSERT OR IGNORE INTO identities (identity_id, account_id, kind, canonical_value, raw_value, is_self, created_at_ms, updated_at_ms) VALUES (?, 'account', 'email', ?, ?, 0, 1, 1)`,
			"identity-"+value, value, value)
		exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id) VALUES ('account', ?, ?)`, conversation, "identity-"+value)
	}
	for _, row := range []struct {
		id    string
		atMS  int64
		at    int
		other bool
	}{
		{id: "other", atMS: 40, other: true},
		{id: "first", atMS: 30, at: 0},
		{id: "middle", atMS: 20, at: total / 2},
		{id: "last", atMS: 10, at: total - 1},
	} {
		exec(`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, last_message_at_ms, created_at_ms, updated_at_ms) VALUES (?, 'account', ?, 'group', ?, 1, 1)`,
			row.id, "remote-"+row.id, row.atMS)
		if row.other {
			participant(row.id, "nobody@example.com")
			participant(row.id, "+13035550147")
			continue
		}
		participant(row.id, addresses[row.at])
		participant(row.id, "+1"+lines[row.at])
	}
	ids := func(conversations []Conversation) []string {
		found := []string{}
		for _, conversation := range conversations {
			found = append(found, conversation.ConversationID)
		}
		return found
	}
	for _, c := range []struct {
		count, limit int
		want         []string
	}{
		{maxAddressesPerQuery, 10, []string{"first"}},
		{maxAddressesPerQuery + 1, 10, []string{"first"}},
		{total, 10, []string{"first", "middle", "last"}},
		{total, 2, []string{"first", "middle"}},
		{total, 1, []string{"first"}},
	} {
		byAddress, err := store.SearchConversationsByAddresses(addresses[:c.count], c.limit)
		if err != nil {
			t.Fatalf("%d addresses at limit %d: %v", c.count, c.limit, err)
		}
		if found := ids(byAddress); !slices.Equal(found, c.want) {
			t.Errorf("%d addresses at limit %d = %v, want %v", c.count, c.limit, found, c.want)
		}
		byLine, err := store.SearchConversationsByPhoneLines(lines[:c.count], c.limit)
		if err != nil {
			t.Fatalf("%d lines at limit %d: %v", c.count, c.limit, err)
		}
		if found := ids(byLine); !slices.Equal(found, c.want) {
			t.Errorf("%d lines at limit %d = %v, want %v", c.count, c.limit, found, c.want)
		}
	}
}
