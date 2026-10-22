package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

const signalAttachmentLine = `{"account":"+15551230000","envelope":{"source":"+15551234567","sourceNumber":"+15551234567","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"message":"two files","attachments":[{"contentType":"text/html","filename":"Report.html","id":"failedRemote.html","size":85841},{"contentType":"image/png","filename":null,"id":"fineRemote.png","size":null}]}}}`

// A frame whose attachments all downloaded must stay byte-identical to the
// frames written before attachment_errors existed: the field is additive.
func TestSignalFrameWithoutAttachmentErrorsKeepsItsShape(t *testing.T) {
	line := []byte(signalAttachmentLine)
	plain, _, err := BuildSignalIngress(signalDecoderAccountID, 1, signalSelf, line, "+15551234567", "", signalDecoderReceivedAt)
	if err != nil {
		t.Fatalf("BuildSignalIngress(): %v", err)
	}
	for _, empty := range []map[string]string{nil, {}} {
		record, _, err := BuildSignalIngressWithAttachmentErrors(
			signalDecoderAccountID, 1, signalSelf, line, "+15551234567", "", empty, signalDecoderReceivedAt,
		)
		if err != nil {
			t.Fatalf("BuildSignalIngressWithAttachmentErrors(%v): %v", empty, err)
		}
		if !bytes.Equal(record.Payload, plain.Payload) {
			t.Fatalf("frame with %v errors =\n%s\nwant\n%s", empty, record.Payload, plain.Payload)
		}
	}
	want := `{"account":"+15551230000","line":` + signalAttachmentLine + `,"resolved_source":"+15551234567","resolved_destination":""}`
	if string(plain.Payload) != want {
		t.Fatalf("frame =\n%s\nwant the pre-existing layout\n%s", plain.Payload, want)
	}
}

func TestSignalDecoderProjectsAttachmentErrorsAndDeclaredSize(t *testing.T) {
	record, _, err := BuildSignalIngressWithAttachmentErrors(
		signalDecoderAccountID, 1, signalSelf, []byte(signalAttachmentLine), "+15551234567", "",
		map[string]string{"failedRemote.html": "java.io.IOException: Connection reset"},
		signalDecoderReceivedAt,
	)
	if err != nil {
		t.Fatalf("BuildSignalIngressWithAttachmentErrors(): %v", err)
	}
	events, err := NewSignalDecoder().Decode(context.Background(), *record)
	if err != nil {
		t.Fatalf("Decode(): %v", err)
	}
	if len(events) != 1 || events[0].Message == nil {
		t.Fatalf("Decode() = %+v, want one message", events)
	}
	attachments := events[0].Message.Attachments
	if len(attachments) != 2 {
		t.Fatalf("attachments = %+v, want 2", attachments)
	}
	if got := attachments[0]; got.RemoteID != "failedRemote.html" || got.Size != 85841 ||
		got.TransportError != "java.io.IOException: Connection reset" {
		t.Fatalf("failed attachment = %+v, want declared size and the receive-time cause", got)
	}
	if got := attachments[1]; got.RemoteID != "fineRemote.png" || got.Size != 0 || got.TransportError != "" {
		t.Fatalf("fine attachment = %+v, want unknown size and no cause", got)
	}
}

