package v2read

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

type statsCase struct {
	Seed int64
}

func (statsCase) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(statsCase{Seed: r.Int63()})
}

// Invariants of the per-account aggregates behind the status surfaces, for
// random stores where several accounts share a platform and one bridge key
// has no mapping of its own:
//   - a platform appears in PlatformStats exactly when it holds a message;
//   - Count is its number of messages, LatestMS the newest occurred_at_ms and
//     LatestRecvMS the newest incoming one (0 when it received nothing);
//   - PlatformLatest is PlatformStats without Count, in the same order;
//   - MessageCount, ConversationCount and LatestTimestamp agree with the same
//     sums and maxima, per platform and in total.
func TestStatsMatchReferenceAggregatesProperty(t *testing.T) {
	bridges := []string{"google_messages", "google_messages", "signal_cli", "whatsmeow", "mystery_bridge"}
	property := func(c statsCase) bool {
		r := rand.New(rand.NewSource(c.Seed))
		store, messages, source := openSourceTestStore(t)
		type account struct{ id, platform string }
		var accounts []account
		for i, bridge := range bridges {
			if r.Intn(3) == 0 {
				continue
			}
			id := fmt.Sprintf("account-%d", i)
			seedSourceAccount(t, store, id, bridge)
			// An unmapped bridge key is its own platform label; PlatformStats
			// would say "unknown" only for a blank one.
			platform := platformForBridgeKey(bridge)
			accounts = append(accounts, account{id: id, platform: platform})
		}
		if len(accounts) == 0 {
			return true
		}
		conversationsPer := map[string]int{}
		var conversations []sqlite.Conversation
		for i := 0; i < 1+r.Intn(6); i++ {
			owner := accounts[r.Intn(len(accounts))]
			conversation := sqlite.Conversation{
				ConversationID:       fmt.Sprintf("conversation-%d", i),
				AccountID:            owner.id,
				RemoteConversationID: fmt.Sprintf("remote-%d", i),
				Kind:                 sqlite.ConversationKindDirect,
				NotificationMode:     sqlite.NotificationModeAll,
			}
			seedSourceConversation(t, store, conversation)
			conversations = append(conversations, conversation)
			conversationsPer[owner.platform]++
		}
		want := map[string]*db.PlatformStat{}
		for i := 0; i < r.Intn(40); i++ {
			conversation := conversations[r.Intn(len(conversations))]
			direction := sqlite.MessageDirectionIncoming
			if r.Intn(2) == 0 {
				direction = sqlite.MessageDirectionOutgoing
			}
			occurred := 1 + r.Int63n(50)
			importSourceMessage(t, messages, sqlite.Message{
				MessageID:       fmt.Sprintf("message-%d", i),
				ConversationID:  conversation.ConversationID,
				AccountID:       conversation.AccountID,
				RemoteMessageID: fmt.Sprintf("remote-message-%d", i),
				Direction:       direction,
				State:           sqlite.MessageStateActive,
				OccurredAtMS:    occurred,
			})
			var platform string
			for _, a := range accounts {
				if a.id == conversation.AccountID {
					platform = a.platform
				}
			}
			stat := want[platform]
			if stat == nil {
				stat = &db.PlatformStat{Platform: platform}
				want[platform] = stat
			}
			stat.Count++
			stat.LatestMS = max(stat.LatestMS, occurred)
			if direction == sqlite.MessageDirectionIncoming {
				stat.LatestRecvMS = max(stat.LatestRecvMS, occurred)
			}
		}
		wantStats := []db.PlatformStat{}
		for _, stat := range want {
			wantStats = append(wantStats, *stat)
		}
		sort.Slice(wantStats, func(i, j int) bool {
			if wantStats[i].LatestMS != wantStats[j].LatestMS {
				return wantStats[i].LatestMS > wantStats[j].LatestMS
			}
			return wantStats[i].Platform < wantStats[j].Platform
		})

		stats, err := source.PlatformStats()
		if err != nil || !reflect.DeepEqual(stats, wantStats) {
			t.Errorf("seed %d: PlatformStats() = %+v, %v; want %+v", c.Seed, stats, err, wantStats)
			return false
		}
		latest, err := source.PlatformLatest()
		if err != nil || len(latest) != len(wantStats) {
			t.Errorf("seed %d: PlatformLatest() = %+v, %v; want %d rows", c.Seed, latest, err, len(wantStats))
			return false
		}
		for i, stat := range wantStats {
			if latest[i] != (db.PlatformLatest{Platform: stat.Platform, LatestMS: stat.LatestMS, LatestRecvMS: stat.LatestRecvMS}) {
				t.Errorf("seed %d: PlatformLatest()[%d] = %+v, want %+v", c.Seed, i, latest[i], stat)
				return false
			}
		}
		platforms := []string{"sms", "signal", "whatsapp", "imessage"}
		for platform := range want {
			platforms = append(platforms, platform)
		}
		total := 0
		for _, stat := range want {
			total += stat.Count
		}
		for _, platform := range platforms {
			var wantCount int
			var wantLatest int64
			if stat := want[platform]; stat != nil {
				wantCount, wantLatest = stat.Count, stat.LatestMS
			}
			count, err := source.MessageCount(platform)
			if err != nil || count != wantCount {
				t.Errorf("seed %d: MessageCount(%q) = %d, %v; want %d", c.Seed, platform, count, err, wantCount)
				return false
			}
			latestMS, err := source.LatestTimestamp(platform)
			if err != nil || latestMS != wantLatest {
				t.Errorf("seed %d: LatestTimestamp(%q) = %d, %v; want %d", c.Seed, platform, latestMS, err, wantLatest)
				return false
			}
			conversations, err := source.ConversationCount(platform)
			if err != nil || conversations != conversationsPer[platform] {
				t.Errorf("seed %d: ConversationCount(%q) = %d, %v; want %d", c.Seed, platform, conversations, err, conversationsPer[platform])
				return false
			}
		}
		if count, err := source.MessageCount(""); err != nil || count != total {
			t.Errorf("seed %d: MessageCount(\"\") = %d, %v; want %d", c.Seed, count, err, total)
			return false
		}
		if count, err := source.ConversationCount(""); err != nil || count != len(conversations) {
			t.Errorf("seed %d: ConversationCount(\"\") = %d, %v; want %d", c.Seed, count, err, len(conversations))
			return false
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 40}); err != nil {
		t.Fatal(err)
	}
}
