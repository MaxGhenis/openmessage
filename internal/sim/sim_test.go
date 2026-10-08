package sim

import (
	"strings"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

// dualSIMConversation mirrors a live dual-SIM thread: two self participants
// (1208 = slot 1, 1207 = slot 2), the phone's default being 1207, plus the
// other party. Participant order deliberately lists the non-default first.
func dualSIMConversation() *gmproto.Conversation {
	return &gmproto.Conversation{
		ConversationID:    "1154",
		DefaultOutgoingID: "1207",
		Participants: []*gmproto.Participant{
			{ID: &gmproto.SmallInfo{Number: "+15550100002", ParticipantID: "1208"}, IsMe: true, SimPayload: &gmproto.SIMPayload{Two: 1, SIMNumber: 1}},
			{ID: &gmproto.SmallInfo{Number: "+15550100001", ParticipantID: "1207"}, IsMe: true, SimPayload: &gmproto.SIMPayload{Two: 1, SIMNumber: 2}},
			{ID: &gmproto.SmallInfo{Number: "+15550100099", ParticipantID: "1275"}},
		},
	}
}

func registryWithCarriers() *Registry {
	r := &Registry{}
	r.SetCards([]*gmproto.SIMCard{
		{SIMParticipant: &gmproto.SIMParticipant{ID: "1208"}, SIMData: &gmproto.SIMData{CarrierName: "Orange", FormattedPhoneNumber: "+1 555 010 0002", SIMPayload: &gmproto.SIMPayload{Two: 1, SIMNumber: 1}}},
		{SIMParticipant: &gmproto.SIMParticipant{ID: "1207"}, SIMData: &gmproto.SIMData{CarrierName: "Play", FormattedPhoneNumber: "+1 555 010 0001", SIMPayload: &gmproto.SIMPayload{Two: 1, SIMNumber: 2}}},
	})
	return r
}

func TestFromConversationDefaultFirstWithCarriers(t *testing.T) {
	slots := FromConversation(dualSIMConversation(), registryWithCarriers())
	if len(slots) != 2 {
		t.Fatalf("got %d slots, want 2", len(slots))
	}
	if !slots[0].IsDefault || slots[0].ParticipantID != "1207" || slots[0].Slot != 2 {
		t.Fatalf("default slot = %+v, want participant 1207 / slot 2", slots[0])
	}
	if slots[0].Carrier != "Play" || slots[1].Carrier != "Orange" {
		t.Fatalf("carriers = %q/%q, want Play/Orange", slots[0].Carrier, slots[1].Carrier)
	}
	if got := slots[0].Label(); got != "SIM 2 (+15550100001, Play)" {
		t.Fatalf("label = %q", got)
	}
}

func TestSelectDefaultHonoursDefaultOutgoingID(t *testing.T) {
	pid, payload, chosen, err := Select(dualSIMConversation(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pid != "+15550100001" || payload.GetSIMNumber() != 2 || chosen == nil || !chosen.IsDefault {
		t.Fatalf("default pick = %q / slot %d / %+v", pid, payload.GetSIMNumber(), chosen)
	}
}

func TestSelectBySlotNumberPhoneAndParticipant(t *testing.T) {
	conv := dualSIMConversation()
	for _, sel := range []string{"1", "SIM 1", "sim1", "+1 555 010 0002", "15550100002", "5550100002", "0100002", "1208"} {
		pid, payload, _, err := Select(conv, sel, nil)
		if err != nil {
			t.Fatalf("%q: %v", sel, err)
		}
		if pid != "+15550100002" || payload.GetSIMNumber() != 1 {
			t.Fatalf("%q picked %q / slot %d, want SIM 1", sel, pid, payload.GetSIMNumber())
		}
	}
	_, _, _, err := Select(conv, "3", nil)
	if err == nil || !strings.Contains(err.Error(), "available:") {
		t.Fatalf("expected descriptive error for unknown SIM, got %v", err)
	}
}

func TestSelectRejectsShortNumberFragments(t *testing.T) {
	// A one- or two-digit typo must fail instead of silently picking a card.
	conv := dualSIMConversation()
	for _, sel := range []string{"5", "0", "95", "100002", "2 "} {
		if _, _, _, err := Select(conv, sel, nil); err == nil {
			// "2 " is a slot number and is allowed; everything else must fail.
			if strings.TrimSpace(sel) != "2" {
				t.Fatalf("%q unexpectedly selected a SIM", sel)
			}
		}
	}
}

func TestSelectCarrierBeatsNumberSuffix(t *testing.T) {
	r := &Registry{}
	r.SetCards([]*gmproto.SIMCard{
		{SIMParticipant: &gmproto.SIMParticipant{ID: "1208"}, SIMData: &gmproto.SIMData{CarrierName: "O2", SIMPayload: &gmproto.SIMPayload{SIMNumber: 1}}},
		{SIMParticipant: &gmproto.SIMParticipant{ID: "1207"}, SIMData: &gmproto.SIMData{CarrierName: "Play", SIMPayload: &gmproto.SIMPayload{SIMNumber: 2}}},
	})
	// Slot 2's number ends in ...0001 and "O2" contains a 2: the carrier must win.
	pid, _, _, err := Select(dualSIMConversation(), "o2", r)
	if err != nil || pid != "+15550100002" {
		t.Fatalf("carrier pick = %q, %v", pid, err)
	}
}

func TestSelectByCarrierName(t *testing.T) {
	pid, _, _, err := Select(dualSIMConversation(), "orange", registryWithCarriers())
	if err != nil || pid != "+15550100002" {
		t.Fatalf("carrier pick = %q, %v", pid, err)
	}
}

func TestSelectFallsBackToConversationSIMCardWithoutSelfParticipants(t *testing.T) {
	fallback := &gmproto.SIMPayload{Two: 2, SIMNumber: 2}
	conv := &gmproto.Conversation{
		Participants: []*gmproto.Participant{{ID: &gmproto.SmallInfo{Number: "+15550001111"}}},
		SimCard:      &gmproto.SIMCard{SIMData: &gmproto.SIMData{SIMPayload: fallback}},
	}
	pid, payload, chosen, err := Select(conv, "", nil)
	if err != nil || pid != "" || payload != fallback || chosen != nil {
		t.Fatalf("fallback = %q %p %v %v", pid, payload, chosen, err)
	}
	if _, _, _, err := Select(conv, "1", nil); err == nil {
		t.Fatal("selecting a SIM with no self participants must fail")
	}
}

func TestSlotsFromParticipantsJSONAndAttribution(t *testing.T) {
	js := `[{"name":"Me","number":"+15550100002","is_me":true,"id":"1208","sim":1},` +
		`{"name":"Me","number":"+15550100001","is_me":true,"id":"1207","sim":2,"sim_default":true},` +
		`{"name":"","number":"+15550100099","id":"1275"}]`
	slots := SlotsFromParticipantsJSON(js)
	if len(slots) != 2 || slots[0].Slot != 2 || !slots[0].IsDefault {
		t.Fatalf("slots = %+v", slots)
	}
	out, how := AttributeMessage(slots, true, "+15550100002")
	if how != SentFrom || out.Slot != 1 {
		t.Fatalf("outgoing attribution = %+v %v", out, how)
	}
	if got := MessageLabel(out, how); got != "SIM 1 (+15550100002)" {
		t.Fatalf("outgoing label = %q", got)
	}
	in, how := AttributeMessage(slots, false, "+15550100099")
	if how != ThreadDefault || in.Slot != 2 {
		t.Fatalf("incoming attribution = %+v %v", in, how)
	}
	if got := MessageLabel(in, how); got != "thread default: SIM 2 (+15550100001)" {
		t.Fatalf("incoming label = %q", got)
	}
	if _, how := AttributeMessage(slots[:1], true, "+15550100002"); how != NotAttributed {
		t.Fatal("single-SIM threads must not be labelled")
	}
	if _, how := AttributeMessage(slots, true, "+15550109999"); how != NotAttributed {
		t.Fatal("outgoing from an unknown number must not be labelled")
	}
}

func TestStoredRowsWithoutDefaultNeverAttributeIncoming(t *testing.T) {
	// Rows stored before slots were recorded: participant order is arbitrary,
	// so incoming messages must stay unlabelled rather than guess a card.
	js := `[{"number":"+15550100002","is_me":true,"id":"1208"},{"number":"+15550100001","is_me":true,"id":"1207"},{"number":"+15550100099","id":"1275"}]`
	slots := SlotsFromParticipantsJSON(js)
	if _, how := AttributeMessage(slots, false, "+15550100099"); how != NotAttributed {
		t.Fatalf("incoming attribution without a recorded default = %v", how)
	}
	if got := LabelFor(js, true, "+15550100001"); got != "SIM (+15550100001)" {
		t.Fatalf("outgoing label = %q", got)
	}
}

func TestRegistryIgnoresEmptyUpdates(t *testing.T) {
	r := registryWithCarriers()
	r.SetCards(nil)
	if len(r.Slots()) != 2 {
		t.Fatalf("empty update wiped registry: %+v", r.Slots())
	}
}

func TestLabelerEnrichesStoredRowsFromRegistry(t *testing.T) {
	// Rows stored before slots were recorded: numbers and ids only.
	js := `[{"number":"+15550100002","is_me":true,"id":"1208"},{"number":"+15550100001","is_me":true,"id":"1207"},{"number":"+15550100099","id":"1275"}]`
	l := NewLabeler(func(string) string { return js }, registryWithCarriers())
	if got := l.Label("c", true, "+15550100001"); got != "SIM 2 (+15550100001, Play)" {
		t.Fatalf("enriched label = %q", got)
	}
	if got := NewLabeler(func(string) string { return js }, nil).Label("c", true, "+15550100001"); got != "SIM (+15550100001)" {
		t.Fatalf("plain label = %q", got)
	}
}

func TestAnnotateMessagesSkipsOtherPlatforms(t *testing.T) {
	js := `[{"number":"+15550100002","is_me":true,"id":"1208","sim":1,"sim_default":true},{"number":"+15550100001","is_me":true,"id":"1207","sim":2}]`
	msgs := []*db.Message{
		{ConversationID: "c", IsFromMe: true, SenderNumber: "+15550100001"},
		{ConversationID: "c", IsFromMe: false, SenderNumber: "+15550100099"},
		{ConversationID: "w", IsFromMe: true, SenderNumber: "+15550100001", SourcePlatform: "whatsapp"},
	}
	AnnotateMessages(msgs, func(id string) string {
		if id == "c" {
			return js
		}
		return ""
	}, nil)
	if msgs[0].SIM != "SIM 2 (+15550100001)" || msgs[1].SIM != "thread default: SIM 1 (+15550100002)" || msgs[2].SIM != "" {
		t.Fatalf("labels = %q / %q / %q", msgs[0].SIM, msgs[1].SIM, msgs[2].SIM)
	}
}

func TestRegistryReset(t *testing.T) {
	r := registryWithCarriers()
	r.Reset()
	if len(r.Slots()) != 0 {
		t.Fatalf("reset left %+v", r.Slots())
	}
}
