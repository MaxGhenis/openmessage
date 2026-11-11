package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
)

// Every libgm client OpenMessage builds waits RequestTimeout for the phone's
// answer, not libgm's longer default.
func TestClientsWaitRequestTimeoutForThePhone(t *testing.T) {
	fromSession, err := NewFromSession(&SessionData{AuthDataJSON: []byte(`{}`)}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	for name, cli := range map[string]*Client{"session": fromSession, "pairing": NewForPairing(zerolog.Nop())} {
		if got := cli.GM.RequestTimeout(); got != RequestTimeout {
			t.Errorf("%s client waits %s, want RequestTimeout (%s)", name, got, RequestTimeout)
		}
	}
	if RequestTimeout <= 0 || RequestTimeout >= libgm.DefaultRequestTimeout {
		t.Errorf("RequestTimeout = %s, want a positive bound below libgm's default %s", RequestTimeout, libgm.DefaultRequestTimeout)
	}
}

// No code builds a libgm client except newGMClient, so none can skip the bound.
func TestLibgmClientsAreBuiltOnlyByNewGMClient(t *testing.T) {
	root := filepath.Join("..", "..")
	var found []string
	check := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i := strings.Count(string(data), "libgm.NewClient("); i > 0; i-- {
			found = append(found, filepath.ToSlash(path))
		}
	}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				check(path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	top, err := filepath.Glob(filepath.Join(root, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range top {
		if !strings.HasSuffix(path, "_test.go") {
			check(path)
		}
	}
	if want := filepath.ToSlash(filepath.Join(root, "internal", "client", "client.go")); len(found) != 1 || found[0] != want {
		t.Fatalf("libgm.NewClient is called from %v; only newGMClient (%s) may build a libgm client", found, want)
	}
}
