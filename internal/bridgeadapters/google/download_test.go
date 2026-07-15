package google

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
)

var _ bridge.MediaDownloader = (*Adapter)(nil)

func TestDownloadMediaGoogleOpaqueV1RoundTripAndStreamWrapping(t *testing.T) {
	opaque, err := json.Marshal(googleDownloadOpaqueV1{
		V:             1,
		MediaID:       "media-id/with:punctuation",
		DecryptionKey: "0001aBff",
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if want := `{"v":1,"media_id":"media-id/with:punctuation","decryption_key":"0001aBff"}`; string(opaque) != want {
		t.Fatalf("opaque = %s, want %s", opaque, want)
	}

	fake := &fakeDownloadClient{
		data: []byte("downloaded Google media"),
		mime: " image/transport ",
	}
	a := newDownloadTestAdapter(t, fake)
	stream, err := a.DownloadMedia(context.Background(), "google-primary", bridge.MediaRef{
		RemoteID: "attachment-row-id-is-not-the-transport-id",
		Opaque:   opaque,
		Filename: "photo.jpg",
		MIME:     "image/from-ref",
	})
	if err != nil {
		t.Fatalf("DownloadMedia() error = %v", err)
	}
	if fake.calls != 1 || fake.mediaID != "media-id/with:punctuation" {
		t.Fatalf("transport calls/media ID = (%d, %q), want (1, media-id/with:punctuation)", fake.calls, fake.mediaID)
	}
	if want := []byte{0x00, 0x01, 0xab, 0xff}; !bytes.Equal(fake.key, want) {
		t.Fatalf("transport decryption key = %x, want %x", fake.key, want)
	}
	if stream.ReadCloser == nil {
		t.Fatal("MediaStream.ReadCloser = nil on success")
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll(MediaStream) error = %v", err)
	}
	if string(data) != "downloaded Google media" || stream.Size != int64(len(data)) {
		t.Fatalf("stream bytes/size = (%q, %d), want downloaded bytes/%d", data, stream.Size, len(data))
	}
	if stream.Filename != "photo.jpg" || stream.MIME != "image/transport" {
		t.Fatalf("stream metadata = (%q, %q), want (photo.jpg, image/transport)", stream.Filename, stream.MIME)
	}
}

func TestDownloadMediaMIMEFallbackOrder(t *testing.T) {
	tests := []struct {
		name          string
		transportMIME string
		refMIME       string
		want          string
	}{
		{name: "transport wins", transportMIME: " image/transport ", refMIME: "image/ref", want: "image/transport"},
		{name: "reference fallback", refMIME: " image/ref ", want: "image/ref"},
		{name: "both empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeDownloadClient{data: []byte("x"), mime: test.transportMIME}
			a := newDownloadTestAdapter(t, fake)
			stream, err := a.DownloadMedia(context.Background(), "google-primary", bridge.MediaRef{
				Opaque: googleDownloadOpaque(t, 1, "media-id", "00"),
				MIME:   test.refMIME,
			})
			if err != nil {
				t.Fatalf("DownloadMedia() error = %v", err)
			}
			defer stream.Close()
			if stream.MIME != test.want {
				t.Fatalf("MediaStream.MIME = %q, want %q", stream.MIME, test.want)
			}
		})
	}
}

func TestDownloadMediaNilDataStillReturnsNonNilReadCloser(t *testing.T) {
	fake := &fakeDownloadClient{}
	a := newDownloadTestAdapter(t, fake)
	stream, err := a.DownloadMedia(context.Background(), "google-primary", bridge.MediaRef{
		Opaque: googleDownloadOpaque(t, 1, "media-id", "00"),
	})
	if err != nil {
		t.Fatalf("DownloadMedia() error = %v", err)
	}
	if stream.ReadCloser == nil {
		t.Fatal("DownloadMedia() returned nil ReadCloser with nil error")
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll(MediaStream) error = %v", err)
	}
	if len(data) != 0 || stream.Size != 0 {
		t.Fatalf("nil transport data wrapped as (%d bytes, size %d), want empty stream", len(data), stream.Size)
	}
}

func TestDownloadMediaUnsupportedOpaqueVersionFailsClosed(t *testing.T) {
	fake := &fakeDownloadClient{}
	a := newDownloadTestAdapter(t, fake)
	_, err := a.DownloadMedia(context.Background(), "google-primary", bridge.MediaRef{
		Opaque: googleDownloadOpaque(t, 2, "media-id", "00"),
	})
	failure := requireDownloadOpError(t, err)
	if failure.Class != bridge.FailureUnsupported ||
		failure.Operation != "download_media" ||
		failure.Fingerprint != "opaque_version_unsupported" ||
		failure.Dispatch != "" {
		t.Fatalf("failure = %+v, want terminal unsupported opaque_version_unsupported", failure)
	}
	if fake.calls != 0 {
		t.Fatalf("DownloadMedia transport calls = %d, want 0", fake.calls)
	}
}

func TestDownloadMediaMalformedOpaqueIsTerminalPreCall(t *testing.T) {
	tests := []struct {
		name   string
		opaque []byte
	}{
		{name: "malformed JSON", opaque: []byte(`{"v":1`)},
		{name: "invalid UTF-8", opaque: []byte{'{', '"', 'v', '"', ':', '1', ',', '"', 'x', '"', ':', '"', 0xff, '"', '}'}},
		{name: "wrong JSON shape", opaque: []byte(`[1,2,3]`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeDownloadClient{}
			a := newDownloadTestAdapter(t, fake)
			_, err := a.DownloadMedia(context.Background(), "google-primary", bridge.MediaRef{Opaque: test.opaque})
			failure := requireDownloadOpError(t, err)
			if failure.Class != bridge.FailureUnsupported ||
				failure.Operation != "download_media" ||
				failure.Fingerprint != "google_opaque_malformed" ||
				failure.Dispatch != "" {
				t.Fatalf("failure = %+v, want terminal unsupported google_opaque_malformed", failure)
			}
			if fake.calls != 0 {
				t.Fatalf("DownloadMedia transport calls = %d, want 0", fake.calls)
			}
		})
	}
}

func TestDownloadMediaInvalidOpaqueFieldsAreTerminalPreCall(t *testing.T) {
	tests := []struct {
		name        string
		mediaID     string
		key         string
		fingerprint string
	}{
		{name: "missing media id", key: "00", fingerprint: "google_opaque_media_id_missing"},
		{name: "blank media id", mediaID: "  ", key: "00", fingerprint: "google_opaque_media_id_missing"},
		{name: "missing key", mediaID: "media-id", fingerprint: "google_opaque_decryption_key_missing"},
		{name: "non-hex key", mediaID: "media-id", key: "not-hex", fingerprint: "google_opaque_decryption_key_invalid"},
		{name: "odd-length key", mediaID: "media-id", key: "abc", fingerprint: "google_opaque_decryption_key_invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeDownloadClient{}
			a := newDownloadTestAdapter(t, fake)
			_, err := a.DownloadMedia(context.Background(), "google-primary", bridge.MediaRef{
				Opaque: googleDownloadOpaque(t, 1, test.mediaID, test.key),
			})
			failure := requireDownloadOpError(t, err)
			if failure.Class != bridge.FailureUnsupported ||
				failure.Operation != "download_media" ||
				failure.Fingerprint != test.fingerprint ||
				failure.Dispatch != "" {
				t.Fatalf("failure = %+v, want terminal unsupported %s", failure, test.fingerprint)
			}
			if fake.calls != 0 {
				t.Fatalf("DownloadMedia transport calls = %d, want 0", fake.calls)
			}
		})
	}
}

