package whatsapplid

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPhoneAndLIDUsersNeedTheirServer(t *testing.T) {
	cases := []struct {
		raw          string
		phone, lid   string
		isPhone, isL bool
	}{
		{raw: "15551234567@s.whatsapp.net", phone: "15551234567", isPhone: true},
		{raw: "15551234567:12@s.whatsapp.net", phone: "15551234567", isPhone: true},
		{raw: "98765432109876@lid", lid: "98765432109876", isL: true},
		{raw: "98765432109876:3@lid", lid: "98765432109876", isL: true},
		{raw: "120363019999999999@g.us"},
		{raw: "120363111111111111@newsletter"},
		{raw: "98765432109876@hosted.lid"},
		{raw: "+15551234567"},
		{raw: "15551234567"},
		{raw: "abc@s.whatsapp.net"},
		{raw: "1@lid@s.whatsapp.net"},
		{raw: ""},
	}
	for _, tc := range cases {
		phone, isPhone := PhoneUser(tc.raw)
		lid, isLID := LIDUser(tc.raw)
		if phone != tc.phone || isPhone != tc.isPhone || lid != tc.lid || isLID != tc.isL {
			t.Errorf("%q: PhoneUser = (%q, %v), LIDUser = (%q, %v); want (%q, %v), (%q, %v)",
				tc.raw, phone, isPhone, lid, isLID, tc.phone, tc.isPhone, tc.lid, tc.isL)
		}
	}
}

func TestJIDPairsPairOnlyALIDWithAnExplicitNumber(t *testing.T) {
	evidence := NewEvidence()
	evidence.AddJIDPair("111@lid", "15550000001@s.whatsapp.net", "a")
	evidence.AddJIDPair("15550000002@s.whatsapp.net", "222@lid", "a")
	evidence.AddJIDPair("333@lid", "+15550000003", "a")
	// Bare digits in an identifier column could be any number.
	evidence.AddJIDPair("444@lid", "15550000004", "a")
	evidence.AddJIDPair("555@lid", "", "a")
	evidence.AddJIDPair("120363019999999999@g.us", "", "a")

	for lid, want := range map[string]string{"111": "15550000001", "222": "15550000002", "333": "15550000003"} {
		if got, ok := evidence.PhoneForLID(lid); !ok || got != want {
			t.Errorf("PhoneForLID(%s) = %q, %v; want %q", lid, got, ok, want)
		}
	}
	for _, lid := range []string{"444", "555"} {
		if got, ok := evidence.PhoneForLID(lid); ok {
			t.Errorf("PhoneForLID(%s) = %q, want no number", lid, got)
		}
		if !evidence.IsLID(lid) {
			t.Errorf("%s is not recorded as a LID", lid)
		}
	}
	if evidence.IsPhone("15550000004") {
		t.Error("bare digits in an identifier were recorded as a phone number")
	}
	if evidence.IsLID("120363019999999999") || evidence.IsPhone("120363019999999999") {
		t.Error("a group JID was recorded as a person")
	}
}

func TestPhoneForLIDRefusesDisagreeingOrSelfContradictoryPairs(t *testing.T) {
	evidence := NewEvidence()
	evidence.AddPair("111", "15550000001", "a")
	evidence.AddPair("111", "15550000009", "b")
	evidence.AddPair("222", "15550000002", "a")
	evidence.AddPair("222", "15550000002", "b")
	evidence.AddPair("333", "444", "a")
	evidence.AddLID("444", "c")

	if got, ok := evidence.PhoneForLID("111"); ok {
		t.Errorf("PhoneForLID with two numbers = %q, want none", got)
	}
	if got, ok := evidence.PhoneForLID("222"); !ok || got != "15550000002" {
		t.Errorf("PhoneForLID with agreeing sources = %q, %v", got, ok)
	}
	if got, ok := evidence.PhoneForLID("333"); ok {
		t.Errorf("PhoneForLID naming a LID as the number = %q, want none", got)
	}
	if got := evidence.PhonesForLID("111"); !reflect.DeepEqual(got, []string{"15550000001", "15550000009"}) {
		t.Errorf("PhonesForLID = %v", got)
	}
	if got := evidence.LIDSources("222"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("LIDSources = %v", got)
	}
}

func TestAddressNeverStoresALIDAsANumber(t *testing.T) {
	evidence := NewEvidence()
	evidence.AddPair("222", "15550000002", "a")
	cases := map[string]string{
		"15550000001@s.whatsapp.net": "+15550000001",
		"111@lid":                    "111@lid",
		"111:4@lid":                  "111@lid",
		"222@lid":                    "+15550000002",
		"120363019999999999@g.us":    "",
		"status@broadcast":           "",
		"":                           "",
	}
	for raw, want := range cases {
		if got := evidence.Address(raw); got != want {
			t.Errorf("Address(%q) = %q, want %q", raw, got, want)
		}
	}
	var none *Evidence
	if got := none.Address("222@lid"); got != "222@lid" {
		t.Errorf("nil evidence Address = %q, want the LID", got)
	}
}

func TestMergeKeepsEverySource(t *testing.T) {
	first, second := NewEvidence(), NewEvidence()
	first.AddPair("111", "15550000001", "a")
	second.AddPair("111", "15550000001", "b")
	second.AddPhone("15550000003", "b")
	first.Merge(second)
	if got := first.LIDSources("111"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("LIDSources after merge = %v", got)
	}
	if !first.IsPhone("15550000003") {
		t.Error("merge dropped a phone number")
	}
}

