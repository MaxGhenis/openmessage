package app

import (
	"testing"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/signallive"
)

type recordingSignalLifecycleNotifier struct {
	reported []error
}

func (n *recordingSignalLifecycleNotifier) ReportError(err error) bool {
	n.reported = append(n.reported, err)
	return true
}

func TestSignalLifecycleNotifierCoversCentralSendPaths(t *testing.T) {
	live, err := signallive.New(t.TempDir(), nil, zerolog.Nop(), signallive.Callbacks{})
	if err != nil {
		t.Fatalf("signallive.New(): %v", err)
	}
	t.Cleanup(func() { _ = live.Close() })

	a := &App{Signal: live, Logger: zerolog.Nop()}
	notifier := &recordingSignalLifecycleNotifier{}
	a.SetSignalLifecycleNotifier(notifier)

	_, _ = a.SendSignalText("signal:+15551234567", "", "")
	_, _ = a.SendSignalMedia("signal:+15551234567", nil, "", "", "", "")
	_ = a.SendSignalReaction("signal:+15551234567", "", "👍", "add")

	if got := len(notifier.reported); got != 3 {
		t.Fatalf("Signal send-path notifications = %d, want 3", got)
	}
}

func TestSignalLifecycleNotifierCanBeCleared(t *testing.T) {
	a := &App{}
	notifier := &recordingSignalLifecycleNotifier{}
	a.SetSignalLifecycleNotifier(notifier)
	a.SetSignalLifecycleNotifier(nil)
	a.reportSignalLifecycleError(assertSignalNotifierError{})
	if len(notifier.reported) != 0 {
		t.Fatalf("cleared Signal notifier was invoked: %v", notifier.reported)
	}
}

type assertSignalNotifierError struct{}

func (assertSignalNotifierError) Error() string { return "test Signal failure" }
