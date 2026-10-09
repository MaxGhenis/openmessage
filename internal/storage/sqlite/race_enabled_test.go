//go:build race

package sqlite

// raceDetectorEnabled reports a -race build, where SQLite runs several times
// slower; see substringSearchQuickConfig.
const raceDetectorEnabled = true