func TestDownloadMediaNotReadyDoesNotConstructOrReachTransport(t *testing.T) {
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	fake := &fakeDownloadClient{}
	probe := installDownloadClient(t, legacy, fake)
	a := New("google-primary", host, func() bool { return true })
	a.newClient = func() (*client.Client, transportClient, error) {
		t.Fatal("DownloadMedia must not construct a Google transport")
		return nil, nil, nil
	}

	stream, err := a.DownloadMedia(context.Background(), "google-primary", bridge.MediaRef{
		Opaque: googleDownloadOpaque(t, 1, "media-id", "00"),
	})
	if stream.ReadCloser != nil {
		stream.Close()
		t.Fatal("failed DownloadMedia returned a non-zero stream")
	}
	failure := requireDownloadOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Operation != "download_media" ||
		failure.Fingerprint != "google_not_connected" ||
		failure.Dispatch != "" {
		t.Fatalf("failure = %+v, want transient google_not_connected", failure)
	}
	if probe.calls != 0 || fake.calls != 0 {
		t.Fatalf("seam/transport calls = (%d, %d), want (0, 0)", probe.calls, fake.calls)
	}
}

func TestDownloadMediaContextGuardsAreClassifiedPreCall(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name        string
		ctx         context.Context
		fingerprint string
	}{
		{name: "nil", fingerprint: "google_download_context_invalid"},
		{name: "done", ctx: done, fingerprint: "google_download_context_done"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeDownloadClient{}
			a := newDownloadTestAdapter(t, fake)
			_, err := a.DownloadMedia(test.ctx, "google-primary", bridge.MediaRef{
				Opaque: googleDownloadOpaque(t, 1, "media-id", "00"),
			})
			failure := requireDownloadOpError(t, err)
			if failure.Class != bridge.FailureTransient ||
				failure.Operation != "download_media" ||
				failure.Fingerprint != test.fingerprint ||
				failure.Dispatch != "" {
				t.Fatalf("failure = %+v, want transient pre-call %s", failure, test.fingerprint)
			}
			if fake.calls != 0 {
				t.Fatalf("DownloadMedia transport calls = %d, want 0", fake.calls)
			}
		})
	}
}

