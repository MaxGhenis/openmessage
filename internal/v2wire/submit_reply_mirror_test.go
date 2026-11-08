package v2wire

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// These tests pin down what a mirror send (legacy-primary with v2 sending)
// leaves behind when it refuses a reply target: nothing for the target. The
// refusal comes before MirrorReplyTarget writes the quoted message's copy,
// so no v2 message, inbox row or outbox row appears, and the mirror never
// mirrors a conversation the caller did not send to.

const (
	mirrorChatA = "google-chat-a"
	mirrorChatB = "google-chat-b"
)

type mirrorReplyFixture struct {
	v2     *sqlite.Store
	v2Path string
	deps   Deps
}

// newMirrorReplyFixture seeds two legacy SMS conversations, each holding one
// message from the other side and one of mine.
func newMirrorReplyFixture(t *testing.T) mirrorReplyFixture {
	t.Helper()
	legacy := openLegacyTestStore(t)
	v2Path := filepath.Join(t.TempDir(), "v2.sqlite3")
	v2, err := sqlite.Open(v2Path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { _ = v2.Close() })
	for index, conversationID := range []string{mirrorChatA, mirrorChatB} {
		seedLegacyConversation(t, legacy, conversationID, "sms", false)
		for offset, fromMe := range []bool{false, true} {
			if err := legacy.UpsertMessage(&db.Message{
				MessageID:      mirrorReplyTargetID(conversationID, fromMe),
				ConversationID: conversationID,
				Body:           "quoted",
				TimestampMS:    1_900_000_000_000 + int64(10*index+offset),
				IsFromMe:       fromMe,
				SourcePlatform: "sms",
			}); err != nil {
				t.Fatalf("UpsertMessage(): %v", err)
			}
		}
	}
	registry := submitTestRegistry{caps: map[string]bridge.CapabilitySet{
		googleAccountID: {TextSend: true},
	}}
	return mirrorReplyFixture{
		v2:     v2,
		v2Path: v2Path,
		deps: Deps{
			Legacy:   legacy,
			V2:       v2,
			Service:  newSubmitTestService(t, v2, registry, nil),
			Registry: registry,
		},
	}
}

func mirrorReplyTargetID(conversationID string, fromMe bool) string {
	if fromMe {
		return conversationID + "-mine"
	}
	return conversationID + "-peer"
}

// mirrorRows counts the v2 rows a send can write for its reply target.
type mirrorRows struct{ Messages, Inbox, Outbox int }

// rows reads the counts, and the mirrored conversations, through a second
// connection to the v2 store.
func (f mirrorReplyFixture) rows(t *testing.T) (mirrorRows, []string) {
	t.Helper()
	inspection, err := sql.Open("sqlite", "file:"+f.v2Path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open a second connection to the v2 store: %v", err)
	}
	defer inspection.Close()
	var counts mirrorRows
	for table, into := range map[string]*int{
		"messages": &counts.Messages,
		"inbox":    &counts.Inbox,
		"outbox":   &counts.Outbox,
	} {
		if err := inspection.QueryRow("SELECT COUNT(*) FROM " + table).Scan(into); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
	}
	result, err := inspection.Query(`SELECT conversation_id FROM conversations ORDER BY conversation_id`)
	if err != nil {
		t.Fatalf("list mirrored conversations: %v", err)
	}
	defer result.Close()
	var conversations []string
	for result.Next() {
		var conversationID string
		if err := result.Scan(&conversationID); err != nil {
			t.Fatalf("scan mirrored conversation: %v", err)
		}
		conversations = append(conversations, conversationID)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("list mirrored conversations: %v", err)
	}
	return counts, conversations
}

func TestSubmitTextRefusesAnotherConversationsMessageBeforeMirroringIt(t *testing.T) {
	for _, fromMe := range []bool{false, true} {
		t.Run(fmt.Sprintf("from me %t", fromMe), func(t *testing.T) {
			fixture := newMirrorReplyFixture(t)
			_, err := SubmitText(context.Background(), fixture.deps, TextInput{
				ConversationID: mirrorChatA,
				Body:           "must not enqueue",
				ReplyToID:      mirrorReplyTargetID(mirrorChatB, fromMe),
				IdempotencyKey: "cross-conversation-reply",
			})
			if !errors.Is(err, ErrReplyTargetUnavailable) {
				t.Fatalf("SubmitText() error = %v, want ErrReplyTargetUnavailable", err)
			}
			counts, conversations := fixture.rows(t)
			if counts != (mirrorRows{}) {
				t.Fatalf("a refused reply target left %+v in v2, want nothing", counts)
			}
			// The send's own conversation is mirrored before any reply check,
			// as it is for every mirror send; the quoted one is not.
			if want := []string{mirrorChatA}; !slices.Equal(conversations, want) {
				t.Fatalf("mirrored conversations = %v, want %v", conversations, want)
			}
		})
	}
}

// mirrorReplyKind names where a mirror send's reply_to_id points, relative
// to the conversation the send goes to.
type mirrorReplyKind int

const (
	mirrorReplyNone mirrorReplyKind = iota
	mirrorReplyPeerHere
	mirrorReplyMineHere
	mirrorReplyPeerElsewhere
	mirrorReplyMineElsewhere
	mirrorReplyUnknown
	mirrorReplyBlank
	mirrorReplyKindCount
)

