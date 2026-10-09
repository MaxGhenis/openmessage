package silencerecovery

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// The model test saves state thousands of times; a full device flush per
	// save would make it take minutes. Renames stay real, so atomic
	// replacement is still exercised.
	syncStateFile = func(*os.File) error { return nil }
	os.Exit(m.Run())
}
