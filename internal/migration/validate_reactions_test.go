package migration

import "testing"

func TestCountsMatchRejectsStrayMigratedReaction(t *testing.T) {
	dataset := legacyDataset{}
	state := &transformState{
		accounts:                  map[string]accountSpec{},
		identities:                map[identityKey]*identityCandidate{},
		conversations:             map[string]*conversationPlan{},
		contactIdentityKeys:       map[identityKey]struct{}{},
		expectedHistoryByPlatform: map[string]map[string]struct{}{},
		scheduledMessages:         map[string]struct{}{},
	}
	report := &Report{Target: TargetReport{Counts: map[string]int64{"reactions": 1}}}
	if countsMatch(dataset, state, report, 0, 0, map[string]int64{}) {
		t.Fatal("countsMatch accepted a migrated store with one stray reactions row")
	}
}
