//go:build !race

package app

// raceDetector reports whether the tests run under -race (race_on_test.go).
const raceDetector = false
