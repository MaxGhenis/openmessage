package signaldesktop

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/signaldesktop/fixture"
)

// nodeRunnerForFixture runs the production helper with a fake SQLCipher addon
// (testdata) over a plain fixture database. It skips where Node.js or its
// built-in node:sqlite is unavailable.
func nodeRunnerForFixture(t *testing.T, archive fixture.Archive) (*NodeRunner, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is not installed")
	}
	if err := exec.Command(node, "-e", "require('node:sqlite')").Run(); err != nil {
		t.Skip("this Node.js has no node:sqlite")
	}
	addon, err := filepath.Abs(filepath.Join("testdata", "fake_sqlcipher_addon.cjs"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "db.sqlite")
	if err := fixture.Write(path, archive); err != nil {
		t.Fatal(err)
	}
	return &NodeRunner{
		DBPath:    path,
		NodePath:  node,
		AddonPath: addon,
		Key:       func(context.Context) (string, error) { return "00ff00ff", nil },
	}, path
}

func richArchive() fixture.Archive {
	archive := baseArchive()
	archive.Messages = []fixture.Message{
		{RowID: 1, ID: "m1", ConversationID: "conv-group", Type: "incoming", Body: "synthetic hello", SentAt: tBaseMS + 1, SourceServiceID: tPeerACI,
			Reactions: []fixture.Reaction{{Emoji: "👍", FromID: "conv-other", Timestamp: tBaseMS + 5}}},
		{RowID: 2, ID: "m2", ConversationID: "conv-peer", Type: "outgoing", SentAt: tBaseMS + 2, Attachments: []fixture.Attachment{{ContentType: "image/png"}}},
		{RowID: 3, ID: "m3", ConversationID: "conv-peer", Type: "story", Body: "synthetic", SentAt: tBaseMS + 3},
		{RowID: 4, ID: "m4", ConversationID: "conv-other", Type: "incoming", SentAt: tBaseMS + 4, SourceServiceID: tOtherACI, Sticker: true},
	}
	return archive
}

// Differential: the Node helper and the Go SQL runner run the same queries
// over the same archive, and the reader turns both into identical chunks.
func TestNodeHelperAndSQLRunnerAgree(t *testing.T) {
	archive := richArchive()
	node, path := nodeRunnerForFixture(t, archive)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	viaNode, nodeStats := scanAll(t, node, emptyFacts(), ScanOptions{})
	viaSQL, sqlStats := scanAll(t, openArchive(t, archive), emptyFacts(), ScanOptions{})
	if !reflect.DeepEqual(viaNode, viaSQL) || !reflect.DeepEqual(nodeStats, sqlStats) {
		t.Fatalf("node=%+v %+v\nsql=%+v %+v", viaNode, nodeStats, viaSQL, sqlStats)
	}
	if len(viaNode) != 3 || nodeStats.RowsIncluded != 3 || nodeStats.AttachmentSource != "typed" {
		t.Fatalf("chunks=%d stats=%+v", len(viaNode), nodeStats)
	}
	tail, tailStats := scanAll(t, node, emptyFacts(), ScanOptions{AfterRowID: 2})
	if len(tail) != 1 || tailStats.RowsScanned != 2 || tailStats.HighWaterRowID != 4 {
		t.Fatalf("incremental via node: chunks=%+v stats=%+v", tail, tailStats)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the helper changed the database file")
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		if info, err := os.Stat(path + suffix); err == nil && info.Size() > 0 {
			t.Fatalf("the helper left a %s file with data", suffix)
		}
	}
}

func TestNodeHelperRefusesWritesAndReportsOptionalFailures(t *testing.T) {
	node, path := nodeRunnerForFixture(t, richArchive())
	ctx := context.Background()
	for _, query := range []Query{
		{Name: "write", SQL: "DELETE FROM messages"},
		{Name: "two statements", SQL: "SELECT 1; DELETE FROM messages"},
		{Name: "pragma", SQL: "PRAGMA journal_mode = DELETE"},
		{Name: "cte write", SQL: "WITH x AS (SELECT 1) DELETE FROM messages"},
	} {
		if _, err := node.Run(ctx, []Query{query}); err == nil {
			t.Fatalf("helper ran %q", query.Name)
		}
	}
	results, err := node.Run(ctx, []Query{
		{Name: "missing", SQL: "SELECT nope FROM messages", Optional: true},
		{Name: "count", SQL: "SELECT COUNT(*) AS n FROM messages WHERE rowid > ?", Params: []any{1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results["missing"].Error == "" || len(results["count"].Rows) != 1 || results["count"].Rows[0].int("n") != 3 {
		t.Fatalf("results = %+v", results)
	}
	if _, err := node.Run(ctx, []Query{{Name: "missing", SQL: "SELECT nope FROM messages"}}); err == nil {
		t.Fatal("a required query's failure did not fail the read")
	}
	db, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil || count != 4 {
		t.Fatalf("messages after refused writes = %d (%v), want 4", count, err)
	}
	bad := *node
	bad.Key = func(context.Context) (string, error) { return "not hex'; DROP TABLE messages; --", nil }
	if _, err := bad.Run(ctx, Queries(0)); err == nil || strings.Contains(err.Error(), "DROP") {
		t.Fatalf("a non-hex key was not refused cleanly: %v", err)
	}
}
