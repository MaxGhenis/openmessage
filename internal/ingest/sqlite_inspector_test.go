package ingest

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// inspectorBusyTimeoutMS matches the busy timeout sqlite.Open gives the
// store's own connections.
const inspectorBusyTimeoutMS = 5000

// openSQLiteInspector opens a second pool on a store that a test may still
// be running a worker against, for reads the test checks. Its connections
// wait out a lock as the store's own connections do.
//
// Without a busy timeout, a read can fail at once with "database is locked
// (261)", SQLITE_BUSY_RECOVERY. When the last connection to a store closes,
// SQLite checkpoints the WAL and deletes it and the -shm file. sqlite.Open
// returns with no connection open, so the next connection, often the
// worker's first, rebuilds the WAL index and holds the recovery lock while
// it does. SQLite retries a busy read only for a connection with a busy
// handler, so a reader opened without busy_timeout fails instead of waiting.
func openSQLiteInspector(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", inspectorDSN(path))
	if err != nil {
		t.Fatalf("sql.Open(inspector %s): %v", path, err)
	}
	return database
}

// inspectorDSN builds the DSN the way sqlite.Open's storeDSN does, so a path
// holding URI characters (t.TempDir keeps '#', '%', '&' and spaces from test
// names) still names the file.
func inspectorDSN(path string) string {
	query := make(url.Values)
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", inspectorBusyTimeoutMS))
	return (&url.URL{
		Scheme:   "file",
		Path:     strings.ReplaceAll(path, `\`, "/"),
		RawQuery: query.Encode(),
	}).String()
}

// The inspector must wait on a lock rather than fail, and must open the file
// the path names even when the path holds URI characters.
func TestOpenSQLiteInspectorWaitsOutLocksAndKeepsThePath(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"plain.sqlite3", "with #hash & 100% space.sqlite3"} {
		path := filepath.Join(dir, name)
		database := openSQLiteInspector(t, path)
		if _, err := database.Exec(`CREATE TABLE t (x)`); err != nil {
			t.Fatalf("%s: create: %v", name, err)
		}
		var timeout int
		if err := database.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
			t.Fatalf("%s: read busy_timeout: %v", name, err)
		}
		if timeout != inspectorBusyTimeoutMS {
			t.Fatalf("%s: busy_timeout = %d, want %d", name, timeout, inspectorBusyTimeoutMS)
		}
		var file string
		if err := database.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&file); err != nil {
			t.Fatalf("%s: read database file: %v", name, err)
		}
		// SQLite reports the path with symlinks resolved (/var is
		// /private/var on macOS).
		want, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if file != want {
			t.Fatalf("%s: inspector opened %q, want %q", name, file, want)
		}
		_ = database.Close()
	}
}
