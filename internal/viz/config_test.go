package viz

import "testing"

func TestDefaultSections(t *testing.T) {
	sections := DefaultSections()

	if len(sections) == 0 {
		t.Fatal("DefaultSections() returned empty slice")
	}

	if sections[0] != "password-gate" {
		t.Errorf("first section = %q, want %q", sections[0], "password-gate")
	}

	if sections[len(sections)-1] != "closing" {
		t.Errorf("last section = %q, want %q", sections[len(sections)-1], "closing")
	}

	// Verify all expected sections are present.
	expected := []string{
		"password-gate", "hero", "timeline-nav",
		"chapter-early", "volume-chart", "chapter-middle",
		"sender-split", "heatmap", "chapter-late",
		"phrases", "silence", "photos", "closing",
	}
	if len(sections) != len(expected) {
		t.Fatalf("got %d sections, want %d", len(sections), len(expected))
	}
	for i, s := range expected {
		if sections[i] != s {
			t.Errorf("sections[%d] = %q, want %q", i, sections[i], s)
		}
	}
}

func TestVizConfigDefaults(t *testing.T) {
	// A zero-value VizConfig should be usable without panics.
	var cfg VizConfig

	if cfg.Person1 != "" {
		t.Errorf("zero-value Person1 = %q, want empty", cfg.Person1)
	}
	if cfg.Person2 != "" {
		t.Errorf("zero-value Person2 = %q, want empty", cfg.Person2)
	}
	if cfg.PasswordHash != "" {
		t.Errorf("zero-value PasswordHash = %q, want empty", cfg.PasswordHash)
	}
	if cfg.Sections != nil {
		t.Errorf("zero-value Sections = %v, want nil", cfg.Sections)
	}
	if cfg.Interludes != nil {
		t.Errorf("zero-value Interludes = %v, want nil", cfg.Interludes)
	}
	if cfg.PhotoPaths != nil {
		t.Errorf("zero-value PhotoPaths = %v, want nil", cfg.PhotoPaths)
	}
}
