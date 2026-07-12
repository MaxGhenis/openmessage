package bridge

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

const (
	PlatformGoogle   Platform = "google"
	PlatformWhatsApp Platform = "whatsapp"
	PlatformSignal   Platform = "signal"
)

var (
	ErrAdapterRequired       = errors.New("bridge: adapter is required")
	ErrInvalidAccountID      = errors.New("bridge: adapter account ID is invalid")
	ErrInvalidPlatform       = errors.New("bridge: adapter platform is invalid")
	ErrAccountAlreadyExists  = errors.New("bridge: account is already registered")
	ErrAccountNotRegistered  = errors.New("bridge: account is not registered")
	ErrCapabilityUnavailable = errors.New("bridge: capability is unavailable")
	ErrUnknownCapability     = errors.New("bridge: unknown capability")
)

// CapabilityDeclarer lets composition-only adapters describe transport
// abilities that are not yet wired to the clean capability interfaces.
// Registry snapshots this metadata once during Register. Declarations affect
// capability reporting only; Acquire still requires a concrete clean interface.
type CapabilityDeclarer interface {
	DeclaredCapabilities() CapabilitySet
}

// DispatchLease is acquired atomically from an online supervisor generation.
// The C2 registry uses generation zero and a no-op release until supervisors
// add generation replacement and drain behavior in C3.
type DispatchLease struct {
	AccountID   string
	Platform    Platform
	Generation  Generation
	Ctx         context.Context
	Text        TextSender
	Media       MediaSender
	Reaction    ReactionSender
	ReadReceipt ReadReceiptSender
	Download    MediaDownloader
	release     func()
	closeOnce   sync.Once
}

func (l *DispatchLease) Close() {
	if l == nil {
		return
	}
	l.closeOnce.Do(func() {
		if l.release != nil {
			l.release()
			l.release = nil
		}
	})
}

// Registry is the application-facing, read-only view of registered adapters.
// Register intentionally exists only on the concrete composition type.
type Registry interface {
	Snapshot(accountID string) (Snapshot, bool)
	Acquire(
		ctx context.Context,
		accountID string,
		capability Capability,
	) (*DispatchLease, error)
	Capabilities(accountID string) CapabilitySet
}

// AccountRegistry indexes immutable adapter capability views by account ID.
// It never exposes an Adapter or invokes transport methods while holding a lock.
type AccountRegistry struct {
	mu      sync.RWMutex
	entries map[string]*registryEntry
}

type registryEntry struct {
	snapshot Snapshot
	caps     CapabilitySet

	lifecycle    Lifecycle
	conversation ConversationStarter
	text         TextSender
	media        MediaSender
	reaction     ReactionSender
	readReceipt  ReadReceiptSender
	download     MediaDownloader
	pairer       Pairer
}

func NewRegistry() *AccountRegistry {
	return &AccountRegistry{entries: make(map[string]*registryEntry)}
}

// Register records one adapter for an account. Optional interfaces and
// declared capability metadata are inspected exactly once and stored as an
// immutable entry. Duplicate account IDs are rejected rather than replaced.
func (r *AccountRegistry) Register(adapter Adapter) error {
	if adapter == nil || isNilAdapter(adapter) {
		return ErrAdapterRequired
	}

	rawAccountID := adapter.AccountID()
	accountID := strings.TrimSpace(rawAccountID)
	if accountID == "" {
		return ErrInvalidAccountID
	}
	if accountID != rawAccountID {
		return fmt.Errorf("%w: %q has surrounding whitespace", ErrInvalidAccountID, rawAccountID)
	}
	rawPlatform := string(adapter.Platform())
	platform := Platform(strings.TrimSpace(rawPlatform))
	if platform == "" {
		return ErrInvalidPlatform
	}
	if string(platform) != rawPlatform {
		return fmt.Errorf("%w: %q has surrounding whitespace", ErrInvalidPlatform, rawPlatform)
	}

	entry := discoverRegistryEntry(adapter)
	entry.snapshot = Snapshot{
		AccountID: accountID,
		Platform:  platform,
		State:     StateStopped,
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]*registryEntry)
	}
	if _, exists := r.entries[accountID]; exists {
		return fmt.Errorf("%w: %q", ErrAccountAlreadyExists, accountID)
	}
	r.entries[accountID] = entry
	return nil
}

