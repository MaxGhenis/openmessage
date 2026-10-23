package importer

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxghenis/openmessage/internal/db"
)

// WhatsApp Desktop keys most chats and group senders by LID. A LID's digits
// are no phone number: the importer stores the number Desktop pairs a LID
// with, or else the LID itself, never "+<lid digits>".

const (
	lidImportUnmappedChat   = "11111111111111"
	lidImportMappedChat     = "22222222222222"
	lidImportMappedChatPN   = "15550000002"
	lidImportUnmappedMember = "33333333333333"
	lidImportMappedMember   = "44444444444444"
	lidImportMappedMemberPN = "15550000004"
	lidImportPhoneMember    = "15550000005"
	lidImportGroup          = "120363019999999999@g.us"
)

func seedLIDDesktop(t *testing.T, withIdentifier bool) string {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "ChatStorage.sqlite")
	waDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open whatsapp db: %v", err)
	}
	defer waDB.Close()
	identifierColumn, chatIdentifier, memberIdentifier := "", "", ""
	if withIdentifier {
		identifierColumn = ", ZCONTACTIDENTIFIER VARCHAR"
		chatIdentifier = ", NULL"
		memberIdentifier = ", NULL"
	}
	schema := `
		CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID VARCHAR, ZPARTNERNAME VARCHAR,
			ZLASTMESSAGEDATE REAL, ZREMOVED INTEGER` + identifierColumn + `);
		CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZMEMBERJID VARCHAR,
			ZCONTACTNAME VARCHAR` + identifierColumn + `);
		CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZSTANZAID VARCHAR, ZTEXT VARCHAR, ZMESSAGEDATE REAL,
			ZISFROMME INTEGER, ZFROMJID VARCHAR, ZPUSHNAME VARCHAR, ZCHATSESSION INTEGER, ZMEDIAITEM INTEGER);
		CREATE TABLE ZWAMEDIAITEM (Z_PK INTEGER PRIMARY KEY, ZMEDIALOCALPATH VARCHAR);`
	if _, err := waDB.Exec(schema); err != nil {
		t.Fatalf("create whatsapp schema: %v", err)
	}
	mappedChatIdentifier := chatIdentifier
	if withIdentifier {
		mappedChatIdentifier = ", '" + lidImportMappedChatPN + "@s.whatsapp.net'"
	}
	rows := []string{
		`INSERT INTO ZWACHATSESSION VALUES (1, '` + lidImportUnmappedChat + `@lid', 'Ana', 1000, 0` + chatIdentifier + `)`,
		`INSERT INTO ZWACHATSESSION VALUES (2, '` + lidImportMappedChat + `@lid', 'Bea', 1001, 0` + mappedChatIdentifier + `)`,
		`INSERT INTO ZWACHATSESSION VALUES (3, '` + lidImportGroup + `', 'Book club', 1002, 0` + chatIdentifier + `)`,
		`INSERT INTO ZWAGROUPMEMBER VALUES (1, 3, '` + lidImportUnmappedMember + `@lid', NULL` + memberIdentifier + `)`,
		`INSERT INTO ZWAGROUPMEMBER VALUES (2, 3, '` + lidImportMappedMember + `@lid', NULL` + memberIdentifier + `)`,
		`INSERT INTO ZWAGROUPMEMBER VALUES (3, 3, '` + lidImportPhoneMember + `@s.whatsapp.net', 'Cy'` + memberIdentifier + `)`,
		`INSERT INTO ZWAMESSAGE VALUES (1, 'direct-unmapped', 'hi', 1000, 0, '` + lidImportUnmappedChat + `@lid', '', 1, NULL)`,
		`INSERT INTO ZWAMESSAGE VALUES (2, 'direct-mapped', 'hi', 1001, 0, '` + lidImportMappedChat + `@lid', '', 2, NULL)`,
		`INSERT INTO ZWAMESSAGE VALUES (3, 'group-unmapped', 'hi', 1002, 0, '` + lidImportUnmappedMember + `@lid', '', 3, NULL)`,
		`INSERT INTO ZWAMESSAGE VALUES (4, 'group-mapped', 'hi', 1003, 0, '` + lidImportMappedMember + `@lid', '', 3, NULL)`,
		`INSERT INTO ZWAMESSAGE VALUES (5, 'group-phone', 'hi', 1004, 0, '` + lidImportPhoneMember + `@s.whatsapp.net', '', 3, NULL)`,
	}
	for _, row := range rows {
		if _, err := waDB.Exec(row); err != nil {
			t.Fatalf("seed %q: %v", row, err)
		}
	}

	lidDB, err := sql.Open("sqlite", filepath.Join(root, "LID.sqlite"))
	if err != nil {
		t.Fatalf("open LID.sqlite: %v", err)
	}
	defer lidDB.Close()
	if _, err := lidDB.Exec(`CREATE TABLE ZWAPHONENUMBERLIDPAIR (Z_PK INTEGER PRIMARY KEY, ZLID VARCHAR, ZPHONENUMBER VARCHAR);
		INSERT INTO ZWAPHONENUMBERLIDPAIR VALUES (1, '` + lidImportMappedMember + `', '` + lidImportMappedMemberPN + `');`); err != nil {
		t.Fatalf("seed LID.sqlite: %v", err)
	}
	return dbPath
}

