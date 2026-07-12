package bridge

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryDiscoversAllOptionalCapabilitiesAndDispatches(t *testing.T) {
	registry := NewRegistry()
	adapter := &allCapabilitiesAdapter{
		fakeAdapter: fakeAdapter{
			accountID: "fake-all",
			platform:  "fake",
		},
	}
	if err := registry.Register(adapter); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if registry.entries[adapter.AccountID()].lifecycle == nil {
		t.Fatal("Register() discarded the adapter lifecycle")
	}
	if _, exposesAdapter := registry.entries[adapter.AccountID()].lifecycle.(Adapter); exposesAdapter {
		t.Fatal("registered lifecycle view exposes the raw adapter")
	}

	wantCapabilities := CapabilitySet{
		StartConversation: true,
		TextSend:          true,
		MediaSend:         true,
		Reactions:         true,
		ReadReceipts:      true,
		PairQR:            true,
		PairPhone:         true,
		MediaDownload:     true,
	}
	if got := registry.Capabilities(adapter.AccountID()); !reflect.DeepEqual(got, wantCapabilities) {
		t.Fatalf("Capabilities() = %+v, want %+v", got, wantCapabilities)
	}
	_, err := registry.Acquire(context.Background(), adapter.AccountID(), CapabilityStartConversation)
	if !errors.Is(err, ErrCapabilityUnavailable) || !strings.Contains(err.Error(), "DispatchLease") {
		t.Fatalf("Acquire(start conversation) error = %v, want explicit lease-shape error", err)
	}

	snapshot, ok := registry.Snapshot(adapter.AccountID())
	if !ok {
		t.Fatal("Snapshot() found no registered adapter")
	}
	if snapshot.AccountID != adapter.AccountID() || snapshot.Platform != adapter.Platform() {
		t.Fatalf("Snapshot() = %+v, want account %q platform %q", snapshot, adapter.AccountID(), adapter.Platform())
	}
	if snapshot.State != StateStopped || snapshot.Generation != 0 {
		t.Fatalf("Snapshot() lifecycle = state %q generation %d, want stopped generation 0", snapshot.State, snapshot.Generation)
	}

	ctx := context.Background()
	lease, err := registry.Acquire(ctx, adapter.AccountID(), CapabilityTextSend)
	if err != nil {
		t.Fatalf("Acquire(text) error = %v", err)
	}
	defer lease.Close()
	if lease.AccountID != adapter.AccountID() || lease.Platform != adapter.Platform() || lease.Ctx != ctx {
		t.Fatalf("Acquire(text) lease = %+v", lease)
	}
	if _, exposesAdapter := lease.Text.(Adapter); exposesAdapter {
		t.Fatal("lease.Text exposes the raw adapter")
	}

	textRequest := TextRequest{
		AccountID:    adapter.AccountID(),
		Conversation: ConversationRef{RemoteID: "conversation-1"},
		Body:         "hello",
		RequestID:    "request-1",
	}
	textResult, err := lease.Text.SendText(ctx, textRequest)
	if err != nil {
		t.Fatalf("lease.Text.SendText() error = %v", err)
	}
	if textResult.RemoteMessageID != "text-remote-id" || adapter.lastText != textRequest {
		t.Fatalf("SendText() result/request = %+v / %+v", textResult, adapter.lastText)
	}

	mediaResult, err := lease.Media.SendMedia(ctx, MediaRequest{RequestID: "media-1"})
	if err != nil || mediaResult.RemoteMessageID != "media-remote-id" {
		t.Fatalf("lease.Media.SendMedia() = %+v, %v", mediaResult, err)
	}
	reactionResult, err := lease.Reaction.SendReaction(ctx, ReactionRequest{RequestID: "reaction-1"})
	if err != nil || reactionResult.RemoteMessageID != "reaction-remote-id" {
		t.Fatalf("lease.Reaction.SendReaction() = %+v, %v", reactionResult, err)
	}
	if err := lease.ReadReceipt.MarkRead(ctx, ReadReceiptRequest{AccountID: adapter.AccountID()}); err != nil {
		t.Fatalf("lease.ReadReceipt.MarkRead() error = %v", err)
	}
	stream, err := lease.Download.DownloadMedia(ctx, adapter.AccountID(), MediaRef{RemoteID: "media-1"})
	if err != nil {
		t.Fatalf("lease.Download.DownloadMedia() error = %v", err)
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil || string(data) != "fake media" {
		t.Fatalf("downloaded media = %q, %v", data, err)
	}

	// Close is intentionally nil-safe and idempotent.
	lease.Close()
	lease.Close()
	var nilLease *DispatchLease
	nilLease.Close()
}

func TestRegistryDiscoversOnlyImplementedOptionalCapabilities(t *testing.T) {
	registry := NewRegistry()
	adapter := &textOnlyAdapter{fakeAdapter: fakeAdapter{accountID: "text-only", platform: "minimal"}}
	if err := registry.Register(adapter); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	want := CapabilitySet{TextSend: true}
	if got := registry.Capabilities(adapter.AccountID()); !reflect.DeepEqual(got, want) {
		t.Fatalf("Capabilities() = %+v, want %+v", got, want)
	}

	lease, err := registry.Acquire(context.Background(), adapter.AccountID(), CapabilityTextSend)
	if err != nil {
		t.Fatalf("Acquire(text) error = %v", err)
	}
	if _, err := lease.Text.SendText(context.Background(), TextRequest{Body: "minimal"}); err != nil {
		t.Fatalf("lease.Text.SendText() error = %v", err)
	}
	lease.Close()

	_, err = registry.Acquire(context.Background(), adapter.AccountID(), CapabilityReactions)
	if !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("Acquire(reactions) error = %v, want ErrCapabilityUnavailable", err)
	}
	if !strings.Contains(err.Error(), adapter.AccountID()) || !strings.Contains(err.Error(), string(CapabilityReactions)) {
		t.Fatalf("Acquire(reactions) error is not clear: %v", err)
	}
}

