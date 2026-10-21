package ingest

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/whatsapplid"
)

// Property tests for the WhatsApp LID repair over random stores and evidence.
// The digits are drawn from two small pools, LID-shaped and phone-shaped, but
// nothing keys on the shape: any digits can be attested as a LID, a phone
// number, both or neither, by the outside records or by the store's own LID
// identities, chats and other accounts. The oracle below re-derives the
// attestations from the scenario; the properties hold for every scenario:
//
//   - safety: an identity is rewritten only if its digits are attested as a
//     LID and not as a phone number, and it is not the account's own; its
//     target is the live-ingest identity for the LID (the one paired number,
//     unless that number is itself attested as a LID, else the LID);
//   - liveness: such an identity with no person link, whose target is not the
//     account's own, is rewritten;
//   - conservation: every message keeps its sender up to the rewrite map, the
//     roster and the reactions are the old ones under that map (one reaction
//     per person and message, the newest), and person links follow;
//   - idempotence: a second plan has no rewrites and refuses the same
//     identities, and applying the first plan again changes nothing;
//   - determinism: planning twice, or with evidence recorded in another
//     order, gives the same report.

var (
	lidPropertyLIDs   = []string{"90000000000001", "90000000000002", "90000000000003", "90000000000004", "90000000000005"}
	lidPropertyPhones = []string{"15550000001", "15550000002", "15550000003", "15550000004", "15550000005"}
	lidPropertyEmoji  = []string{"👍", "🔥", "😂"}
)

type lidPropertyIdentity struct {
	Kind   string // "e164" ("+<digits>") or "jid" ("<digits>@lid")
	Digits string
	Name   string
	Self   bool
	Person int // -1 for none
}

func (identity lidPropertyIdentity) canonical() string {
	if identity.Kind == "jid" {
		return identity.Digits + "@lid"
	}
	return "+" + identity.Digits
}

type lidPropertyConversation struct {
	Remote  string
	Kind    string
	Title   string
	Members map[int]string // identity index -> display name
}

type lidPropertyMessage struct {
	Conversation int
	Sender       int // -1 outgoing
	Reactions    map[int][2]int64
	Emoji        map[int]int
}

type lidPropertyScenario struct {
	Identities    []lidPropertyIdentity // identities on the WhatsApp account
	SMSNumbers    []string              // e164 identities on the SMS account
	Conversations []lidPropertyConversation
	Messages      []lidPropertyMessage
	// Outside records.
	LIDs   map[string][]string
	Phones map[string][]string
	Pairs  map[string][]string // lid -> numbers
}

