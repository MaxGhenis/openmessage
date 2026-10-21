package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

// fakeGoogleSend replaces the scheduler's Google calls (lookup, upload, send)
// for one test and counts the sends.
type fakeGoogleSend struct {
	lookupErr error
	sendErr   error
	sends     int
	uploads   int
}

func installFakeGoogleSend(t *testing.T, fake *fakeGoogleSend) {
	t.Helper()
	lookup, send, upload := getGoogleConversationForSend, sendGoogleTextPayload, uploadGoogleMedia
	t.Cleanup(func() {
		getGoogleConversationForSend, sendGoogleTextPayload, uploadGoogleMedia = lookup, send, upload
	})
	getGoogleConversationForSend = func(*App, string) (*gmproto.Conversation, error) {
		if fake.lookupErr != nil {
			return nil, fake.lookupErr
		}
		return &gmproto.Conversation{}, nil
	}
	sendGoogleTextPayload = func(*App, *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
		fake.sends++
		if fake.sendErr != nil {
			return nil, fake.sendErr
		}
		return &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS}, nil
	}
	uploadGoogleMedia = func(*App, []byte, string, string) (*gmproto.MediaContent, error) {
		fake.uploads++
		return &gmproto.MediaContent{MediaID: "media"}, nil
	}
}

func seedDueMedia(t *testing.T, a *App, id string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if err := a.Store.CreateScheduledMessage(&db.ScheduledMessage{
		ID: id, ConversationID: "c1", SendAt: now - 1000,
		Status: db.ScheduleStatusPending, CreatedAt: now,
		MediaData: []byte{0x89, 0x50, 0x4e, 0x47}, MediaFilename: "pic.png", MediaMime: "image/png",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// A scheduled SMS (text or media) whose SendMessage may have reached Google
// (the phone never answered, a disconnect cut it off, or a network error
// ended it) fails once and is never sent again: each retry builds a new TmpID
// and could deliver it twice. This drives the real sendSMSText and
// sendSMSMedia through their Google hooks.
func TestPossiblySentScheduledSMSIsNotResent(t *testing.T) {
	reasons := []error{
		&libgm.UnansweredRequestError{Action: gmproto.ActionType_SEND_MESSAGE, Reason: libgm.ErrPhoneNotResponding},
		&libgm.UnansweredRequestError{Action: gmproto.ActionType_SEND_MESSAGE, Reason: libgm.ErrConnectionClosed},
		errors.New("Post \"https://instantmessaging-pa.googleapis.com\": connection reset by peer"),
	}
	for _, kind := range []string{"text", "media"} {
		for _, reason := range reasons {
			t.Run(kind+"/"+reason.Error(), func(t *testing.T) {
				a := newSchedulerTestApp(t)
				fake := &fakeGoogleSend{sendErr: reason}
				installFakeGoogleSend(t, fake)
				if kind == "text" {
					seedDue(t, a, "s1")
				} else {
					seedDueMedia(t, a, "s1")
				}
				for i := 0; i < scheduleMaxAttempts+1; i++ {
					a.processDueScheduledMessages()
				}
				got, _ := a.Store.GetScheduledMessage("s1")
				if fake.sends != 1 {
					t.Fatalf("SendMessage attempts = %d, want 1", fake.sends)
				}
				if got.Status != db.ScheduleStatusFailed || !strings.Contains(got.LastError, "may have gone out") {
					t.Fatalf("after a possibly sent SMS = %+v, want failed as possibly sent", got)
				}
			})
		}
	}
}

// A send Google refused (auth) or never made (no client) was not accepted, and
// a conversation lookup the phone never answered sent nothing: the scheduler
// retries those as before, up to its attempt budget.
func TestNotSentScheduledSMSIsRetried(t *testing.T) {
	for _, c := range []struct {
		name string
		fake *fakeGoogleSend
	}{
		{"unanswered lookup", &fakeGoogleSend{lookupErr: &libgm.UnansweredRequestError{
			Action: gmproto.ActionType_GET_CONVERSATION, Reason: libgm.ErrPhoneNotResponding,
		}}},
		{"auth rejection", &fakeGoogleSend{sendErr: errors.New("HTTP 401: invalid authentication credentials")}},
		{"no client", &fakeGoogleSend{sendErr: fmt.Errorf(ErrNotConnected)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newSchedulerTestApp(t)
			installFakeGoogleSend(t, c.fake)
			seedDue(t, a, "s1")
			a.processDueScheduledMessages()
			if got, _ := a.Store.GetScheduledMessage("s1"); got.Status != db.ScheduleStatusPending || got.Attempts != 1 {
				t.Fatalf("after a failure that sent nothing = %+v, want pending for a retry", got)
			}
			for i := 0; i < scheduleMaxAttempts; i++ {
				a.processDueScheduledMessages()
			}
			if got, _ := a.Store.GetScheduledMessage("s1"); got.Status != db.ScheduleStatusFailed || got.Attempts+1 < scheduleMaxAttempts {
				t.Fatalf("after the budget = %+v, want failed after %d attempts", got, scheduleMaxAttempts)
			}
		})
	}
}

func TestGoogleSendStepErrorRetriesOnlyRefusedSends(t *testing.T) {
	for _, c := range []struct {
		err         error
		unconfirmed bool
	}{
		{&libgm.UnansweredRequestError{Action: gmproto.ActionType_SEND_MESSAGE, Reason: libgm.ErrPhoneNotResponding}, true},
		{&libgm.UnansweredRequestError{Action: gmproto.ActionType_SEND_MESSAGE, Reason: libgm.ErrConnectionClosed}, true},
		{errors.New("connection reset"), true},
		{errors.New("HTTP 500: internal error"), true},
		{errors.New("HTTP 401: invalid authentication credentials"), false},
		{errors.New("session cookie expired"), false},
		{fmt.Errorf(ErrNotConnected), false},
	} {
		err := googleSendStepError(c.err)
		if got := errors.Is(err, errScheduledSendUnconfirmed); got != c.unconfirmed {
			t.Errorf("googleSendStepError(%v) unconfirmed = %t, want %t", c.err, got, c.unconfirmed)
		}
		if !errors.Is(err, c.err) {
			t.Errorf("googleSendStepError(%v) = %v, lost its cause", c.err, err)
		}
	}
}