func TestDownloadMediaTransientFailureDoesNotRetireGeneration(t *testing.T) {
	host := newTestApp(t)
	lifecycleTransport := &fakeTransport{}
	a := newTestAdapter(t, host, lifecycleTransport)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })
	lifecycleTransport.emit(&gmproto.Conversation{ConversationID: "ready"})
	<-run.Ready()

	fake := &fakeDownloadClient{err: errors.New("media server timeout")}
	installDownloadClient(t, host.GetClient(), fake)
	_, err = a.DownloadMedia(context.Background(), "google-primary", bridge.MediaRef{
		Opaque: googleDownloadOpaque(t, 1, "media-id", "00"),
	})
	failure := requireDownloadOpError(t, err)
	if failure.Class != bridge.FailureTransient ||
		failure.Operation != "download_media" ||
		failure.Fingerprint != "google_media_download_failed" ||
		failure.Dispatch != "" {
		t.Fatalf("failure = %+v, want plain transient google_media_download_failed", failure)
	}
	if !host.Connected.Load() {
		t.Fatal("transient download failure marked the receive generation disconnected")
	}
	select {
	case terminal := <-run.Done():
		t.Fatalf("transient download failure retired the receive generation with %v", terminal)
	default:
	}
	if host.GoogleStatus().NeedsRepair {
		t.Fatal("transient download failure parked the Google lifecycle")
	}
}

func TestDownloadMediaAuthExpiryReportsToLifecycle(t *testing.T) {
	host := newTestApp(t)
	lifecycleTransport := &fakeTransport{}
	a := newTestAdapter(t, host, lifecycleTransport)
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	lifecycleTransport.emit(&gmproto.Conversation{ConversationID: "ready"})
	<-run.Ready()

	fake := &fakeDownloadClient{err: errors.New("Google returned HTTP 401 for DownloadMedia")}
	installDownloadClient(t, host.GetClient(), fake)
	_, err = a.DownloadMedia(context.Background(), "google-primary", bridge.MediaRef{
		Opaque: googleDownloadOpaque(t, 1, "media-id", "00"),
	})
	failure := requireDownloadOpError(t, err)
	if failure.Class != bridge.FailureCredentialsExpired ||
		failure.Fingerprint != "google_auth_expired" ||
		failure.Operation != "download_media" ||
		failure.Dispatch != "" {
		t.Fatalf("failure = %+v, want credentials-expired google_auth_expired", failure)
	}
	select {
	case terminal := <-run.Done():
		terminalFailure, ok := asOpError(terminal)
		if !ok || (terminalFailure.Class != bridge.FailureCredentialsExpired &&
			terminalFailure.Class != bridge.FailureUpgradeRequired) {
			t.Fatalf("terminal = %v, want credentials_expired/upgrade_required", terminal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auth-expired download failure did not reach the lifecycle owner")
	}
}

func googleDownloadOpaque(t *testing.T, version int, mediaID, key string) []byte {
	t.Helper()
	opaque, err := json.Marshal(googleDownloadOpaqueV1{
		V:             version,
		MediaID:       mediaID,
		DecryptionKey: key,
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return opaque
}

func newDownloadTestAdapter(t *testing.T, fake *fakeDownloadClient) *Adapter {
	t.Helper()
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	host.Connected.Store(true)
	installDownloadClient(t, legacy, fake)
	return New("google-primary", host, func() bool { return true })
}

type downloadClientSeamProbe struct {
	calls int
}

func installDownloadClient(t *testing.T, want *client.Client, fake downloadClient) *downloadClientSeamProbe {
	t.Helper()
	probe := &downloadClientSeamProbe{}
	previous := downloadClientFor
	downloadClientFor = func(got *client.Client) downloadClient {
		probe.calls++
		if got != want {
			t.Fatalf("download client source = %p, want App.GetClient() %p", got, want)
		}
		return fake
	}
	t.Cleanup(func() { downloadClientFor = previous })
	return probe
}

func requireDownloadOpError(t *testing.T, err error) bridge.OpError {
	t.Helper()
	if err == nil {
		t.Fatal("DownloadMedia() error = nil, want classified failure")
	}
	failure, ok := asOpError(err)
	if !ok {
		t.Fatalf("DownloadMedia() error = %T %v, want bridge.OpError", err, err)
	}
	return failure
}

type fakeDownloadClient struct {
	data []byte
	mime string
	err  error

	calls   int
	mediaID string
	key     []byte
}

func (f *fakeDownloadClient) DownloadMedia(mediaID string, key []byte) ([]byte, string, error) {
	f.calls++
	f.mediaID = mediaID
	f.key = append([]byte(nil), key...)
	return f.data, f.mime, f.err
}
