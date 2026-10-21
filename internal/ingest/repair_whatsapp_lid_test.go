package ingest

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/whatsapplid"
)

// The WhatsApp LID repair rewrites an e164 identity only when a record that
// keeps the JID's server names its digits as a LID and none names them as a
// phone number. These tests seed v2 stores the way migration left them: a
// LID sender stored as "+<lid digits>".

const (
	lidRepairAccount = "whatsapp-primary"
	lidRepairSMS     = "google-primary"
	lidRepairNowMS   = int64(1790000000000)
)

type lidRepairHarness struct {
	t     *testing.T
	path  string
	store *sqlite.Store
	db    *sql.DB
}

func newLIDRepairHarness(t *testing.T) *lidRepairHarness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.sqlite3")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	t.Cleanup(func() { store.Close() })
	database, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open seeder: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })
	h := &lidRepairHarness{t: t, path: path, store: store, db: database}
	h.exec(`INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES (?, 'whatsmeow', 1, 1)`, lidRepairAccount)
	h.exec(`INSERT INTO accounts (account_id, bridge_key, created_at_ms, updated_at_ms) VALUES (?, 'google_messages', 1, 1)`, lidRepairSMS)
	return h
}

func (h *lidRepairHarness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(query, args...); err != nil {
		h.t.Fatalf("%s: %v", strings.TrimSpace(query), err)
	}
}

func lidRepairIdentityID(account, kind, canonical string) string {
	return v2keys.DeriveID("identity", account, kind+"\x1f"+canonical)
}

// identity seeds an identity under the id live ingest and migration derive.
func (h *lidRepairHarness) identity(account, kind, canonical, name string, self bool) string {
	id := lidRepairIdentityID(account, kind, canonical)
	h.exec(`INSERT INTO identities (identity_id, account_id, kind, canonical_value, raw_value, display_name, is_self, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, 10, 10)`, id, account, kind, canonical, canonical, name, self)
	return id
}

func (h *lidRepairHarness) conversation(remote, kind, title string) string {
	id := v2keys.DeriveID("conversation", lidRepairAccount, remote)
	h.exec(`INSERT INTO conversations (conversation_id, account_id, remote_conversation_id, kind, title, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, 10, 10)`, id, lidRepairAccount, remote, kind, title)
	return id
}

func (h *lidRepairHarness) participant(conversation, identity, name string) {
	h.exec(`INSERT INTO conversation_participants (account_id, conversation_id, identity_id, display_name) VALUES (?, ?, ?, ?)`,
		lidRepairAccount, conversation, identity, name)
}

func (h *lidRepairHarness) message(conversation, remote, sender string, occurred int64) string {
	id := v2keys.DeriveID("message", lidRepairAccount, conversation+"\x1f"+remote)
	direction := "incoming"
	var senderID any = sender
	if sender == "" {
		direction, senderID = "outgoing", nil
	}
	h.exec(`INSERT INTO messages (message_id, conversation_id, account_id, remote_message_id, sender_identity_id, direction, occurred_at_ms, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, 10, 10)`, id, conversation, lidRepairAccount, remote, senderID, direction, occurred)
	return id
}

func (h *lidRepairHarness) reaction(message, conversation, reactor, emoji string, occurred, seq int64) {
	h.exec(`INSERT INTO reactions (message_id, reactor_key, account_id, conversation_id, reactor_identity_id, emoji, occurred_at_ms, source_seq_ms, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 10, 10)`, message, reactor, lidRepairAccount, conversation, reactor, emoji, occurred, seq)
}

func (h *lidRepairHarness) link(identity, person string, primary bool) {
	h.exec(`INSERT OR IGNORE INTO people (person_id, created_at_ms, updated_at_ms) VALUES (?, 10, 10)`, person)
	h.exec(`INSERT INTO person_identities (identity_id, person_id, provenance, confidence, is_primary, linked_at_ms) VALUES (?, ?, 'import', 1.0, ?, 10)`,
		identity, person, primary)
}

func (h *lidRepairHarness) plan(evidence *whatsapplid.Evidence) WhatsAppLIDRepairReport {
	h.t.Helper()
	report, err := PlanWhatsAppLIDRepair(context.Background(), h.store, WhatsAppLIDRepairOptions{AccountID: lidRepairAccount, Evidence: evidence})
	if err != nil {
		h.t.Fatalf("PlanWhatsAppLIDRepair(): %v", err)
	}
	return report
}