func (r *AccountRegistry) Snapshot(accountID string) (Snapshot, bool) {
	r.mu.RLock()
	entry, ok := r.entries[accountID]
	r.mu.RUnlock()
	if !ok {
		return Snapshot{}, false
	}
	return entry.snapshot, true
}

func (r *AccountRegistry) Capabilities(accountID string) CapabilitySet {
	r.mu.RLock()
	entry := r.entries[accountID]
	r.mu.RUnlock()
	if entry == nil {
		return CapabilitySet{}
	}
	return entry.caps
}

func (r *AccountRegistry) Acquire(
	ctx context.Context,
	accountID string,
	capability Capability,
) (*DispatchLease, error) {
	if ctx == nil {
		return nil, errors.New("bridge: acquire context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	r.mu.RLock()
	entry := r.entries[accountID]
	r.mu.RUnlock()
	if entry == nil {
		return nil, fmt.Errorf("%w: %q", ErrAccountNotRegistered, accountID)
	}
	// The literal section 2.2 DispatchLease has no ConversationStarter field.
	// Return an explicit error rather than a successful lease that cannot expose
	// the requested operation. A later contract revision can add that lease view.
	if capability == CapabilityStartConversation && capabilityReported(entry.caps, capability) {
		return nil, fmt.Errorf(
			"%w: account %q reports %q but DispatchLease does not expose it",
			ErrCapabilityUnavailable,
			accountID,
			capability,
		)
	}

	available, known := entry.hasDispatchCapability(capability)
	if !known {
		return nil, fmt.Errorf("%w: %q", ErrUnknownCapability, capability)
	}
	if !available {
		if capabilityReported(entry.caps, capability) {
			return nil, fmt.Errorf(
				"%w: account %q declares %q but its clean interface is not wired",
				ErrCapabilityUnavailable,
				accountID,
				capability,
			)
		}
		return nil, fmt.Errorf(
			"%w: account %q does not support %q",
			ErrCapabilityUnavailable,
			accountID,
			capability,
		)
	}

	return &DispatchLease{
		AccountID:   entry.snapshot.AccountID,
		Platform:    entry.snapshot.Platform,
		Generation:  entry.snapshot.Generation,
		Ctx:         ctx,
		Text:        narrowTextSender(entry.text),
		Media:       narrowMediaSender(entry.media),
		Reaction:    narrowReactionSender(entry.reaction),
		ReadReceipt: narrowReadReceiptSender(entry.readReceipt),
		Download:    narrowMediaDownloader(entry.download),
		release:     func() {},
	}, nil
}

func discoverRegistryEntry(adapter Adapter) *registryEntry {
	entry := &registryEntry{lifecycle: lifecycleView{target: adapter}}
	entry.conversation, _ = adapter.(ConversationStarter)
	entry.text, _ = adapter.(TextSender)
	entry.media, _ = adapter.(MediaSender)
	entry.reaction, _ = adapter.(ReactionSender)
	entry.readReceipt, _ = adapter.(ReadReceiptSender)
	entry.download, _ = adapter.(MediaDownloader)
	// A Pairer accepts a PairMethod but does not reveal which methods it
	// supports. Store the interface once, while deriving PairQR/PairPhone only
	// from explicit declared metadata below.
	entry.pairer, _ = adapter.(Pairer)

	entry.caps = CapabilitySet{
		StartConversation: entry.conversation != nil,
		TextSend:          entry.text != nil,
		MediaSend:         entry.media != nil,
		Reactions:         entry.reaction != nil,
		ReadReceipts:      entry.readReceipt != nil,
		MediaDownload:     entry.download != nil,
	}
	if declarer, ok := adapter.(CapabilityDeclarer); ok {
		entry.caps = mergeCapabilitySets(entry.caps, declarer.DeclaredCapabilities())
	}
	return entry
}

func (e *registryEntry) hasDispatchCapability(capability Capability) (available, known bool) {
	switch capability {
	case CapabilityStartConversation:
		return false, true
	case CapabilityTextSend:
		return e.text != nil, true
	case CapabilityMediaSend:
		return e.media != nil, true
	case CapabilityReactions:
		return e.reaction != nil, true
	case CapabilityReadReceipts:
		return e.readReceipt != nil, true
	case CapabilityMediaDownload:
		return e.download != nil, true
	default:
		return false, false
	}
}

func capabilityReported(caps CapabilitySet, capability Capability) bool {
	switch capability {
	case CapabilityStartConversation:
		return caps.StartConversation
	case CapabilityTextSend:
		return caps.TextSend
	case CapabilityMediaSend:
		return caps.MediaSend
	case CapabilityReactions:
		return caps.Reactions
	case CapabilityReadReceipts:
		return caps.ReadReceipts
	case CapabilityMediaDownload:
		return caps.MediaDownload
	default:
		return false
	}
}

func mergeCapabilitySets(a, b CapabilitySet) CapabilitySet {
	return CapabilitySet{
		StartConversation: a.StartConversation || b.StartConversation,
		TextSend:          a.TextSend || b.TextSend,
		MediaSend:         a.MediaSend || b.MediaSend,
		Reactions:         a.Reactions || b.Reactions,
		ReadReceipts:      a.ReadReceipts || b.ReadReceipts,
		PairQR:            a.PairQR || b.PairQR,
		PairPhone:         a.PairPhone || b.PairPhone,
		MediaDownload:     a.MediaDownload || b.MediaDownload,
	}
}

func isNilAdapter(adapter Adapter) bool {
	value := reflect.ValueOf(adapter)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

type lifecycleView struct{ target Lifecycle }

func (v lifecycleView) Start(
	ctx context.Context,
	req StartRequest,
	sink ConnectionSink,
) (Run, error) {
	return v.target.Start(ctx, req, sink)
}

// Narrow proxy types prevent callers from recovering the raw Adapter through
// a type assertion on a capability stored in DispatchLease.
type textSenderView struct{ target TextSender }

func (v textSenderView) SendText(ctx context.Context, req TextRequest) (SendResult, error) {
	return v.target.SendText(ctx, req)
}

func narrowTextSender(target TextSender) TextSender {
	if target == nil {
		return nil
	}
	return textSenderView{target: target}
}

type mediaSenderView struct{ target MediaSender }

func (v mediaSenderView) SendMedia(ctx context.Context, req MediaRequest) (SendResult, error) {
	return v.target.SendMedia(ctx, req)
}

func narrowMediaSender(target MediaSender) MediaSender {
	if target == nil {
		return nil
	}
	return mediaSenderView{target: target}
}

type reactionSenderView struct{ target ReactionSender }

func (v reactionSenderView) SendReaction(ctx context.Context, req ReactionRequest) (SendResult, error) {
	return v.target.SendReaction(ctx, req)
}

func narrowReactionSender(target ReactionSender) ReactionSender {
	if target == nil {
		return nil
	}
	return reactionSenderView{target: target}
}

type readReceiptSenderView struct{ target ReadReceiptSender }

func (v readReceiptSenderView) MarkRead(ctx context.Context, req ReadReceiptRequest) error {
	return v.target.MarkRead(ctx, req)
}

func narrowReadReceiptSender(target ReadReceiptSender) ReadReceiptSender {
	if target == nil {
		return nil
	}
	return readReceiptSenderView{target: target}
}

type mediaDownloaderView struct{ target MediaDownloader }

func (v mediaDownloaderView) DownloadMedia(
	ctx context.Context,
	accountID string,
	ref MediaRef,
) (MediaStream, error) {
	return v.target.DownloadMedia(ctx, accountID, ref)
}

func narrowMediaDownloader(target MediaDownloader) MediaDownloader {
	if target == nil {
		return nil
	}
	return mediaDownloaderView{target: target}
}

var _ Registry = (*AccountRegistry)(nil)
