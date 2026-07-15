package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"

	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

func TestAdapterImplementsMediaDownloader(t *testing.T) {
	var _ bridge.MediaDownloader = (*Adapter)(nil)
}

func TestNewWiresDownloadMediaRefSeam(t *testing.T) {
	a := New("whatsapp-primary", &whatsapplive.Bridge{})
	if a.downloadMediaRef == nil {
		t.Fatal("New() did not wire the retained DownloadMediaRef method")
	}
}

func TestWhatsAppMediaOpaqueV1RoundTrip(t *testing.T) {
	want := whatsappMediaOpaqueV1{
		Kind:          "remote",
		DirectPath:    "/mms/image",
		FileSHA256:    "0506",
		FileEncSHA256: "0304",
		MediaKey:      "0102",
		FileLength:    9,
		LocalRef:      "",
	}
	raw, err := marshalWhatsAppMediaOpaqueV1(want)
	if err != nil {
		t.Fatalf("marshalWhatsAppMediaOpaqueV1(): %v", err)
	}
	const wantJSON = `{"v":1,"kind":"remote","direct_path":"/mms/image","file_sha256":"0506","file_enc_sha256":"0304","media_key":"0102","file_length":9,"local_ref":""}`
	if string(raw) != wantJSON {
		t.Fatalf("opaque JSON = %s, want %s", raw, wantJSON)
	}

	got, err := unmarshalWhatsAppMediaOpaque(raw)
	if err != nil {
		t.Fatalf("unmarshalWhatsAppMediaOpaque(): %v", err)
	}
	want.V = 1
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip payload = %#v, want %#v", got, want)
	}
}

func TestWhatsAppMediaOpaqueV1AcceptsExplicitZeroFileLength(t *testing.T) {
	got, err := unmarshalWhatsAppMediaOpaque([]byte(`{"v":1,"kind":"remote","direct_path":"/mms/empty","file_sha256":"","file_enc_sha256":"03","media_key":"01","file_length":0,"local_ref":""}`))
	if err != nil {
		t.Fatalf("unmarshalWhatsAppMediaOpaque() explicit zero file_length: %v", err)
	}
	if got.FileLength != 0 {
		t.Fatalf("file_length = %d, want explicit zero preserved", got.FileLength)
	}
}

func TestDownloadMediaRemoteRoutesExactOpaqueFieldsAndWrapsStream(t *testing.T) {
	opaque, err := marshalWhatsAppMediaOpaqueV1(whatsappMediaOpaqueV1{
		Kind:          "remote",
		DirectPath:    "/mms/image",
		FileSHA256:    "0506",
		FileEncSHA256: "0304",
		MediaKey:      "0102",
		FileLength:    9,
		LocalRef:      "unused-local-ref",
	})
	if err != nil {
		t.Fatal(err)
	}

	var gotKind, gotMediaKey, gotMIME, gotLocalRef string
	var gotRef whatsapplive.StoredMediaRef
	a := &Adapter{
		mediaReady: func() bool { return true },
		downloadMediaRef: func(kind string, ref whatsapplive.StoredMediaRef, mediaKeyHex, mime, localRef string) ([]byte, string, error) {
			gotKind, gotRef, gotMediaKey, gotMIME, gotLocalRef = kind, ref, mediaKeyHex, mime, localRef
			return []byte("image-bytes"), "image/transport", nil
		},
	}

	stream, err := a.DownloadMedia(context.Background(), "whatsapp-primary", bridge.MediaRef{
		Opaque:   opaque,
		Filename: "photo.jpg",
		MIME:     "image/fallback",
	})
	if err != nil {
		t.Fatalf("DownloadMedia(): %v", err)
	}
	defer stream.Close()
	data, err := io.ReadAll(stream.ReadCloser)
	if err != nil {
		t.Fatalf("read MediaStream: %v", err)
	}
	if !bytes.Equal(data, []byte("image-bytes")) || stream.Size != 11 || stream.Filename != "photo.jpg" || stream.MIME != "image/transport" {
		t.Fatalf("MediaStream = bytes=%q size=%d filename=%q mime=%q", data, stream.Size, stream.Filename, stream.MIME)
	}
	if gotKind != "remote" || gotMediaKey != "0102" || gotMIME != "image/fallback" || gotLocalRef != "unused-local-ref" {
		t.Fatalf("DownloadMediaRef scalar args = kind=%q key=%q mime=%q local=%q", gotKind, gotMediaKey, gotMIME, gotLocalRef)
	}
	if gotRef.DirectPath != "/mms/image" || gotRef.FileSHA256 != "0506" || gotRef.FileEncSHA256 != "0304" || gotRef.FileLength != 9 {
		t.Fatalf("DownloadMediaRef stored ref = %#v", gotRef)
	}
}

