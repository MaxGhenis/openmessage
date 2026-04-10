package db

import "testing"

func TestRepairLegacyArtifactsDeletesLegacyWhatsAppReactionPlaceholders(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "whatsapp:group@g.us",
		Name:           "Group",
		IsGroup:        true,
		LastMessageTS:  3000,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "whatsapp:m1",
		ConversationID: "whatsapp:group@g.us",
		Body:           "real message",
		TimestampMS:    1000,
		SourcePlatform: "whatsapp",
		SourceID:       "m1",
	}); err != nil {
		t.Fatalf("seed real message: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "whatsapp:reaction-bad",
		ConversationID: "whatsapp:group@g.us",
		Body:           "[Reaction]",
		TimestampMS:    3000,
		SourcePlatform: "whatsapp",
		SourceID:       "reaction-bad",
	}); err != nil {
		t.Fatalf("seed legacy reaction placeholder: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "sms:literal",
		ConversationID: "whatsapp:group@g.us",
		Body:           "[Reaction]",
		TimestampMS:    2000,
		SourcePlatform: "sms",
		SourceID:       "literal",
	}); err != nil {
		t.Fatalf("seed literal message: %v", err)
	}

	report, err := store.RepairLegacyArtifacts()
	if err != nil {
		t.Fatalf("RepairLegacyArtifacts(): %v", err)
	}
	if report.DeletedWhatsAppReactionPlaceholders != 1 {
		t.Fatalf("deleted = %d, want 1", report.DeletedWhatsAppReactionPlaceholders)
	}

	deleted, err := store.GetMessageByID("whatsapp:reaction-bad")
	if err != nil {
		t.Fatalf("GetMessageByID(deleted): %v", err)
	}
	if deleted != nil {
		t.Fatal("expected legacy reaction placeholder to be deleted")
	}

	literal, err := store.GetMessageByID("sms:literal")
	if err != nil {
		t.Fatalf("GetMessageByID(literal): %v", err)
	}
	if literal == nil {
		t.Fatal("expected literal [Reaction] message to remain")
	}

	convo, err := store.GetConversation("whatsapp:group@g.us")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo.LastMessageTS != 2000 {
		t.Fatalf("last_message_ts = %d, want 2000", convo.LastMessageTS)
	}
}

func TestRepairLegacyArtifactsDeletesLegacySignalReactionPlaceholders(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "signal-group:abc",
		Name:           "Signal Group",
		IsGroup:        true,
		LastMessageTS:  3000,
		SourcePlatform: "signal",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "signal:real",
		ConversationID: "signal-group:abc",
		Body:           "real message",
		TimestampMS:    1000,
		SourcePlatform: "signal",
		SourceID:       "real",
	}); err != nil {
		t.Fatalf("seed real message: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "signal:reaction-bad",
		ConversationID: "signal-group:abc",
		Body:           "[Reaction]",
		TimestampMS:    3000,
		SourcePlatform: "signal",
		SourceID:       "reaction-bad",
	}); err != nil {
		t.Fatalf("seed legacy reaction placeholder: %v", err)
	}

	report, err := store.RepairLegacyArtifacts()
	if err != nil {
		t.Fatalf("RepairLegacyArtifacts(): %v", err)
	}
	if report.DeletedSignalReactionPlaceholders != 1 {
		t.Fatalf("deleted = %d, want 1", report.DeletedSignalReactionPlaceholders)
	}

	deleted, err := store.GetMessageByID("signal:reaction-bad")
	if err != nil {
		t.Fatalf("GetMessageByID(deleted): %v", err)
	}
	if deleted != nil {
		t.Fatal("expected legacy Signal reaction placeholder to be deleted")
	}

	convo, err := store.GetConversation("signal-group:abc")
	if err != nil {
		t.Fatalf("GetConversation(): %v", err)
	}
	if convo.LastMessageTS != 1000 {
		t.Fatalf("last_message_ts = %d, want 1000", convo.LastMessageTS)
	}
}

