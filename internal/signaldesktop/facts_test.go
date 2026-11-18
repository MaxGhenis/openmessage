package signaldesktop

import (
	"fmt"
	"math/rand"
	"strconv"
	"testing"

	"github.com/maxghenis/openmessage/internal/v2keys"
)

// The feed dedupes against signal-cli only if it keys each contact by the form
// the live bridge used (the worker's S8 property assumes exactly that). For
// any mix of contacts, whenever v2 holds live evidence for a contact (a thread
// bound to that form, or that form as its only identity), ChooseAddress picks
// it, and HasMessage finds every message v2 stored under that form or under
// the legacy outgoing alias.
//
// With no evidence and both forms known to Desktop it picks the number, the
// live bridge's own precedence. If signal-cli does not know that number, live
// keys the contact by ACI and the two threads split: the one case this rule
// cannot see from v2.
func TestChooseAddressFollowsLiveEvidenceProperty(t *testing.T) {
	random := rand.New(rand.NewSource(20261118))
	pick := func(n uint64) uint64 { return (random.Uint64() >> 33) % n }
	type contact struct {
		e164, aci, live string
		bound           bool
	}
	categories := make([]int, 6)
	for caseIndex := 0; caseIndex < 64; caseIndex++ {
		var contacts []contact
		var identities, bound []string
		var stored []MessageKey
		for index := 0; index < 5; index++ {
			c := contact{}
			forms := pick(3) // 0: number only, 1: ACI only, 2: both
			if forms != 1 {
				c.e164 = fmt.Sprintf("+1555%07d", caseIndex*10+index)
			}
			if forms != 0 {
				c.aci = fmt.Sprintf("%08x-0000-4000-8000-%012x", caseIndex, index)
			}
			c.live = c.e164
			if c.e164 == "" || (c.aci != "" && pick(2) == 0) {
				c.live = c.aci
			}
			c.bound = pick(2) == 0
			categories[int(forms)*2+map[bool]int{false: 0, true: 1}[c.bound]]++
			identities = append(identities, c.live)
			if c.bound {
				bound = append(bound, "signal:"+c.live)
				// v2 may also have learned the other form; a bound thread
				// still decides.
				if c.e164 != "" && c.aci != "" && pick(2) == 0 {
					other := c.e164
					if c.live == c.e164 {
						other = c.aci
					}
					identities = append(identities, other)
				}
				conversation := "signal:" + c.live
				ts := tBaseMS + int64(caseIndex*100+index)
				stored = append(stored,
					MessageKey{MessageID: v2keys.DeriveID("message", tAccount, conversation+"\x1f"+v2keys.SignalIncomingSourceID(conversation, c.live, ts))},
					MessageKey{MessageID: "migrated-" + strconv.Itoa(index), RemoteConversationID: conversation, RemoteMessageID: v2keys.SignalLocalAlias(conversation, ts+1)},
				)
			}
			contacts = append(contacts, c)
		}
		facts := NewFacts(tAccount, tSelf, identities, bound, stored)
		for index, c := range contacts {
			if got := facts.ChooseAddress(c.e164, c.aci); got != c.live {
				t.Fatalf("case %d contact %d (%+v): ChooseAddress = %q, want the live form", caseIndex, index, c, got)
			}
			if !c.bound {
				continue
			}
			conversation := "signal:" + c.live
			if !facts.Bound(conversation) {
				t.Fatalf("case %d contact %d: thread not bound", caseIndex, index)
			}
			ts := tBaseMS + int64(caseIndex*100+index)
			if !facts.HasMessage(conversation, v2keys.SignalIncomingSourceID(conversation, c.live, ts)) {
				t.Fatalf("case %d contact %d: stored incoming message not found", caseIndex, index)
			}
			if !facts.HasMessage(conversation, v2keys.SignalLocalAlias(conversation, ts+1)) {
				t.Fatalf("case %d contact %d: migrated alias row not found", caseIndex, index)
			}
		}
	}
	for category, count := range categories {
		if count == 0 {
			t.Fatalf("category %d never exercised", category)
		}
	}

	// No evidence at all: the number, when Desktop has one.
	empty := NewFacts(tAccount, tSelf, nil, nil, nil)
	if got := empty.ChooseAddress(tPeerE164, tPeerACI); got != tPeerE164 {
		t.Fatalf("no evidence: ChooseAddress = %q, want the number", got)
	}
	if got := empty.ChooseAddress("", tPeerACI); got != tPeerACI {
		t.Fatalf("no number: ChooseAddress = %q, want the ACI", got)
	}
}
