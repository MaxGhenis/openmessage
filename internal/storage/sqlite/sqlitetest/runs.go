package sqlitetest

import "testing"

// PropertyRuns scales a property test's case count: n normally, a fifth of n
// (at least 4) under -short or the race detector, which slows these SQLite-heavy
// properties roughly fivefold.
func PropertyRuns(n int) int {
	if raceEnabled || testing.Short() {
		return max(n/5, 4)
	}
	return n
}