func TestDownloadMediaLocalRoutingAndMIMEFallback(t *testing.T) {
	opaque, err := marshalWhatsAppMediaOpaqueV1(whatsappMediaOpaqueV1{
		Kind:     "local",
		LocalRef: "walocal:dGVzdA",
	})
	if err != nil {
		t.Fatal(err)
	}
	var gotKind, gotLocalRef string
	a := &Adapter{
		mediaReady: func() bool { return true },
		downloadMediaRef: func(kind string, _ whatsapplive.StoredMediaRef, _ string, _ string, localRef string) ([]byte, string, error) {
			gotKind, gotLocalRef = kind, localRef
			return []byte("local-bytes"), "", nil
		},
	}

	stream, err := a.DownloadMedia(context.Background(), "whatsapp-primary", bridge.MediaRef{
		Opaque:   opaque,
		Filename: "voice.opus",
		MIME:     "audio/ogg",
	})
	if err != nil {
		t.Fatalf("DownloadMedia(): %v", err)
	}
	defer stream.Close()
	if gotKind != "local" || gotLocalRef != "walocal:dGVzdA" {
		t.Fatalf("DownloadMediaRef routing = kind=%q local=%q", gotKind, gotLocalRef)
	}
	if stream.MIME != "audio/ogg" {
		t.Fatalf("MediaStream MIME = %q, want ref fallback audio/ogg", stream.MIME)
	}
}

func TestDownloadMediaAlwaysReturnsNonNilReadCloserOnSuccess(t *testing.T) {
	opaque, err := marshalWhatsAppMediaOpaqueV1(whatsappMediaOpaqueV1{Kind: "local", LocalRef: "walocal:dGVzdA"})
	if err != nil {
		t.Fatal(err)
	}
	a := &Adapter{
		mediaReady: func() bool { return true },
		downloadMediaRef: func(string, whatsapplive.StoredMediaRef, string, string, string) ([]byte, string, error) {
			return nil, "", nil
		},
	}
	stream, err := a.DownloadMedia(context.Background(), "whatsapp-primary", bridge.MediaRef{Opaque: opaque})
	if err != nil {
		t.Fatalf("DownloadMedia(): %v", err)
	}
	if stream.ReadCloser == nil {
		t.Fatal("DownloadMedia() returned nil ReadCloser with nil error")
	}
	defer stream.Close()
	if stream.Size != 0 {
		t.Fatalf("MediaStream size = %d, want zero", stream.Size)
	}
}

func TestDownloadMediaRequiresReadyTransportWithoutLifecycleAction(t *testing.T) {
	var connectCalls, downloadCalls int
	a := &Adapter{
		connect: func(context.Context) error {
			connectCalls++
			return nil
		},
		mediaReady: func() bool { return false },
		downloadMediaRef: func(string, whatsapplive.StoredMediaRef, string, string, string) ([]byte, string, error) {
			downloadCalls++
			return nil, "", nil
		},
	}

	stream, err := a.DownloadMedia(context.Background(), "whatsapp-primary", bridge.MediaRef{})
	if stream.ReadCloser != nil {
		t.Fatal("DownloadMedia() returned a stream on readiness failure")
	}
	failure, ok := asOpError(err)
	if !ok || failure.Class != bridge.FailureTransient || failure.Operation != "download_media" ||
		failure.Fingerprint != "whatsapp_not_connected" || failure.Dispatch != "" ||
		!errors.Is(failure.Cause, whatsmeow.ErrNotConnected) {
		t.Fatalf("DownloadMedia() error = %v, want passive not-connected failure", err)
	}
	if connectCalls != 0 || downloadCalls != 0 {
		t.Fatalf("connect/download calls = %d/%d, want zero/zero", connectCalls, downloadCalls)
	}
}