func importLIDDesktop(t *testing.T, dbPath string) *db.Store {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := (&WhatsAppNative{DBPath: dbPath, SinceMS: -1}).ImportFromDB(store); err != nil {
		t.Fatalf("ImportFromDB: %v", err)
	}
	return store
}

func lidImportSender(t *testing.T, store *db.Store, stanza string) *db.Message {
	t.Helper()
	msg, err := store.GetMessageByID("whatsapp:" + stanza)
	if err != nil || msg == nil {
		t.Fatalf("GetMessageByID(%s): %v, %v", stanza, msg, err)
	}
	return msg
}

func lidImportParticipants(t *testing.T, store *db.Store, conversationID string) []map[string]string {
	t.Helper()
	convo, err := store.GetConversation(conversationID)
	if err != nil || convo == nil {
		t.Fatalf("GetConversation(%s): %v, %v", conversationID, convo, err)
	}
	var participants []map[string]string
	if err := json.Unmarshal([]byte(convo.Participants), &participants); err != nil {
		t.Fatalf("participants %q: %v", convo.Participants, err)
	}
	return participants
}

func TestImportStoresLIDsByTheirNumberOrAsLIDs(t *testing.T) {
	store := importLIDDesktop(t, seedLIDDesktop(t, true))

	senders := map[string]string{
		"direct-unmapped": lidImportUnmappedChat + "@lid",
		"direct-mapped":   "+" + lidImportMappedChatPN,
		"group-unmapped":  lidImportUnmappedMember + "@lid",
		"group-mapped":    "+" + lidImportMappedMemberPN,
		"group-phone":     "+" + lidImportPhoneMember,
	}
	for stanza, want := range senders {
		if got := lidImportSender(t, store, stanza).SenderNumber; got != want {
			t.Errorf("%s sender number = %q, want %q", stanza, got, want)
		}
	}
	if got := lidImportSender(t, store, "group-unmapped").SenderName; got != lidImportUnmappedMember+"@lid" {
		t.Errorf("unnamed LID sender name = %q, want its LID", got)
	}

	if got := lidImportParticipants(t, store, "whatsapp:"+lidImportUnmappedChat+"@lid"); len(got) != 1 || got[0]["number"] != lidImportUnmappedChat+"@lid" {
		t.Errorf("unmapped chat participants = %v, want its LID", got)
	}
	if got := lidImportParticipants(t, store, "whatsapp:"+lidImportMappedChat+"@lid"); len(got) != 1 || got[0]["number"] != "+"+lidImportMappedChatPN {
		t.Errorf("mapped chat participants = %v, want its number", got)
	}
	members := map[string]string{}
	for _, member := range lidImportParticipants(t, store, "whatsapp:"+lidImportGroup) {
		members[member["number"]] = member["name"]
	}
	want := map[string]string{
		lidImportUnmappedMember + "@lid": "",
		"+" + lidImportMappedMemberPN:    "+" + lidImportMappedMemberPN,
		"+" + lidImportPhoneMember:       "Cy",
	}
	if len(members) != len(want) {
		t.Errorf("group members = %v, want %v", members, want)
	}
	for number, name := range want {
		if got, ok := members[number]; !ok || got != name {
			t.Errorf("group member %s = %q (present %v), want name %q", number, got, ok, name)
		}
	}

	for _, lid := range []string{lidImportUnmappedChat, lidImportMappedChat, lidImportUnmappedMember, lidImportMappedMember} {
		assertNoLIDAsPhone(t, store, lid)
	}
}

// A Desktop schema without ZCONTACTIDENTIFIER pairs nothing from its chats;
// those LIDs are stored as LIDs, and LID.sqlite still pairs its own.
func TestImportWithoutContactIdentifiersStoresUnpairedLIDsAsLIDs(t *testing.T) {
	store := importLIDDesktop(t, seedLIDDesktop(t, false))
	if got := lidImportSender(t, store, "direct-mapped").SenderNumber; got != lidImportMappedChat+"@lid" {
		t.Errorf("chat sender without an identifier = %q, want its LID", got)
	}
	if got := lidImportSender(t, store, "group-mapped").SenderNumber; got != "+"+lidImportMappedMemberPN {
		t.Errorf("member paired by LID.sqlite = %q, want its number", got)
	}
	for _, lid := range []string{lidImportUnmappedChat, lidImportMappedChat, lidImportUnmappedMember, lidImportMappedMember} {
		assertNoLIDAsPhone(t, store, lid)
	}
}

func assertNoLIDAsPhone(t *testing.T, store *db.Store, lid string) {
	t.Helper()
	convs, err := store.ListConversations(100)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	for _, convo := range convs {
		if strings.Contains(convo.Participants, `"+`+lid+`"`) || convo.Name == "+"+lid {
			t.Errorf("conversation %s stores LID %s as a phone number: %q / %q", convo.ConversationID, lid, convo.Name, convo.Participants)
		}
		msgs, err := store.GetMessagesByConversation(convo.ConversationID, 100)
		if err != nil {
			t.Fatalf("GetMessagesByConversation: %v", err)
		}
		for _, msg := range msgs {
			if msg.SenderNumber == "+"+lid || msg.SenderName == "+"+lid {
				t.Errorf("message %s stores LID %s as a phone number: %q / %q", msg.MessageID, lid, msg.SenderNumber, msg.SenderName)
			}
		}
	}
}
