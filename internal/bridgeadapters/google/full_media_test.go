package google

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
)

type fakeFullSizeImageClient struct {
	err   error
	calls [][2]string
}

func (f *fakeFullSizeImageClient) GetFullSizeImage(messageID, actionMessageID string) (*gmproto.GetFullSizeImageResponse, error) {
	f.calls = append(f.calls, [2]string{messageID, actionMessageID})
	return &gmproto.GetFullSizeImageResponse{}, f.err
}

func newFullMediaTestAdapter(t *testing.T, fake *fakeFullSizeImageClient) *Adapter {
	t.Helper()
	host := newTestApp(t)
	legacy := newLegacyClient(t)
	generation := host.BeginGoogleGeneration(legacy)
	t.Cleanup(generation.Release)
	host.Connected.Store(true)
	previous := fullSizeImageClientFor
	fullSizeImageClientFor = func(got *client.Client) fullSizeImageClient {
		if got != legacy {
			t.Fatalf("full-size image client source = %p, want App.GetClient() %p", got, legacy)
		}
		return fake
	}
	t.Cleanup(func() { fullSizeImageClientFor = previous })
	return New("google-primary", host, func() bool { return true })
}

func googleOpaque(t *testing.T, payload googleDownloadOpaqueV1) []byte {
	t.Helper()
	opaque, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal(): %v", err)
	}
	return opaque
}

// A thumbnail reference asks the phone for the full-size image of exactly its
// message part, as upstream mautrix-gmessages does after a thumbnail.
func TestRequestFullMediaAsksForThePartOfAThumbnail(t *testing.T) {
	fake := &fakeFullSizeImageClient{}
	adapter := newFullMediaTestAdapter(t, fake)
	opaque := googleOpaque(t, googleDownloadOpaqueV1{
		V: 1, MediaID: "thumb-1", DecryptionKey: "0a", MessageID: "89926", PartID: "88846", Preview: true,
	})
	if err := adapter.RequestFullMedia(context.Background(), "google-primary", bridge.MediaRef{RemoteID: "thumb-1", Opaque: opaque}); err != nil {
		t.Fatalf("RequestFullMedia(): %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0] != [2]string{"89926", "88846"} {
		t.Fatalf("GetFullSizeImage calls = %v, want one for message 89926 part 88846", fake.calls)
	}
}

// Nothing is asked for a reference that is not a thumbnail, or one packed
// before references carried the message and part.
func TestRequestFullMediaAsksNothingWithoutAThumbnailPart(t *testing.T) {
	for name, payload := range map[string]googleDownloadOpaqueV1{
		"full-size":       {V: 1, MediaID: "full-1", DecryptionKey: "0a", MessageID: "89926", PartID: "88846"},
		"no message ID":   {V: 1, MediaID: "thumb-1", DecryptionKey: "0a", PartID: "88846", Preview: true},
		"no part ID":      {V: 1, MediaID: "thumb-1", DecryptionKey: "0a", MessageID: "89926", Preview: true},
		"packed long ago": {V: 1, MediaID: "thumb-1", DecryptionKey: "0a"},
	} {
		fake := &fakeFullSizeImageClient{}
		adapter := newFullMediaTestAdapter(t, fake)
		if err := adapter.RequestFullMedia(context.Background(), "google-primary", bridge.MediaRef{Opaque: googleOpaque(t, payload)}); err != nil {
			t.Fatalf("RequestFullMedia(%s): %v", name, err)
		}
		if len(fake.calls) != 0 {
			t.Fatalf("RequestFullMedia(%s) called GetFullSizeImage %v, want no call", name, fake.calls)
		}
	}
}

func TestRequestFullMediaFailures(t *testing.T) {
	opaque := googleOpaque(t, googleDownloadOpaqueV1{
		V: 1, MediaID: "thumb-1", DecryptionKey: "0a", MessageID: "89926", PartID: "88846", Preview: true,
	})

	disconnected := &fakeFullSizeImageClient{}
	adapter := newFullMediaTestAdapter(t, disconnected)
	adapter.host.Connected.Store(false)
	err := adapter.RequestFullMedia(context.Background(), "google-primary", bridge.MediaRef{Opaque: opaque})
	var failure bridge.OpError
	if !errors.As(err, &failure) || failure.Class != bridge.FailureTransient || len(disconnected.calls) != 0 {
		t.Fatalf("RequestFullMedia(disconnected) = %v after %d calls, want a transient error and no call", err, len(disconnected.calls))
	}

	refused := &fakeFullSizeImageClient{err: errors.New("phone said no")}
	adapter = newFullMediaTestAdapter(t, refused)
	if err := adapter.RequestFullMedia(context.Background(), "google-primary", bridge.MediaRef{Opaque: opaque}); err == nil || len(refused.calls) != 1 {
		t.Fatalf("RequestFullMedia(refused) = %v after %d calls, want the transport's error after one call", err, len(refused.calls))
	}

	malformed := &fakeFullSizeImageClient{}
	adapter = newFullMediaTestAdapter(t, malformed)
	if err := adapter.RequestFullMedia(context.Background(), "google-primary", bridge.MediaRef{Opaque: []byte("{")}); err == nil || len(malformed.calls) != 0 {
		t.Fatalf("RequestFullMedia(malformed) = %v after %d calls, want an error and no call", err, len(malformed.calls))
	}
}

// The new optional fields round-trip, and an old reference still decodes.
func TestDecodeGoogleDownloadOpaqueCarriesThePreviewPart(t *testing.T) {
	decoded, err := decodeGoogleDownloadOpaque([]byte(`{"v":1,"media_id":"thumb-1","decryption_key":"0a","message_id":" 89926 ","part_id":"88846","preview":true}`))
	if err != nil {
		t.Fatalf("decode(preview): %v", err)
	}
	if !decoded.preview || decoded.messageID != "89926" || decoded.partID != "88846" || decoded.mediaID != "thumb-1" {
		t.Fatalf("decode(preview) = %+v", decoded)
	}
	old, err := decodeGoogleDownloadOpaque([]byte(`{"v":1,"media_id":"full-1","decryption_key":"0a"}`))
	if err != nil {
		t.Fatalf("decode(old): %v", err)
	}
	if old.preview || old.messageID != "" || old.partID != "" || old.mediaID != "full-1" {
		t.Fatalf("decode(old) = %+v", old)
	}
}
