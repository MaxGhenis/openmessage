package db

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"sort"
	"testing"
)

func TestNativeTapbackKind(t *testing.T) {
	cases := []struct {
		assocType int64
		emoji     string
		kind      int
		add       bool
		shown     string
		ok        bool
	}{
		{0, "", 0, false, "", false},
		{1000, "", 0, false, "", false}, // sticker
		{1999, "", 0, false, "", false},
		{2000, "", 0, true, "❤️", true},
		{2001, "", 1, true, "👍", true},
		{2002, "", 2, true, "👎", true},
		{2003, "", 3, true, "😂", true},
		{2004, "", 4, true, "‼️", true},
		{2005, "🎉", 5, true, "❓", true}, // the emoji column only matters for kind 6
		{2006, " 🎉 ", 6, true, "🎉", true},
		{2006, "", 0, false, "", false}, // a custom add with nothing to show
		{2007, "", 0, false, "", false}, // sticker reaction
		{3000, "", 0, false, "❤️", true},
		{3005, "", 5, false, "❓", true},
		{3006, "🎉", 6, false, "🎉", true},
		{3006, "", 6, false, "", true}, // removes whatever custom emoji
		{3007, "", 0, false, "", false},
	}
	for _, c := range cases {
		kind, add, shown, ok := NativeTapbackKind(c.assocType, c.emoji)
		if kind != c.kind || add != c.add || shown != c.shown || ok != c.ok {
			t.Errorf("NativeTapbackKind(%d, %q) = (%d, %v, %q, %v), want (%d, %v, %q, %v)",
				c.assocType, c.emoji, kind, add, shown, ok, c.kind, c.add, c.shown, c.ok)
		}
		if IsNativeTapbackType(c.assocType) != (c.assocType >= 2000 && c.assocType <= 2006 || c.assocType >= 3000 && c.assocType <= 3006) {
			t.Errorf("IsNativeTapbackType(%d) wrong", c.assocType)
		}
	}
}

