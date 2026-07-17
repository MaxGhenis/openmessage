package ingest

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const defaultWorkerQueueCapacity = 64

// SinkConfig contains the durable repository and asynchronous worker used by
// a ConnectionSink. IDs defaults to messaging.CryptoIDSource.
type SinkConfig struct {
	Messages *sqlite.MessageRepository
	Worker   *Worker
	Counters *Counters
	IDs      messaging.IDSource
}

// Sink durably appends raw ingress and only then notifies the projection
// worker. It deliberately performs no decoding or projection inline.
type Sink struct {
	messages *sqlite.MessageRepository
	worker   *Worker
	counters *Counters
	ids      messaging.IDSource
}

var _ bridge.ConnectionSink = (*Sink)(nil)

// RecordIngressError counts a capture or append fault surfaced by a transport
// adapter. The frame itself was logged and dropped at the transport edge; the
// counter keeps /api/status honest about ingest losses.
func (s *Sink) RecordIngressError(accountID string) {
	s.counters.account(accountID).appendErrors.Add(1)
}

// NewSink constructs a storage-backed ConnectionSink.
func NewSink(config SinkConfig) (*Sink, error) {
	if config.Messages == nil {
		return nil, fmt.Errorf("create ingest sink: message repository is nil")
	}
	if config.Worker == nil {
		return nil, fmt.Errorf("create ingest sink: worker is nil")
	}
	if config.Messages != config.Worker.messages {
		return nil, fmt.Errorf("create ingest sink: message repository does not match worker repository")
	}
	if config.Counters == nil {
		config.Counters = config.Worker.counters
	}
	if config.Counters != config.Worker.counters {
		return nil, fmt.Errorf("create ingest sink: counters do not match worker counters")
	}
	if config.IDs == nil {
		config.IDs = messaging.CryptoIDSource{}
	}
	return &Sink{
		messages: config.Messages,
		worker:   config.Worker,
		counters: config.Counters,
		ids:      config.IDs,
	}, nil
}

// AppendIngress validates and durably inserts exactly one raw frame before a
// best-effort, non-blocking worker notification.
func (s *Sink) AppendIngress(ctx context.Context, record bridge.RawIngressRecord) error {
	if err := validateIngress(ctx, record); err != nil {
		return err
	}

	inboxID, err := s.ids.NewID()
	if err != nil {
		return fmt.Errorf("append ingress: generate inbox ID: %w", err)
	}
	if strings.TrimSpace(inboxID) == "" {
		return fmt.Errorf("append ingress: generated inbox ID is empty")
	}

	payload := bytes.Clone(record.Payload)
	if payload == nil {
		payload = []byte{}
	}
	record.Payload = payload
	effectiveID, err := s.messages.AppendInbox(ctx, sqlite.InboxRecord{
		InboxID:      inboxID,
		AccountID:    record.AccountID,
		Generation:   int64(record.Generation),
		DedupeKey:    record.DedupeKey,
		Codec:        record.Codec,
		CodecVersion: int64(record.CodecVersion),
		Payload:      payload,
	})
	if err != nil {
		return fmt.Errorf("append ingress for account %q: %w", record.AccountID, err)
	}

	counters := s.counters.account(record.AccountID)
	replay := effectiveID != inboxID
	if replay {
		counters.deduped.Add(1)
	} else {
		counters.appended.Add(1)
	}
	s.worker.enqueue(workItem{
		inboxID: effectiveID,
		record:  record,
		replay:  replay,
	})
	return nil
}

// EmitEphemeral counts ephemeral activity. Legacy remains the sole typing UI
// publisher for this slice.
func (s *Sink) EmitEphemeral(_ context.Context, event bridge.EphemeralEvent) error {
	s.counters.account(event.AccountID).ephemeral.Add(1)
	return nil
}

// Beat is intentionally a no-op: generationSink consumes liveness beats
// before forwarding to a configured ConnectionSink.
func (*Sink) Beat(bridge.Generation, time.Time, string) {}

func validateIngress(ctx context.Context, record bridge.RawIngressRecord) error {
	if ctx == nil {
		return fmt.Errorf("append ingress: context is nil")
	}
	if strings.TrimSpace(record.AccountID) == "" {
		return fmt.Errorf("append ingress: account ID is empty")
	}
	if strings.TrimSpace(record.Codec) == "" {
		return fmt.Errorf("append ingress: codec is empty")
	}
	if record.DedupeKey != "" && strings.TrimSpace(record.DedupeKey) == "" {
		return fmt.Errorf("append ingress: dedupe key is whitespace")
	}
	if uint64(record.Generation) > math.MaxInt64 {
		return fmt.Errorf("append ingress: generation %d exceeds SQLite integer range", record.Generation)
	}
	if record.ReceivedAt.IsZero() || record.ReceivedAt.UnixMilli() <= 0 {
		return fmt.Errorf("append ingress: received time is not positive")
	}
	return nil
}
