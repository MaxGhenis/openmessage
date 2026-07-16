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
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// localDeviceID derives the per-account local-installation device id.
// devices.device_id is a GLOBAL primary key (0002) and UpsertDevice conflicts
// on it alone, so a constant id would let the second account silently steal
// the first account's device row and break its read_cursors composite FK.
func localDeviceID(accountID string) string {
	return "local-primary:" + accountID
}

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

	logger zerolog.Logger
}

func v2SendEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPENMESSAGES_V2_SEND"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func newV2Stack(deps v2StackDeps) (_ *v2Stack, resultErr error) {
	v2Dir := filepath.Join(deps.DataDir, "v2")
	if err := os.MkdirAll(v2Dir, 0o700); err != nil {
		return nil, fmt.Errorf("configure v2 send stack: create data directory %q: %w", v2Dir, err)
	}

	storePath := filepath.Join(v2Dir, "store.sqlite3")
	store, err := sqlite.Open(storePath)
	if err != nil {
		return nil, fmt.Errorf("configure v2 send stack: open store %q: %w", storePath, err)
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
		return nil, fmt.Errorf("configure v2 send stack: create blob store %q: %w", blobPath, err)
	}

	registry := bridge.NewRegistry()
	if deps.Google != nil {
		if err := registry.Register(deps.Google); err != nil {
			return nil, fmt.Errorf("configure v2 send stack: register Google adapter: %w", err)
		}
	}
	if deps.WhatsApp != nil {
		if err := registry.Register(deps.WhatsApp); err != nil {
			return nil, fmt.Errorf("configure v2 send stack: register WhatsApp adapter: %w", err)
		}
	}
	if deps.Signal != nil {
		if err := registry.Register(deps.Signal); err != nil {
			return nil, fmt.Errorf("configure v2 send stack: register Signal adapter: %w", err)
		}
	}

	clock := messaging.SystemClock{}
	service, err := messaging.NewMessageService(
		store,
		registry,
		blobs,
		clock,
		messaging.CryptoIDSource{},
	)
	if err != nil {
		return nil, fmt.Errorf("configure v2 send stack: create message service: %w", err)
	}
	mediaService, err := media.NewService(store, registry, blobs, clock)
	if err != nil {
		return nil, fmt.Errorf("configure v2 send stack: create media service: %w", err)
	}

	closeStore = false
	return &v2Stack{
		Store:    store,
		Blobs:    blobs,
		Registry: registry,
		Service:  service,
		Media:    mediaService,
		logger:   deps.Logger,
	}, nil
}

func (s *v2Stack) Start(ctx context.Context) (stop func()) {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Service.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			s.logger.Error().Err(err).Msg("V2 message dispatcher stopped")
		}
	}()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(func() {
			cancel()
			<-done
			if err := s.Store.Close(); err != nil {
				s.logger.Warn().Err(err).Msg("Failed to close v2 message store")
			}
		})
	}
}

func ensureLocalDevice(store *sqlite.Store, accountID string) error {
	if store == nil {
		return fmt.Errorf("ensure local device: store is nil")
	}
	if strings.TrimSpace(accountID) == "" {
		return fmt.Errorf("ensure local device: account ID is empty")
	}

	nowMS := time.Now().UnixMilli()
	if err := store.UpsertDevice(sqlite.Device{
		DeviceID:    localDeviceID(accountID),
		AccountID:   accountID,
		Kind:        sqlite.DeviceKindLocalInstallation,
		DisplayName: "OpenMessage",
		State:       sqlite.DeviceStateActive,
		IsCurrent:   true,
		CreatedAtMS: nowMS,
		UpdatedAtMS: nowMS,
	}); err != nil {
		return fmt.Errorf("ensure local device for account %q: %w", accountID, err)
	}
	return nil
}