func TestRegistryRegistrationAndAcquireErrors(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(nil); !errors.Is(err, ErrAdapterRequired) {
		t.Fatalf("Register(nil) error = %v, want ErrAdapterRequired", err)
	}
	if err := registry.Register(&textOnlyAdapter{fakeAdapter: fakeAdapter{
		accountID: " padded ",
		platform:  "fake",
	}}); !errors.Is(err, ErrInvalidAccountID) {
		t.Fatalf("Register(padded account ID) error = %v, want ErrInvalidAccountID", err)
	}

	adapter := &textOnlyAdapter{fakeAdapter: fakeAdapter{accountID: "duplicate", platform: "fake"}}
	if err := registry.Register(adapter); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := registry.Register(adapter); !errors.Is(err, ErrAccountAlreadyExists) {
		t.Fatalf("duplicate Register() error = %v, want ErrAccountAlreadyExists", err)
	}

	_, err := registry.Acquire(context.Background(), "missing", CapabilityTextSend)
	if !errors.Is(err, ErrAccountNotRegistered) {
		t.Fatalf("Acquire(missing) error = %v, want ErrAccountNotRegistered", err)
	}
	_, err = registry.Acquire(context.Background(), adapter.AccountID(), Capability("future"))
	if !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("Acquire(unknown capability) error = %v, want ErrUnknownCapability", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = registry.Acquire(canceled, adapter.AccountID(), CapabilityTextSend)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire(canceled) error = %v, want context.Canceled", err)
	}
}

func TestRegistrySnapshotsDeclaredCapabilitiesOnce(t *testing.T) {
	registry := NewRegistry()
	adapter := &declaringAdapter{
		fakeAdapter: fakeAdapter{accountID: "declared", platform: "fake"},
		declared:    CapabilitySet{PairQR: true},
	}
	if err := registry.Register(adapter); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	adapter.declared = CapabilitySet{PairPhone: true}

	want := CapabilitySet{PairQR: true}
	if got := registry.Capabilities(adapter.AccountID()); !reflect.DeepEqual(got, want) {
		t.Fatalf("Capabilities() after declaration mutation = %+v, want snapshotted %+v", got, want)
	}
	if adapter.declarationCalls != 1 {
		t.Fatalf("DeclaredCapabilities() calls = %d, want 1", adapter.declarationCalls)
	}
}

func TestDispatchLeaseConcurrentCloseReleasesOnce(t *testing.T) {
	var releaseCalls atomic.Int32
	lease := &DispatchLease{release: func() { releaseCalls.Add(1) }}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease.Close()
		}()
	}
	wg.Wait()

	if got := releaseCalls.Load(); got != 1 {
		t.Fatalf("release calls = %d, want 1", got)
	}
}

type fakeAdapter struct {
	accountID string
	platform  Platform
}

func (a *fakeAdapter) AccountID() string {
	return a.accountID
}

func (a *fakeAdapter) Platform() Platform {
	return a.platform
}

