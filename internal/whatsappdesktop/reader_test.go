package whatsappdesktop

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/importer"
	"github.com/maxghenis/openmessage/internal/whatsappmedia"
)

func desktopFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	execFixture(t, db, `
		CREATE TABLE ZWACHATSESSION (
			Z_PK INTEGER PRIMARY KEY, ZCONTACTJID TEXT, ZCONTACTIDENTIFIER TEXT,
			ZPARTNERNAME TEXT, ZLASTMESSAGEDATE REAL, ZREMOVED INTEGER);
		CREATE TABLE ZWAGROUPMEMBER (
			Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZMEMBERJID TEXT,
			ZCONTACTIDENTIFIER TEXT, ZCONTACTNAME TEXT);
		CREATE TABLE ZWAMEDIAITEM (
			Z_PK INTEGER PRIMARY KEY, ZTITLE TEXT, ZMEDIALOCALPATH TEXT,
			ZMEDIAORIGIN INTEGER, ZFILESIZE INTEGER, ZMETADATA BLOB, ZVCARDSTRING TEXT);
		CREATE TABLE ZWAMESSAGE (
			Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZSTANZAID TEXT,
			ZMESSAGETYPE INTEGER, ZTEXT TEXT, ZMESSAGEDATE REAL,
			ZISFROMME INTEGER, ZGROUPMEMBER INTEGER, ZFROMJID TEXT,
			ZPUSHNAME TEXT, ZMEDIAITEM INTEGER);
	`)
	return path, db
}

func execFixture(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	if _, err := db.Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}

func fixtureChat(t *testing.T, db *sql.DB, pk int, jid, identifier string) {
	t.Helper()
	execFixture(t, db, `INSERT INTO ZWACHATSESSION VALUES (?,?,?,?,1000,0)`, pk, jid, identifier, fmt.Sprintf("Synthetic chat %d", pk))
}

func fixtureMessage(t *testing.T, db *sql.DB, pk, chatPK, kind, fromMe, memberPK, mediaPK int, date float64, body string) {
	t.Helper()
	execFixture(t, db, `INSERT INTO ZWAMESSAGE VALUES (?,?,?,?,?,?,?,?,?,'',?)`, pk, chatPK,
		fmt.Sprintf("SYNTHETIC-%d", pk), kind, body, date, fromMe, memberPK, "15550009999@s.whatsapp.net", mediaPK)
}

