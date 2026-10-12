//go:build race

package importer

// raceDetectorEnabled: the race detector slows the pure-Go SQLite engine
// about tenfold, so under -race the cost test logs its timings instead of
// asserting the budget and the property test runs fewer seeds. The plain
// test job asserts and runs everything.
const raceDetectorEnabled = true
