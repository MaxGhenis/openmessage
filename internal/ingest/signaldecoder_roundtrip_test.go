package ingest_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	"github.com/maxghenis/openmessage/internal/ingest"
)

func TestSignalAttachmentRemoteRefRoundTripsThroughAdapter(t *testing.T) {
	line := []byte(`{"account":"+15551230000","envelope":{"sourceNumber":"+15551234567","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"groupInfo":{"groupId":"group-token"},"attachments":[{"contentType":"image/png","id":"raw-att-42","filename":"photo.png"}]}}}`)
	record, ephemeral, err := ingest.BuildSignalIngress(
		"signal-primary",
		5,
		"+15551230000",
		line,
		"",
		"",
		time.UnixMilli(1_700_000_010_000),
	)
	if err != nil {
		t.Fatalf("BuildSignalIngress(): %v", err)
	}
	if record == nil || ephemeral != nil {
		t.Fatalf("classification = (%+v, %+v), want durable", record, ephemeral)
	}
	events, err := (&ingest.SignalDecoder{}).Decode(context.Background(), *record)
	if err != nil {
		t.Fatalf("Decode(): %v", err)
	}
	if len(events) != 1 || events[0].Kind != bridge.EventMessage ||
		events[0].Message == nil || len(events[0].Message.Attachments) != 1 {
		t.Fatalf("events = %+v, want one message attachment", events)
	}
	attachment := events[0].Message.Attachments[0]
	if attachment.RemoteID != "raw-att-42" || strings.Contains(string(attachment.RemoteRef), "signalatt:") {
		t.Fatalf("attachment = %+v, want raw attachment ID without legacy prefix", attachment)
	}
	var opaque struct {
		Version        int    `json:"v"`
		Kind           string `json:"kind"`
		AttachmentID   string `json:"att_id"`
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(attachment.RemoteRef, &opaque); err != nil {
		t.Fatalf("json.Unmarshal(RemoteRef): %v", err)
	}
	if opaque.Version != 1 || opaque.Kind != "remote" ||
		opaque.AttachmentID != "raw-att-42" ||
		opaque.ConversationID != "signal-group:group-token" {
		t.Fatalf("RemoteRef = %+v, want complete Signal remote v1 shape", opaque)
	}
	if err := signaladapter.ValidateDownloadOpaque(attachment.RemoteRef); err != nil {
		t.Fatalf("ValidateDownloadOpaque(decoder RemoteRef): %v", err)
	}
}

func TestSignalAttachmentRemoteRefRejectsMissingConversationID(t *testing.T) {
	missingConversation := []byte(`{"v":1,"kind":"remote","att_id":"raw-att-42"}`)
	if err := signaladapter.ValidateDownloadOpaque(missingConversation); err == nil {
		t.Fatal("ValidateDownloadOpaque() accepted remote ref without conversation_id")
	}
}
