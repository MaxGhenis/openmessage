package bridge

import (
	"context"
	"testing"
)

type fullMediaAdapter struct {
	allCapabilitiesAdapter
	requested []MediaRef
}

func (a *fullMediaAdapter) RequestFullMedia(_ context.Context, _ string, ref MediaRef) error {
	a.requested = append(a.requested, ref)
	return nil
}

// fullMediaWithoutDownloadAdapter can ask for originals but download nothing.
type fullMediaWithoutDownloadAdapter struct {
	textOnlyAdapter
}

func (a *fullMediaWithoutDownloadAdapter) RequestFullMedia(context.Context, string, MediaRef) error {
	return nil
}

// A lease carries a narrowed FullMediaRequester exactly when the adapter
// both downloads media and can ask for originals.
func TestRegistryLeasesFullMediaOnlyBesideADownloader(t *testing.T) {
	registry := NewRegistry()
	requester := &fullMediaAdapter{allCapabilitiesAdapter: allCapabilitiesAdapter{
		fakeAdapter: fakeAdapter{accountID: "fake-full-media", platform: "fake"},
	}}
	downloaderOnly := &allCapabilitiesAdapter{fakeAdapter: fakeAdapter{accountID: "fake-download", platform: "fake"}}
	noDownloader := &fullMediaWithoutDownloadAdapter{textOnlyAdapter{fakeAdapter{accountID: "fake-no-download", platform: "fake"}}}
	for _, adapter := range []Adapter{requester, downloaderOnly, noDownloader} {
		if err := registry.Register(adapter); err != nil {
			t.Fatalf("Register(%s): %v", adapter.AccountID(), err)
		}
	}
	ctx := context.Background()

	lease, err := registry.Acquire(ctx, requester.AccountID(), CapabilityMediaDownload)
	if err != nil {
		t.Fatalf("Acquire(full media) error = %v", err)
	}
	defer lease.Close()
	if lease.FullMedia == nil {
		t.Fatal("lease.FullMedia is nil for an adapter that downloads and asks for originals")
	}
	if _, exposesAdapter := lease.FullMedia.(Adapter); exposesAdapter {
		t.Fatal("lease.FullMedia exposes the raw adapter")
	}
	if err := lease.FullMedia.RequestFullMedia(ctx, requester.AccountID(), MediaRef{RemoteID: "thumb-1"}); err != nil {
		t.Fatalf("RequestFullMedia(): %v", err)
	}
	if len(requester.requested) != 1 || requester.requested[0].RemoteID != "thumb-1" {
		t.Fatalf("adapter saw %+v, want the one request", requester.requested)
	}

	plain, err := registry.Acquire(ctx, downloaderOnly.AccountID(), CapabilityMediaDownload)
	if err != nil {
		t.Fatalf("Acquire(download only) error = %v", err)
	}
	defer plain.Close()
	if plain.Download == nil || plain.FullMedia != nil {
		t.Fatalf("download-only lease: Download=%v FullMedia=%v, want a downloader and no requester", plain.Download, plain.FullMedia)
	}

	text, err := registry.Acquire(ctx, noDownloader.AccountID(), CapabilityTextSend)
	if err != nil {
		t.Fatalf("Acquire(no downloader) error = %v", err)
	}
	defer text.Close()
	if text.FullMedia != nil {
		t.Fatal("lease.FullMedia is set for an adapter that cannot download media")
	}
}

var (
	_ FullMediaRequester = (*fullMediaAdapter)(nil)
	_ FullMediaRequester = (*fullMediaWithoutDownloadAdapter)(nil)
)
