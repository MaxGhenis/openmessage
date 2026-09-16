package sim

import (
	"strings"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
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
	for _, sel := range []string{"1", "SIM 1", "sim1", "+1 555 010 0002", "5550100002", "1208"} {
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
	out, ok := AttributeMessage(slots, true, "+15550100002")
	if !ok || out.Slot != 1 {
		t.Fatalf("outgoing attribution = %+v %v", out, ok)
	}
	in, ok := AttributeMessage(slots, false, "+15550100099")
	if !ok || in.Slot != 2 {
		t.Fatalf("incoming attribution = %+v %v", in, ok)
	}
	if _, ok := AttributeMessage(slots[:1], true, "+15550100002"); ok {
		t.Fatal("single-SIM threads must not be labelled")
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
