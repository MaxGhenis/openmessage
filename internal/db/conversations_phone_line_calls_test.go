package db

import (
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"

	"github.com/maxghenis/openmessage/internal/phoneline"
)

// countedPhoneLineMatch is phoneline's SQL function under another name, and
// countedPhoneLineMatches counts its calls.
const countedPhoneLineMatch = "om_test_counted_phone_line_match"

var countedPhoneLineMatches atomic.Int64

// countedPhoneLineMatchSQL writes a call of countedPhoneLineMatch, casting
// each argument to BLOB as phoneline.SQL does.
func countedPhoneLineMatchSQL(args ...string) string {
	cast := make([]string, len(args))
	for index, arg := range args {
		cast[index] = "CAST(" + arg + " AS BLOB)"
	}
	return countedPhoneLineMatch + "(" + strings.Join(cast, ", ") + ")"
}

func init() {
	sqlite.MustRegisterDeterministicScalarFunction(countedPhoneLineMatch, -1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		countedPhoneLineMatches.Add(1)
		values := make([]string, 0, len(args))
		for _, arg := range args {
			switch arg := arg.(type) {
			case string:
				values = append(values, arg)
			case []byte:
				values = append(values, string(arg))
			default:
				values = append(values, "")
			}
		}
		if len(values) >= 2 && phoneline.MatchFirst(values[0], values[1:]...) {
			return int64(1), nil
		}
		return int64(0), nil
	})
}

// The line search calls the Go matcher once for each participant entry and
// once for each distinct sender number, never once for each message: a
// thread can hold thousands of messages from the same few people, and the
// search runs once for each number on an address-book card a name finds.
func TestSearchConversationsByPhoneLineCallsTheMatcherOncePerSenderNumber(t *testing.T) {
	store := newTestStore(t)
	conversations := []*Conversation{
		{ConversationID: "work", Name: "Work", IsGroup: true, SourcePlatform: "sms", LastMessageTS: 20,
			Participants: `[{"name":"Bob Example","number":"+15555550199"},{"name":"Dee Example","number":"+15555550188"},{"name":"Me","number":"+15550009999","is_me":true}]`},
		{ConversationID: "club", Name: "Club", IsGroup: true, SourcePlatform: "sms", LastMessageTS: 10,
			Participants: `[{"name":"Eve Example","number":"+15555550177"}]`},
	}
	var messages []*Message
	for index := 0; index < 900; index++ {
		message := &Message{
			MessageID: "m" + strconv.Itoa(index), Body: "hello", TimestampMS: int64(index + 1), SourcePlatform: "sms",
			ConversationID: []string{"work", "club", "work"}[index%3],
			SenderNumber:   []string{"+12025550123", "+13035550147", "+15550009999"}[index%3],
			IsFromMe:       index%3 == 2,
		}
		messages = append(messages, message)
	}
	seedSearchStore(t, store, conversations, messages, nil)

	countedPhoneLineMatches.Store(0)
	rows, err := store.db.Query(phoneLineSearchSQL(countedPhoneLineMatchSQL), "2025550123", "2025550123", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	found, err := scanConversations(rows)
	rows.Close()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	ids := []string{}
	for _, conversation := range found {
		ids = append(ids, conversation.ConversationID)
	}
	if fmt.Sprint(ids) != "[work]" {
		t.Errorf("line 2025550123 = %v, want Work, where its number wrote", ids)
	}
	const entries, senderNumbers = 4, 3
	if calls := countedPhoneLineMatches.Load(); calls > entries+senderNumbers {
		t.Errorf("the search called the matcher %d times for %d entries and %d sender numbers in %d messages", calls, entries, senderNumbers, len(messages))
	}
}
