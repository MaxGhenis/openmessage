package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJoinNames(t *testing.T) {
	tests := []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"A [sms]"}, "A [sms]"},
		{[]string{"A [sms]", "B [imessage]"}, "A [sms], B [imessage]"},
		{[]string{"A", "B", "C", "D", "E"}, "A, B, C, +2 more"},
	}
	for _, tt := range tests {
		got := joinNames(tt.names)
		if got != tt.want {
			t.Errorf("joinNames(%v) = %q, want %q", tt.names, got, tt.want)
		}
	}
}

func TestResolveExportWritePathConfinement(t *testing.T) {
	root := t.TempDir()
	t.Setenv(exportDirEnv, root)

	got, err := resolveExportWritePath("stories/out.html")
	if err != nil {
		t.Fatalf("resolve relative output: %v", err)
	}
	wantPrefix := filepath.Join(root, "stories") + string(filepath.Separator)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("resolved path = %q, want under %q", got, wantPrefix)
	}

	if _, err := resolveExportWritePath("../escape.html"); err == nil {
		t.Fatal("expected traversal outside export root to fail")
	}

	outside := filepath.Join(t.TempDir(), "out.html")
	t.Setenv(allowAnyExportPath, "1")
	got, err = resolveExportWritePath(outside)
	if err != nil {
		t.Fatalf("resolve override output: %v", err)
	}
	if got != outside {
		t.Fatalf("override path = %q, want %q", got, outside)
	}
}

func TestResolveExportWritePathRejectsSymlinkParentBeforeCreatingDirs(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	t.Setenv(exportDirEnv, root)

	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := resolveExportWritePath("link/new/out.html"); err == nil {
		t.Fatal("expected symlink parent to be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatalf("outside directory was created despite rejection: %v", err)
	}
}
