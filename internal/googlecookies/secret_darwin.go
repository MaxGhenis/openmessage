//go:build darwin

package googlecookies

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// NativeSupported reports whether this build can refresh cookies without an
// external script: macOS with a readable Chrome profile present. The first
// keychain read may show a one-time "OpenMessage wants to access Chrome Safe
// Storage" prompt; Always Allow persists it.
func NativeSupported() bool {
	profile := DefaultChromeProfile()
	if profile == "" {
		return false
	}
	for _, c := range []string{
		filepath.Join(profile, "Network", "Cookies"),
		filepath.Join(profile, "Cookies"),
	} {
		if _, err := os.Stat(c); err == nil {
			return true
		}
	}
	return false
}

func defaultChromeProfileDir(home string) string {
	return filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "Default")
}

// chromeSafeStorageSecret reads Chrome's cookie-encryption password from the
// login keychain. The value itself is never logged.
func chromeSafeStorageSecret(ctx context.Context) ([]byte, error) {
	out, err := exec.CommandContext(ctx,
		"/usr/bin/security", "find-generic-password", "-w", "-s", "Chrome Safe Storage",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("read Chrome Safe Storage from keychain: %w", err)
	}
	secret := bytes.TrimRight(out, "\n")
	if len(secret) == 0 {
		return nil, fmt.Errorf("Chrome Safe Storage key was empty")
	}
	return secret, nil
}
