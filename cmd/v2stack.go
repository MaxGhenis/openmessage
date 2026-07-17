package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/bridge"
	googleadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/google"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	whatsappadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/whatsapp"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2wire"
)

type v2StackDeps struct {
	Logger  zerolog.Logger
	DataDir string

	// These concrete adapter pointer types are a compile-time fence: metadata-only
	// adapters from bridgeadapters/legacy.go cannot be registered for dispatch.
	Google   *googleadapter.Adapter
	WhatsApp *whatsappadapter.Adapter
	Signal   *signaladapter.Adapter
}

type v2Stack struct {
	Store    *sqlite.Store
	Blobs    *blob.BlobStore
	Registry *bridge.AccountRegistry
	Service  *messaging.MessageService
	Media    *media.Service
	Sink     *ingest.Sink
	Counters *ingest.Counters

	outbox *sqlite.OutboxRepository
	worker *ingest.Worker
	logger zerolog.Logger

	registerMu sync.Mutex
}

func v2SendEnabled() bool {
	return v2FlagEnabled("OPENMESSAGES_V2_SEND")
}

func v2IngestEnabled() bool {
	return v2FlagEnabled("OPENMESSAGES_V2_INGEST")
}

func v2FlagEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

type v2LiveAccountSpec struct {
	bridgeKey   string
	displayName string
}

func liveAccountSpec(platform bridge.Platform) (v2LiveAccountSpec, error) {
	switch platform {
	case bridge.PlatformGoogle:
		return v2LiveAccountSpec{bridgeKey: "google_messages", displayName: "Google Messages"}, nil
	case bridge.PlatformWhatsApp:
		return v2LiveAccountSpec{bridgeKey: "whatsmeow", displayName: "WhatsApp"}, nil
	case bridge.PlatformSignal:
		return v2LiveAccountSpec{bridgeKey: "signal_cli", displayName: "Signal"}, nil
	default:
		return v2LiveAccountSpec{}, fmt.Errorf("unsupported live account platform %q", platform)
	}
}

// RegisterAdapter query-first bootstraps the adapter's account row, then adds
// the adapter to the dispatch registry. Existing account metadata is preserved
// verbatim. Registration is serialized so the validity/duplicate preflight
// makes the final Register deterministic for concrete adapters.
func (s *v2Stack) RegisterAdapter(adapter bridge.Adapter) error {
	if s == nil || s.Store == nil || s.Registry == nil {
		return fmt.Errorf("register v2 adapter: stack is incomplete")
	}

	s.registerMu.Lock()
	defer s.registerMu.Unlock()

	probe := bridge.NewRegistry()
	if err := probe.Register(adapter); err != nil {
		return fmt.Errorf("register v2 adapter: %w", err)
	}
	accountID := adapter.AccountID()
	platform := adapter.Platform()
	if _, exists := s.Registry.Snapshot(accountID); exists {
		return fmt.Errorf("register v2 adapter: %w: %q", bridge.ErrAccountAlreadyExists, accountID)
	}
	spec, err := liveAccountSpec(platform)
	if err != nil {
		return fmt.Errorf("register v2 adapter %q: %w", accountID, err)
	}

	if _, err := s.Store.GetAccount(accountID); err != nil {
		if !errors.Is(err, sqlite.ErrNotFound) {
			return fmt.Errorf("register v2 adapter %q: load account: %w", accountID, err)
		}
		nowMS := time.Now().UnixMilli()
		if err := s.Store.UpsertAccount(sqlite.Account{
			AccountID:   accountID,
			BridgeKey:   spec.bridgeKey,
			DisplayName: spec.displayName,
			Mode:        sqlite.AccountModeLive,
			Enabled:     true,
			ConfigJSON:  "{}",
			CreatedAtMS: nowMS,
			UpdatedAtMS: nowMS,
		}); err != nil {
			return fmt.Errorf("register v2 adapter %q: bootstrap account: %w", accountID, err)
		}
	}
	if err := s.Registry.Register(adapter); err != nil {
		return fmt.Errorf("register v2 adapter %q after preflight: %w", accountID, err)
	}
	return nil
}