func TestNativeReactionsJSONExamples(t *testing.T) {
	add := func(date int64, kind int, actor string) NativeReaction {
		return NativeReaction{Date: date, RowID: date, Add: true, Kind: kind, Actor: actor}
	}
	remove := func(date int64, kind int, actor string) NativeReaction {
		return NativeReaction{Date: date, RowID: date, Kind: kind, Actor: actor}
	}
	cases := []struct {
		name   string
		events []NativeReaction
		want   string
	}{
		{"none", nil, ""},
		{"one", []NativeReaction{add(1, 0, "me")}, `[{"emoji":"❤️","count":1,"actors":["me"]}]`},
		{"a later add replaces the actor's reaction", []NativeReaction{add(1, 0, "a"), add(2, 1, "a")}, `[{"emoji":"👍","count":1,"actors":["a"]}]`},
		{"removal of the current kind", []NativeReaction{add(1, 0, "a"), remove(2, 0, "a")}, ""},
		{"removal of another kind leaves it", []NativeReaction{add(1, 1, "a"), remove(2, 0, "a")}, `[{"emoji":"👍","count":1,"actors":["a"]}]`},
		{"a late, older add before a removal", []NativeReaction{remove(3, 0, "a"), add(2, 0, "a")}, ""},
		{"a late, older removal before an add", []NativeReaction{add(3, 0, "a"), remove(2, 0, "a")}, `[{"emoji":"❤️","count":1,"actors":["a"]}]`},
		{"two actors, one emoji", []NativeReaction{add(2, 0, "b"), add(1, 0, "a")}, `[{"emoji":"❤️","count":2,"actors":["a","b"]}]`},
		{"emojis by earliest active add", []NativeReaction{add(1, 3, "a"), add(2, 0, "b"), add(3, 3, "c")}, `[{"emoji":"😂","count":2,"actors":["a","c"]},{"emoji":"❤️","count":1,"actors":["b"]}]`},
		{"custom emoji", []NativeReaction{{Date: 1, RowID: 1, Add: true, Kind: 6, Emoji: "🎉", Actor: "a"}}, `[{"emoji":"🎉","count":1,"actors":["a"]}]`},
		{"custom removal of another emoji leaves it", []NativeReaction{{Date: 1, RowID: 1, Add: true, Kind: 6, Emoji: "🎉", Actor: "a"}, {Date: 2, RowID: 2, Kind: 6, Emoji: "🔥", Actor: "a"}}, `[{"emoji":"🎉","count":1,"actors":["a"]}]`},
		{"custom removal without an emoji clears it", []NativeReaction{{Date: 1, RowID: 1, Add: true, Kind: 6, Emoji: "🎉", Actor: "a"}, {Date: 2, RowID: 2, Kind: 6, Actor: "a"}}, ""},
		{"ties in date break by ROWID", []NativeReaction{{Date: 5, RowID: 9, Kind: 0, Actor: "a"}, {Date: 5, RowID: 8, Add: true, Kind: 0, Actor: "a"}}, ""},
	}
	for _, c := range cases {
		if got := NativeReactionsJSON(c.events); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// referenceReactions is NativeReactionsJSON written another way, as a
// closed form instead of a replay: an actor's reaction is their latest add,
// unless a matching removal comes after it.
func referenceReactions(events []NativeReaction) map[string]string {
	later := func(a, b NativeReaction) bool { // a after b
		return a.Date > b.Date || (a.Date == b.Date && a.RowID > b.RowID)
	}
	latestAdd := map[string]NativeReaction{}
	for _, e := range events {
		if !e.Add {
			continue
		}
		if cur, ok := latestAdd[e.Actor]; !ok || later(e, cur) {
			latestAdd[e.Actor] = e
		}
	}
	out := map[string]string{}
	for actor, a := range latestAdd {
		removed := false
		for _, e := range events {
			if e.Add || e.Actor != actor || !later(e, a) || e.Kind != a.Kind {
				continue
			}
			if e.Kind == 6 && e.Emoji != "" && e.Emoji != a.Emoji {
				continue
			}
			removed = true
		}
		if removed {
			continue
		}
		emoji := a.Emoji
		if a.Kind < 6 {
			emoji = nativeTapbackEmoji[a.Kind]
		}
		out[actor] = emoji
	}
	return out
}

// TestNativeReactionsJSONIsAPureFunction (I3): for random event sets, the
// stored reactions equal the closed-form reference, the same whatever order
// the events arrive in, every actor appears once, counts match actors, and
// emojis are ordered by their earliest active add.
func TestNativeReactionsJSONIsAPureFunction(t *testing.T) {
	actors := []string{"me", "+15550000001", "+15550000002", "a@example.com"}
	customs := []string{"🎉", "🔥", ""}
	var kinds [7]int
	adds, removes, ties := 0, 0, 0
	for seed := uint64(0); seed < 3000; seed++ {
		r := rand.New(rand.NewPCG(seed, 0x1a2b))
		n := r.IntN(12)
		events := make([]NativeReaction, n)
		seen := map[int64]bool{}
		for i := range events {
			e := NativeReaction{
				Date:  int64(r.IntN(8)),
				RowID: int64(i + 1),
				Add:   r.IntN(3) != 0,
				Kind:  r.IntN(7),
				Actor: actors[r.IntN(len(actors))],
			}
			if e.Kind == 6 {
				e.Emoji = customs[r.IntN(len(customs))]
				if e.Add && e.Emoji == "" {
					e.Emoji = "🎉" // NativeTapbackKind drops custom adds without one
				}
			}
			if seen[e.Date] {
				ties++
			}
			seen[e.Date] = true
			kinds[e.Kind]++
			if e.Add {
				adds++
			} else {
				removes++
			}
			events[i] = e
		}
		got := NativeReactionsJSON(events)
		for p := 0; p < 3; p++ {
			shuffled := append([]NativeReaction(nil), events...)
			r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			if again := NativeReactionsJSON(shuffled); again != got {
				t.Fatalf("seed %d: arrival order changed the result: %s vs %s", seed, got, again)
			}
		}
		want := referenceReactions(events)
		var decoded []tapbackReaction
		if got != "" {
			if err := json.Unmarshal([]byte(got), &decoded); err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
		}
		have := map[string]string{}
		for _, reaction := range decoded {
			if reaction.Count != len(reaction.Actors) || reaction.Count == 0 {
				t.Fatalf("seed %d: count %d for actors %v", seed, reaction.Count, reaction.Actors)
			}
			for _, actor := range reaction.Actors {
				if _, dup := have[actor]; dup {
					t.Fatalf("seed %d: actor %s holds two reactions in %s", seed, actor, got)
				}
				have[actor] = reaction.Emoji
			}
		}
		if !reflect.DeepEqual(have, want) && !(len(have) == 0 && len(want) == 0) {
			t.Fatalf("seed %d: got %v, reference %v (events %+v)", seed, have, want, events)
		}
		// Emoji order: by the earliest (date, rowid) of an active add.
		first := func(emoji string) [2]int64 {
			best := [2]int64{1 << 62, 1 << 62}
			for _, e := range events {
				if !e.Add || want[e.Actor] != emoji {
					continue
				}
				shown := e.Emoji
				if e.Kind < 6 {
					shown = nativeTapbackEmoji[e.Kind]
				}
				if shown != emoji {
					continue
				}
				// Only the actor's latest add is active.
				latest := true
				for _, o := range events {
					if o.Add && o.Actor == e.Actor && (o.Date > e.Date || (o.Date == e.Date && o.RowID > e.RowID)) {
						latest = false
					}
				}
				if latest && (e.Date < best[0] || (e.Date == best[0] && e.RowID < best[1])) {
					best = [2]int64{e.Date, e.RowID}
				}
			}
			return best
		}
		order := make([]string, len(decoded))
		for i, reaction := range decoded {
			order[i] = reaction.Emoji
		}
		if !sort.SliceIsSorted(order, func(i, j int) bool {
			a, b := first(order[i]), first(order[j])
			return a[0] < b[0] || (a[0] == b[0] && a[1] < b[1])
		}) {
			t.Fatalf("seed %d: emojis out of order in %s", seed, got)
		}
	}
	for kind, n := range kinds {
		if n < 1000 {
			t.Fatalf("kind %d drawn %d times; the generator is skewed", kind, n)
		}
	}
	if adds < 5000 || removes < 3000 || ties < 3000 {
		t.Fatalf("draws: %d adds, %d removes, %d date ties", adds, removes, ties)
	}
	t.Logf("draws: kinds %v, %d adds, %d removes, %d date ties", kinds, adds, removes, ties)
}
