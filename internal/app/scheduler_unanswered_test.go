package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

// A scheduled SMS whose SendMessage the phone never answered, or that a
// disconnect cut off, may still go out: the server had accepted it. It fails
// once and is never sent again, because each retry builds a new TmpID and
// could deliver it twice.
func TestUnansweredScheduledSendIsNotResent(t *testing.T) {
	for _, reason := range []error{libgm.ErrPhoneNotResponding, libgm.ErrConnectionClosed} {
		t.Run(reason.Error(), func(t *testing.T) {
			a := newSchedulerTestApp(t)
			seedDue(t, a, "s1")
			sends := 0
			a.sendTextOverride = func(cid, body, rt string) (*db.Message, error) {
				sends++
				return nil, googleSendStepError(&libgm.UnansweredRequestError{Action: gmproto.ActionType_SEND_MESSAGE, Reason: reason})
			}
			for i := 0; i < scheduleMaxAttempts+1; i++ {
				a.processDueScheduledMessages()
			}
			got, _ := a.Store.GetScheduledMessage("s1")
			if sends != 1 {
				t.Fatalf("SendMessage attempts = %d, want 1", sends)
			}
			if got.Status != db.ScheduleStatusFailed || !strings.Contains(got.LastError, "may have gone out") {
				t.Fatalf("after an unanswered send = %+v, want failed as possibly sent", got)
			}
		})
	}
}

// A conversation lookup the phone never answered sent nothing, so the
// scheduler retries it as before, up to its attempt budget.
func TestUnansweredScheduledLookupIsRetried(t *testing.T) {
	a := newSchedulerTestApp(t)
	seedDue(t, a, "s1")
	sends := 0
	a.sendTextOverride = func(cid, body, rt string) (*db.Message, error) {
		sends++
		return nil, fmt.Errorf("get conversation: %w", &libgm.UnansweredRequestError{
			Action: gmproto.ActionType_GET_CONVERSATION, Reason: libgm.ErrPhoneNotResponding,
		})
	}
	a.processDueScheduledMessages()
	if got, _ := a.Store.GetScheduledMessage("s1"); got.Status != db.ScheduleStatusPending || got.Attempts != 1 {
		t.Fatalf("after an unanswered lookup = %+v, want pending for a retry", got)
	}
	for i := 0; i < scheduleMaxAttempts; i++ {
		a.processDueScheduledMessages()
	}
	if sends != scheduleMaxAttempts {
		t.Fatalf("attempts = %d, want the full budget %d", sends, scheduleMaxAttempts)
	}
}

func TestGoogleSendStepErrorMarksOnlyUnansweredSends(t *testing.T) {
	for _, c := range []struct {
		err         error
		unconfirmed bool
	}{
		{&libgm.UnansweredRequestError{Action: gmproto.ActionType_SEND_MESSAGE, Reason: libgm.ErrPhoneNotResponding}, true},
		{&libgm.UnansweredRequestError{Action: gmproto.ActionType_SEND_MESSAGE, Reason: libgm.ErrConnectionClosed}, true},
		{errors.New("HTTP 401: invalid authentication credentials"), false},
		{errors.New("connection reset"), false},
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
