package migration

import "testing"

// TestValidationRequiresSearchIndexes pins the gate a staged store must clear:
// a report that passes every check passes, and failing any one check, the
// search-index check among them, fails the migration.
func TestValidationRequiresSearchIndexes(t *testing.T) {
	passing := func() *Report {
		checksums := make([]string, 12)
		checksums[11] = migration0012Checksum
		return &Report{
			Target: TargetReport{SchemaVersion: 12, MigrationChecksums: checksums},
			Validation: ValidationReport{
				QuickCheck:           "ok",
				CountsMatched:        true,
				SampledHashesMatched: true,
				BlobReferencesValid:  true,
				SourceUnchanged:      true,
				SearchIndexesValid:   true,
			},
		}
	}
	if !validationPassed(passing()) {
		t.Fatal("a report that clears every check does not pass")
	}
	for name, fail := range map[string]func(*Report){
		"quick_check":        func(r *Report) { r.Validation.QuickCheck = "row 3 missing from index" },
		"schema version":     func(r *Report) { r.Target.SchemaVersion = 11 },
		"migration count":    func(r *Report) { r.Target.MigrationChecksums = r.Target.MigrationChecksums[:11] },
		"latest checksum":    func(r *Report) { r.Target.MigrationChecksums[11] = "stale" },
		"foreign keys":       func(r *Report) { r.Validation.ForeignKeyViolations = []ForeignKeyViolation{{Table: "messages"}} },
		"orphans":            func(r *Report) { r.Validation.Orphans.MessageSenders = 1 },
		"counts":             func(r *Report) { r.Validation.CountsMatched = false },
		"sampled hashes":     func(r *Report) { r.Validation.SampledHashesMatched = false },
		"blob references":    func(r *Report) { r.Validation.BlobReferencesValid = false },
		"source unchanged":   func(r *Report) { r.Validation.SourceUnchanged = false },
		"search indexes":     func(r *Report) { r.Validation.SearchIndexesValid = false },
		"message collisions": func(r *Report) { r.MessageCollisions = []MessageCollision{{}} },
	} {
		report := passing()
		fail(report)
		if validationPassed(report) {
			t.Errorf("failing %s still passes validation", name)
		}
	}
}