func (a *fakeAdapter) Start(
	_ context.Context,
	_ StartRequest,
	_ ConnectionSink,
) (Run, error) {
	ready := make(chan struct{})
	close(ready)
	done := make(chan error, 1)
	done <- nil
	close(done)
	return &fakeRun{ready: ready, done: done}, nil
}

type fakeRun struct {
	ready <-chan struct{}
	done  <-chan error
}

func (r *fakeRun) Ready() <-chan struct{} {
	return r.ready
}

func (r *fakeRun) Done() <-chan error {
	return r.done
}

func (r *fakeRun) Probe(_ context.Context) (Liveness, error) {
	return Liveness{AliveAt: time.Now(), Detail: "fake"}, nil
}

func (r *fakeRun) Stop(_ context.Context) error {
	return nil
}

type textOnlyAdapter struct {
	fakeAdapter
}

func (a *textOnlyAdapter) SendText(_ context.Context, _ TextRequest) (SendResult, error) {
	return SendResult{RemoteMessageID: "minimal-text-id"}, nil
}

type allCapabilitiesAdapter struct {
	fakeAdapter
	lastText TextRequest
}

func (a *allCapabilitiesAdapter) StartConversation(
	_ context.Context,
	_ StartConversationRequest,
) (StartConversationResult, error) {
	return StartConversationResult{RemoteConversationID: "conversation-remote-id"}, nil
}

func (a *allCapabilitiesAdapter) SendText(_ context.Context, req TextRequest) (SendResult, error) {
	a.lastText = req
	return SendResult{RemoteMessageID: "text-remote-id"}, nil
}

func (a *allCapabilitiesAdapter) SendMedia(_ context.Context, _ MediaRequest) (SendResult, error) {
	return SendResult{RemoteMessageID: "media-remote-id"}, nil
}

func (a *allCapabilitiesAdapter) SendReaction(_ context.Context, _ ReactionRequest) (SendResult, error) {
	return SendResult{RemoteMessageID: "reaction-remote-id"}, nil
}

func (a *allCapabilitiesAdapter) MarkRead(_ context.Context, _ ReadReceiptRequest) error {
	return nil
}

func (a *allCapabilitiesAdapter) DownloadMedia(
	_ context.Context,
	_ string,
	_ MediaRef,
) (MediaStream, error) {
	return MediaStream{
		ReadCloser: io.NopCloser(strings.NewReader("fake media")),
		Size:       int64(len("fake media")),
		Filename:   "fake.txt",
		MIME:       "text/plain",
	}, nil
}

func (a *allCapabilitiesAdapter) Pair(
	_ context.Context,
	_ PairRequest,
	_ PairSink,
) (PairResult, error) {
	return PairResult{RemoteAccountID: "remote-account"}, nil
}

func (a *allCapabilitiesAdapter) Unpair(_ context.Context, _ string) error {
	return nil
}

func (a *allCapabilitiesAdapter) DeclaredCapabilities() CapabilitySet {
	return CapabilitySet{PairQR: true, PairPhone: true}
}

type declaringAdapter struct {
	fakeAdapter
	declared         CapabilitySet
	declarationCalls int
}

func (a *declaringAdapter) DeclaredCapabilities() CapabilitySet {
	a.declarationCalls++
	return a.declared
}

func (a *declaringAdapter) Pair(
	_ context.Context,
	_ PairRequest,
	_ PairSink,
) (PairResult, error) {
	return PairResult{}, nil
}

func (a *declaringAdapter) Unpair(_ context.Context, _ string) error {
	return nil
}

var (
	_ Adapter             = (*allCapabilitiesAdapter)(nil)
	_ ConversationStarter = (*allCapabilitiesAdapter)(nil)
	_ TextSender          = (*allCapabilitiesAdapter)(nil)
	_ MediaSender         = (*allCapabilitiesAdapter)(nil)
	_ ReactionSender      = (*allCapabilitiesAdapter)(nil)
	_ ReadReceiptSender   = (*allCapabilitiesAdapter)(nil)
	_ MediaDownloader     = (*allCapabilitiesAdapter)(nil)
	_ Pairer              = (*allCapabilitiesAdapter)(nil)
	_ CapabilityDeclarer  = (*allCapabilitiesAdapter)(nil)

	_ Adapter    = (*textOnlyAdapter)(nil)
	_ TextSender = (*textOnlyAdapter)(nil)

	_ Adapter            = (*declaringAdapter)(nil)
	_ CapabilityDeclarer = (*declaringAdapter)(nil)
	_ Pairer             = (*declaringAdapter)(nil)
)
