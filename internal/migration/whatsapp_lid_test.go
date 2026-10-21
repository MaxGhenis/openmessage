package migration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	legacydb "github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/whatsapplid"
)

// From the legacy store to a repaired v2 store: a LID sender the fixed bridge
// stored as "<digits>@lid" migrates to the identity v2 live ingest mints for
// that LID, and one a pre-fix build stored as "+<digits>" migrates to an e164
// identity that the WhatsApp LID repair rewrites once a record names the
// digits a LID.
func TestMigratedWhatsAppLIDSendersReachTheLiveIdentity(t *testing.T) {
	const (
		fixedLID   = "98765432109876"
		chatLID    = "22222222222222"
		unknownLID = "11111111111111"
		group      = "whatsapp:120363019999999999@g.us"
	)
	root := t.TempDir()
	legacyPath := filepath.Join(root, "messages.db")
	legacy, err := legacydb.New(legacyPath)
	mustNoError(t, err)
	mustNoError(t, legacy.UpsertConversation(&legacydb.Conversation{
		ConversationID: group, Name: "Book club", IsGroup: true, SourcePlatform: "whatsapp", LastMessageTS: fixtureBaseMS + 2,
		Participants: `[{"name":"Ana","number":"` + fixedLID + `@lid"},{"name":"+` + unknownLID + `","number":"+` + unknownLID + `"}]`,
	}))
	mustNoError(t, legacy.UpsertConversation(&legacydb.Conversation{
		ConversationID: "whatsapp:" + chatLID + "@lid", Name: "+" + chatLID, SourcePlatform: "whatsapp", LastMessageTS: fixtureBaseMS + 3,
		Participants: `[{"name":"+` + chatLID + `","number":"+` + chatLID + `"}]`,
	}))
	for index, message := range []legacydb.Message{
		{MessageID: "whatsapp:fixed", ConversationID: group, SenderName: "Ana", SenderNumber: fixedLID + "@lid"},
		{MessageID: "whatsapp:unknown", ConversationID: group, SenderName: "+" + unknownLID, SenderNumber: "+" + unknownLID},
		{MessageID: "whatsapp:chat", ConversationID: "whatsapp:" + chatLID + "@lid", SenderName: "+" + chatLID, SenderNumber: "+" + chatLID},
	} {
		message.Body, message.Status, message.SourcePlatform = "hi", "delivered", "whatsapp"
		message.SourceID = message.MessageID[len("whatsapp:"):]
		message.TimestampMS = fixtureBaseMS + int64(index+1)
		mustNoError(t, legacy.UpsertMessage(&message))
	}
	mustNoError(t, legacy.Close())

	staged := filepath.Join(root, "store.sqlite3.tmp")
	_, err = Transform(context.Background(), Options{
		SourcePath: legacyPath, TempStorePath: staged, TempBlobPath: filepath.Join(root, "blobs.tmp"),
		TargetPath: filepath.Join(root, "canonical"), TargetStorePath: filepath.Join(root, "canonical", "store.sqlite3"), Check: true,
	})
	mustNoError(t, err)
	store, err := sqlite.Open(staged)
	mustNoError(t, err)
	defer store.Close()

	senderOf := func(remote string) sqlite.Identity {
		t.Helper()
		var identityID string
		target := openReadOnlyTestDB(t, staged)
		mustNoError(t, target.QueryRow(`SELECT sender_identity_id FROM messages WHERE remote_message_id = ?`, remote).Scan(&identityID))
		identity, err := store.GetIdentity(identityID)
		mustNoError(t, err)
		return identity
	}
	// The id v2 live ingest derives for a frame from this LID.
	liveID := v2keys.DeriveID("identity", "whatsapp-primary", "jid\x1f"+fixedLID+"@lid")
	if got := senderOf("fixed"); got.IdentityID != liveID || got.Kind != "jid" || got.CanonicalValue != fixedLID+"@lid" {
		t.Fatalf("fixed-bridge LID sender migrated to %+v, want the live identity %s", got, liveID)
	}
	if got := senderOf("chat"); got.Kind != "e164" || got.CanonicalValue != "+"+chatLID {
		t.Fatalf("pre-fix LID sender migrated to %+v, want the e164 the repair fixes", got)
	}

	// The 1:1 chat keyed by the LID names its digits a LID; nothing names
	// the other pre-fix sender's digits one, so it stays.
	report, err := ingest.PlanWhatsAppLIDRepair(context.Background(), store, ingest.WhatsAppLIDRepairOptions{AccountID: "whatsapp-primary"})
	mustNoError(t, err)
	if len(report.Rewrites) != 1 || report.Rewrites[0].FromCanonicalValue != "+"+chatLID || report.Rewrites[0].ToCanonicalValue != chatLID+"@lid" {
		t.Fatalf("plan without outside evidence = %+v", report.Rewrites)
	}
	mustNoError(t, ingest.ApplyWhatsAppLIDRepair(context.Background(), store, report, time.UnixMilli(fixtureBaseMS+10)))
	if got := senderOf("chat"); got.Kind != "jid" || got.CanonicalValue != chatLID+"@lid" {
		t.Fatalf("repaired chat sender = %+v", got)
	}
	if got := senderOf("unknown"); got.Kind != "e164" {
		t.Fatalf("a sender no record names a LID was rewritten: %+v", got)
	}

	// Once the session store pairs it with a number, it becomes that number.
	evidence := whatsapplid.NewEvidence()
	evidence.AddPair(unknownLID, "15550000001", whatsapplid.SourceSessionLIDMap)
	report, err = ingest.PlanWhatsAppLIDRepair(context.Background(), store, ingest.WhatsAppLIDRepairOptions{AccountID: "whatsapp-primary", Evidence: evidence})
	mustNoError(t, err)
	mustNoError(t, ingest.ApplyWhatsAppLIDRepair(context.Background(), store, report, time.UnixMilli(fixtureBaseMS+20)))
	if got := senderOf("unknown"); got.Kind != "e164" || got.CanonicalValue != "+15550000001" || got.DisplayName != "" {
		t.Fatalf("paired sender = %+v, want the number, without the fake number as its name", got)
	}
}