// Property: attachment_errors round-trips through the frame and the decoder
// for any ids and causes, reaches exactly the attachments it names, and the
// frame stays valid JSON.
func TestSignalFrameAttachmentErrorsRoundTripProperty(t *testing.T) {
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		count := 1 + rng.Intn(4)
		attachments := make([]string, 0, count)
		errors := map[string]string{}
		for index := 0; index < count; index++ {
			id := fmt.Sprintf("remote%d-%d.bin", index, rng.Intn(1000))
			attachments = append(attachments, fmt.Sprintf(`{"contentType":"application/octet-stream","id":%q,"size":%d}`, id, rng.Intn(1<<20)))
			if rng.Intn(2) == 0 {
				errors[id] = strings.Repeat(string(rune('a'+rng.Intn(26))), 1+rng.Intn(40)) + " \"quoted\" \\ é \n"
			}
		}
		// A failure for an attachment the line does not carry is ignored.
		errors["absent.bin"] = "unrelated"
		line := fmt.Sprintf(
			`{"account":"+15551230000","envelope":{"source":"+15551234567","timestamp":1700000000123,"dataMessage":{"timestamp":1700000000123,"attachments":[%s]}}}`,
			strings.Join(attachments, ","),
		)
		record, _, err := BuildSignalIngressWithAttachmentErrors(
			signalDecoderAccountID, 1, signalSelf, []byte(line), "+15551234567", "", errors, signalDecoderReceivedAt,
		)
		if err != nil || !json.Valid(record.Payload) {
			return false
		}
		var frame signalJSONRPCFrame
		if err := json.Unmarshal(record.Payload, &frame); err != nil || !reflect.DeepEqual(frame.AttachmentErrors, errors) {
			return false
		}
		events, err := NewSignalDecoder().Decode(context.Background(), *record)
		if err != nil || len(events) != 1 || events[0].Message == nil {
			return false
		}
		for _, attachment := range events[0].Message.Attachments {
			if attachment.TransportError != strings.TrimSpace(errors[attachment.RemoteID]) {
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(20261021))}); err != nil {
		t.Fatal(err)
	}
}

func TestSignalWorkerRecordsReceiveTimeAttachmentFailure(t *testing.T) {
	harness := newSignalWorkerHarness(t, "attachment-failure.sqlite3")
	record, _, err := BuildSignalIngressWithAttachmentErrors(
		signalDecoderAccountID, 1, signalSelf, []byte(signalAttachmentLine), "+15551234567", "",
		map[string]string{"failedRemote.html": "java.io.IOException: Connection reset"},
		signalDecoderReceivedAt,
	)
	if err != nil {
		t.Fatalf("BuildSignalIngressWithAttachmentErrors(): %v", err)
	}
	if err := harness.sink.AppendIngress(context.Background(), *record); err != nil {
		t.Fatalf("AppendIngress(): %v", err)
	}
	harness.start(t)

	conversation := "signal:+15551234567"
	messageID := v2keys.DeriveID("message", signalDecoderAccountID,
		conversation+"\x1f"+v2keys.SignalIncomingSourceID(conversation, "+15551234567", 1_700_000_000_123))
	attachments, err := sqlite.NewMessageAttachmentRepository(harness.store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	read := func(ordinal int64) (sqlite.MessageAttachment, error) {
		return attachments.GetForDownload(context.Background(), messageID, ordinal)
	}
	waitSignalCondition(t, "projected attachments", func() bool {
		_, err := read(1)
		return err == nil
	})
	failed, err := read(0)
	if err != nil {
		t.Fatalf("GetForDownload(0): %v", err)
	}
	if failed.RemoteID != "failedRemote.html" || failed.LastError == nil ||
		*failed.LastError != "receive: java.io.IOException: Connection reset" ||
		failed.SizeBytes == nil || *failed.SizeBytes != 85841 || failed.State != "pending" {
		t.Fatalf("failed attachment = %+v, want pending, 85841 bytes, receive-time cause", failed)
	}
	fine, err := read(1)
	if err != nil || fine.LastError != nil {
		t.Fatalf("fine attachment = %+v, %v; want no last_error", fine, err)
	}

	// A replay of the same envelope without the call's stderr (WAL and
	// recovery replays keep only JSON lines) must not erase the cause.
	replay, _, err := BuildSignalIngress(signalDecoderAccountID, 2, signalSelf, []byte(signalAttachmentLine), "+15551234567", "", signalDecoderReceivedAt)
	if err != nil {
		t.Fatalf("BuildSignalIngress(replay): %v", err)
	}
	replay.DedupeKey += ":replay"
	if err := harness.sink.AppendIngress(context.Background(), *replay); err != nil {
		t.Fatalf("AppendIngress(replay): %v", err)
	}
	waitSignalCondition(t, "replay processed", func() bool {
		pending, err := harness.messages.Unprocessed(context.Background())
		return err == nil && len(pending) == 0
	})
	after, err := read(0)
	if err != nil || after.LastError == nil || *after.LastError != *failed.LastError {
		t.Fatalf("after replay attachment = %+v, %v; want last_error %q kept", after, err, *failed.LastError)
	}
}
