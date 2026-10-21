package messaging

import (
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/client"
)

// libgm gives up on a request the phone never answers after
// client.RequestTimeout. That has to come before this package's bound on the
// whole transport call, with room for the HTTP send that precedes the wait, so
// the Google adapter, which knows whether anything was sent, classifies the
// failure rather than the dispatcher abandoning the call as uncertain. The
// fork's HTTP client waits up to 20 s for response headers; 5 s more is margin.
func TestLibgmGivesUpBeforeTheTransportBound(t *testing.T) {
	const httpSendRoom = 20*time.Second + 5*time.Second
	if client.RequestTimeout+httpSendRoom > defaultTransportCallTimeout {
		t.Fatalf("client.RequestTimeout (%s) + %s for the HTTP send exceeds the %s transport bound",
			client.RequestTimeout, httpSendRoom, defaultTransportCallTimeout)
	}
}
