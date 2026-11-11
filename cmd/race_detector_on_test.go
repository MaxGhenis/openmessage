//go:build race

package cmd

// raceDetectorEnabled lets the store compaction tests build smaller stores
// under -race: the race detector slows the pure-Go SQLite engine about
// tenfold, and those tests check data, not concurrency. The plain test job
// builds them at full size.
const raceDetectorEnabled = true