func (h *lidRepairHarness) apply(report WhatsAppLIDRepairReport) {
	h.t.Helper()
	if err := ApplyWhatsAppLIDRepair(context.Background(), h.store, report, time.UnixMilli(lidRepairNowMS)); err != nil {
		h.t.Fatalf("ApplyWhatsAppLIDRepair(): %v", err)
	}
}

func (h *lidRepairHarness) scalar(query string, args ...any) string {
	h.t.Helper()
	var value sql.NullString
	if err := h.db.QueryRow(query, args...).Scan(&value); err != nil && err != sql.ErrNoRows {
		h.t.Fatalf("%s: %v", query, err)
	}
	return value.String
}

func lidRepairCandidate(t *testing.T, report WhatsAppLIDRepairReport, canonical string) WhatsAppLIDRepairCandidate {
	t.Helper()
	for _, candidate := range report.Candidates {
		if candidate.CanonicalValue == canonical {
			return candidate
		}
	}
	t.Fatalf("no candidate %s in %+v", canonical, report.Candidates)
	return WhatsAppLIDRepairCandidate{}
}

func TestLIDRepairClassifiesByEvidenceNotDigits(t *testing.T) {
	h := newLIDRepairHarness(t)
	group := h.conversation("whatsapp:120363019999999999@g.us", "group", "Book club")
	// Paired with a number by the session store: becomes that number.
	paired := h.identity(lidRepairAccount, "e164", "+90000000000001", "+90000000000001", false)
	// Named a LID by Desktop only: becomes the LID.
	desktop := h.identity(lidRepairAccount, "e164", "+90000000000002", "Bea", false)
	// Named a LID by the store's own LID chat: becomes the LID.
	lidChat := h.conversation("whatsapp:90000000000003@lid", "direct", "+90000000000003")
	chatPeer := h.identity(lidRepairAccount, "e164", "+90000000000003", "", false)
	// Named a LID by Desktop, but texted from on SMS: refused.
	collision := h.identity(lidRepairAccount, "e164", "+90000000000004", "", false)
	h.identity(lidRepairSMS, "e164", "+90000000000004", "", false)
	// No record names it at all, whatever its length: untouched.
	unknown := h.identity(lidRepairAccount, "e164", "+90000000000005", "", false)
	// A real phone number: untouched.
	phone := h.identity(lidRepairAccount, "e164", "+15557654321", "Cy", false)
	// The account's own number, even if a record names its digits a LID.
	self := h.identity(lidRepairAccount, "e164", "+15550000099", "Me", true)
	for _, id := range []string{paired, desktop, collision, unknown, phone} {
		h.participant(group, id, "")
		h.message(group, "m-"+id, id, 100)
	}
	h.participant(lidChat, chatPeer, "+90000000000003")
	h.message(lidChat, "m-chat", chatPeer, 100)

	evidence := whatsapplid.NewEvidence()
	evidence.AddPair("90000000000001", "15550000001", whatsapplid.SourceSessionLIDMap)
	evidence.AddLID("90000000000002", whatsapplid.SourceDesktopMessages)
	evidence.AddLID("90000000000004", whatsapplid.SourceDesktopMembers)
	evidence.AddLID("15550000099", whatsapplid.SourceDesktopMembers)
	report := h.plan(evidence)

	if report.PhoneIdentities != 7 || len(report.Candidates) != 5 || len(report.Rewrites) != 3 || report.Refused != 2 {
		t.Fatalf("report counts = %d identities, %d candidates, %d rewrites, %d refused: %+v",
			report.PhoneIdentities, len(report.Candidates), len(report.Rewrites), report.Refused, report.Candidates)
	}
	want := map[string][3]string{
		"+90000000000001": {lidVerdictRewrite, "e164", "+15550000001"},
		"+90000000000002": {lidVerdictRewrite, "jid", "90000000000002@lid"},
		"+90000000000003": {lidVerdictRewrite, "jid", "90000000000003@lid"},
		"+90000000000004": {lidVerdictAlsoAPhone, "", ""},
		"+15550000099":    {lidVerdictOwnIdentity, "", ""},
	}
	for canonical, verdict := range want {
		got := lidRepairCandidate(t, report, canonical)
		if got.Verdict != verdict[0] || got.TargetKind != verdict[1] || got.TargetCanonicalValue != verdict[2] {
			t.Errorf("%s = %s %s %s, want %v (%+v)", canonical, got.Verdict, got.TargetKind, got.TargetCanonicalValue, verdict, got)
		}
	}
	if got := lidRepairCandidate(t, report, "+90000000000004").PhoneSources; !reflect.DeepEqual(got, []string{"v2_number_on_google"}) {
		t.Errorf("collision phone sources = %v", got)
	}
	if got := lidRepairCandidate(t, report, "+90000000000003").LIDSources; !reflect.DeepEqual(got, []string{lidSourceStoreChat}) {
		t.Errorf("chat peer LID sources = %v", got)
	}

	h.apply(report)
	for _, id := range []string{paired, desktop, chatPeer} {
		if h.scalar(`SELECT COUNT(*) FROM identities WHERE identity_id = ?`, id) != "0" {
			t.Errorf("identity %s survived its rewrite", id)
		}
	}
	for _, id := range []string{collision, unknown, phone, self} {
		if h.scalar(`SELECT COUNT(*) FROM identities WHERE identity_id = ?`, id) != "1" {
			t.Errorf("identity %s was rewritten without evidence", id)
		}
	}
	numberID := lidRepairIdentityID(lidRepairAccount, "e164", "+15550000001")
	if got := h.scalar(`SELECT COUNT(*) FROM messages WHERE sender_identity_id = ?`, numberID); got != "1" {
		t.Errorf("messages from the paired number = %s", got)
	}
	if got := h.scalar(`SELECT display_name FROM identities WHERE identity_id = ?`, numberID); got != "" {
		t.Errorf("paired number name = %q, want the fake number not carried", got)
	}
	if got := h.scalar(`SELECT display_name FROM identities WHERE canonical_value = '90000000000002@lid'`); got != "Bea" {
		t.Errorf("LID name = %q, want the real name carried", got)
	}
	if got := h.scalar(`SELECT title FROM conversations WHERE conversation_id = ?`, lidChat); got != "" {
		t.Errorf("LID chat title = %q, want the fake number cleared", got)
	}
	if got := h.plan(evidence); len(got.Rewrites) != 0 || got.Refused != 2 {
		t.Errorf("second plan = %d rewrites, %d refused; want 0 and the same 2 refusals", len(got.Rewrites), got.Refused)
	}
}

