package client

import (
	"errors"
	"fmt"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// ErrNoResponsePayload and ResponsePayloadError come from the old
// MaxGhenis/gmessages fork, which failed data requests the phone answered only
// with payload-less frames. Upstream libgm (v0.2611) does not produce them; the
// types stay so pull-health classification and its tests keep compiling and
// work again if that behavior is ported upstream.
var ErrNoResponsePayload = errors.New("phone answered without a response payload")

type ResponsePayloadError struct {
	Action        gmproto.ActionType
	Frames        int
	AccountSwitch bool
	Waited        time.Duration
}

func (e *ResponsePayloadError) Error() string {
	reason := "no response payload"
	if e.AccountSwitch {
		reason = "no response payload; the phone sent a Google-account switch notice instead"
	}
	return fmt.Sprintf("%s: %s after %d frame(s) in %s", e.Action, reason, e.Frames, e.Waited.Round(time.Millisecond))
}

func (e *ResponsePayloadError) Is(target error) bool { return target == ErrNoResponsePayload }