func capture(t *testing.T, reader *Reader, options ScanOptions) ([]Chunk, Stats) {
	t.Helper()
	var chunks []Chunk
	stats, err := reader.Scan(context.Background(), options, func(chunk Chunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return chunks, stats
}

func TestI3NoLIDConversationAndCaptureCanonicalization(t *testing.T) {
	path, db := desktopFixture(t)
	fixtureChat(t, db, 1, "111@lid", "15550000001@s.whatsapp.net")
	fixtureChat(t, db, 2, "222@lid", "")
	fixtureChat(t, db, 3, "333@lid", "")
	fixtureChat(t, db, 4, "444@lid", "")
	fixtureChat(t, db, 5, "555@lid", "")
	fixtureChat(t, db, 6, "15550000006@s.whatsapp.net", "")
	fixtureChat(t, db, 7, "15550000000-1700000000@g.us", "")
	fixtureChat(t, db, 8, "100@lid.status", "status")
	fixtureChat(t, db, 9, "123@broadcast", "")
	fixtureChat(t, db, 10, "0@s.whatsapp.net", "")
	fixtureChat(t, db, 11, "123@newsletter", "")
	fixtureChat(t, db, 12, "13135550002@s.whatsapp.net", "")
	fixtureChat(t, db, 13, "123@bot", "")
	execFixture(t, db, `INSERT INTO ZWAGROUPMEMBER VALUES
		(1,7,'222@lid','15550000002@s.whatsapp.net','Synthetic member'),
		(2,7,'666@lid',NULL,'Unmapped member'),
		(3,7,'333@lid',NULL,'Mapped by Desktop'),
		(4,7,'444@lid',NULL,'Mapped by session'),
		(5,7,'15550009999@s.whatsapp.net',NULL,'Synthetic self')`)
	for pk := 1; pk <= 13; pk++ {
		fixtureMessage(t, db, pk, pk, 0, 0, 0, 0, 1000, "synthetic body")
	}
	fixtureMessage(t, db, 14, 7, 0, 0, 1, 0, 1001, "group mapped")
	fixtureMessage(t, db, 15, 7, 0, 0, 2, 0, 1002, "group unmapped")
	fixtureMessage(t, db, 16, 7, 0, 1, 0, 0, 1003, "outgoing")
	lidPath := filepath.Join(filepath.Dir(path), "LID.sqlite")
	lidDB, err := sql.Open("sqlite", lidPath)
	if err != nil {
		t.Fatal(err)
	}
	execFixture(t, lidDB, `CREATE TABLE ZWAPHONENUMBERLIDPAIR(ZLID TEXT,ZPHONENUMBER TEXT);
		INSERT INTO ZWAPHONENUMBERLIDPAIR VALUES ('333','15550000003');`)
	lidDB.Close()
	sessionPath := filepath.Join(filepath.Dir(path), "synthetic-session.db")
	sessionDB, err := sql.Open("sqlite", sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	execFixture(t, sessionDB, `CREATE TABLE whatsmeow_lid_map(lid TEXT PRIMARY KEY,pn TEXT UNIQUE NOT NULL);
		INSERT INTO whatsmeow_lid_map VALUES ('444@lid','15550000004@s.whatsapp.net');`)
	sessionDB.Close()
	chunks, stats := capture(t, New(Options{DBPath: path, SessionDBPath: sessionPath, SelfJID: "15550009999@s.whatsapp.net"}), ScanOptions{})
	if stats.ChatsResolved != 6 || stats.UnresolvedLIDChats != 1 || stats.HighWaterPK != 16 {
		t.Fatalf("unexpected counts: %+v", stats)
	}
	wantReasons := map[string]int64{"unresolved_lid": 1, "status": 1, "broadcast": 1, "system_chat": 1, "newsletter": 1, "bot": 2}
	if !reflect.DeepEqual(stats.ChatsSkippedByReason, wantReasons) {
		t.Fatalf("skip reasons = %v, want %v", stats.ChatsSkippedByReason, wantReasons)
	}
	var sawMapped, sawUnmapped, sawSelf bool
	for _, chunk := range chunks {
		if strings.HasSuffix(chunk.Chat.JID, "@lid") {
			t.Fatalf("LID conversation escaped capture: %s", chunk.Chat.JID)
		}
		for _, participant := range chunk.Chat.Participants {
			if participant.JID == "15550009999@s.whatsapp.net" || participant.Name != "" {
				t.Fatalf("self/name leaked into roster: %+v", participant)
			}
		}
		for _, message := range chunk.Messages {
			switch message.PK {
			case 14:
				sawMapped = message.SenderJID == "15550000002@s.whatsapp.net"
			case 15:
				sawUnmapped = message.SenderJID == "666@lid"
			case 16:
				sawSelf = message.SenderJID == "15550009999@s.whatsapp.net" && message.IsFromMe
			default:
				if !chunk.Chat.IsGroup && message.SenderJID != chunk.Chat.JID {
					t.Fatalf("direct sender differs from canonical peer: %+v", message)
				}
			}
		}
	}
	if !sawMapped || !sawUnmapped || !sawSelf {
		t.Fatalf("sender coverage mapped=%v unmapped=%v self=%v", sawMapped, sawUnmapped, sawSelf)
	}
}

func TestI4RowIdentityTimestampDirectionAndTypeAllowlist(t *testing.T) {
	path, db := desktopFixture(t)
	fixtureChat(t, db, 1, "101@lid", "15550000001@s.whatsapp.net")
	allowed := []int{0, 7, 1, 2, 3, 8, 11, 15}
	execFixture(t, db, `INSERT INTO ZWAMEDIAITEM (Z_PK,ZMEDIAORIGIN,ZVCARDSTRING) VALUES (1,0,'audio/mp4')`)
	for index, kind := range allowed {
		mediaPK := 0
		if kind == 3 {
			mediaPK = 1
		}
		fixtureMessage(t, db, index+1, 1, kind, index%2, 0, mediaPK, 1000+float64(index)+0.6, "synthetic text")
	}
	for index, kind := range []int{6, 10, 14, 41, 43, 59, 60, 66, 75, 999} {
		fixtureMessage(t, db, index+9, 1, kind, 0, 0, 0, 1000, "must not render as a body")
	}
	fixtureMessage(t, db, 19, 1, 0, 0, 0, 0, 1000, "empty ID")
	execFixture(t, db, `UPDATE ZWAMESSAGE SET ZSTANZAID='' WHERE Z_PK=19`)
	fixtureMessage(t, db, 20, 1, 0, 0, 0, 0, 1000, "invalid date")
	execFixture(t, db, `UPDATE ZWAMESSAGE SET ZMESSAGEDATE=NULL WHERE Z_PK=20`)
	chunks, stats := capture(t, New(Options{DBPath: path}), ScanOptions{SinceMS: int64(coreDataEpoch+1001) * 1000})
	if len(chunks) != 1 || len(chunks[0].Messages) != 8 {
		t.Fatalf("chunks=%d, want one with eight allowed types", len(chunks))
	}
	for index, message := range chunks[0].Messages {
		wantTimestamp := int64(math.Round(1000+float64(index)+0.6+coreDataEpoch)) * 1000
		if message.StanzaID != fmt.Sprintf("SYNTHETIC-%d", index+1) || message.TimestampMS != wantTimestamp ||
			message.IsFromMe != (index%2 == 1) || chunks[0].Chat.JID != "15550000001@s.whatsapp.net" {
			t.Fatalf("message identity/timestamp/direction mismatch: %+v", message)
		}
		if message.IsFromMe && message.SenderJID != "" {
			t.Fatalf("reader invented a self identity: %+v", message)
		}
	}
	if stats.RowsSkippedByReason["unsupported_type"] != 10 || stats.RowsSkippedByReason["empty_stanza_id"] != 1 ||
		stats.RowsSkippedByReason["invalid_timestamp"] != 1 || stats.RowsSinceMS != 8 {
		t.Fatalf("incorrect row counters: %+v", stats)
	}
	for _, kind := range allowed {
		if stats.RowsIncludedByType[kind] != 1 || stats.RowsSinceIncludedByType[kind] != 1 {
			t.Fatalf("allowlist type %d absent from included counts", kind)
		}
	}
}

func TestDesktopMediaOnlyAttachesExistingFileInsideRoot(t *testing.T) {
	path, db := desktopFixture(t)
	fixtureChat(t, db, 1, "15550000001@s.whatsapp.net", "")
	root := filepath.Join(filepath.Dir(path), "Message")
	if err := os.MkdirAll(filepath.Join(root, "Media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Media", "photo.jpg"), []byte("synthetic image"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(path), "outside.jpg")
	if err := os.WriteFile(outside, []byte("synthetic outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "Media", "escape.jpg")); err != nil {
		t.Fatal(err)
	}
	paths := []string{"Media/photo.jpg", "Media/missing.jpg", "../outside.jpg", "Media/escape.jpg", "Media", ""}
	for index, mediaPath := range paths {
		execFixture(t, db, `INSERT INTO ZWAMEDIAITEM VALUES (?, 'synthetic caption', ?, 0, 0, NULL, NULL)`, index+1, mediaPath)
		fixtureMessage(t, db, index+1, 1, 1, 0, 0, index+1, 1000, "")
	}
	chunks, _ := capture(t, New(Options{DBPath: path}), ScanOptions{})
	if len(chunks) != 1 || len(chunks[0].Messages) != len(paths) {
		t.Fatal("missing media rows must still import")
	}
	for index, message := range chunks[0].Messages {
		if message.Caption != "synthetic caption" {
			t.Fatalf("caption not preserved for media %d", index)
		}
		if index == 0 {
			if message.LocalMediaRef == "" || message.MIMEType != "image/jpeg" || message.Size == 0 {
				t.Fatalf("valid file not captured: %+v", message)
			}
			if relative, err := whatsappmedia.DecodeLocalMediaRef(message.LocalMediaRef); err != nil || relative != paths[index] {
				t.Fatalf("local ref = %q, %v", relative, err)
			}
		} else if message.LocalMediaRef != "" {
			t.Fatalf("unsafe/missing media attached for %q", paths[index])
		}
	}
}

func TestDesktopSnapshotDeterministicBoundedAndIncremental(t *testing.T) {
	path, db := desktopFixture(t)
	fixtureChat(t, db, 1, "15550000001@s.whatsapp.net", "")
	for pk := 1; pk <= 125; pk++ {
		fixtureMessage(t, db, pk, 1, 0, pk%2, 0, 0, float64(pk), "synthetic")
	}
	reader := New(Options{DBPath: path})
	first, stats := capture(t, reader, ScanOptions{ChunkSize: 1000})
	second, replayStats := capture(t, reader, ScanOptions{})
	if len(first) != 2 || len(first[0].Messages) != 100 || len(first[1].Messages) != 25 ||
		!reflect.DeepEqual(first, second) || !reflect.DeepEqual(stats, replayStats) {
		t.Fatal("snapshot must replay deterministically in chunks bounded at 100")
	}
	empty, incrementalStats := capture(t, reader, ScanOptions{AfterPK: stats.HighWaterPK})
	if len(empty) != 0 || incrementalStats.HighWaterPK != 125 {
		t.Fatal("incremental scan replayed old rows")
	}
	fixtureMessage(t, db, 126, 1, 14, 0, 0, 0, 126, "revocation ID")
	fixtureMessage(t, db, 127, 1, 0, 1, 0, 0, 127, "new synthetic text")
	newChunks, nextStats := capture(t, reader, ScanOptions{AfterPK: stats.HighWaterPK})
	if len(newChunks) != 1 || len(newChunks[0].Messages) != 1 || newChunks[0].Messages[0].PK != 127 ||
		nextStats.HighWaterPK != 127 || nextStats.RowsSkippedByReason["unsupported_type"] != 1 {
		t.Fatalf("incremental scan did not include only new eligible row: %+v", nextStats)
	}
}

func TestDesktopReaderSeesWALAndCannotWriteOrCreateDatabase(t *testing.T) {
	path, writer := desktopFixture(t)
	execFixture(t, writer, `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;`)
	fixtureChat(t, writer, 1, "15550000001@s.whatsapp.net", "")
	fixtureMessage(t, writer, 1, 1, 0, 0, 0, 0, 1000, "uncheckpointed synthetic text")
	chunks, _ := capture(t, New(Options{DBPath: path}), ScanOptions{})
	if len(chunks) != 1 || len(chunks[0].Messages) != 1 {
		t.Fatal("read-only reader missed WAL message")
	}
	readerDB, err := openReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer readerDB.Close()
	if _, err := readerDB.Exec(`UPDATE ZWAMESSAGE SET ZTEXT='forbidden'`); err == nil {
		t.Fatal("reader connection allowed a write")
	}
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	if _, err := New(Options{DBPath: missing}).Scan(context.Background(), ScanOptions{}, nil); err == nil {
		t.Fatal("missing database silently created")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("read-only open created missing path: %v", err)
	}
}

func TestDesktopReaderAppendFailureAndCancellation(t *testing.T) {
	path, db := desktopFixture(t)
	fixtureChat(t, db, 1, "15550000001@s.whatsapp.net", "")
	fixtureMessage(t, db, 1, 1, 0, 0, 0, 0, 1000, "synthetic")
	want := errors.New("synthetic append failure")
	if _, err := New(Options{DBPath: path}).Scan(context.Background(), ScanOptions{}, func(Chunk) error { return want }); !errors.Is(err, want) {
		t.Fatalf("append failure not propagated: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(Options{DBPath: path}).Scan(ctx, ScanOptions{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan = %v", err)
	}
}

func TestDesktopInvalidChatsMissingSenderAndMapFailureAreCounted(t *testing.T) {
	path, db := desktopFixture(t)
	fixtureChat(t, db, 1, "", "")
	fixtureChat(t, db, 2, "15550000001@unknown", "")
	fixtureChat(t, db, 3, "15550000003@s.whatsapp.net", "")
	execFixture(t, db, `UPDATE ZWACHATSESSION SET ZREMOVED=1 WHERE Z_PK=3`)
	fixtureChat(t, db, 4, "15550000000-1@g.us", "")
	fixtureChat(t, db, 5, "101@lid", "")
	for pk := 1; pk <= 6; pk++ {
		fixtureMessage(t, db, pk, pk, 0, 0, 0, 0, 1000, "synthetic")
	}
	execFixture(t, db, `UPDATE ZWAMESSAGE SET ZFROMJID='' WHERE Z_PK=4`)
	badMap := filepath.Join(filepath.Dir(path), "synthetic-bad-map.db")
	if err := os.WriteFile(badMap, []byte("synthetic non-database"), 0o644); err != nil {
		t.Fatal(err)
	}
	chunks, stats := capture(t, New(Options{DBPath: path, SessionDBPath: badMap}), ScanOptions{})
	if len(chunks) != 0 || stats.HighWaterPK != 6 || stats.MapErrorsBySource["session_lid"] != 1 {
		t.Fatalf("invalid optional map should preserve safe scanning: %+v", stats)
	}
	for _, reason := range []string{"chat_invalid_jid", "chat_unsupported_jid", "chat_removed", "missing_group_sender", "chat_unresolved_lid", "chat_missing"} {
		if stats.RowsSkippedByReason[reason] != 1 {
			t.Fatalf("skip reason %s count=%d, want 1", reason, stats.RowsSkippedByReason[reason])
		}
	}
}

func TestI3I4SeededChatAndRowCategories(t *testing.T) {
	path, db := desktopFixture(t)
	rng := rand.New(rand.NewSource(20261011))
	counts := [8]int{}
	expectedRows := 0
	for index := 0; index < 160; index++ {
		// Use high random bits: low-bit generators can silently miss entire
		// categories. Explicit coverage assertions below defend that property.
		state := rng.Uint64()
		category := int((state >> 33) % 8)
		counts[category]++
		pk := index + 1
		pn := fmt.Sprintf("1555%07d@s.whatsapp.net", pk)
		jid, identifier := pn, ""
		switch category {
		case 1:
			jid, identifier = fmt.Sprintf("%d@lid", pk), pn
		case 2:
			jid = fmt.Sprintf("%d@lid", pk)
		case 3:
			jid = fmt.Sprintf("15550000000-%d@g.us", pk)
		case 4:
			jid = fmt.Sprintf("%d@lid.status", pk)
		case 5:
			jid = "13135550002@s.whatsapp.net"
		case 6:
			jid = fmt.Sprintf("%d@broadcast", pk)
		case 7:
			jid = fmt.Sprintf("%d@newsletter", pk)
		}
		fixtureChat(t, db, pk, jid, identifier)
		if category == 3 {
			execFixture(t, db, `INSERT INTO ZWAGROUPMEMBER VALUES (?,?,?,NULL,'Synthetic peer')`, pk, pk, pn)
		}
		fromMe := int((state >> 48) & 1)
		fixtureMessage(t, db, pk, pk, 0, fromMe, 0, 0, float64(pk)+0.4, "synthetic category")
		if category == 0 || category == 1 || category == 3 {
			expectedRows++
		}
	}
	for category, count := range counts {
		if count == 0 {
			t.Fatalf("category %d was never generated", category)
		}
	}
	chunks, stats := capture(t, New(Options{DBPath: path, SelfJID: "15550009999@s.whatsapp.net"}), ScanOptions{})
	actualRows := 0
	for _, chunk := range chunks {
		if strings.HasSuffix(chunk.Chat.JID, "@lid") || (!strings.HasSuffix(chunk.Chat.JID, "@g.us") && !strings.HasSuffix(chunk.Chat.JID, "@s.whatsapp.net")) {
			t.Fatalf("invalid canonical chat escaped: %s", chunk.Chat.JID)
		}
		for _, message := range chunk.Messages {
			actualRows++
			if message.TimestampMS != (coreDataEpoch+message.PK)*1000 || message.StanzaID != fmt.Sprintf("SYNTHETIC-%d", message.PK) {
				t.Fatalf("row identity mismatch: %+v", message)
			}
		}
	}
	if actualRows != expectedRows || stats.UnresolvedLIDChats != int64(counts[2]) {
		t.Fatalf("category projection rows=%d want=%d unresolved=%d want=%d", actualRows, expectedRows, stats.UnresolvedLIDChats, counts[2])
	}
}

func TestI5AmbiguousRostersSkipAllThreadsAndSameJIDMerges(t *testing.T) {
	path, db := desktopFixture(t)
	fixtureChat(t, db, 1, "15550000000-1@g.us", "")
	fixtureChat(t, db, 2, "15550000000-2@g.us", "")
	fixtureChat(t, db, 3, "15550000000-3@g.us", "")
	fixtureChat(t, db, 4, "15550000000-3@g.us", "")
	for pk := 1; pk <= 4; pk++ {
		peer := "15550000001@s.whatsapp.net"
		if pk >= 3 {
			peer = "15550000002@s.whatsapp.net"
		}
		execFixture(t, db, `INSERT INTO ZWAGROUPMEMBER VALUES (?,?,?,NULL,'Synthetic peer')`, pk, pk, peer)
		fixtureMessage(t, db, pk, pk, 0, 0, pk, 0, 1000+float64(pk), "synthetic")
	}
	chunks, stats := capture(t, New(Options{DBPath: path}), ScanOptions{ChunkSize: 1})
	if stats.ChatsSkippedByReason["ambiguous_roster"] != 2 || stats.RowsSkippedByReason["ambiguous_roster"] != 2 ||
		stats.ChatsResolved != 2 || len(chunks) != 2 {
		t.Fatalf("identical distinct threads not conservatively skipped: %+v", stats)
	}
	for _, chunk := range chunks {
		if chunk.Chat.JID != "15550000000-3@g.us" ||
			chunk.Chat.Title != "Synthetic chat 3" || len(chunk.Chat.Participants) != 1 {
			t.Fatalf("same-JID merged snapshot changed by raw session: %+v", chunk.Chat)
		}
	}
}

func TestI5EmptyGroupRostersAreNotAmbiguityEvidence(t *testing.T) {
	path, db := desktopFixture(t)
	for pk := 1; pk <= 2; pk++ {
		fixtureChat(t, db, pk, fmt.Sprintf("15550000000-%d@g.us", pk), "")
		fixtureMessage(t, db, pk, pk, 0, 0, 0, 0, 1000, "synthetic group text")
	}
	chunks, stats := capture(t, New(Options{DBPath: path}), ScanOptions{})
	if len(chunks) != 2 || stats.ChatsResolved != 2 || stats.ChatsSkippedByReason["ambiguous_roster"] != 0 {
		t.Fatalf("empty rosters were treated as equal evidence: %+v", stats)
	}
}

func TestDesktopInvalidTimestampCannotPoisonFrame(t *testing.T) {
	path, db := desktopFixture(t)
	fixtureChat(t, db, 1, "15550000001@s.whatsapp.net", "")
	for index, date := range []float64{1000, -coreDataEpoch, -coreDataEpoch - 100, 1e16, 1e300, math.Inf(1)} {
		fixtureMessage(t, db, index+1, 1, 0, 0, 0, 0, date, "synthetic timestamp row")
	}
	chunks, stats := capture(t, New(Options{DBPath: path}), ScanOptions{})
	if len(chunks) != 1 || len(chunks[0].Messages) != 1 || chunks[0].Messages[0].PK != 1 ||
		chunks[0].Messages[0].TimestampMS != (coreDataEpoch+1000)*1000 || stats.RowsSkippedByReason["invalid_timestamp"] != 5 {
		t.Fatalf("bad timestamp hid valid history: %+v", stats)
	}
}

func TestDesktopSelfChatCarriesSelfParticipant(t *testing.T) {
	path, db := desktopFixture(t)
	selfJID := "15550009999@s.whatsapp.net"
	fixtureChat(t, db, 1, selfJID, "")
	fixtureMessage(t, db, 1, 1, 0, 1, 0, 0, 1000, "synthetic self note")
	chunks, _ := capture(t, New(Options{DBPath: path, SelfJID: selfJID}), ScanOptions{})
	if len(chunks) != 1 || len(chunks[0].Chat.Participants) != 1 ||
		chunks[0].Chat.Participants[0].JID != selfJID || chunks[0].Messages[0].SenderJID != selfJID || !chunks[0].Messages[0].IsFromMe {
		t.Fatal("self-chat needs a participant for the feeder to mark IsSelf")
	}
}

func TestDesktopAudioVoiceNoteOriginInferenceAndUnknownSkip(t *testing.T) {
	path, db := desktopFixture(t)
	fixtureChat(t, db, 1, "15550000001@s.whatsapp.net", "")
	for index, origin := range []int{0, 1, 99} {
		mimeType := "audio/mp4"
		if origin == 1 {
			mimeType = "audio/ogg"
		}
		execFixture(t, db, `INSERT INTO ZWAMEDIAITEM (Z_PK,ZMEDIAORIGIN,ZVCARDSTRING) VALUES (?,?,?)`, index+1, origin, mimeType)
		fixtureMessage(t, db, index+1, 1, 3, 0, 0, index+1, 1000+float64(index), "")
	}
	fixtureMessage(t, db, 4, 1, 3, 0, 0, 0, 2000, "")
	chunks, stats := capture(t, New(Options{DBPath: path}), ScanOptions{})
	if len(chunks) != 1 || len(chunks[0].Messages) != 2 || chunks[0].Messages[0].VoiceNote ||
		!chunks[0].Messages[1].VoiceNote || chunks[0].Messages[0].MIMEType != "audio/mp4" ||
		chunks[0].Messages[1].MIMEType != "audio/ogg" || stats.RowsSkippedByReason["audio_origin_unknown"] != 2 {
		t.Fatalf("audio origin capture did not keep faithful cases: %+v", stats)
	}
}

func TestDesktopReaderDifferentialLegacyTextRows(t *testing.T) {
	path, fixture := desktopFixture(t)
	fixtureChat(t, fixture, 1, "101@lid", "15550000001@s.whatsapp.net")
	fixtureChat(t, fixture, 2, "15550000002@s.whatsapp.net", "")
	fixtureChat(t, fixture, 3, "15550000000-1700000000@g.us", "")
	execFixture(t, fixture, `INSERT INTO ZWAGROUPMEMBER VALUES (1,3,'202@lid','15550000002@s.whatsapp.net','Synthetic member')`)
	for pk := 1; pk <= 6; pk++ {
		kind := 0
		if pk == 2 {
			kind = 7
		}
		fixtureMessage(t, fixture, pk, 1+(pk-1)/2, kind, pk%2, 1, 0, 1000+float64(pk), fmt.Sprintf("synthetic text %d", pk))
	}
	// Empty media/system text is ignored by legacy; the differential concerns
	// every text row it imports, including an ordinary text link.
	fixtureMessage(t, fixture, 7, 1, 1, 0, 0, 0, 1007, "")
	fixtureMessage(t, fixture, 8, 1, 6, 0, 0, 0, 1008, "")
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	result, err := (&importer.WhatsAppNative{DBPath: path, SinceMS: -1}).ImportFromDB(store)
	if err != nil || len(result.Errors) != 0 || result.MessagesImported != 6 {
		t.Fatalf("legacy fixture import: %+v, %v", result, err)
	}
	chunks, _ := capture(t, New(Options{DBPath: path}), ScanOptions{})
	newText := map[string]struct {
		chat string
		msg  Message
	}{}
	for _, chunk := range chunks {
		for _, message := range chunk.Messages {
			if message.Type == 0 || message.Type == 7 {
				newText[message.StanzaID] = struct {
					chat string
					msg  Message
				}{chunk.Chat.JID, message}
			}
		}
	}
	conversations, err := store.ListConversationsByPlatform("whatsapp", 20)
	if err != nil {
		t.Fatal(err)
	}
	compared := 0
	for _, conversation := range conversations {
		legacyJID := strings.TrimPrefix(conversation.ConversationID, "whatsapp:")
		canonical, reason := canonicalChat(legacyJID, "", map[string]string{"101@lid": "15550000001@s.whatsapp.net"})
		if reason != "" {
			t.Fatalf("legacy fixture chat canonicalization: %s", reason)
		}
		messages, err := store.GetMessagesByConversation(conversation.ConversationID, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, legacy := range messages {
			captured, exists := newText[legacy.SourceID]
			if !exists || captured.chat != canonical || captured.msg.StanzaID != legacy.SourceID ||
				captured.msg.TimestampMS != legacy.TimestampMS || captured.msg.IsFromMe != legacy.IsFromMe ||
				captured.msg.Text != legacy.Body {
				t.Fatalf("synthetic legacy/new row disagreed for stanza %s", legacy.SourceID)
			}
			compared++
		}
	}
	if compared != 6 || len(newText) != compared {
		t.Fatalf("compared=%d new text=%d, want all six", compared, len(newText))
	}
}