func TestDownloadMediaContextGuardsRunBeforeDecodeAndTransport(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name            string
		ctx             context.Context
		wantFingerprint string
		wantCause       error
	}{
		{name: "nil", ctx: nil, wantFingerprint: "whatsapp_download_context_missing"},
		{name: "done", ctx: done, wantFingerprint: "whatsapp_download_context_done", wantCause: context.Canceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			a := &Adapter{
				mediaReady: func() bool { return true },
				downloadMediaRef: func(string, whatsapplive.StoredMediaRef, string, string, string) ([]byte, string, error) {
					calls++
					return nil, "", nil
				},
			}
			_, err := a.DownloadMedia(test.ctx, "whatsapp-primary", bridge.MediaRef{Opaque: []byte("not-json")})
			failure, ok := asOpError(err)
			if !ok || failure.Class != bridge.FailureTransient || failure.Operation != "download_media" ||
				failure.Fingerprint != test.wantFingerprint || failure.Dispatch != "" {
				t.Fatalf("DownloadMedia() error = %v, want context failure %q", err, test.wantFingerprint)
			}
			if test.wantCause != nil && !errors.Is(failure.Cause, test.wantCause) {
				t.Fatalf("failure cause = %v, want %v", failure.Cause, test.wantCause)
			}
			if calls != 0 {
				t.Fatalf("transport calls = %d, want zero", calls)
			}
		})
	}
}

func TestDownloadMediaRejectsUnsupportedOpaqueVersion(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"v":2,"kind":"local","local_ref":"walocal:dGVzdA"}`),
		[]byte(`{"kind":"local","local_ref":"walocal:dGVzdA"}`),
	} {
		a := &Adapter{
			mediaReady: func() bool { return true },
			downloadMediaRef: func(string, whatsapplive.StoredMediaRef, string, string, string) ([]byte, string, error) {
				t.Fatal("transport called for unsupported Opaque version")
				return nil, "", nil
			},
		}
		_, err := a.DownloadMedia(context.Background(), "whatsapp-primary", bridge.MediaRef{Opaque: raw})
		failure, ok := asOpError(err)
		if !ok || failure.Class != bridge.FailureUnsupported || failure.Operation != "download_media" ||
			failure.Fingerprint != "opaque_version_unsupported" || failure.Dispatch != "" {
			t.Fatalf("DownloadMedia() error = %v, want terminal unsupported version", err)
		}
	}
}

func TestDownloadMediaRejectsMalformedOrIncompleteOpaqueBeforeTransport(t *testing.T) {
	invalidUTF8 := append([]byte(`{"v":1,"kind":"local","local_ref":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	tests := []struct {
		name            string
		raw             []byte
		wantFingerprint string
	}{
		{name: "malformed JSON", raw: []byte(`{"v":1`), wantFingerprint: "whatsapp_opaque_malformed"},
		{name: "invalid UTF-8", raw: invalidUTF8, wantFingerprint: "whatsapp_opaque_malformed"},
		{name: "unknown kind", raw: []byte(`{"v":1,"kind":"future"}`), wantFingerprint: "whatsapp_opaque_kind_unsupported"},
		{name: "whitespace-wrapped kind", raw: []byte(`{"v":1,"kind":" remote ","direct_path":"/mms","file_enc_sha256":"03","media_key":"01","file_length":1}`), wantFingerprint: "whatsapp_opaque_kind_unsupported"},
		{name: "remote direct path missing", raw: []byte(`{"v":1,"kind":"remote","file_enc_sha256":"03","media_key":"01","file_length":1}`), wantFingerprint: "whatsapp_opaque_remote_invalid"},
		{name: "remote encrypted hash missing", raw: []byte(`{"v":1,"kind":"remote","direct_path":"/mms","media_key":"01","file_length":1}`), wantFingerprint: "whatsapp_opaque_remote_invalid"},
		{name: "remote media key missing", raw: []byte(`{"v":1,"kind":"remote","direct_path":"/mms","file_enc_sha256":"03","file_length":1}`), wantFingerprint: "whatsapp_opaque_remote_invalid"},
		{name: "remote file length missing", raw: []byte(`{"v":1,"kind":"remote","direct_path":"/mms","file_enc_sha256":"03","media_key":"01"}`), wantFingerprint: "whatsapp_opaque_remote_invalid"},
		{name: "invalid encrypted hash hex", raw: []byte(`{"v":1,"kind":"remote","direct_path":"/mms","file_enc_sha256":"xyz","media_key":"01","file_length":1}`), wantFingerprint: "whatsapp_opaque_hex_invalid"},
		{name: "invalid optional plaintext hash hex", raw: []byte(`{"v":1,"kind":"remote","direct_path":"/mms","file_sha256":"xyz","file_enc_sha256":"03","media_key":"01","file_length":1}`), wantFingerprint: "whatsapp_opaque_hex_invalid"},
		{name: "invalid media key hex", raw: []byte(`{"v":1,"kind":"remote","direct_path":"/mms","file_enc_sha256":"03","media_key":"xyz","file_length":1}`), wantFingerprint: "whatsapp_opaque_hex_invalid"},
		{name: "local ref missing", raw: []byte(`{"v":1,"kind":"local"}`), wantFingerprint: "whatsapp_opaque_local_invalid"},
		{name: "local ref encoding malformed", raw: []byte(`{"v":1,"kind":"local","local_ref":"walocal:not*base64"}`), wantFingerprint: "whatsapp_opaque_local_invalid"},
		{name: "local ref escapes root", raw: []byte(`{"v":1,"kind":"local","local_ref":"walocal:Li4vLi4vZXNjYXBl"}`), wantFingerprint: "whatsapp_opaque_local_invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			a := &Adapter{
				mediaReady: func() bool { return true },
				downloadMediaRef: func(string, whatsapplive.StoredMediaRef, string, string, string) ([]byte, string, error) {
					calls++
					return nil, "", nil
				},
			}
			_, err := a.DownloadMedia(context.Background(), "whatsapp-primary", bridge.MediaRef{Opaque: test.raw})
			failure, ok := asOpError(err)
			if !ok || failure.Class != bridge.FailureUnsupported || failure.Operation != "download_media" ||
				failure.Fingerprint != test.wantFingerprint || failure.Dispatch != "" {
				t.Fatalf("DownloadMedia() error = %v, want terminal structural failure %q", err, test.wantFingerprint)
			}
			if calls != 0 {
				t.Fatalf("transport calls = %d, want zero", calls)
			}
		})
	}
}

