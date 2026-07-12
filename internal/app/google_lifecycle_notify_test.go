package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

type recordingGoogleLifecycleNotifier struct {
	reported []error
	parked   []error
}

func (n *recordingGoogleLifecycleNotifier) ReportError(err error) bool {
	n.reported = append(n.reported, err)
	return true
}

func (n *recordingGoogleLifecycleNotifier) ParkCurrent(err error) bool {
	n.parked = append(n.parked, err)
	return true
}

func TestGoogleLifecycleNotifierReportsAuthExpiryOnce(t *testing.T) {
	a := &App{Logger: zerolog.Nop()}
	notifier := &recordingGoogleLifecycleNotifier{}
	a.SetGoogleLifecycleNotifier(notifier)

	transient := errors.New("dial tcp: i/o timeout")
	if a.HandleGoogleAuthExpiredError(transient) {
		t.Fatal("transient error treated as auth expiry")
	}
	if len(notifier.reported) != 0 || len(notifier.parked) != 0 {
		t.Fatal("transient auth check notified the lifecycle")
	}

	authErr := errors.New("send message: HTTP 401: invalid authentication credentials")
	if !a.HandleGoogleAuthExpiredError(authErr) {
		t.Fatal("auth-expiry error was not handled")
	}
	if len(notifier.reported) != 1 || notifier.reported[0] != authErr {
		t.Fatalf("reported = %v, want exactly [%v]", notifier.reported, authErr)
	}
	if len(notifier.parked) != 0 {
		t.Fatalf("auth expiry parked lifecycle: %v", notifier.parked)
	}

	if !a.HandleGoogleAuthExpiredError(authErr) {
		t.Fatal("repeated auth-expiry error was not handled")
	}
	if len(notifier.reported) != 1 {
		t.Fatalf("repeated auth expiry reported %d times, want 1", len(notifier.reported))
	}
}

func TestGoogleLifecycleNotifierParksOnRepairThresholdOnce(t *testing.T) {
	a := &App{}
	notifier := &recordingGoogleLifecycleNotifier{}
	a.SetGoogleLifecycleNotifier(notifier)

	for range googleRepairThreshold - 1 {
		a.RecordGoogleSendOutcome(false)
	}
	if len(notifier.parked) != 0 {
		t.Fatalf("parked before repair threshold: %v", notifier.parked)
	}

	a.RecordGoogleSendOutcome(false)
	if len(notifier.parked) != 1 {
		t.Fatalf("park notifications = %d, want 1", len(notifier.parked))
	}
	if !strings.Contains(notifier.parked[0].Error(), "repeatedly failed") {
		t.Fatalf("park error = %q, want repeated-send detail", notifier.parked[0])
	}

	a.RecordGoogleSendOutcome(false)
	if len(notifier.parked) != 1 {
		t.Fatalf("failure after threshold produced %d park notifications, want 1", len(notifier.parked))
	}

	a.RecordGoogleSendOutcome(true)
	for range googleRepairThreshold {
		a.RecordGoogleSendOutcome(false)
	}
	if len(notifier.parked) != 2 {
		t.Fatalf("new failure streak produced %d total park notifications, want 2", len(notifier.parked))
	}
}

func TestGoogleLifecycleNotifierKeepsPhoneOfflineFailuresRecoverable(t *testing.T) {
	a := &App{}
	notifier := &recordingGoogleLifecycleNotifier{}
	a.SetGoogleLifecycleNotifier(notifier)

	for range googleRepairThreshold * 2 {
		a.RecordGoogleSendOutcomeWithPhone(false, false)
	}
	if len(notifier.parked) != 0 || len(notifier.reported) != 0 {
		t.Fatalf("phone-offline failures notified lifecycle: parked=%v reported=%v", notifier.parked, notifier.reported)
	}
	if a.googleNeedsRepair.Load() {
		t.Fatal("phone-offline failures marked Google for repair")
	}
}

func TestGoogleLifecycleNotifierParksAuthInvalidSendErrorNotTransient(t *testing.T) {
	a := &App{}
	notifier := &recordingGoogleLifecycleNotifier{}
	a.SetGoogleLifecycleNotifier(notifier)

	a.RecordGoogleSendError(errors.New("dial tcp: i/o timeout"))
	if len(notifier.parked) != 0 || len(notifier.reported) != 0 {
		t.Fatalf("transient send error notified lifecycle: parked=%v reported=%v", notifier.parked, notifier.reported)
	}

	authErr := errors.New("rpc error: code = Unauthenticated desc = linked device rejected")
	a.RecordGoogleSendError(authErr)
	if len(notifier.parked) != 1 || notifier.parked[0] != authErr {
		t.Fatalf("parked = %v, want exactly [%v]", notifier.parked, authErr)
	}
	if len(notifier.reported) != 0 {
		t.Fatalf("auth-invalid send error was reported for refresh instead of parked: %v", notifier.reported)
	}

	a.RecordGoogleSendError(authErr)
	if len(notifier.parked) != 1 {
		t.Fatalf("repeated auth-invalid error parked %d times, want 1", len(notifier.parked))
	}
}

func TestGoogleLifecycleNotifierCanBeCleared(t *testing.T) {
	a := &App{}
	notifier := &recordingGoogleLifecycleNotifier{}
	a.SetGoogleLifecycleNotifier(notifier)
	a.SetGoogleLifecycleNotifier(nil)

	a.RecordGoogleSendError(errors.New("rpc error: code = Unauthenticated"))
	if len(notifier.parked) != 0 || len(notifier.reported) != 0 {
		t.Fatalf("cleared notifier was invoked: parked=%v reported=%v", notifier.parked, notifier.reported)
	}
}