func (lidPropertyScenario) Generate(r *rand.Rand, _ int) reflect.Value {
	s := lidPropertyScenario{LIDs: map[string][]string{}, Phones: map[string][]string{}, Pairs: map[string][]string{}}
	all := append(append([]string{}, lidPropertyLIDs...), lidPropertyPhones...)
	sources := []string{"session_lid_map", "desktop_messages"}
	selfTaken := false
	for position, digits := range all {
		// Any digits can be attested as anything, but LID-shaped ones more
		// often as LIDs, so that numbers a LID is paired with stay usable.
		lidOdds := 6
		if position >= len(lidPropertyLIDs) {
			lidOdds = 2
		}
		for _, kind := range []string{"e164", "jid"} {
			if (kind == "e164" && r.Intn(10) >= 6) || (kind == "jid" && r.Intn(20) >= lidOdds) {
				continue
			}
			identity := lidPropertyIdentity{Kind: kind, Digits: digits, Person: -1}
			switch r.Intn(3) {
			case 0:
				identity.Name = "+" + digits
			case 1:
				identity.Name = "Name " + digits[len(digits)-1:]
			}
			if kind == "e164" && !selfTaken && r.Intn(8) == 0 {
				identity.Self, selfTaken = true, true
			}
			if r.Intn(3) == 0 {
				identity.Person = r.Intn(3)
			}
			s.Identities = append(s.Identities, identity)
		}
		if r.Intn(10) < 1 {
			s.SMSNumbers = append(s.SMSNumbers, digits)
		}
		if r.Intn(12) < lidOdds {
			s.LIDs[digits] = []string{sources[r.Intn(2)]}
		}
		if r.Intn(6) == 0 {
			s.Phones[digits] = []string{sources[r.Intn(2)]}
		}
		// Most pairs name a phone-shaped number; some name anything, which
		// attests a LID-shaped one as a phone number.
		pick := func() string {
			if r.Intn(5) == 0 {
				return all[r.Intn(len(all))]
			}
			return lidPropertyPhones[r.Intn(len(lidPropertyPhones))]
		}
		if r.Intn(2) == 0 {
			s.Pairs[digits] = []string{pick()}
			if r.Intn(6) == 0 {
				s.Pairs[digits] = append(s.Pairs[digits], pick())
			}
		}
	}
	for g := range 2 {
		s.Conversations = append(s.Conversations, lidPropertyConversation{
			Remote: fmt.Sprintf("whatsapp:12036301999999999%d@g.us", g), Kind: "group", Members: map[int]string{},
		})
	}
	remotes := map[string]bool{}
	for index, identity := range s.Identities {
		server := []string{"lid", "s.whatsapp.net"}[r.Intn(2)]
		if remote := "whatsapp:" + identity.Digits + "@" + server; r.Intn(3) == 0 && !remotes[remote] {
			remotes[remote] = true
			title := []string{"", "+" + identity.Digits, "Chat"}[r.Intn(3)]
			s.Conversations = append(s.Conversations, lidPropertyConversation{
				Remote: "whatsapp:" + identity.Digits + "@" + server, Kind: "direct", Title: title,
				Members: map[int]string{index: []string{"", "+" + identity.Digits, identity.Digits + "@lid", "Pal"}[r.Intn(4)]},
			})
		}
		for g := range 2 {
			if r.Intn(2) == 0 {
				s.Conversations[g].Members[index] = []string{"", "+" + identity.Digits, "Member"}[r.Intn(3)]
			}
		}
	}
	if len(s.Identities) > 0 {
		for range r.Intn(12) {
			message := lidPropertyMessage{
				Conversation: r.Intn(len(s.Conversations)), Sender: -1,
				Reactions: map[int][2]int64{}, Emoji: map[int]int{},
			}
			if r.Intn(4) != 0 {
				message.Sender = r.Intn(len(s.Identities))
			}
			for range r.Intn(5) {
				reactor := r.Intn(len(s.Identities))
				message.Reactions[reactor] = [2]int64{1 + int64(r.Intn(3)), 1 + int64(r.Intn(3))}
				message.Emoji[reactor] = r.Intn(len(lidPropertyEmoji))
			}
			s.Messages = append(s.Messages, message)
		}
	}
	return reflect.ValueOf(s)
}

func lidPropertyDirect(s lidPropertyScenario, conversationID string, h *lidRepairHarness) bool {
	return h.scalar(`SELECT kind FROM conversations WHERE conversation_id = ?`, conversationID) == "direct"
}

// oracle re-derives what records attest about each digit string.
type lidPropertyOracle struct {
	lid, phone map[string]bool
	pairs      map[string]map[string]bool
}

func newLIDPropertyOracle(s lidPropertyScenario) lidPropertyOracle {
	o := lidPropertyOracle{lid: map[string]bool{}, phone: map[string]bool{}, pairs: map[string]map[string]bool{}}
	for digits := range s.LIDs {
		o.lid[digits] = true
	}
	for digits := range s.Phones {
		o.phone[digits] = true
	}
	for lid, numbers := range s.Pairs {
		for _, number := range numbers {
			o.lid[lid], o.phone[number] = true, true
			if o.pairs[lid] == nil {
				o.pairs[lid] = map[string]bool{}
			}
			o.pairs[lid][number] = true
		}
	}
	for _, digits := range s.SMSNumbers {
		o.phone[digits] = true
	}
	for _, identity := range s.Identities {
		if identity.Kind == "jid" {
			o.lid[identity.Digits] = true
		}
		if identity.Self {
			o.phone[identity.Digits] = true
		}
	}
	for _, conversation := range s.Conversations {
		jid := strings.TrimPrefix(conversation.Remote, "whatsapp:")
		if digits, ok := strings.CutSuffix(jid, "@lid"); ok {
			o.lid[digits] = true
		} else if digits, ok := strings.CutSuffix(jid, "@s.whatsapp.net"); ok {
			o.phone[digits] = true
		}
	}
	return o
}

