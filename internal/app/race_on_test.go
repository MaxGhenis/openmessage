//go:build race

package app

// raceDetector reports whether the tests run under -race. The race detector
// slows the startup-backfill property tests about 25 times, so they run fewer
// cases then; the plain run keeps the full count.
const raceDetector = true