func (k mirrorReplyKind) String() string {
	return [...]string{
		"no reply", "their message here", "my message here",
		"their message elsewhere", "my message elsewhere", "unknown ID", "blank ID",
	}[k]
}

func (k mirrorReplyKind) refused() bool { return k >= mirrorReplyPeerElsewhere }

type mirrorReplyStep struct {
	ToB    bool
	Kind   mirrorReplyKind
	Padded bool
}

// ids returns the step's conversation, the reply_to_id it sends, and the
// legacy message ID an accepted reply must quote.
func (s mirrorReplyStep) ids() (conversationID, replyToID, targetID string) {
	conversationID, other := mirrorChatA, mirrorChatB
	if s.ToB {
		conversationID, other = mirrorChatB, mirrorChatA
	}
	switch s.Kind {
	case mirrorReplyNone:
		return conversationID, "", ""
	case mirrorReplyBlank:
		return conversationID, "   ", ""
	case mirrorReplyPeerHere:
		targetID = mirrorReplyTargetID(conversationID, false)
	case mirrorReplyMineHere:
		targetID = mirrorReplyTargetID(conversationID, true)
	case mirrorReplyPeerElsewhere:
		targetID = mirrorReplyTargetID(other, false)
	case mirrorReplyMineElsewhere:
		targetID = mirrorReplyTargetID(other, true)
	case mirrorReplyUnknown:
		targetID = "missing"
	}
	replyToID = targetID
	if s.Padded {
		replyToID = " " + targetID + " "
	}
	return conversationID, replyToID, targetID
}

type mirrorReplyScript []mirrorReplyStep

func (mirrorReplyScript) Generate(r *rand.Rand, _ int) reflect.Value {
	script := make(mirrorReplyScript, 1+r.Intn(6))
	for index := range script {
		script[index] = mirrorReplyStep{
			ToB:    r.Intn(2) == 1,
			Kind:   mirrorReplyKind(r.Intn(int(mirrorReplyKindCount))),
			Padded: r.Intn(3) == 0,
		}
	}
	return reflect.ValueOf(script)
}

// For every sequence of mirror sends: a send is refused exactly when its
// reply target lies outside its conversation (another conversation's
// message, an unknown ID or a blank one); a refusal leaves the v2 message,
// inbox and outbox counts as they were; an accepted send queues one outbox
// row whose message quotes its target's remote ID; and v2 never holds a
// conversation the caller did not send to.
func TestMirrorReplyTargetPropertiesHold(t *testing.T) {
	property := func(script mirrorReplyScript) bool {
		fixture := newMirrorReplyFixture(t)
		repository, err := sqlite.NewMessageRepository(fixture.v2, time.Now)
		if err != nil {
			t.Fatalf("NewMessageRepository(): %v", err)
		}
		ctx := context.Background()
		sentTo := map[string]bool{}
		for index, step := range script {
			conversationID, replyToID, targetID := step.ids()
			before, _ := fixture.rows(t)
			submission, err := SubmitText(ctx, fixture.deps, TextInput{
				ConversationID: conversationID,
				Body:           "property",
				ReplyToID:      replyToID,
				IdempotencyKey: fmt.Sprintf("mirror-property-%d", index),
			})
			sentTo[conversationID] = true
			after, conversations := fixture.rows(t)
			for _, mirrored := range conversations {
				if !sentTo[mirrored] {
					t.Logf("step %d (%s to %s): v2 mirrored %s, which no send went to", index, step.Kind, conversationID, mirrored)
					return false
				}
			}
			if step.Kind.refused() {
				if !errors.Is(err, ErrReplyTargetUnavailable) {
					t.Logf("step %d (%s): error = %v, want ErrReplyTargetUnavailable", index, step.Kind, err)
					return false
				}
				if after != before {
					t.Logf("step %d (%s): refusal moved v2 rows from %+v to %+v", index, step.Kind, before, after)
					return false
				}
				continue
			}
			if err != nil {
				t.Logf("step %d (%s %q): SubmitText() = %v", index, step.Kind, replyToID, err)
				return false
			}
			if after.Outbox != before.Outbox+1 {
				t.Logf("step %d (%s): outbox rows %d -> %d, want one more", index, step.Kind, before.Outbox, after.Outbox)
				return false
			}
			stored, err := repository.GetMessage(ctx, submission.LocalMessageID)
			if err != nil {
				t.Logf("step %d (%s): GetMessage(%q) = %v", index, step.Kind, submission.LocalMessageID, err)
				return false
			}
			quoted := ""
			if stored.ReplyToRemoteID != nil {
				quoted = *stored.ReplyToRemoteID
			}
			if quoted != targetID || stored.ConversationID != conversationID {
				t.Logf("step %d (%s): stored message quotes %q in %s, want %q in %s",
					index, step.Kind, quoted, stored.ConversationID, targetID, conversationID)
				return false
			}
		}
		return true
	}
	config := &quick.Config{MaxCount: 12, Rand: rand.New(rand.NewSource(20261108))}
	if err := quick.Check(property, config); err != nil {
		t.Fatal(err)
	}
}
