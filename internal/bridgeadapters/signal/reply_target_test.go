package signal

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/signallive"
)

func TestReplyTargetCarriesEveryMessageRefField(t *testing.T) {
	if got := ReplyTarget(nil); got != (signallive.ReplyTarget{}) {
		t.Fatalf("ReplyTarget(nil) = %+v, want no reply", got)
	}

	ref := bridge.MessageRef{
		RemoteID:       "f48818f15483f503bb133d92360ca8e2fbd8287e",
		AuthorID:       "+15551234567",
		SentAt:         time.UnixMilli(1_700_000_000_123),
		Text:           "quoted",
		HasAttachment:  true,
		AttachmentMIME: "image/jpeg",
	}
	got := reflect.ValueOf(ReplyTarget(&ref))
	source := reflect.ValueOf(ref)
	// Every MessageRef field must reach signallive under the same name, so a
	// field added to the contract cannot be dropped here silently.
	for index := 0; index < source.NumField(); index++ {
		name := source.Type().Field(index).Name
		field := got.FieldByName(name)
		if !field.IsValid() {
			t.Fatalf("signallive.ReplyTarget has no %s field", name)
		}
		if !reflect.DeepEqual(field.Interface(), source.Field(index).Interface()) {
			t.Fatalf("ReplyTarget().%s = %v, want %v", name, field.Interface(), source.Field(index).Interface())
		}
		if source.Field(index).IsZero() {
			t.Fatalf("fixture leaves MessageRef.%s zero; set it so the copy is checked", name)
		}
	}
	if got.NumField() != source.NumField() {
		t.Fatalf("ReplyTarget has %d fields, MessageRef %d", got.NumField(), source.NumField())
	}
}

func TestSendTextAndMediaHandTheDescribedReplyToThePoller(t *testing.T) {
	poller := newFakePoller()
	poller.status = signallive.StatusSnapshot{Connected: true, Paired: true, Account: "+15551230000"}
	poller.textTimestamp = 1700000000124
	poller.mediaTimestamp = 1700000000125
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	reply := &bridge.MessageRef{
		RemoteID: "1700000000555",
		SentAt:   time.UnixMilli(1_700_000_000_000),
		Text:     "sent through the v2 outbox",
	}
	want := signallive.ReplyTarget{
		RemoteID: "1700000000555",
		SentAt:   time.UnixMilli(1_700_000_000_000),
		Text:     "sent through the v2 outbox",
	}

	if _, err := adapter.SendText(context.Background(), bridge.TextRequest{
		AccountID:    "signal-primary",
		Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
		Body:         "replying",
		ReplyTo:      reply,
	}); err != nil {
		t.Fatalf("SendText(): %v", err)
	}
	if got := poller.lastTextRequest().reply; got != want {
		t.Fatalf("text reply handed to signallive = %+v, want %+v", got, want)
	}

	content := []byte("png")
	if _, err := adapter.SendMedia(context.Background(), bridge.MediaRequest{
		AccountID:    "signal-primary",
		Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
		Reader:       bytes.NewReader(content),
		Size:         int64(len(content)),
		Filename:     "reply.png",
		MIME:         "image/png",
		ReplyTo:      reply,
	}); err != nil {
		t.Fatalf("SendMedia(): %v", err)
	}
	if got := poller.lastMediaRequest().reply; got != want {
		t.Fatalf("media reply handed to signallive = %+v, want %+v", got, want)
	}

	if _, err := adapter.SendText(context.Background(), bridge.TextRequest{
		AccountID:    "signal-primary",
		Conversation: bridge.ConversationRef{RemoteID: "signal:+15551234567"},
		Body:         "no reply",
	}); err != nil {
		t.Fatalf("SendText(no reply): %v", err)
	}
	if got := poller.lastTextRequest().reply; got != (signallive.ReplyTarget{}) {
		t.Fatalf("text without a reply handed %+v, want none", got)
	}
}