// target is the (kind, canonical) the repair must give digits a LID.
func (o lidPropertyOracle) target(digits string) (string, string) {
	if len(o.pairs[digits]) == 1 {
		for number := range o.pairs[digits] {
			if !o.lid[number] {
				return "e164", "+" + number
			}
		}
	}
	return "jid", digits + "@lid"
}

func (s lidPropertyScenario) evidence(reverse bool) *whatsapplid.Evidence {
	evidence := whatsapplid.NewEvidence()
	var steps []func()
	keys := func(m map[string][]string) []string {
		out := make([]string, 0, len(m))
		for key := range m {
			out = append(out, key)
		}
		sort.Strings(out)
		return out
	}
	for _, digits := range keys(s.LIDs) {
		for _, source := range s.LIDs[digits] {
			steps = append(steps, func() { evidence.AddLID(digits, source) })
		}
	}
	for _, digits := range keys(s.Phones) {
		for _, source := range s.Phones[digits] {
			steps = append(steps, func() { evidence.AddPhone(digits, source) })
		}
	}
	for _, lid := range keys(s.Pairs) {
		for _, number := range s.Pairs[lid] {
			steps = append(steps, func() { evidence.AddPair(lid, number, "desktop_lid_pairs") })
		}
	}
	if reverse {
		for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
			steps[i], steps[j] = steps[j], steps[i]
		}
	}
	for _, step := range steps {
		step()
	}
	return evidence
}

type lidPropertySeeded struct {
	identityIDs []string
	messageIDs  []string
	byID        map[string]lidPropertyIdentity
}

func seedLIDProperty(h *lidRepairHarness, s lidPropertyScenario) lidPropertySeeded {
	seeded := lidPropertySeeded{byID: map[string]lidPropertyIdentity{}}
	for _, identity := range s.Identities {
		id := h.identity(lidRepairAccount, identity.Kind, identity.canonical(), identity.Name, identity.Self)
		seeded.identityIDs = append(seeded.identityIDs, id)
		seeded.byID[id] = identity
	}
	for _, digits := range s.SMSNumbers {
		h.identity(lidRepairSMS, "e164", "+"+digits, "", false)
	}
	primaryTaken := map[int]bool{}
	for index, identity := range s.Identities {
		if identity.Person >= 0 {
			person := fmt.Sprintf("person-%d", identity.Person)
			h.link(seeded.identityIDs[index], person, !primaryTaken[identity.Person])
			primaryTaken[identity.Person] = true
		}
	}
	conversationIDs := make([]string, len(s.Conversations))
	for index, conversation := range s.Conversations {
		conversationIDs[index] = h.conversation(conversation.Remote, conversation.Kind, conversation.Title)
		members := make([]int, 0, len(conversation.Members))
		for member := range conversation.Members {
			members = append(members, member)
		}
		sort.Ints(members)
		for _, member := range members {
			h.participant(conversationIDs[index], seeded.identityIDs[member], conversation.Members[member])
		}
	}
	for index, message := range s.Messages {
		sender := ""
		if message.Sender >= 0 {
			sender = seeded.identityIDs[message.Sender]
		}
		conversation := conversationIDs[message.Conversation]
		id := h.message(conversation, fmt.Sprintf("m%d", index), sender, 100)
		seeded.messageIDs = append(seeded.messageIDs, id)
		reactors := make([]int, 0, len(message.Reactions))
		for reactor := range message.Reactions {
			reactors = append(reactors, reactor)
		}
		sort.Ints(reactors)
		for _, reactor := range reactors {
			times := message.Reactions[reactor]
			h.reaction(id, conversation, seeded.identityIDs[reactor], lidPropertyEmoji[message.Emoji[reactor]], times[0], times[1])
		}
	}
	return seeded
}

