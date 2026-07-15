package signal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/signallive"
)

func TestAdapterImplementsMediaDownloader(t *testing.T) {
	var _ bridge.MediaDownloader = (*Adapter)(nil)
}

func TestSignalDownloadOpaqueV1PinsWaveFourWireShape(t *testing.T) {
	opaque, err := json.Marshal(signalDownloadOpaqueV1{
		Version:        1,
		Kind:           "remote",
		AttachmentID:   "attachment-1",
		Path:           "",
		ConversationID: "signal:+15551234567",
	})
	if err != nil {
		t.Fatalf("marshal Signal v1 Opaque: %v", err)
	}
	const want = `{"v":1,"kind":"remote","att_id":"attachment-1","path":"","conversation_id":"signal:+15551234567"}`
	if string(opaque) != want {
		t.Fatalf("Signal v1 Opaque = %s, want %s", opaque, want)
	}

	poller := readyDownloadPoller()
	poller.downloadData = []byte("wire-contract")
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	stream, err := adapter.DownloadMedia(context.Background(), "signal-primary", bridge.MediaRef{Opaque: opaque})
	if err != nil {
		t.Fatalf("DownloadMedia(v1 Opaque): %v", err)
	}
	defer stream.Close()
	if got := poller.lastDownloadRequest(); got != (fakeDownloadRequest{
		account:      "+15551230000",
		kind:         "remote",
		attachmentID: "attachment-1",
		target:       "+15551234567",
	}) {
		t.Fatalf("DownloadMediaRef request = %+v, want decoded v1 fields", got)
	}
}