func newV2Stack(deps v2StackDeps) (_ *v2Stack, resultErr error) {
	v2Dir := filepath.Join(deps.DataDir, "v2")
	if err := os.MkdirAll(v2Dir, 0o700); err != nil {
		return nil, fmt.Errorf("configure v2 stack: create data directory %q: %w", v2Dir, err)
	}

	storePath := filepath.Join(v2Dir, "store.sqlite3")
	store, err := sqlite.Open(storePath)
	if err != nil {
		return nil, fmt.Errorf("configure v2 stack: open store %q: %w", storePath, err)
	}
	closeStore := true
	defer func() {
		if !closeStore {
			return
		}
		if err := store.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close v2 store after configuration failure: %w", err))
		}
	}()

	blobPath := filepath.Join(v2Dir, "blobs")
	blobs, err := blob.New(blobPath)
	if err != nil {
		return nil, fmt.Errorf("configure v2 stack: create blob store %q: %w", blobPath, err)
	}

	stack := &v2Stack{
		Store:    store,
		Blobs:    blobs,
		Registry: bridge.NewRegistry(),
		logger:   deps.Logger,
	}
	if deps.Google != nil {
		if err := stack.RegisterAdapter(deps.Google); err != nil {
			return nil, fmt.Errorf("configure v2 stack: register Google adapter: %w", err)
		}
	}
	if deps.WhatsApp != nil {
		if err := stack.RegisterAdapter(deps.WhatsApp); err != nil {
			return nil, fmt.Errorf("configure v2 stack: register WhatsApp adapter: %w", err)
		}
	}
	if deps.Signal != nil {
		if err := stack.RegisterAdapter(deps.Signal); err != nil {
			return nil, fmt.Errorf("configure v2 stack: register Signal adapter: %w", err)
		}
	}

	clock := messaging.SystemClock{}
	service, err := messaging.NewMessageService(
		store,
		stack.Registry,
		blobs,
		clock,
		messaging.CryptoIDSource{},
	)
	if err != nil {
		return nil, fmt.Errorf("configure v2 stack: create message service: %w", err)
	}
	messages, err := sqlite.NewMessageRepository(store, clock.Now)
	if err != nil {
		return nil, fmt.Errorf("configure v2 stack: create ingest message repository: %w", err)
	}
	counters := &ingest.Counters{}
	worker, err := ingest.NewWorker(ingest.WorkerConfig{
		Store:        store,
		Messages:     messages,
		EchoObserver: service,
		Counters:     counters,
		Logger:       deps.Logger,
		Decoders: []ingest.DecoderRegistration{
			{
				Codec:    ingest.GoogleCodec,
				Platform: bridge.PlatformGoogle,
				Decoder:  ingest.NewGoogleDecoder(counters),
			},
			ingest.NewWhatsAppDecoderRegistration(),
			{
				Codec:    ingest.SignalJSONRPCCodec,
				Platform: bridge.PlatformSignal,
				Decoder:  ingest.NewSignalDecoder(),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("configure v2 stack: create ingest worker: %w", err)
	}
	sink, err := ingest.NewSink(ingest.SinkConfig{
		Messages: messages,
		Worker:   worker,
		Counters: counters,
	})
	if err != nil {
		return nil, fmt.Errorf("configure v2 stack: create ingest sink: %w", err)
	}
	outbox, err := sqlite.NewOutboxRepository(store, clock.Now)
	if err != nil {
		return nil, fmt.Errorf("configure v2 stack: create projector outbox: %w", err)
	}
	mediaService, err := media.NewService(store, stack.Registry, blobs, clock)
	if err != nil {
		return nil, fmt.Errorf("configure v2 stack: create media service: %w", err)
	}

	closeStore = false
	stack.Service = service
	stack.Media = mediaService
	stack.Sink = sink
	stack.Counters = counters
	stack.outbox = outbox
	stack.worker = worker
	return stack, nil
}

func (s *v2Stack) Start(ctx context.Context, legacy *db.Store, events v2wire.EventPublisher) (stop func()) {
	runCtx, cancel := context.WithCancel(ctx)
	projector := &v2wire.Projector{
		V2Store: s.Store,
		Outbox:  s.outbox,
		Legacy:  legacy,
		Blobs:   s.Blobs,
		Events:  events,
		Service: s.Service,
		Logger:  s.logger,
	}

	var runWG sync.WaitGroup
	runWG.Add(3)
	go func() {
		defer runWG.Done()
		if err := s.worker.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Error().Err(err).Msg("V2 ingest worker stopped")
		}
	}()
	go func() {
		defer runWG.Done()
		if err := s.Service.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Error().Err(err).Msg("V2 message dispatcher stopped")
		}
	}()
	go func() {
		defer runWG.Done()
		if err := projector.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Error().Err(err).Msg("V2 legacy projector stopped")
		}
	}()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			cancel()
			runWG.Wait()
			if err := s.Store.Close(); err != nil {
				s.logger.Warn().Err(err).Msg("Failed to close v2 message store")
			}
		})
	}
}