// lidPropertyState is every row the repair may touch, as sortable strings.
type lidPropertyState struct {
	Identities   map[string]string // id -> kind|canonical|raw|name|self|updated
	Senders      map[string]string // message -> sender
	Participants map[[2]string]string
	Reactions    map[[2]string]string // (message, reactor) -> emoji|occurred|seq|identity|updated
	Links        map[string]string    // identity -> person|primary
	Titles       map[string]string
	Messages     map[string]string // message -> updated
}

func readLIDPropertyState(h *lidRepairHarness) lidPropertyState {
	state := lidPropertyState{
		Identities: map[string]string{}, Senders: map[string]string{}, Participants: map[[2]string]string{},
		Reactions: map[[2]string]string{}, Links: map[string]string{}, Titles: map[string]string{}, Messages: map[string]string{},
	}
	rows := func(query string, scan func(values []string)) {
		h.t.Helper()
		result, err := h.db.Query(query)
		if err != nil {
			h.t.Fatalf("%s: %v", query, err)
		}
		defer result.Close()
		columns, _ := result.Columns()
		for result.Next() {
			values := make([]string, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := result.Scan(pointers...); err != nil {
				h.t.Fatalf("scan: %v", err)
			}
			scan(values)
		}
	}
	rows(`SELECT identity_id, kind || '|' || canonical_value || '|' || raw_value || '|' || display_name || '|' || is_self || '|' || updated_at_ms FROM identities`,
		func(v []string) { state.Identities[v[0]] = v[1] })
	rows(`SELECT message_id, COALESCE(sender_identity_id, ''), updated_at_ms FROM messages`,
		func(v []string) { state.Senders[v[0]] = v[1]; state.Messages[v[0]] = v[2] })
	rows(`SELECT conversation_id, identity_id, role || '|' || display_name || '|' || is_active FROM conversation_participants`,
		func(v []string) { state.Participants[[2]string{v[0], v[1]}] = v[2] })
	rows(`SELECT message_id, reactor_key, emoji || '|' || occurred_at_ms || '|' || source_seq_ms || '|' || COALESCE(reactor_identity_id, '') || '|' || updated_at_ms FROM reactions`,
		func(v []string) { state.Reactions[[2]string{v[0], v[1]}] = v[2] })
	rows(`SELECT identity_id, person_id || '|' || is_primary FROM person_identities`,
		func(v []string) { state.Links[v[0]] = v[1] })
	rows(`SELECT conversation_id, title FROM conversations`,
		func(v []string) { state.Titles[v[0]] = v[1] })
	return state
}

func TestLIDRepairProperties(t *testing.T) {
	// coverage counts what the scenarios exercised, so a generator that stops
	// reaching a case shows up in -v output.
	coverage := map[string]int{}
	check := func(s lidPropertyScenario) bool {
		h := newLIDRepairHarness(t)
		seeded := seedLIDProperty(h, s)
		oracle := newLIDPropertyOracle(s)
		before := readLIDPropertyState(h)

		report := h.plan(s.evidence(false))
		if again := h.plan(s.evidence(false)); !reflect.DeepEqual(report, again) {
			t.Errorf("planning twice gave different reports:\n%+v\n%+v", report, again)
			return false
		}
		if reordered := h.plan(s.evidence(true)); !reflect.DeepEqual(report, reordered) {
			t.Errorf("evidence order changed the report:\n%+v\n%+v", report, reordered)
			return false
		}
		h.apply(report)
		after := readLIDPropertyState(h)
		for _, candidate := range report.Candidates {
			coverage[candidate.Verdict]++
			if candidate.Verdict == lidVerdictRewrite {
				coverage["rewrite to "+candidate.TargetKind]++
				if candidate.TargetExists {
					coverage["rewrite into an existing identity"]++
				}
			}
		}
		if len(after.Reactions) < len(before.Reactions) {
			coverage["reaction collisions"]++
		}
		if len(after.Participants) < len(before.Participants) {
			coverage["roster merges"]++
		}

		// The rewrite map: identity id -> the id it now is.
		mapped := map[string]string{}
		for _, rewrite := range report.Rewrites {
			id := h.scalar(`SELECT identity_id FROM identities WHERE account_id = ? AND kind = ? AND canonical_value = ?`, lidRepairAccount, rewrite.ToKind, rewrite.ToCanonicalValue)
			mapped[rewrite.FromIdentityID] = id
		}
		f := func(id string) string {
			if target, ok := mapped[id]; ok {
				return target
			}
			return id
		}
		targetSelf := func(digits string) bool {
			kind, canonical := oracle.target(digits)
			for _, other := range s.Identities {
				if kind == other.Kind && other.Self && other.canonical() == canonical {
					return true
				}
			}
			return false
		}

		for _, id := range seeded.identityIDs {
			identity := seeded.byID[id]
			_, gone := mapped[id]
			if identity.Kind != "e164" {
				if gone {
					t.Errorf("safety: LID identity %s was rewritten", identity.canonical())
					return false
				}
				continue
			}
			allowed := oracle.lid[identity.Digits] && !oracle.phone[identity.Digits] && !identity.Self
			if gone && !allowed {
				t.Errorf("safety: +%s was rewritten (lid %v, phone %v, self %v)", identity.Digits,
					oracle.lid[identity.Digits], oracle.phone[identity.Digits], identity.Self)
				return false
			}
			if gone {
				kind, canonical := oracle.target(identity.Digits)
				if got := after.Identities[mapped[id]]; !strings.HasPrefix(got, kind+"|"+canonical+"|") {
					t.Errorf("safety: +%s became %q, want %s %s", identity.Digits, got, kind, canonical)
					return false
				}
				if _, still := after.Identities[id]; still {
					t.Errorf("+%s was rewritten but survives", identity.Digits)
					return false
				}
			} else if _, still := after.Identities[id]; !still {
				t.Errorf("+%s was deleted without a rewrite", identity.Digits)
				return false
			}
			if allowed && identity.Person < 0 && !targetSelf(identity.Digits) && !gone {
				t.Errorf("liveness: +%s is a LID with nothing in the way but was not rewritten: %+v", identity.Digits, report.Candidates)
				return false
			}
		}

		// Untouched identities keep every field.
		for id, row := range before.Identities {
			if _, rewritten := mapped[id]; rewritten {
				continue
			}
			isTarget := false
			for _, target := range mapped {
				isTarget = isTarget || target == id
			}
			if !isTarget && after.Identities[id] != row {
				t.Errorf("identity %s changed from %q to %q without being rewritten or merged into", id, row, after.Identities[id])
				return false
			}
		}

		// Conservation.
		if len(after.Senders) != len(before.Senders) {
			t.Errorf("messages %d -> %d", len(before.Senders), len(after.Senders))
			return false
		}
		for message, sender := range before.Senders {
			if want := f(sender); after.Senders[message] != want {
				t.Errorf("message %s sender %s -> %s, want %s", message, sender, after.Senders[message], want)
				return false
			}
		}
		wantRoster := map[[2]string]bool{}
		for key := range before.Participants {
			wantRoster[[2]string{key[0], f(key[1])}] = true
		}
		gotRoster := map[[2]string]bool{}
		for key := range after.Participants {
			gotRoster[key] = true
		}
		if !reflect.DeepEqual(wantRoster, gotRoster) {
			t.Errorf("roster %v, want %v", gotRoster, wantRoster)
			return false
		}
		type reactionPick struct {
			occurred, seq string
			emoji         map[string]bool
		}
		newest := map[[2]string]*reactionPick{}
		for key, row := range before.Reactions {
			fields := strings.Split(row, "|")
			mappedKey := [2]string{key[0], f(key[1])}
			pick := newest[mappedKey]
			occurred, seq := fmt.Sprintf("%03s", fields[1]), fmt.Sprintf("%03s", fields[2])
			switch {
			case pick == nil || occurred+seq > pick.occurred+pick.seq:
				newest[mappedKey] = &reactionPick{occurred: occurred, seq: seq, emoji: map[string]bool{fields[0]: true}}
			case occurred+seq == pick.occurred+pick.seq:
				pick.emoji[fields[0]] = true
			}
		}
		if len(newest) != len(after.Reactions) {
			t.Errorf("reactions %d after, want %d", len(after.Reactions), len(newest))
			return false
		}
		for key, pick := range newest {
			row, ok := after.Reactions[key]
			if !ok {
				t.Errorf("reaction %v lost", key)
				return false
			}
			fields := strings.Split(row, "|")
			if fmt.Sprintf("%03s", fields[1]) != pick.occurred || fmt.Sprintf("%03s", fields[2]) != pick.seq || !pick.emoji[fields[0]] || fields[3] != key[1] {
				t.Errorf("reaction %v = %q, want the newest of %+v", key, row, pick)
				return false
			}
		}
		for id, link := range before.Links {
			person := strings.Split(link, "|")[0]
			if got := strings.Split(after.Links[f(id)], "|")[0]; got != person {
				t.Errorf("identity %s linked to %s; its identity %s now links to %q", id, person, f(id), got)
				return false
			}
		}
		// No name that only repeats a rewritten LID's digits survives on the
		// identity it became, its rows or its direct conversations.
		for _, rewrite := range report.Rewrites {
			digits := strings.TrimPrefix(rewrite.FromCanonicalValue, "+")
			target := f(rewrite.FromIdentityID)
			if name := strings.Split(after.Identities[target], "|")[3]; isRawUserName(name, digits) {
				t.Errorf("identity %s keeps the raw name %q", target, name)
				return false
			}
			for key, row := range after.Participants {
				if key[1] != target {
					continue
				}
				if name := strings.Split(row, "|")[1]; isRawUserName(name, digits) {
					t.Errorf("participant %v keeps the raw name %q", key, name)
					return false
				}
				if title := after.Titles[key[0]]; isRawUserName(title, digits) && lidPropertyDirect(s, key[0], h) {
					t.Errorf("direct conversation %s keeps the raw title %q", key[0], title)
					return false
				}
			}
		}

		// Idempotence.
		second := h.plan(s.evidence(false))
		if len(second.Rewrites) != 0 {
			t.Errorf("second plan rewrites %+v", second.Rewrites)
			return false
		}
		refused := func(r WhatsAppLIDRepairReport) []string {
			var out []string
			for _, candidate := range r.Candidates {
				if candidate.Verdict != lidVerdictRewrite {
					out = append(out, candidate.CanonicalValue+" "+candidate.Verdict)
				}
			}
			return out
		}
		if !reflect.DeepEqual(refused(report), refused(second)) {
			t.Errorf("refusals changed: %v -> %v", refused(report), refused(second))
			return false
		}
		h.apply(report)
		if again := readLIDPropertyState(h); !reflect.DeepEqual(after, again) {
			t.Errorf("applying the plan again changed the store")
			return false
		}
		return true
	}
	config := repairPropertyConfig(t, 200, 40)
	if err := quick.Check(check, config); err != nil {
		t.Fatal(err)
	}
	t.Logf("coverage: %v", coverage)
	if config.MaxCount < 200 {
		return
	}
	for _, want := range []string{lidVerdictRewrite, lidVerdictAlsoAPhone, lidVerdictOwnIdentity, "rewrite to e164", "rewrite to jid",
		"rewrite into an existing identity", "reaction collisions", "roster merges"} {
		if coverage[want] == 0 {
			t.Errorf("no scenario exercised %q", want)
		}
	}
}