func TestDownloadMediaRoutesVersionOneOpaqueReferences(t *testing.T) {
	tests := []struct {
		name      string
		opaque    map[string]any
		refMIME   string
		transport string
		want      fakeDownloadRequest
		wantMIME  string
	}{
		{
			name: "remote recipient",
			opaque: map[string]any{
				"v":               1,
				"kind":            "remote",
				"att_id":          "attachment-recipient",
				"conversation_id": "signal:+15551234567",
			},
			refMIME:   " image/png ",
			transport: " image/transport ",
			want: fakeDownloadRequest{
				account:      "+15551230000",
				kind:         "remote",
				attachmentID: "attachment-recipient",
				target:       "+15551234567",
			},
			wantMIME: "image/transport",
		},
		{
			name: "remote group",
			opaque: map[string]any{
				"v":               1,
				"kind":            "remote",
				"att_id":          "attachment-group",
				"conversation_id": "signal-group:group-token",
			},
			refMIME:   " video/mp4 ",
			transport: "   ",
			want: fakeDownloadRequest{
				account:      "+15551230000",
				kind:         "remote",
				attachmentID: "attachment-group",
				target:       "group-token",
				isGroup:      true,
			},
			wantMIME: "video/mp4",
		},
		{
			name: "local path",
			opaque: map[string]any{
				"v":    1,
				"kind": "local",
				"path": "/tmp/local Signal attachment",
			},
			refMIME: "audio/ogg",
			want: fakeDownloadRequest{
				account: "+15551230000",
				kind:    "local",
				path:    "/tmp/local Signal attachment",
			},
			wantMIME: "audio/ogg",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opaque, err := json.Marshal(tc.opaque)
			if err != nil {
				t.Fatalf("marshal v1 opaque payload: %v", err)
			}
			poller := readyDownloadPoller()
			poller.downloadData = []byte("signal attachment bytes")
			poller.downloadMIME = tc.transport
			adapter := &Adapter{accountID: "signal-primary", poller: poller}

			stream, err := adapter.DownloadMedia(context.Background(), "signal-primary", bridge.MediaRef{
				RemoteID: "remote-row-id",
				Opaque:   opaque,
				Filename: "attachment.bin",
				MIME:     tc.refMIME,
			})
			if err != nil {
				t.Fatalf("DownloadMedia() error = %v", err)
			}
			if stream.ReadCloser == nil {
				t.Fatal("DownloadMedia() returned a nil ReadCloser with nil error")
			}
			t.Cleanup(func() { _ = stream.Close() })
			data, err := io.ReadAll(stream)
			if err != nil {
				t.Fatalf("read MediaStream: %v", err)
			}
			if !bytes.Equal(data, poller.downloadData) {
				t.Fatalf("MediaStream bytes = %q, want %q", data, poller.downloadData)
			}
			if stream.Size != int64(len(poller.downloadData)) ||
				stream.Filename != "attachment.bin" || stream.MIME != tc.wantMIME {
				t.Fatalf("MediaStream metadata = {Size:%d Filename:%q MIME:%q}", stream.Size, stream.Filename, stream.MIME)
			}
			if got := poller.downloadCallCount(); got != 1 {
				t.Fatalf("DownloadMediaRef calls = %d, want 1", got)
			}
			if got := poller.lastDownloadRequest(); got != tc.want {
				t.Fatalf("DownloadMediaRef request = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDownloadMediaRejectsUnsupportedOpaqueVersionBeforeTransport(t *testing.T) {
	poller := readyDownloadPoller()
	adapter := &Adapter{accountID: "signal-primary", poller: poller}

	_, err := adapter.DownloadMedia(context.Background(), "signal-primary", bridge.MediaRef{
		Opaque: []byte(`{"v":2,"kind":"local","path":"/tmp/attachment"}`),
	})
	assertDownloadOpError(t, err, bridge.FailureUnsupported, "opaque_version_unsupported")
	if got := poller.downloadCallCount(); got != 0 {
		t.Fatalf("DownloadMediaRef calls = %d, want 0 for unsupported version", got)
	}
}

func TestDownloadMediaRejectsStructurallyInvalidOpaqueBeforeTransport(t *testing.T) {
	tests := []struct {
		name            string
		opaque          []byte
		wantFingerprint string
	}{
		{
			name:            "invalid UTF-8",
			opaque:          []byte{'{', '"', 'v', '"', ':', '1', ',', '"', 'x', '"', ':', '"', 0xff, '"', '}'},
			wantFingerprint: "signal_download_media_opaque_invalid_utf8",
		},
		{
			name:            "malformed JSON",
			opaque:          []byte(`{"v":1`),
			wantFingerprint: "signal_download_media_opaque_malformed",
		},
		{
			name:            "version is not an integer",
			opaque:          []byte(`{"v":"1","kind":"local","path":"/tmp/a"}`),
			wantFingerprint: "signal_download_media_opaque_malformed",
		},
		{
			name:            "kind missing",
			opaque:          []byte(`{"v":1,"path":"/tmp/a"}`),
			wantFingerprint: "signal_download_media_kind_missing",
		},
		{
			name:            "unknown kind",
			opaque:          []byte(`{"v":1,"kind":"archive","path":"/tmp/a"}`),
			wantFingerprint: "signal_download_media_kind_unsupported",
		},
		{
			name:            "whitespace-wrapped kind",
			opaque:          []byte(`{"v":1,"kind":" remote ","att_id":"attachment","conversation_id":"signal:+15551234567"}`),
			wantFingerprint: "signal_download_media_kind_unsupported",
		},
		{
			name:            "remote attachment missing",
			opaque:          []byte(`{"v":1,"kind":"remote","conversation_id":"signal:+15551234567"}`),
			wantFingerprint: "signal_download_media_remote_missing_field",
		},
		{
			name:            "remote conversation missing",
			opaque:          []byte(`{"v":1,"kind":"remote","att_id":"attachment"}`),
			wantFingerprint: "signal_download_media_remote_missing_field",
		},
		{
			name:            "remote conversation invalid",
			opaque:          []byte(`{"v":1,"kind":"remote","att_id":"attachment","conversation_id":"whatsapp:+15551234567"}`),
			wantFingerprint: "signal_download_media_conversation_invalid",
		},
		{
			name:            "local path missing",
			opaque:          []byte(`{"v":1,"kind":"local"}`),
			wantFingerprint: "signal_download_media_local_missing_field",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			poller := readyDownloadPoller()
			adapter := &Adapter{accountID: "signal-primary", poller: poller}

			_, err := adapter.DownloadMedia(context.Background(), "signal-primary", bridge.MediaRef{Opaque: tc.opaque})
			assertDownloadOpError(t, err, bridge.FailureUnsupported, tc.wantFingerprint)
			if got := poller.downloadCallCount(); got != 0 {
				t.Fatalf("DownloadMediaRef calls = %d, want 0 for invalid Opaque", got)
			}
		})
	}
}

func TestDownloadMediaRequiresReadyRetainedBridge(t *testing.T) {
	tests := []struct {
		name   string
		poller poller
	}{
		{name: "nil retained bridge"},
		{name: "not connected", poller: newFakePoller()},
		{
			name: "connected without account",
			poller: &fakePoller{
				started: make(chan *fakePollerRun, 1),
				status:  signallive.StatusSnapshot{Connected: true},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &Adapter{accountID: "signal-primary", poller: tc.poller}
			stream, err := adapter.DownloadMedia(context.Background(), "signal-primary", bridge.MediaRef{
				Opaque: []byte(`{"v":1,"kind":"remote","att_id":"attachment","conversation_id":"signal:+15551234567"}`),
			})
			if stream.ReadCloser != nil {
				t.Fatal("DownloadMedia() failure returned a non-nil ReadCloser")
			}
			assertDownloadOpError(t, err, bridge.FailureTransient, "signal_not_connected")
			if fake, ok := tc.poller.(*fakePoller); ok && fake.downloadCallCount() != 0 {
				t.Fatalf("DownloadMediaRef calls = %d, want 0 while not ready", fake.downloadCallCount())
			}
		})
	}
}

func TestDownloadMediaRejectsInvalidContextBeforeTransport(t *testing.T) {
	tests := []struct {
		name            string
		context         func() context.Context
		wantFingerprint string
	}{
		{
			name:            "nil context",
			context:         func() context.Context { return nil },
			wantFingerprint: "signal_download_media_context_missing",
		},
		{
			name: "done context",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantFingerprint: "signal_download_media_context_done",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			poller := readyDownloadPoller()
			adapter := &Adapter{accountID: "signal-primary", poller: poller}

			_, err := adapter.DownloadMedia(tc.context(), "signal-primary", bridge.MediaRef{
				Opaque: []byte(`{"v":1,"kind":"local","path":"/tmp/attachment"}`),
			})
			assertDownloadOpError(t, err, bridge.FailureTransient, tc.wantFingerprint)
			if got := poller.downloadCallCount(); got != 0 {
				t.Fatalf("DownloadMediaRef calls = %d, want 0 after context failure", got)
			}
		})
	}
}

func TestDownloadMediaClassifiesTransportFailureWithoutWideningC6(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "local read failure", err: errors.New("read local Signal attachment: unavailable")},
		{name: "recipient failure", err: signallive.NewCommandError("download Signal attachment: recipient is not registered")},
		{name: "network failure", err: signallive.NewCommandError("download Signal attachment: Connection reset by peer")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			poller := readyDownloadPoller()
			poller.downloadErr = tc.err
			adapter := &Adapter{accountID: "signal-primary", poller: poller}
			run, err := adapter.Start(context.Background(), bridge.StartRequest{
				AccountID: "signal-primary", Generation: 1,
			}, nil)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			receiveValue(t, poller.started, "retained poller start")
			t.Cleanup(func() { stopAdapterRun(t, run) })

			_, err = adapter.DownloadMedia(context.Background(), "signal-primary", bridge.MediaRef{
				Opaque: []byte(`{"v":1,"kind":"remote","att_id":"attachment","conversation_id":"signal:+15551234567"}`),
			})
			failure := assertDownloadOpError(t, err, bridge.FailureTransient, "signal_download_media_failed")
			if !errors.Is(failure.Cause, tc.err) {
				t.Fatalf("DownloadMedia() cause = %v, want %v", failure.Cause, tc.err)
			}
			select {
			case terminal := <-run.Done():
				t.Fatalf("non-account download failure retired receive generation: %v", terminal)
			default:
			}
			if got := poller.appliedCount(); got != 0 {
				t.Fatalf("non-account download failure applied %d lifecycle transitions, want 0", got)
			}
		})
	}
}

