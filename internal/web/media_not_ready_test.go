package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/maxghenis/openmessage/internal/bridge"
)

// The 2026-10-24 symptom: an image Google had not named yet answered
// /api/media with 501 "account bridge cannot download media", and the UI took
// that as final. It is temporary: every media route answers 503 with a
// Retry-After the UI waits out, HEAD included (the UI probes with HEAD), and
// the same request succeeds once the reference arrives.
func TestV2MediaNotNamedYetIs503WithRetryAfterNotA501(t *testing.T) {
	var named atomic.Bool
	handler := newMediaGoneHandler(t, 0, func(context.Context, string, bridge.MediaRef) (bridge.MediaStream, error) {
		if !named.Load() {
			return bridge.MediaStream{}, fmt.Errorf("%w: Google has sent no media ID for this attachment yet", bridge.ErrMediaNotReady)
		}
		return bridge.MediaStream{ReadCloser: io.NopCloser(strings.NewReader("<p>report</p>")), MIME: "text/html"}, nil
	})
	paths := []string{
		"/api/media/" + mediaGoneMessageID,
		"/api/media/v2msg:" + mediaGoneMessageID + ":0?v=0123456789ab&retry=1",
		"/api/v1/messages/" + mediaGoneMessageID + "/attachments/0",
	}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			recorder := serveMediaRequest(handler, method, path, nil, nil)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s %s status = %d, want 503; body=%s", method, path, recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Retry-After"); got != "2" {
				t.Fatalf("%s %s Retry-After = %q, want \"2\"", method, path, got)
			}
			if method == http.MethodGet && !strings.Contains(errorBody(t, recorder), "not available from its transport yet") {
				t.Fatalf("%s %s error = %q, want the not-ready reason", method, path, errorBody(t, recorder))
			}
		}
	}

	named.Store(true)
	recorder := serveMediaRequest(handler, http.MethodGet, paths[1], nil, nil)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "<p>report</p>" {
		t.Fatalf("GET after the media is named = %d %q, want 200 with the bytes", recorder.Code, recorder.Body.String())
	}
}
