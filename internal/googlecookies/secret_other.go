//go:build !darwin

package googlecookies

import (
	"context"
	"fmt"
	"path/filepath"
)

// NativeSupported is darwin-only for now; Linux installs configure
// OPENMESSAGE_COOKIE_REFRESH_SCRIPT (see scripts/) instead.
func NativeSupported() bool {
	return false
}

func defaultChromeProfileDir(home string) string {
	return filepath.Join(home, ".config", "google-chrome", "Default")
}

func chromeSafeStorageSecret(ctx context.Context) ([]byte, error) {
	return nil, fmt.Errorf("native Chrome cookie refresh is not supported on this platform")
}
