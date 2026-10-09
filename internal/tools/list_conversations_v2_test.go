package tools

import (
	"context"
	"encoding/json"
	"math/rand"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/sqlite/sqlitetest"
	"github.com/maxghenis/openmessage/internal/v2read"
)

// plainReads hides every method but ReadSource's, so list_conversations takes
// its original path: map every conversation, filter by platform in Go.
type plainReads struct{ readsource.ReadSource }

// On v2, list_conversations with source_platform reads only that platform's
// conversations; its result must be byte-identical to the original filter over
// ListConversations(math.MaxInt), for every platform spelling and limit.
func TestListConversationsPlatformFilterMatchesFullScanProperty(t *testing.T) {
	a := testApp(t)
	config := &quick.Config{MaxCount: sqlitetest.PropertyRuns(25), Rand: rand.New(rand.NewSource(20261009))}
	property := func(seed int64) bool {
		rs := sqlitetest.BuildRandomStore(t, seed, sqlitetest.DenseShape)
		source := v2read.New(rs.Store)
		batched := listConversationsHandler(a, Options{Reads: source, V2Primary: true})
		fullScan := listConversationsHandler(a, Options{Reads: plainReads{source}, V2Primary: true})
		ok := true
		for _, platform := range []string{"sms", "whatsapp", "signal", "gchat", "imessage", "custom_bridge", " ", "SMS", "nope"} {
			for _, limit := range []any{nil, 0, 1, 3, 500} {
				args := map[string]any{"source_platform": platform}
				if limit != nil {
					args["limit"] = limit
				}
				got, err := batched(context.Background(), toolRequest(args))
				if err != nil {
					t.Fatal(err)
				}
				want, err := fullScan(context.Background(), toolRequest(args))
				if err != nil {
					t.Fatal(err)
				}
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(want)
				if string(gotJSON) != string(wantJSON) {
					t.Errorf("seed %d platform %q limit %v:\n got: %s\nwant: %s", seed, platform, limit, gotJSON, wantJSON)
					ok = false
				}
			}
		}
		return ok
	}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}

// The platform-filtered listing maps only what it returns: its statement
// count does not grow with the number of conversations stored.
func TestListConversationsPlatformFilterStatementsDoNotGrow(t *testing.T) {
	a := testApp(t)
	counts := map[int]int{}
	for _, conversations := range []int{4, 200} {
		built := sqlitetest.BuildUniformStore(t, sqlitetest.Uniform{
			Accounts: 3, Conversations: conversations, Participants: 4, MessagesPer: 1,
		})
		handler := listConversationsHandler(a, Options{Reads: v2read.New(built.Store), V2Primary: true})
		built.Counter.Reset()
		result, err := handler(context.Background(), toolRequest(map[string]any{"source_platform": "whatsapp", "limit": 20}))
		if err != nil || result.IsError {
			t.Fatalf("list_conversations: %v %+v", err, result)
		}
		counts[conversations] = built.Counter.Count()
	}
	t.Logf("list_conversations(source_platform=whatsapp, limit=20) statements by stored conversations per account: %v", counts)
	if counts[4] != counts[200] || counts[200] > 3 {
		t.Fatalf("statements = %v, want equal and <= 3 (accounts, one recency read, one roster read)", counts)
	}
}
