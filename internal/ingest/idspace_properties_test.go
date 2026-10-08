package ingest

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// Property tests for Google wire-id binding. Each case generates a random
// phone: threads with wire ids, kinds, titles and rosters, deliberately
// reusing rosters so several live threads share one. Frames are fed through a
// real worker and store.
//
// Invariants (no id-space reset):
//   N1. Every announced thread keeps the row its wire id was bound to when it
//       was first announced, for every later frame.
//   N2. Announced threads never share a row.
//   N3. Each announced thread's row carries that thread's title.
//   N4. A message sent to an announced thread lands in that thread's row.
//   N5. With each thread announced before its first message, no frame rebinds
//       or displaces anything.
//   N6. No message is lost: every message fed is stored in some thread.
// Invariants (one id-space reset, then the new phone lists its threads twice):
//   R1. After the listings, every new-phone thread has its own row, titled for
//       that thread, and messages land there.
//   R2. The second listing changes no binding and rebinds nothing.
//   R3. When the very first new-phone frame collides with a different old-phone
//       thread (so the reset is detected before any fresh id is seen), every
//       thread whose roster is unique continues its old row.
//   R4. No message is lost.
//
// Direct peers and group members come from disjoint pools, and two group
// rosters are either identical or disjoint. Overlapping-but-unequal rosters
// are treated as one evolving group by design (rostersConsistent), which these
// properties do not model.

var (
	propDirectPeers  = []string{"+15551000001", "+15551000002", "+15551000003"}
	propGroupRosters = [][]string{
		{"+15552000001", "+15552000002"},
		{"+15552000003", "+15552000004", "+15552000005"},
	}
)

type propThread struct {
	remoteID string
	kind     string
	title    string
	roster   []string
}

func (p propThread) conversationEvent() bridge.Event {
	return idsConversationEvent(p.remoteID, p.kind, p.title, p.roster...)
}

func (p propThread) rosterKey() string {
	return p.kind + fmt.Sprint(p.roster)
}

func propCases() int {
	if value, err := strconv.Atoi(os.Getenv("OPENMESSAGE_PROPERTY_CASES")); err == nil && value > 0 {
		return value
	}
	return 60
}

func propRandomThread(rng *rand.Rand, remoteID, title string) propThread {
	if rng.IntN(2) == 0 {
		return propThread{
			remoteID: remoteID,
			kind:     "direct",
			title:    title,
			roster:   []string{propDirectPeers[rng.IntN(len(propDirectPeers))]},
		}
	}
	return propThread{
		remoteID: remoteID,
		kind:     "group",
		title:    title,
		roster:   propGroupRosters[rng.IntN(len(propGroupRosters))],
	}
}

type propRunner struct {
	t       *testing.T
	script  *idsScript
	harness *i01Harness
	frames  int
	nextMsg int
	clockMS int64
}

func newPropRunner(t *testing.T) *propRunner {
	script := &idsScript{}
	return &propRunner{
		t:       t,
		script:  script,
		harness: i01NewHarness(t, script, nil),
		clockMS: idsAnimalsTimeMS,
	}
}

func (r *propRunner) feed(events ...bridge.Event) {
	r.t.Helper()
	r.frames++
	name := "prop-" + strconv.Itoa(r.frames)
	r.script.add(name, events...)
	idsRun(r.t, r.harness, name)
}

// message feeds one incoming message from a roster member and returns its
// remote message id.
func (r *propRunner) message(rng *rand.Rand, thread propThread) string {
	r.t.Helper()
	r.nextMsg++
	r.clockMS += 1000
	id := "m" + strconv.Itoa(r.nextMsg)
	sender := thread.roster[rng.IntN(len(thread.roster))]
	r.feed(idsIncoming(thread.remoteID, id, sender, "body "+id, r.clockMS))
	return id
}

func (r *propRunner) assertNoMessageLost(label string) {
	r.t.Helper()
	stored := i01QueryInt64(r.t, r.harness.path, `SELECT COUNT(*) FROM messages`)
	if stored != int64(r.nextMsg) {
		r.t.Fatalf("%s: %d messages stored, want all %d fed", label, stored, r.nextMsg)
	}
}

func (r *propRunner) boundRow(remoteID string) (string, string, bool) {
	conversation, err := r.harness.store.GetConversationByRemote(i01AccountID, remoteID)
	if err != nil {
		return "", "", false
	}
	return conversation.ConversationID, conversation.Title, true
}

