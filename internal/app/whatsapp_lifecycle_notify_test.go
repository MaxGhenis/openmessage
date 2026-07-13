package app

import (
	"errors"
	"sync"
	"testing"
)

type recordingWhatsAppLifecycleNotifier struct {
	mu       sync.Mutex
	reported []error
}

func (n *recordingWhatsAppLifecycleNotifier) ReportError(err error) bool {
	n.mu.Lock()
	n.reported = append(n.reported, err)
	n.mu.Unlock()
	return true
}

func (n *recordingWhatsAppLifecycleNotifier) errors() []error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]error(nil), n.reported...)
}

func TestWhatsAppLifecycleNotifierReportsConnectionError(t *testing.T) {
	a := &App{}
	notifier := &recordingWhatsAppLifecycleNotifier{}
	a.SetWhatsAppLifecycleNotifier(notifier)

	want := errors.New("WhatsApp transport disconnected")
	a.reportWhatsAppLifecycleError(want)

	reported := notifier.errors()
	if len(reported) != 1 || reported[0] != want {
		t.Fatalf("reported = %v, want exactly [%v]", reported, want)
	}
}

func TestWhatsAppLifecycleNotifierCanBeReplacedAndCleared(t *testing.T) {
	a := &App{}
	first := &recordingWhatsAppLifecycleNotifier{}
	second := &recordingWhatsAppLifecycleNotifier{}
	a.SetWhatsAppLifecycleNotifier(first)
	a.SetWhatsAppLifecycleNotifier(second)

	want := errors.New("send failed")
	a.reportWhatsAppLifecycleError(want)
	if reported := first.errors(); len(reported) != 0 {
		t.Fatalf("replaced notifier was invoked: %v", reported)
	}
	if reported := second.errors(); len(reported) != 1 || reported[0] != want {
		t.Fatalf("replacement reported = %v, want exactly [%v]", reported, want)
	}

	a.SetWhatsAppLifecycleNotifier(nil)
	a.reportWhatsAppLifecycleError(errors.New("ignored after clear"))
	if reported := second.errors(); len(reported) != 1 {
		t.Fatalf("cleared notifier was invoked: %v", reported)
	}
}
