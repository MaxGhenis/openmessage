//go:build race

package sqlite

// raceDetectorEnabled lets property tests run fewer cases under -race: the
// race detector slows the pure-Go SQLite engine about tenfold, and the
// properties check data, not concurrency. The plain test job runs every case.
const raceDetectorEnabled = true