func TestGooglePropertyLiveThreadsKeepTheirRows(t *testing.T) {
	for seed := uint64(1); seed <= uint64(propCases()); seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 0x6f6d))
			announceFirst := seed%2 == 1
			threads := make([]propThread, 2+rng.IntN(5))
			for i := range threads {
				threads[i] = propRandomThread(rng, strconv.Itoa(8000+i), "thread "+strconv.Itoa(i))
			}
			r := newPropRunner(t)
			announcedRow := map[string]string{}
			messageThread := map[string]string{}

			check := func(step string) {
				t.Helper()
				owners := map[string]string{}
				for _, thread := range threads {
					want, announced := announcedRow[thread.remoteID]
					if !announced {
						continue
					}
					row, title, ok := r.boundRow(thread.remoteID)
					if !ok || row != want {
						t.Fatalf("%s: N1 %s bound to %q (bound=%v), want %q", step, thread.remoteID, row, ok, want)
					}
					if other, taken := owners[row]; taken {
						t.Fatalf("%s: N2 %s and %s share row %q", step, other, thread.remoteID, row)
					}
					owners[row] = thread.remoteID
					if title != thread.title {
						t.Fatalf("%s: N3 %s title %q, want %q", step, thread.remoteID, title, thread.title)
					}
				}
				for id, remoteID := range messageThread {
					if home := idsMessageHome(t, r.harness, id); home != announcedRow[remoteID] {
						t.Fatalf("%s: N4 message %s in %q, want %s's row %q", step, id, home, remoteID, announcedRow[remoteID])
					}
				}
			}

			steps := len(threads) * (2 + rng.IntN(4))
			for step := 0; step < steps; step++ {
				thread := threads[rng.IntN(len(threads))]
				_, announced := announcedRow[thread.remoteID]
				if (announceFirst && !announced) || rng.IntN(3) == 0 {
					r.feed(thread.conversationEvent())
					if !announced {
						row, _, ok := r.boundRow(thread.remoteID)
						if !ok {
							t.Fatalf("step %d: %s unbound right after its event", step, thread.remoteID)
						}
						announcedRow[thread.remoteID] = row
					}
				} else {
					id := r.message(rng, thread)
					if announced {
						messageThread[id] = thread.remoteID
					}
				}
				check(fmt.Sprintf("step %d", step))
			}
			for _, thread := range threads {
				r.feed(thread.conversationEvent())
				if _, announced := announcedRow[thread.remoteID]; !announced {
					row, _, _ := r.boundRow(thread.remoteID)
					announcedRow[thread.remoteID] = row
				}
			}
			check("final listing")
			r.assertNoMessageLost("N6")
			if announceFirst {
				if got := idsRebinds(r.harness); got != 0 {
					t.Fatalf("N5 remote_rebinds = %d, want 0", got)
				}
				if got := idsDisplacedCount(t, r.harness); got != 0 {
					t.Fatalf("N5 displaced rows = %d, want 0", got)
				}
			}
		})
	}
}

func TestGooglePropertyResetRekeysAndSettles(t *testing.T) {
	for seed := uint64(1); seed <= uint64(propCases()); seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 0x7265))
			count := 2 + rng.IntN(5)

			// Old phone: ids 100..; each thread announced with one message.
			old := make([]propThread, count)
			for i := range old {
				old[i] = propRandomThread(rng, strconv.Itoa(100+i), "thread "+strconv.Itoa(i))
			}
			r := newPropRunner(t)
			oldRow := make([]string, count)
			for i, thread := range old {
				r.feed(thread.conversationEvent())
				r.message(rng, thread)
				oldRow[i], _, _ = r.boundRow(thread.remoteID)
			}

			// New phone: the same threads under fresh ids drawn from a range
			// that overlaps the old ids, plus possibly one extra thread whose
			// roster twins an existing one.
			pool := rng.Perm(2 * count)
			fresh := make([]propThread, count)
			for i, thread := range old {
				thread.remoteID = strconv.Itoa(100 + pool[i])
				fresh[i] = thread
			}
			if rng.IntN(2) == 0 {
				twin := fresh[rng.IntN(count)]
				twin.remoteID = strconv.Itoa(100 + pool[count])
				twin.title = "twin of " + twin.title
				fresh = append(fresh, twin)
			}
			rosterUses := map[string]int{}
			for _, thread := range fresh {
				rosterUses[thread.rosterKey()]++
			}

			order := rng.Perm(len(fresh))
			first := fresh[order[0]]
			detectedFirst := false
			for i, thread := range old {
				if thread.remoteID == first.remoteID && first.rosterKey() != thread.rosterKey() {
					detectedFirst = true
					_ = i
				}
			}

			for _, index := range order {
				r.feed(fresh[index].conversationEvent())
			}
			for _, index := range rng.Perm(len(fresh)) {
				r.feed(fresh[index].conversationEvent())
			}
			rows := map[string]string{}
			owners := map[string]string{}
			for _, thread := range fresh {
				row, title, ok := r.boundRow(thread.remoteID)
				if !ok {
					t.Fatalf("R1 %s unbound after two listings", thread.remoteID)
				}
				if other, taken := owners[row]; taken {
					t.Fatalf("R1 %s and %s share row %q", other, thread.remoteID, row)
				}
				owners[row] = thread.remoteID
				if title != thread.title {
					t.Fatalf("R1 %s title %q, want %q", thread.remoteID, title, thread.title)
				}
				rows[thread.remoteID] = row
			}

			rebinds := idsRebinds(r.harness)
			for _, index := range rng.Perm(len(fresh)) {
				r.feed(fresh[index].conversationEvent())
			}
			if got := idsRebinds(r.harness); got != rebinds {
				t.Fatalf("R2 third listing rebinds = %d, want %d", got, rebinds)
			}
			for _, thread := range fresh {
				if row, _, _ := r.boundRow(thread.remoteID); row != rows[thread.remoteID] {
					t.Fatalf("R2 %s moved from %q to %q on re-listing", thread.remoteID, rows[thread.remoteID], row)
				}
			}

			for _, thread := range fresh {
				id := r.message(rng, thread)
				if home := idsMessageHome(t, r.harness, id); home != rows[thread.remoteID] {
					t.Fatalf("R1 message %s in %q, want %s's row %q", id, home, thread.remoteID, rows[thread.remoteID])
				}
			}

			r.assertNoMessageLost("R4")
			if detectedFirst {
				for i := range old {
					thread := fresh[i]
					if rosterUses[thread.rosterKey()] != 1 {
						continue
					}
					oldRoster := 0
					for _, o := range old {
						if o.rosterKey() == thread.rosterKey() {
							oldRoster++
						}
					}
					if oldRoster != 1 {
						continue
					}
					if rows[thread.remoteID] != oldRow[i] {
						t.Fatalf("R3 %s (old id %s) bound to %q, want its old row %q",
							thread.remoteID, old[i].remoteID, rows[thread.remoteID], oldRow[i])
					}
				}
			}
		})
	}
}
