package sqlite

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"testing/quick"
)

// TestPairEchoesFindsEveryTrueEchoProperty generates sends at least 2 s
// apart, each echo received 0.3 s before to 1 s after its send went out (the
// live store shows 0.28-0.79 s after; "before" covers a frame that outruns
// the transport's answer), some echoes missing, plus identical texts typed on
// the phone at least 30 s from every send. For any such input, in any input
// order, pairEchoes returns exactly the true pairs: no phone text is taken for
// an echo, no send takes another's echo, no pairs cross, and every pair beats
// leaving its send and echo unpaired.
//
// Closer than that, the input is ambiguous, not the rule: when two identical
// texts go out a second apart and the first one's echo never arrives, the
// lone echo fits either send. Either answer shows the same two messages.
func TestPairEchoesFindsEveryTrueEchoProperty(t *testing.T) {
	check := func(seed int64) bool {
		r := rand.New(rand.NewSource(seed))
		var intents []echoIntent
		var echoes []echoCandidate
		truth := map[string]string{}
		var sendTimes []int64
		at := int64(0)
		for i, n := 0, 1+r.Intn(8); i < n; i++ {
			at += 2000 + int64(r.Intn(60_000))
			id := fmt.Sprintf("req-%d", i)
			intents = append(intents, echoIntent{transportRequestID: id, sentAtMS: at})
			sendTimes = append(sendTimes, at)
			if r.Intn(5) == 0 {
				continue // this send's echo has not arrived
			}
			echo := fmt.Sprintf("echo-%d", i)
			echoes = append(echoes, echoCandidate{remoteMessageID: echo, seenAtMS: at - 300 + int64(r.Intn(1300))})
			truth[id] = echo
		}
		for k, n := 0, r.Intn(4); k < n; k++ {
			stamp := int64(r.Intn(int(at + 120_000)))
			far := true
			for _, s := range sendTimes {
				if d := stamp - s; d > -30_000 && d < 30_000 {
					far = false
				}
			}
			if far {
				echoes = append(echoes, echoCandidate{remoteMessageID: fmt.Sprintf("phone-%d", k), seenAtMS: stamp})
			}
		}
		r.Shuffle(len(intents), func(i, j int) { intents[i], intents[j] = intents[j], intents[i] })
		r.Shuffle(len(echoes), func(i, j int) { echoes[i], echoes[j] = echoes[j], echoes[i] })

		pairs := pairEchoes(intents, echoes)
		got := map[string]string{}
		var last *echoPair
		for i := range pairs {
			p := pairs[i]
			got[p.intent.transportRequestID] = p.echo.remoteMessageID
			if c, ok := echoPairCost(p.intent, p.echo); !ok || c >= 2*echoUnpairedCost {
				t.Logf("pair that leaving both unpaired beats: %+v (cost %d)", p, c)
				return false
			}
			if last != nil && (p.intent.sentAtMS < last.intent.sentAtMS || p.echo.seenAtMS < last.echo.seenAtMS) {
				t.Logf("pairs cross: %+v then %+v", *last, p)
				return false
			}
			last = &pairs[i]
		}
		if len(got) != len(truth) {
			t.Logf("seed %d: got %v, want %v", seed, got, truth)
			return false
		}
		for id, echo := range truth {
			if got[id] != echo {
				t.Logf("seed %d: %s paired with %q, want %q (got %v)", seed, id, got[id], echo, got)
				return false
			}
		}
		return true
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 20_000}); err != nil {
		t.Fatal(err)
	}
}

func TestPairEchoesTwoIdenticalTextsASecondApart(t *testing.T) {
	// The case closest-first gets wrong: each send is nearer the other's echo.
	intents := []echoIntent{{transportRequestID: "a", sentAtMS: 32_000}, {transportRequestID: "b", sentAtMS: 33_000}}
	echoes := []echoCandidate{{remoteMessageID: "echo-a", seenAtMS: 32_780}, {remoteMessageID: "echo-b", seenAtMS: 33_300}}
	pairs := pairEchoes(intents, echoes)
	if len(pairs) != 2 || pairs[0].echo.remoteMessageID != "echo-a" || pairs[1].echo.remoteMessageID != "echo-b" {
		t.Fatalf("pairs = %+v, want a->echo-a, b->echo-b", pairs)
	}
}

// A send whose echo never arrived, the next send's echo, and the same words
// typed on the phone a few seconds later: summed linear distance (5.4 s +
// 7 s) is less than leaving two items unpaired, so a linear cost shifts every
// pair by one. The squared cost does not.
func TestPairEchoesDoesNotShiftPastAMissingEcho(t *testing.T) {
	intents := []echoIntent{{transportRequestID: "no-echo", sentAtMS: 0}, {transportRequestID: "next", sentAtMS: 5_000}}
	echoes := []echoCandidate{{remoteMessageID: "echo-next", seenAtMS: 5_400}, {remoteMessageID: "phone", seenAtMS: 12_000}}
	pairs := pairEchoes(intents, echoes)
	if len(pairs) != 1 || pairs[0].intent.transportRequestID != "next" || pairs[0].echo.remoteMessageID != "echo-next" {
		t.Fatalf("pairs = %+v, want only next->echo-next", pairs)
	}
}

func TestEchoPairCostBounds(t *testing.T) {
	for _, tc := range []struct {
		lagMS    int64
		pairable bool // beats leaving both unpaired
	}{
		{-6_000, false}, {-5_000, true}, {-300, true}, {500, true}, {800, true}, {6_000, true}, {6_500, false}, {30_000, false}, {120_000, false},
		// Weeks apart: the squared lag must not overflow into a cheap pair.
		{30 * 24 * 3600 * 1000, false}, {-30 * 24 * 3600 * 1000, false}, {math.MaxInt64 / 2, false},
	} {
		c, ok := echoPairCost(echoIntent{transportRequestID: "s", sentAtMS: 1_000_000}, echoCandidate{remoteMessageID: "e", seenAtMS: 1_000_000 + tc.lagMS})
		if got := ok && c < 2*echoUnpairedCost; got != tc.pairable {
			t.Errorf("lag %d ms: cost %d (ok=%v), pairable=%v, want %v", tc.lagMS, c, ok, got, tc.pairable)
		}
	}
}