func TestDownloadMediaClearsDispatchCertaintyFromWrappedOpError(t *testing.T) {
	opaque, err := marshalWhatsAppMediaOpaqueV1(whatsappMediaOpaqueV1{Kind: "local", LocalRef: "walocal:dGVzdA"})
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("lower-layer failure")
	wrapped := fmt.Errorf("wrapped retained error: %w", bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "lower_operation",
		Fingerprint: "lower_fingerprint",
		Dispatch:    bridge.DispatchNotCalled,
		Cause:       cause,
	})
	a := &Adapter{
		mediaReady: func() bool { return true },
		downloadMediaRef: func(string, whatsapplive.StoredMediaRef, string, string, string) ([]byte, string, error) {
			return nil, "", wrapped
		},
	}

	_, err = a.DownloadMedia(context.Background(), "whatsapp-primary", bridge.MediaRef{Opaque: opaque})
	failure, ok := asOpError(err)
	if !ok || failure.Class != bridge.FailureTransient || failure.Operation != "lower_operation" ||
		failure.Fingerprint != "lower_fingerprint" || failure.Dispatch != "" || !errors.Is(failure.Cause, cause) {
		t.Fatalf("DownloadMedia() error = %v, want wrapped classification with empty dispatch certainty", err)
	}
}

func TestDownloadMediaTransportClassificationDoesNotRetireReceiveGeneration(t *testing.T) {
	opaque, err := marshalWhatsAppMediaOpaqueV1(whatsappMediaOpaqueV1{Kind: "local", LocalRef: "walocal:dGVzdA"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name            string
		err             error
		wantClass       bridge.FailureClass
		wantFingerprint string
	}{
		{name: "plain transient", err: errors.New("media CDN unavailable"), wantClass: bridge.FailureTransient, wantFingerprint: "whatsapp_download_media_failed"},
		{name: "terminal session error", err: wastore.ErrDeviceDeleted, wantClass: bridge.FailureReauthRequired, wantFingerprint: "whatsapp_session_invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ready := make(chan struct{})
			close(ready)
			r := &run{ready: ready, finish: make(chan error, 1), admitting: true}
			a := &Adapter{
				current:    r,
				mediaReady: func() bool { return true },
				downloadMediaRef: func(string, whatsapplive.StoredMediaRef, string, string, string) ([]byte, string, error) {
					return nil, "", test.err
				},
			}
			stream, err := a.DownloadMedia(context.Background(), "whatsapp-primary", bridge.MediaRef{Opaque: opaque})
			if stream.ReadCloser != nil {
				t.Fatal("DownloadMedia() returned stream on transport error")
			}
			failure, ok := asOpError(err)
			if !ok || failure.Class != test.wantClass || failure.Operation != "download_media" ||
				failure.Fingerprint != test.wantFingerprint || failure.Dispatch != "" || !errors.Is(failure.Cause, test.err) {
				t.Fatalf("DownloadMedia() error = %v, want class=%q fingerprint=%q", err, test.wantClass, test.wantFingerprint)
			}
			select {
			case reported := <-r.finish:
				t.Fatalf("DownloadMedia() retired the receive generation with %v", reported)
			default:
			}
		})
	}
}