func execAll(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestReadSessionStoreReadsLIDMapDeviceAndContacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whatsapp-session.db")
	execAll(t, path,
		`CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL)`,
		`INSERT INTO whatsmeow_lid_map VALUES ('111', '15550000001'), ('222@lid', '15550000002@s.whatsapp.net')`,
		`CREATE TABLE whatsmeow_device (jid TEXT PRIMARY KEY, lid TEXT)`,
		`INSERT INTO whatsmeow_device VALUES ('15550000009:12@s.whatsapp.net', '999:12@lid')`,
		`CREATE TABLE whatsmeow_contacts (our_jid TEXT, their_jid TEXT)`,
		`INSERT INTO whatsmeow_contacts VALUES ('x', '333@lid'), ('x', '15550000004@s.whatsapp.net')`,
	)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence := NewEvidence()
	report := ReadSessionStore(context.Background(), path, evidence)
	if !report.Read || report.Error != "" || report.Rows != 5 {
		t.Fatalf("report = %+v", report)
	}
	for lid, pn := range map[string]string{"111": "15550000001", "222": "15550000002", "999": "15550000009"} {
		if got, ok := evidence.PhoneForLID(lid); !ok || got != pn {
			t.Errorf("PhoneForLID(%s) = %q, %v; want %q", lid, got, ok, pn)
		}
	}
	if !evidence.IsLID("333") || !evidence.IsPhone("15550000004") {
		t.Error("contacts were not read")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the reader wrote the session store")
	}
}

func TestReadersReportMissingFilesAndTables(t *testing.T) {
	evidence := NewEvidence()
	missing := ReadSessionStore(context.Background(), filepath.Join(t.TempDir(), "none.db"), evidence)
	if missing.Read || !missing.Missing || missing.Error != "" {
		t.Fatalf("missing file report = %+v", missing)
	}
	empty := filepath.Join(t.TempDir(), "empty.db")
	execAll(t, empty, `CREATE TABLE unrelated (x TEXT)`)
	report := ReadSessionStore(context.Background(), empty, evidence)
	if !report.Read || !reflect.DeepEqual(report.Skipped, []string{"whatsmeow_lid_map", "whatsmeow_device", "whatsmeow_contacts"}) {
		t.Fatalf("empty store report = %+v", report)
	}
	desktop := ReadDesktop(context.Background(), empty, evidence)
	if !desktop.Read || len(desktop.Skipped) != 3 {
		t.Fatalf("empty desktop report = %+v", desktop)
	}
	pairs := ReadDesktopLIDPairs(context.Background(), empty, evidence)
	if !pairs.Read || !reflect.DeepEqual(pairs.Skipped, []string{"ZWAPHONENUMBERLIDPAIR"}) {
		t.Fatalf("empty LID.sqlite report = %+v", pairs)
	}
}

func TestReadDesktopReadsChatsMembersAndSenders(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ChatStorage.sqlite")
	execAll(t, path,
		`CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID VARCHAR, ZCONTACTIDENTIFIER VARCHAR)`,
		`INSERT INTO ZWACHATSESSION VALUES (1, '111@lid', '15550000001@s.whatsapp.net'), (2, '222@lid', NULL), (3, '15550000003@s.whatsapp.net', NULL)`,
		`CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, ZMEMBERJID VARCHAR)`,
		`INSERT INTO ZWAGROUPMEMBER VALUES (1, '444@lid'), (2, '15550000005@s.whatsapp.net')`,
		`CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZFROMJID VARCHAR)`,
		`INSERT INTO ZWAMESSAGE VALUES (1, '666@lid'), (2, '666@lid'), (3, '15550000007@s.whatsapp.net'), (4, NULL), (5, '120363019999999999@g.us')`,
	)
	execAll(t, DesktopLIDPath(path),
		`CREATE TABLE ZWAPHONENUMBERLIDPAIR (Z_PK INTEGER PRIMARY KEY, ZLID VARCHAR, ZPHONENUMBER VARCHAR)`,
		`INSERT INTO ZWAPHONENUMBERLIDPAIR VALUES (1, '444', '+15550000004'), (2, '888@lid', '15550000008')`,
	)
	evidence := NewEvidence()
	report := ReadDesktop(context.Background(), path, evidence)
	if !report.Read || report.Error != "" || !reflect.DeepEqual(report.Skipped, []string{"ZWAGROUPMEMBER.ZCONTACTIDENTIFIER"}) {
		t.Fatalf("desktop report = %+v", report)
	}
	pairs := ReadDesktopLIDPairs(context.Background(), DesktopLIDPath(path), evidence)
	if !pairs.Read || pairs.Rows != 2 {
		t.Fatalf("LID.sqlite report = %+v", pairs)
	}
	for _, lid := range []string{"111", "222", "444", "666", "888"} {
		if !evidence.IsLID(lid) {
			t.Errorf("%s is not recorded as a LID", lid)
		}
	}
	for _, pn := range []string{"15550000001", "15550000003", "15550000004", "15550000005", "15550000007", "15550000008"} {
		if !evidence.IsPhone(pn) {
			t.Errorf("%s is not recorded as a phone number", pn)
		}
	}
	for lid, pn := range map[string]string{"111": "15550000001", "444": "15550000004", "888": "15550000008"} {
		if got, ok := evidence.PhoneForLID(lid); !ok || got != pn {
			t.Errorf("PhoneForLID(%s) = %q, %v; want %q", lid, got, ok, pn)
		}
	}
	if got := evidence.LIDSources("666"); !reflect.DeepEqual(got, []string{SourceDesktopMessages}) {
		t.Errorf("sender LID sources = %v", got)
	}
}