func TestDownloadMediaReportsOnlyUnambiguousLocalAccountFailure(t *testing.T) {
	poller := readyDownloadPoller()
	poller.downloadErr = signallive.NewCommandError("download Signal attachment: authorization failed")
	adapter := &Adapter{accountID: "signal-primary", poller: poller}
	run, err := adapter.Start(context.Background(), bridge.StartRequest{
		AccountID: "signal-primary", Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	receiveValue(t, poller.started, "retained poller start")

	_, err = adapter.DownloadMedia(context.Background(), "signal-primary", bridge.MediaRef{
		Opaque: []byte(`{"v":1,"kind":"remote","att_id":"attachment","conversation_id":"signal:+15551234567"}`),
	})
	assertDownloadOpError(t, err, bridge.FailureTransient, "signal_download_media_failed")
	terminal := receiveValue(t, run.Done(), "local-account download terminal")
	assertOpError(t, terminal, bridge.FailureTransient, "signal_send_account_check")
	if got := poller.appliedCount(); got != 1 {
		t.Fatalf("local-account download status projections = %d, want 1", got)
	}
}

func TestDownloadMediaAlwaysReturnsReaderOnSuccess(t *testing.T) {
	poller := readyDownloadPoller()
	poller.downloadData = nil
	adapter := &Adapter{accountID: "signal-primary", poller: poller}

	stream, err := adapter.DownloadMedia(context.Background(), "signal-primary", bridge.MediaRef{
		Opaque: []byte(`{"v":1,"kind":"local","path":"/tmp/empty"}`),
	})
	if err != nil {
		t.Fatalf("DownloadMedia() error = %v", err)
	}
	if stream.ReadCloser == nil {
		t.Fatal("DownloadMedia() returned nil ReadCloser with nil error")
	}
	defer stream.Close()
	if stream.Size != 0 {
		t.Fatalf("MediaStream.Size = %d, want 0", stream.Size)
	}
}

func readyDownloadPoller() *fakePoller {
	poller := newFakePoller()
	poller.status = signallive.StatusSnapshot{
		Connected: true,
		Paired:    true,
		Account:   "+15551230000",
	}
	return poller
}

func assertDownloadOpError(
	t *testing.T,
	err error,
	wantClass bridge.FailureClass,
	wantFingerprint string,
) bridge.OpError {
	t.Helper()
	var failure bridge.OpError
	if !errors.As(err, &failure) {
		t.Fatalf("DownloadMedia() error = %v (%T), want bridge.OpError", err, err)
	}
	if failure.Class != wantClass || failure.Operation != "download_media" ||
		failure.Fingerprint != wantFingerprint || failure.Dispatch != "" {
		t.Fatalf("DownloadMedia() OpError = %+v, want class %q operation download_media fingerprint %q", failure, wantClass, wantFingerprint)
	}
	return failure
}