// A LID live ingest already stored as "<digits>@lid" after cutover, and the
// migrated "+<digits>" copy of the same LID, become one identity.
func TestLIDRepairMergesIntoTheIdentityLiveIngestMinted(t *testing.T) {
	h := newLIDRepairHarness(t)
	group := h.conversation("whatsapp:120363019999999999@g.us", "group", "")
	fake := h.identity(lidRepairAccount, "e164", "+90000000000001", "", false)
	live := h.identity(lidRepairAccount, "jid", "90000000000001@lid", "Ana", false)
	h.participant(group, fake, "")
	h.participant(group, live, "Ana")
	old := h.message(group, "old", fake, 100)
	h.message(group, "new", live, 200)
	h.reaction(old, group, fake, "🔥", 300, 3)
	h.reaction(old, group, live, "👍", 200, 2)
	h.link(fake, "p", true)

	report := h.plan(nil)
	if len(report.Rewrites) != 1 || !report.Candidates[0].TargetExists || report.Candidates[0].TargetIdentityID != live {
		t.Fatalf("plan = %+v", report)
	}
	if got := report.Candidates[0].LIDSources; !reflect.DeepEqual(got, []string{lidSourceStoreIdentity}) {
		t.Fatalf("LID sources = %v", got)
	}
	h.apply(report)
	if got := h.scalar(`SELECT COUNT(*) FROM messages WHERE sender_identity_id = ?`, live); got != "2" {
		t.Fatalf("messages from the LID = %s, want both", got)
	}
	if got := h.scalar(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ?`, group); got != "1" {
		t.Fatalf("participants = %s, want the two rows merged", got)
	}
	if got := h.scalar(`SELECT emoji FROM reactions WHERE message_id = ? AND reactor_key = ?`, old, live); got != "🔥" {
		t.Fatalf("reaction = %q, want the newer one", got)
	}
	if got := h.scalar(`SELECT person_id || is_primary FROM person_identities WHERE identity_id = ?`, live); got != "p1" {
		t.Fatalf("person link = %q", got)
	}
}

func TestLIDRepairRefusesWhatItCannotDecide(t *testing.T) {
	h := newLIDRepairHarness(t)
	group := h.conversation("whatsapp:120363019999999999@g.us", "group", "")
	// Linked to another person than the identity it would merge into.
	other := h.identity(lidRepairAccount, "e164", "+90000000000001", "", false)
	target := h.identity(lidRepairAccount, "e164", "+15550000001", "", false)
	h.link(other, "p", false)
	h.link(target, "q", false)
	// Paired with this account's own number.
	own := h.identity(lidRepairAccount, "e164", "+90000000000002", "", false)
	h.identity(lidRepairAccount, "e164", "+15550000099", "Me", true)
	// Paired with two numbers: kept as the LID.
	twice := h.identity(lidRepairAccount, "e164", "+90000000000003", "", false)
	for _, id := range []string{other, own, twice} {
		h.message(group, "m-"+id, id, 100)
	}
	evidence := whatsapplid.NewEvidence()
	evidence.AddPair("90000000000001", "15550000001", whatsapplid.SourceSessionLIDMap)
	evidence.AddPair("90000000000002", "15550000099", whatsapplid.SourceSessionDevice)
	evidence.AddPair("90000000000003", "15550000003", whatsapplid.SourceSessionLIDMap)
	evidence.AddPair("90000000000003", "15550000004", whatsapplid.SourceDesktopLIDPairs)
	report := h.plan(evidence)

	if got := lidRepairCandidate(t, report, "+90000000000001"); got.Verdict != lidVerdictOtherPerson {
		t.Errorf("person conflict = %+v", got)
	}
	if got := lidRepairCandidate(t, report, "+90000000000002"); got.Verdict != lidVerdictTargetIsOwn {
		t.Errorf("own number = %+v", got)
	}
	if got := lidRepairCandidate(t, report, "+90000000000003"); got.Verdict != lidVerdictRewrite || got.TargetCanonicalValue != "90000000000003@lid" || got.Detail == "" {
		t.Errorf("disagreeing pairs = %+v", got)
	}
	if len(report.Rewrites) != 1 {
		t.Fatalf("rewrites = %+v", report.Rewrites)
	}
}

// Two LIDs a record pairs with one number merge into that number's identity
// (minted by the first), and the plan says so.
func TestLIDRepairTwoLIDsForOneNumber(t *testing.T) {
	h := newLIDRepairHarness(t)
	group := h.conversation("whatsapp:120363019999999999@g.us", "group", "")
	first := h.identity(lidRepairAccount, "e164", "+90000000000001", "", false)
	second := h.identity(lidRepairAccount, "e164", "+90000000000002", "", false)
	h.link(first, "p", false)
	h.link(second, "q", false)
	h.message(group, "a", first, 100)
	h.message(group, "b", second, 100)
	evidence := whatsapplid.NewEvidence()
	evidence.AddPair("90000000000001", "15550000001", whatsapplid.SourceSessionLIDMap)
	evidence.AddPair("90000000000002", "15550000001", whatsapplid.SourceDesktopLIDPairs)
	report := h.plan(evidence)
	if got := lidRepairCandidate(t, report, "+90000000000001"); got.Verdict != lidVerdictRewrite || got.TargetExists {
		t.Fatalf("first = %+v", got)
	}
	// The second's person differs from the one the first moves there.
	if got := lidRepairCandidate(t, report, "+90000000000002"); got.Verdict != lidVerdictOtherPerson || !got.TargetExists {
		t.Fatalf("second = %+v", got)
	}
	h.apply(report)
	if got := h.scalar(`SELECT person_id FROM person_identities WHERE identity_id = ?`, lidRepairIdentityID(lidRepairAccount, "e164", "+15550000001")); got != "p" {
		t.Fatalf("number's person = %q", got)
	}
}

func TestIsRawUserName(t *testing.T) {
	for name, want := range map[string]bool{
		"+98765432109876":     true,
		"98765432109876":      true,
		"98765432109876@lid":  true,
		"98765432109876@LID":  true,
		"+98 7654 321-09876":  true,
		" +98765432109876 ":   true,
		"+9876543210987":      false,
		"Ana":                 false,
		"Ana +98765432109876": false,
		"":                    false,
	} {
		if got := isRawUserName(name, "98765432109876"); got != want {
			t.Errorf("isRawUserName(%q) = %v, want %v", name, got, want)
		}
	}
}