func TestRepairLegacyArtifactsReportsLegacyWhatsAppMediaPlaceholders(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "whatsapp:direct@s.whatsapp.net",
		Name:           "Jenn",
		LastMessageTS:  1000,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "whatsapp:photo",
		ConversationID: "whatsapp:direct@s.whatsapp.net",
		Body:           "[Photo]",
		TimestampMS:    1000,
		SourcePlatform: "whatsapp",
		SourceID:       "photo",
	}); err != nil {
		t.Fatalf("seed placeholder media: %v", err)
	}

	report, err := store.RepairLegacyArtifacts()
	if err != nil {
		t.Fatalf("RepairLegacyArtifacts(): %v", err)
	}
	if report.RemainingWhatsAppMediaPlaceholders != 1 {
		t.Fatalf("remaining media placeholders = %d, want 1", report.RemainingWhatsAppMediaPlaceholders)
	}

	msg, err := store.GetMessageByID("whatsapp:photo")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if msg == nil {
		t.Fatal("expected legacy media placeholder to remain untouched")
	}
}

func TestRepairLegacyArtifactsFixesOutgoingGoogleMessageAttribution(t *testing.T) {
	store := newTestStore(t)

	if err := store.UpsertConversation(&Conversation{
		ConversationID: "sms:will",
		Name:           "Will",
		LastMessageTS:  3000,
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "sms:good-outgoing",
		ConversationID: "sms:will",
		Body:           "Known good outgoing",
		TimestampMS:    1000,
		Status:         "OUTGOING_COMPLETE",
		IsFromMe:       true,
		SenderName:     "Max Ghenis",
		SenderNumber:   "+16506303657",
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed known outgoing: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "sms:bad-outgoing",
		ConversationID: "sms:will",
		Body:           "Should be from me",
		TimestampMS:    2000,
		Status:         "OUTGOING_COMPLETE",
		IsFromMe:       false,
	}); err != nil {
		t.Fatalf("seed broken outgoing: %v", err)
	}
	if err := store.UpsertMessage(&Message{
		MessageID:      "sms:incoming",
		ConversationID: "sms:will",
		Body:           "Still incoming",
		TimestampMS:    3000,
		Status:         "INCOMING_COMPLETE",
		IsFromMe:       false,
		SenderName:     "Will",
		SenderNumber:   "+19145550123",
		SourcePlatform: "sms",
	}); err != nil {
		t.Fatalf("seed incoming: %v", err)
	}

	report, err := store.RepairLegacyArtifacts()
	if err != nil {
		t.Fatalf("RepairLegacyArtifacts(): %v", err)
	}
	if report.FixedGoogleOutgoingAttributionRows != 1 {
		t.Fatalf("fixed rows = %d, want 1", report.FixedGoogleOutgoingAttributionRows)
	}

	fixed, err := store.GetMessageByID("sms:bad-outgoing")
	if err != nil {
		t.Fatalf("GetMessageByID(fixed): %v", err)
	}
	if fixed == nil {
		t.Fatal("expected fixed outgoing message to remain")
	}
	if !fixed.IsFromMe {
		t.Fatal("expected fixed outgoing message to be marked from me")
	}
	if fixed.SenderName != "Max Ghenis" {
		t.Fatalf("sender_name = %q, want Max Ghenis", fixed.SenderName)
	}
	if fixed.SenderNumber != "+16506303657" {
		t.Fatalf("sender_number = %q, want +16506303657", fixed.SenderNumber)
	}
	if fixed.SourcePlatform != "sms" {
		t.Fatalf("source_platform = %q, want sms", fixed.SourcePlatform)
	}

	incoming, err := store.GetMessageByID("sms:incoming")
	if err != nil {
		t.Fatalf("GetMessageByID(incoming): %v", err)
	}
	if incoming == nil {
		t.Fatal("expected incoming message to remain")
	}
	if incoming.IsFromMe {
		t.Fatal("expected incoming message to remain not-from-me")
	}
}
