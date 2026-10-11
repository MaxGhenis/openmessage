//go:build race

package whatsapplive

// raceDetectorEnabled lets the ingress spool's and the history-sync queue's
// exhaustive and property tests run fewer cases under -race: the race
// detector slows the pure-Go SQLite engine about tenfold, and those tests
// check data, not concurrency. The plain test job runs every case.
const raceDetectorEnabled = true
