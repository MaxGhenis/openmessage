package google

import (
	"testing"
	"time"

	gmproto "go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/messaging"
)

// A phone that stops answering, end to end through the real dispatcher and v2
// store, with only libgm's transport faked. Every conversation lookup goes
// unanswered for an hour while two texts and a reaction wait. Each would
// otherwise be retried every 5 s, holding the dispatch loop for a full libgm
// timeout each time. Instead:
//
//   - nothing becomes uncertain or rejected: a lookup sends nothing, so every
//     intent stays retryable;
//   - the phone is asked at most once per probe window (30 s doubling to
//     5 minutes), so in an hour the loop is held about 16 times, not ~2,000;
//   - once the phone answers, every intent is delivered within the window.
func TestSilentPhoneHoldsTheLoopOncePerWindowAndDeliversAfterwards(t *testing.T) {
	f := newReactionDispatchFixture(t)
	f.adapter.silence.now = f.clock.Now
	f.text.conversationErr = unanswered(gmproto.ActionType_GET_CONVERSATION)
	f.reaction.conversationErr = unanswered(gmproto.ActionType_GET_CONVERSATION)
	f.reaction.sendResult = &gmproto.SendReactionResponse{Success: true}

	first, err := f.service.SendText(f.ctx, messaging.SendTextCommand{CommonCommand: f.common(), Body: "one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.service.SendText(f.ctx, messaging.SendTextCommand{CommonCommand: f.common(), Body: "two"})
	if err != nil {
		t.Fatal(err)
	}
	reaction := f.react("v2-target")
	intents := []string{first.OutboxID, second.OutboxID, reaction.OutboxID}

	lookups := func() int { return f.text.conversationCalls + f.reaction.conversationCalls }
	start := f.clock.Now()
	var probeTimes []time.Time
	for f.clock.Now().Sub(start) < time.Hour {
		before := lookups()
		if _, err := f.service.DispatchDue(f.ctx, 4); err != nil {
			t.Fatalf("DispatchDue at +%s: %v", f.clock.Now().Sub(start), err)
		}
		switch n := lookups() - before; {
		case n > 1:
			t.Fatalf("%d phone requests in one pass at +%s, want at most 1", n, f.clock.Now().Sub(start))
		case n == 1:
			probeTimes = append(probeTimes, f.clock.Now())
		}
		for _, id := range intents {
			if state := f.delivery(id).State; state != messaging.OutboxNotDispatched && state != messaging.OutboxQueued {
				t.Fatalf("intent %s is %s at +%s, want it still retryable", id, state, f.clock.Now().Sub(start))
			}
		}
		f.clock.Advance(5 * time.Second)
	}
	if n := len(probeTimes); n < 12 || n > 17 {
		t.Fatalf("phone asked %d times in an hour, want one per window (about 16)", n)
	}
	for i := 1; i < len(probeTimes); i++ {
		if gap := probeTimes[i].Sub(probeTimes[i-1]); gap < wantBackoff(i) {
			t.Fatalf("probes %d and %d were %s apart, inside the %s window", i, i+1, gap, wantBackoff(i))
		}
	}
	if f.text.sendCalls != 0 || f.reaction.sendCalls != 0 {
		t.Fatalf("sends reached the phone while lookups went unanswered: text=%d reaction=%d", f.text.sendCalls, f.reaction.sendCalls)
	}

	// The phone answers again. Within one window plus the dispatcher's own
	// retry delay, every intent is delivered.
	f.text.conversationErr = nil
	f.reaction.conversationErr = nil
	recovered := f.clock.Now()
	allConfirmed := func() bool {
		for _, id := range intents {
			if f.delivery(id).State != messaging.OutboxConfirmed {
				return false
			}
		}
		return true
	}
	for !allConfirmed() {
		if f.clock.Now().Sub(recovered) > 5*time.Minute+30*time.Second {
			for _, id := range intents {
				t.Logf("intent %s: %s", id, f.delivery(id).State)
			}
			t.Fatalf("not every intent was delivered within a window plus 30s of the phone answering")
		}
		if _, err := f.service.DispatchDue(f.ctx, 4); err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(5 * time.Second)
	}
	t.Logf("phone asked %d times in an hour of silence; everything delivered %s after it answered again",
		len(probeTimes), f.clock.Now().Sub(recovered))
	if f.text.sendCalls != 2 || f.reaction.sendCalls != 1 {
		t.Fatalf("sends after recovery: text=%d reaction=%d, want 2 and 1", f.text.sendCalls, f.reaction.sendCalls)
	}
}
