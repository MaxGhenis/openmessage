package migration

import (
	"testing"

	"github.com/maxghenis/openmessage/internal/v2keys"
)

func TestPlanLegacyReactionsNormalizesPlatformActorsAndSelf(t *testing.T) {
	wa := platformAccounts["whatsapp"]
	signal := platformAccounts["signal"]
	state := reactionPlannerState(map[string]accountSpec{"wa": wa, "signal": signal})
	dataset := legacyDataset{messages: []legacyMessage{
		{ID: "own-wa", ConversationID: "wa", Platform: "whatsapp", SenderNumber: "+16506303657", IsFromMe: true, TimestampMS: 100},
		{ID: "wa-contact", ConversationID: "wa", Platform: "whatsapp", Reactions: `[{"emoji":"👍","count":1,"actors":["14155550200@s.whatsapp.net"]}]`, TimestampMS: 200},
		{ID: "wa-self", ConversationID: "wa", Platform: "whatsapp", Reactions: `[{"emoji":"🔥","count":1,"actors":["16506303657@s.whatsapp.net"]}]`, TimestampMS: 300},
		{ID: "signal-aci", ConversationID: "signal", Platform: "signal", Reactions: `[{"emoji":"❤️","count":1,"actors":["AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"]}]`, TimestampMS: 400},
		{ID: "signal-e164", ConversationID: "signal", Platform: "signal", Reactions: `[{"emoji":"😂","count":1,"actors":["+15551234567"]}]`, TimestampMS: 500},
	}}
	report := &Report{}
	planLegacyReactions(dataset, state, report)
	if len(state.reactions) != 4 {
		t.Fatalf("rows = %d, want 4", len(state.reactions))
	}
	waID := v2keys.DeriveID("identity", wa.AccountID, "e164\x1f+14155550200")
	aciID := v2keys.DeriveID("identity", signal.AccountID, "signal_aci\x1faaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	signalID := v2keys.DeriveID("identity", signal.AccountID, "e164\x1f+15551234567")
	want := []struct {
		key, label string
		self       bool
	}{{waID, "", false}, {"self", "me", true}, {aciID, "", false}, {signalID, "", false}}
	for index, expected := range want {
		row := state.reactions[index].Row
		if row.ReactorKey != expected.key || row.ReactorLabel != expected.label || row.ReactorIsSelf != expected.self {
			t.Errorf("row %d = key %q label %q self %v, want %+v", index, row.ReactorKey, row.ReactorLabel, row.ReactorIsSelf, expected)
		}
	}
}

func TestPlanLegacyReactionDegradationsAndOrdering(t *testing.T) {
	state := reactionPlannerState(map[string]accountSpec{"sms": platformAccounts["sms"]})
	dataset := legacyDataset{messages: []legacyMessage{{ID: "m", ConversationID: "sms", Platform: "sms", TimestampMS: 100,
		Reactions: `[{"emoji":"🔥","count":3},{"emoji":"👍","count":3,"actors":["+1","+2"]},{"emoji":"❤️","count":1,"actors":["+3"]}]`}}}
	report := &Report{}
	planLegacyReactions(dataset, state, report)
	if report.Reactions.ActorlessCountCollapsed != 2 || report.Reactions.ActorCountSurplusDropped != 1 || report.Reactions.RowsPlannable != 4 {
		t.Fatalf("reaction accounting = %+v", report.Reactions)
	}
	if len(state.reactions) != 4 {
		t.Fatalf("rows = %d, want 4", len(state.reactions))
	}
	wantTimes := []int64{100, 101, 101, 102}
	for i, want := range wantTimes {
		if state.reactions[i].Row.OccurredAtMS != want {
			t.Errorf("row %d time = %d, want %d", i, state.reactions[i].Row.OccurredAtMS, want)
		}
	}
}

func TestPlanLegacyReactionMalformedAndUnmappableBranches(t *testing.T) {
	state := reactionPlannerState(map[string]accountSpec{"sms": platformAccounts["sms"]})
	dataset := legacyDataset{messages: []legacyMessage{
		{ID: "empty", ConversationID: "sms", Platform: "sms", TimestampMS: 100, Reactions: `[{"emoji":" ","count":1}]`},
		{ID: "bad-identity", ConversationID: "sms", Platform: "sms", TimestampMS: 101, Reactions: `[{"emoji":"🔥","count":1,"actors":["+"]}]`},
	}}
	report := &Report{}
	planLegacyReactions(dataset, state, report)
	if report.Dropped.MalformedReactions != 1 || report.Dropped.UnmappableReactions != 1 || len(state.reactions) != 0 {
		t.Fatalf("report/state = %+v / %d rows", report.Dropped, len(state.reactions))
	}
}

func reactionPlannerState(conversations map[string]accountSpec) *transformState {
	plans := map[string]*conversationPlan{}
	for id, account := range conversations {
		plans[id] = &conversationPlan{Account: account, V2ID: "v2-" + id}
	}
	return &transformState{accounts: map[string]accountSpec{}, identities: map[identityKey]*identityCandidate{}, conversations: plans}
}
