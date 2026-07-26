package ingest

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/rs/zerolog"
)

// EchoObserver is the transport-neutral echo reconciliation seam implemented
// by messaging.MessageService.
type EchoObserver interface {
	ObserveTransportEcho(context.Context, messaging.TransportEcho) (messaging.EchoOutcome, error)
}

// DecoderRegistration binds a durable codec to both its pure decoder and the
// platform needed for shared key derivation.
type DecoderRegistration struct {
	Codec    string
	Platform bridge.Platform
	Decoder  bridge.Decoder
}

// WorkerConfig configures the single-goroutine projection worker.
type WorkerConfig struct {
	Store         *sqlite.Store
	Messages      *sqlite.MessageRepository
	Reactions     *sqlite.ReactionRepository
	EchoObserver  EchoObserver
	Counters      *Counters
	Logger        zerolog.Logger
	Now           func() time.Time
	QueueCapacity int
	// Decoders explicitly defines the codecs this worker can project.
	Decoders []DecoderRegistration
}

type registeredDecoder struct {
	platform bridge.Platform
	decoder  bridge.Decoder
}

type workItem struct {
	inboxID string
	record  bridge.RawIngressRecord
	replay  bool
}

type googleReactionSnapshot struct {
	accountID      string
	conversationID string
	messageID      string
	entries        []sqlite.ReactionSnapshotEntry
}

// Worker drains durable inbox records and applies normalized events without
// touching the legacy store.
type Worker struct {
	store     *sqlite.Store
	messages  *sqlite.MessageRepository
	reactions *sqlite.ReactionRepository
	echoes    EchoObserver
	counters  *Counters
	logger    zerolog.Logger
	now       func() time.Time
	decoders  map[string]registeredDecoder
	work      chan workItem
	running   atomic.Bool

	changeMu sync.Mutex
	changed  chan struct{}
}

// NewWorker validates and copies its explicitly configured decoder
// registrations. Run starts no work until explicitly invoked by the owner.
func NewWorker(config WorkerConfig) (*Worker, error) {
	if config.Store == nil {
		return nil, fmt.Errorf("create ingest worker: store is nil")
	}
	if config.Messages == nil {
		return nil, fmt.Errorf("create ingest worker: message repository is nil")
	}
	if config.Reactions == nil {
		return nil, fmt.Errorf("create ingest worker: reaction repository is nil")
	}
	if config.Counters == nil {
		config.Counters = &Counters{}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.QueueCapacity <= 0 {
		config.QueueCapacity = defaultWorkerQueueCapacity
	}

	decoders := make(map[string]registeredDecoder, len(config.Decoders))
	for index, registration := range config.Decoders {
		codec := strings.TrimSpace(registration.Codec)
		if codec == "" {
			return nil, fmt.Errorf("create ingest worker: decoder %d codec is empty", index)
		}
		if registration.Decoder == nil {
			return nil, fmt.Errorf("create ingest worker: decoder for codec %q is nil", codec)
		}
		switch registration.Platform {
		case bridge.PlatformGoogle, bridge.PlatformWhatsApp, bridge.PlatformSignal:
		default:
			return nil, fmt.Errorf(
				"create ingest worker: decoder for codec %q has invalid platform %q",
				codec,
				registration.Platform,
			)
		}
		if _, exists := decoders[codec]; exists {
			return nil, fmt.Errorf("create ingest worker: duplicate decoder codec %q", codec)
		}
		decoders[codec] = registeredDecoder{
			platform: registration.Platform,
			decoder:  registration.Decoder,
		}
	}

	return &Worker{
		store:     config.Store,
		messages:  config.Messages,
		reactions: config.Reactions,
		echoes:    config.EchoObserver,
		counters:  config.Counters,
		logger:    config.Logger,
		now:       config.Now,
		decoders:  decoders,
		work:      make(chan workItem, config.QueueCapacity),
		changed:   make(chan struct{}),
	}, nil
}

// Counters returns the worker's shared per-account counter set.
func (w *Worker) Counters() *Counters { return w.counters }

// Run performs a startup crash-recovery drain, then consumes non-blocking sink
// notifications. Only one Run call may be active at a time.
func (w *Worker) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("run ingest worker: context is nil")
	}
	if !w.running.CompareAndSwap(false, true) {
		return fmt.Errorf("run ingest worker: already running")
	}
	defer w.running.Store(false)

	w.drain(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case item := <-w.work:
			// New records are loaded from durable storage. A deduplicated replay
			// is absent from Unprocessed once processed, so re-decode the stable
			// dedupe-equivalent frame to exercise stale-replay classification.
			if item.replay {
				w.handleRecord(ctx, item.inboxID, item.record)
			}
			w.drain(ctx)
		}
	}
}

func (w *Worker) enqueue(item workItem) {
	select {
	case w.work <- item:
	default:
	}
}

func (w *Worker) signalChange() {
	w.changeMu.Lock()
	close(w.changed)
	w.changed = make(chan struct{})
	w.changeMu.Unlock()
}

func (w *Worker) changeChannel() <-chan struct{} {
	w.changeMu.Lock()
	defer w.changeMu.Unlock()
	return w.changed
}

// Changes returns the current ingest-change broadcast channel. The channel
// closes on the next change; callers must call Changes again after it closes.
func (w *Worker) Changes() <-chan struct{} {
	return w.changeChannel()
}

func (w *Worker) drain(ctx context.Context) {
	var records []sqlite.InboxRecord
	err := retryTransient(ctx, func() error {
		var err error
		records, err = w.messages.Unprocessed(ctx)
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Warn().Err(err).Msg("Failed to list unprocessed ingest frames")
		}
		return
	}
	changed := false
	for _, record := range records {
		if ctx.Err() != nil {
			return
		}
		if w.handleRecord(ctx, record.InboxID, rawIngressRecord(record)) {
			changed = true
		}
	}
	if changed {
		w.signalChange()
	}
}

func rawIngressRecord(record sqlite.InboxRecord) bridge.RawIngressRecord {
	return bridge.RawIngressRecord{
		AccountID:    record.AccountID,
		Generation:   bridge.Generation(record.Generation),
		DedupeKey:    record.DedupeKey,
		Codec:        record.Codec,
		CodecVersion: uint32(record.CodecVersion),
		ReceivedAt:   time.UnixMilli(record.ReceivedAtMS),
		Payload:      record.Payload,
	}
}

func (w *Worker) handleRecord(
	ctx context.Context,
	inboxID string,
	record bridge.RawIngressRecord,
) (changed bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			changed = false
			w.markQuarantined(ctx, inboxID, record, fmt.Errorf(
				"ingest frame panic: %v\n%s",
				recovered,
				debug.Stack(),
			))
		}
	}()

	err := retryTransient(ctx, func() error {
		var err error
		changed, err = w.processRecordResult(ctx, inboxID, record)
		return err
	})
	if err == nil || ctx.Err() != nil {
		return changed
	}
	partialChanged := changed
	changed = false

	var stale staleReplayError
	if errors.As(err, &stale) {
		w.counters.account(record.AccountID).staleReplays.Add(1)
		w.logger.Debug().
			Err(stale.cause).
			Str("account_id", record.AccountID).
			Str("inbox_id", inboxID).
			Str("codec", record.Codec).
			Msg("Ignored stale ingest replay")
		return false
	}

	var deterministic quarantineError
	if errors.As(err, &deterministic) {
		w.markQuarantined(ctx, inboxID, record, deterministic.cause)
		return partialChanged
	}

	var exhausted transientExhaustedError
	if errors.As(err, &exhausted) || isTransientDBError(err) {
		w.logger.Warn().
			Err(err).
			Str("account_id", record.AccountID).
			Str("inbox_id", inboxID).
			Str("codec", record.Codec).
			Msg("Ingest projection exhausted transient retries; leaving frame unprocessed")
		return partialChanged
	}

	w.markQuarantined(ctx, inboxID, record, err)
	return partialChanged
}

func (w *Worker) markQuarantined(
	ctx context.Context,
	inboxID string,
	record bridge.RawIngressRecord,
	cause error,
) {
	err := retryTransient(ctx, func() error {
		return w.messages.MarkInboxProcessed(ctx, inboxID, record.AccountID)
	})
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Warn().
				Err(err).
				Str("account_id", record.AccountID).
				Str("inbox_id", inboxID).
				Str("codec", record.Codec).
				Msg("Failed to mark quarantined ingest frame processed")
		}
		return
	}
	w.counters.account(record.AccountID).quarantined.Add(1)
	w.logger.Warn().
		Err(cause).
		Str("account_id", record.AccountID).
		Str("inbox_id", inboxID).
		Str("codec", record.Codec).
		Msg("Quarantined ingest frame")
}

func (w *Worker) processRecord(
	ctx context.Context,
	inboxID string,
	record bridge.RawIngressRecord,
) error {
	_, err := w.processRecordResult(ctx, inboxID, record)
	return err
}

func (w *Worker) processRecordResult(
	ctx context.Context,
	inboxID string,
	record bridge.RawIngressRecord,
) (bool, error) {
	registration, exists := w.decoders[record.Codec]
	if !exists {
		return false, quarantine(fmt.Errorf("unknown ingress codec %q", record.Codec))
	}
	events, err := decodeSafely(ctx, registration.decoder, record)
	if err != nil {
		return false, quarantine(err)
	}
	for index := range events {
		events[index].AccountID = record.AccountID
		events[index].SourceInboxID = inboxID
		if err := validateDecodedEvent(events[index]); err != nil {
			return false, quarantine(fmt.Errorf("decoded event %d: %w", index, err))
		}
	}
	w.counters.account(record.AccountID).decodedEvents.Add(uint64(len(events)))
	changed, err := w.applyEvents(ctx, registration.platform, record, inboxID, events)
	return changed, classifyApplyError(err)
}

func decodeSafely(
	ctx context.Context,
	decoder bridge.Decoder,
	record bridge.RawIngressRecord,
) (events []bridge.Event, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("decoder panic: %v\n%s", recovered, debug.Stack())
			events = nil
		}
	}()
	return decoder.Decode(ctx, record)
}

func validateDecodedEvent(event bridge.Event) error {
	payloads := 0
	if event.Conversation != nil {
		payloads++
	}
	if event.Message != nil {
		payloads++
	}
	if event.MessageMutation != nil {
		payloads++
	}
	if event.Reaction != nil {
		payloads++
	}
	if event.Receipt != nil {
		payloads++
	}
	if payloads != 1 {
		return fmt.Errorf("event has %d payloads, want exactly one", payloads)
	}
	switch event.Kind {
	case bridge.EventConversation:
		if event.Conversation == nil {
			return fmt.Errorf("conversation event has wrong payload")
		}
	case bridge.EventMessage:
		if event.Message == nil {
			return fmt.Errorf("message event has wrong payload")
		}
	case bridge.EventMessageMutation:
		if event.MessageMutation == nil {
			return fmt.Errorf("message mutation event has wrong payload")
		}
	case bridge.EventReaction:
		if event.Reaction == nil {
			return fmt.Errorf("reaction event has wrong payload")
		}
	case bridge.EventReceipt:
		if event.Receipt == nil {
			return fmt.Errorf("receipt event has wrong payload")
		}
	default:
		return fmt.Errorf("unknown event kind %q", event.Kind)
	}
	return nil
}

func (w *Worker) applyEvents(
	ctx context.Context,
	platform bridge.Platform,
	record bridge.RawIngressRecord,
	inboxID string,
	events []bridge.Event,
) (bool, error) {
	accountID := record.AccountID
	changed := false
	googleSnapshots := make(map[string]*googleReactionSnapshot)
	googleSnapshotOrder := make([]string, 0)
	for _, event := range events {
		if event.Kind != bridge.EventConversation {
			continue
		}
		if _, err := w.refreshConversation(accountID, platform, *event.Conversation); err != nil {
			return false, err
		}
		changed = true
	}

	messageCount := 0
	projected := false
	for _, event := range events {
		if event.Kind != bridge.EventMessage {
			continue
		}
		projection, err := w.messageProjection(
			ctx,
			accountID,
			platform,
			inboxID,
			*event.Message,
			messageCount == 0,
		)
		if err != nil {
			return false, err
		}
		if messageCount == 0 {
			if err := w.messages.ProjectMessage(ctx, projection); err != nil {
				return false, err
			}
			projected = true
			w.counters.account(accountID).projected.Add(1)
		} else {
			if err := w.messages.ImportMessage(ctx, projection); err != nil {
				return false, err
			}
			w.counters.account(accountID).imported.Add(1)
		}
		if platform == bridge.PlatformGoogle {
			// Message upserts preserve the first local primary key on a remote-key
			// conflict (including an optimistic outgoing row repointed by the echo
			// observer). Recover that effective parent before binding the embedded
			// snapshot; projection.MessageID is only the would-be derived key.
			effective, err := w.messages.GetMessageByRemote(
				ctx,
				projection.Message.AccountID,
				projection.Message.ConversationID,
				projection.Message.RemoteMessageID,
			)
			if err != nil {
				return false, err
			}
			projection.Message.MessageID = effective.MessageID
		}
		changed = true
		// Conversation rows are never re-upserted from message frames, so
		// recency advances through this targeted monotone bump instead.
		if err := w.store.BumpConversationRecency(
			projection.Message.ConversationID,
			projection.Message.OccurredAtMS,
		); err != nil {
			return false, err
		}
		if platform == bridge.PlatformGoogle {
			key := reactionRemoteTargetKey(
				platform,
				event.Message.RemoteConversationID,
				event.Message.RemoteMessageID,
			)
			if _, exists := googleSnapshots[key]; !exists {
				googleSnapshotOrder = append(googleSnapshotOrder, key)
			}
			googleSnapshots[key] = &googleReactionSnapshot{
				accountID:      projection.Message.AccountID,
				conversationID: projection.Message.ConversationID,
				messageID:      projection.Message.MessageID,
			}
		}
		messageCount++
	}

	for _, event := range events {
		if event.Kind != bridge.EventMessageMutation {
			continue
		}
		mutation := *event.MessageMutation
		if strings.TrimSpace(mutation.RemoteMessageID) == "" {
			return false, fmt.Errorf("message mutation remote ID is empty")
		}
		conversation, remoteConversationID, err := w.existingConversation(
			accountID,
			platform,
			mutation.RemoteConversationID,
		)
		conversationID := conversation.ConversationID
		if errors.Is(err, sqlite.ErrNotFound) {
			// A mutation whose conversation/message was never stored is the same
			// benign missing-target case handled by ApplyMessageMutation. Use the
			// canonical would-be PK without creating a conversation row.
			conversationID = v2keys.DeriveID("conversation", accountID, remoteConversationID)
		} else if err != nil {
			return false, err
		}
		updated, err := w.messages.ApplyMessageMutation(
			ctx,
			accountID,
			conversationID,
			strings.TrimSpace(mutation.RemoteMessageID),
			mutation.Kind,
			mutation.Body,
			inboxID,
		)
		if err != nil {
			return false, err
		}
		if updated {
			w.counters.account(accountID).mutations.Add(1)
			changed = true
		} else {
			w.logger.Debug().
				Str("account_id", accountID).
				Str("inbox_id", inboxID).
				Str("remote_message_id", mutation.RemoteMessageID).
				Msg("Ignored mutation for missing message")
		}
	}

	if platform == bridge.PlatformGoogle {
		for _, event := range events {
			if event.Kind != bridge.EventReaction {
				continue
			}
			reaction := *event.Reaction
			key := reactionRemoteTargetKey(
				platform,
				reaction.RemoteConversationID,
				reaction.TargetRemoteMessageID,
			)
			snapshot, exists := googleSnapshots[key]
			if strings.TrimSpace(reaction.TargetRemoteMessageID) == "" || !exists {
				w.countOrphanReaction(accountID, inboxID, reaction)
				continue
			}
			entry, ok, err := w.prepareReactionSnapshotEntry(accountID, platform, reaction)
			if err != nil {
				return changed, err
			}
			if !ok {
				continue
			}
			snapshot.entries = append(snapshot.entries, entry)
		}

		sourceSeqMS := record.ReceivedAt.UnixMilli()
		if sourceSeqMS <= 0 {
			return changed, fmt.Errorf("reaction snapshot source sequence is not positive")
		}
		for _, key := range googleSnapshotOrder {
			snapshot := googleSnapshots[key]
			changes, err := w.reactions.ReplaceEmbeddedReactions(
				ctx,
				snapshot.messageID,
				snapshot.accountID,
				snapshot.conversationID,
				snapshot.entries,
				sourceSeqMS,
			)
			if err != nil {
				return changed, err
			}
			if changes.Applied > 0 {
				w.counters.account(accountID).reactionsApplied.Add(uint64(changes.Applied))
			}
			if changes.Removed > 0 {
				w.counters.account(accountID).reactionsRemoved.Add(uint64(changes.Removed))
			}
			if changes.Applied > 0 || changes.Removed > 0 {
				changed = true
			}
		}
	} else {
		for _, event := range events {
			if event.Kind != bridge.EventReaction {
				continue
			}
			reaction := *event.Reaction
			target, found, err := w.resolveReactionTarget(
				ctx,
				accountID,
				platform,
				reaction,
			)
			if err != nil {
				return changed, err
			}
			if !found {
				w.countOrphanReaction(accountID, inboxID, reaction)
				continue
			}
			apply, err := w.prepareReactionApply(
				accountID,
				platform,
				target,
				reaction,
				record.ReceivedAt.UnixMilli(),
			)
			if err != nil {
				return changed, err
			}
			applied, err := w.reactions.ApplyReaction(ctx, apply)
			if err != nil {
				return changed, err
			}
			if !applied {
				continue
			}
			switch reaction.Action {
			case bridge.ReactionRemove:
				w.counters.account(accountID).reactionsRemoved.Add(1)
			default:
				w.counters.account(accountID).reactionsApplied.Add(1)
			}
			changed = true
		}
	}

	for _, event := range events {
		if event.Kind != bridge.EventReceipt {
			continue
		}
		if !event.Receipt.Actor.IsSelf {
			w.counters.account(accountID).receiptsDropped.Add(1)
			continue
		}
		// Only read-class self receipts advance the read cursor. A delivered
		// acknowledgement for an outgoing message says nothing about what the
		// local user has read.
		if event.Receipt.Status != "read" {
			w.counters.account(accountID).receiptsDropped.Add(1)
			continue
		}
		applied, err := w.advanceReadCursor(ctx, accountID, platform, *event.Receipt)
		if err != nil {
			return false, err
		}
		if applied {
			w.counters.account(accountID).receiptsSelf.Add(1)
			changed = true
		} else {
			// The conversation or local device is not known yet. That is an
			// ordering gap, not a defect: drop the cursor advance benignly
			// rather than quarantining a valid frame.
			w.counters.account(accountID).receiptsDropped.Add(1)
		}
	}

	if !projected {
		if err := w.messages.MarkInboxProcessed(ctx, inboxID, accountID); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func (w *Worker) refreshConversation(
	accountID string,
	platform bridge.Platform,
	event bridge.ConversationEvent,
) (sqlite.Conversation, error) {
	remoteID := v2keys.NormalizeRemoteConversationID(string(platform), event.RemoteConversationID)
	if strings.TrimSpace(remoteID) == "" {
		return sqlite.Conversation{}, fmt.Errorf("conversation remote ID is empty")
	}
	type preparedParticipant struct {
		participant bridge.Participant
		role        sqlite.ParticipantRole
	}
	prepared := make([]preparedParticipant, 0, len(event.Participants))
	seenIdentities := make(map[string]struct{}, len(event.Participants))
	for _, participant := range event.Participants {
		raw := identityRaw(participant.Identity)
		key, keyErr := v2keys.IdentityKey(accountID, string(platform), raw)
		if keyErr != nil {
			return sqlite.Conversation{}, keyErr
		}
		naturalKey := key.Kind + "\x1f" + key.Canonical
		if _, duplicate := seenIdentities[naturalKey]; duplicate {
			return sqlite.Conversation{}, fmt.Errorf(
				"conversation participant identity %q is duplicated",
				key.Canonical,
			)
		}
		seenIdentities[naturalKey] = struct{}{}
		role, roleErr := participantRole(participant.Role)
		if roleErr != nil {
			return sqlite.Conversation{}, roleErr
		}
		prepared = append(prepared, preparedParticipant{participant: participant, role: role})
	}
	nowMS, err := w.nowMS()
	if err != nil {
		return sqlite.Conversation{}, err
	}

	conversation, err := w.store.GetConversationByRemote(accountID, remoteID)
	if errors.Is(err, sqlite.ErrNotFound) {
		kind, kindErr := conversationKind(event.Kind, sqlite.ConversationKindDirect)
		if kindErr != nil {
			return sqlite.Conversation{}, kindErr
		}
		conversation = sqlite.Conversation{
			ConversationID:       v2keys.DeriveID("conversation", accountID, remoteID),
			AccountID:            accountID,
			RemoteConversationID: remoteID,
			Kind:                 kind,
			Title:                event.Title,
			RemoteRevision:       optionalTrimmed(event.RemoteRevision),
			NotificationMode:     sqlite.NotificationModeAll,
			MetadataJSON:         "{}",
			CreatedAtMS:          nowMS,
			UpdatedAtMS:          nowMS,
		}
	} else if err != nil {
		return sqlite.Conversation{}, err
	} else {
		if strings.TrimSpace(event.Kind) != "" {
			kind, kindErr := conversationKind(event.Kind, conversation.Kind)
			if kindErr != nil {
				return sqlite.Conversation{}, kindErr
			}
			conversation.Kind = kind
		}
		conversation.Title = event.Title
		conversation.RemoteRevision = optionalTrimmed(event.RemoteRevision)
		conversation.UpdatedAtMS = nowMS
	}
	if err := w.store.UpsertConversation(conversation); err != nil {
		return sqlite.Conversation{}, err
	}
	conversation, err = w.store.GetConversationByRemote(accountID, remoteID)
	if err != nil {
		return sqlite.Conversation{}, err
	}

	participants := make([]sqlite.ConversationParticipant, 0, len(prepared))
	for _, item := range prepared {
		identity, identityErr := w.resolveIdentity(accountID, platform, item.participant.Identity)
		if identityErr != nil {
			return sqlite.Conversation{}, identityErr
		}
		participants = append(participants, sqlite.ConversationParticipant{
			AccountID:      accountID,
			ConversationID: conversation.ConversationID,
			IdentityID:     identity.IdentityID,
			Role:           item.role,
			DisplayName:    item.participant.Identity.Name,
			IsActive:       item.participant.Active,
		})
	}
	if err := w.store.ReplaceConversationParticipants(conversation.ConversationID, participants); err != nil {
		return sqlite.Conversation{}, err
	}
	return conversation, nil
}

func (w *Worker) ensureMessageConversation(
	accountID string,
	platform bridge.Platform,
	remoteConversationID string,
	occurredAtMS int64,
) (sqlite.Conversation, string, error) {
	conversation, remoteID, err := w.existingConversation(accountID, platform, remoteConversationID)
	if err == nil {
		return conversation, remoteID, nil
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Conversation{}, "", err
	}
	if occurredAtMS <= 0 {
		occurredAtMS, err = w.nowMS()
		if err != nil {
			return sqlite.Conversation{}, "", err
		}
	}
	// Message events carry no kind, so infer it from the remote ID where that
	// is authoritative (Signal/WhatsApp); otherwise keep the direct default and
	// let a ConversationEvent correct it.
	kind := sqlite.ConversationKindDirect
	if inferred, ok := conversationKindForRemoteID(platform, remoteID); ok {
		kind = inferred
	}
	conversation = sqlite.Conversation{
		ConversationID:       v2keys.DeriveID("conversation", accountID, remoteID),
		AccountID:            accountID,
		RemoteConversationID: remoteID,
		Kind:                 kind,
		NotificationMode:     sqlite.NotificationModeAll,
		LastMessageAtMS:      occurredAtMS,
		MetadataJSON:         "{}",
		CreatedAtMS:          occurredAtMS,
		UpdatedAtMS:          occurredAtMS,
	}
	if err := w.store.UpsertConversation(conversation); err != nil {
		return sqlite.Conversation{}, "", err
	}
	conversation, err = w.store.GetConversationByRemote(accountID, remoteID)
	return conversation, remoteID, err
}

func (w *Worker) existingConversation(
	accountID string,
	platform bridge.Platform,
	remoteConversationID string,
) (sqlite.Conversation, string, error) {
	remoteID := v2keys.NormalizeRemoteConversationID(string(platform), remoteConversationID)
	if strings.TrimSpace(remoteID) == "" {
		return sqlite.Conversation{}, "", fmt.Errorf("conversation remote ID is empty")
	}
	conversation, err := w.store.GetConversationByRemote(accountID, remoteID)
	return conversation, remoteID, err
}

func (w *Worker) resolveIdentity(
	accountID string,
	platform bridge.Platform,
	reference bridge.IdentityRef,
) (sqlite.Identity, error) {
	raw := identityRaw(reference)
	key, err := v2keys.IdentityKey(accountID, string(platform), raw)
	if err != nil {
		return sqlite.Identity{}, err
	}
	nowMS, err := w.nowMS()
	if err != nil {
		return sqlite.Identity{}, err
	}
	kind := sqlite.IdentityKind(key.Kind)
	identity, err := w.store.GetIdentityByCanonical(accountID, kind, key.Canonical)
	if errors.Is(err, sqlite.ErrNotFound) {
		identity = sqlite.Identity{
			IdentityID:     v2keys.DeriveID("identity", accountID, key.Kind+"\x1f"+key.Canonical),
			AccountID:      accountID,
			Kind:           kind,
			CanonicalValue: key.Canonical,
			RawValue:       raw,
			DisplayName:    strings.TrimSpace(reference.Name),
			IsSelf:         reference.IsSelf,
			MetadataJSON:   "{}",
			CreatedAtMS:    nowMS,
			UpdatedAtMS:    nowMS,
		}
	} else if err != nil {
		return sqlite.Identity{}, err
	} else {
		identity.RawValue = raw
		if name := strings.TrimSpace(reference.Name); name != "" {
			identity.DisplayName = name
		}
		identity.IsSelf = identity.IsSelf || reference.IsSelf
		identity.UpdatedAtMS = nowMS
	}
	if err := w.store.UpsertIdentity(identity); err != nil {
		return sqlite.Identity{}, err
	}
	return w.store.GetIdentityByCanonical(accountID, kind, key.Canonical)
}

func identityRaw(reference bridge.IdentityRef) string {
	raw := strings.TrimSpace(reference.Raw)
	if raw == "" {
		raw = strings.TrimSpace(reference.Canonical)
	}
	return raw
}

type preparedReactionActor struct {
	key        string
	identityID *string
	isSelf     bool
	label      string
}

func (w *Worker) prepareReactionActor(
	accountID string,
	platform bridge.Platform,
	reference bridge.IdentityRef,
	emoji string,
) (preparedReactionActor, error) {
	if reference.IsSelf {
		return preparedReactionActor{
			key:    "self",
			isSelf: true,
			label:  "me",
		}, nil
	}
	if identityRaw(reference) == "" {
		// On the delta path, an anonymous remove with an empty emoji derives
		// "anon:" and cannot correlate with an earlier "anon:<emoji>" add. This
		// is acceptable: well-formed WhatsApp/Signal frames carry an actor, while
		// Google snapshots remove absent reactors without using this key.
		return preparedReactionActor{key: "anon:" + emoji}, nil
	}
	identity, err := w.resolveIdentity(accountID, platform, reference)
	if err != nil {
		return preparedReactionActor{}, err
	}
	identityID := identity.IdentityID
	return preparedReactionActor{
		key:        identityID,
		identityID: &identityID,
	}, nil
}

func (w *Worker) prepareReactionSnapshotEntry(
	accountID string,
	platform bridge.Platform,
	reaction bridge.ReactionEvent,
) (sqlite.ReactionSnapshotEntry, bool, error) {
	emoji := strings.TrimSpace(reaction.Emoji)
	if emoji == "" {
		return sqlite.ReactionSnapshotEntry{}, false, nil
	}
	actor, err := w.prepareReactionActor(accountID, platform, reaction.Actor, emoji)
	if err != nil {
		return sqlite.ReactionSnapshotEntry{}, false, err
	}
	return sqlite.ReactionSnapshotEntry{
		ReactorKey:        actor.key,
		ReactorIdentityID: actor.identityID,
		ReactorIsSelf:     actor.isSelf,
		ReactorLabel:      actor.label,
		Emoji:             emoji,
	}, true, nil
}

func (w *Worker) prepareReactionApply(
	accountID string,
	platform bridge.Platform,
	target sqlite.Message,
	reaction bridge.ReactionEvent,
	sourceSeqMS int64,
) (sqlite.ReactionApply, error) {
	actor, err := w.prepareReactionActor(accountID, platform, reaction.Actor, reaction.Emoji)
	if err != nil {
		return sqlite.ReactionApply{}, err
	}
	occurredAtMS := reaction.OccurredAt.UnixMilli()
	if occurredAtMS <= 0 {
		return sqlite.ReactionApply{}, fmt.Errorf("reaction occurrence time is not positive")
	}
	if sourceSeqMS <= 0 {
		return sqlite.ReactionApply{}, fmt.Errorf("reaction source sequence is not positive")
	}
	return sqlite.ReactionApply{
		AccountID:         accountID,
		ConversationID:    target.ConversationID,
		MessageID:         target.MessageID,
		ReactorKey:        actor.key,
		ReactorIdentityID: actor.identityID,
		ReactorIsSelf:     actor.isSelf,
		ReactorLabel:      actor.label,
		Emoji:             reaction.Emoji,
		Action:            reaction.Action,
		OccurredAtMS:      occurredAtMS,
		SourceSeqMS:       sourceSeqMS,
	}, nil
}

func (w *Worker) resolveReactionTarget(
	ctx context.Context,
	accountID string,
	platform bridge.Platform,
	reaction bridge.ReactionEvent,
) (sqlite.Message, bool, error) {
	conversation, remoteConversationID, err := w.existingConversation(
		accountID,
		platform,
		reaction.RemoteConversationID,
	)
	if errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Message{}, false, nil
	}
	if err != nil {
		return sqlite.Message{}, false, err
	}
	targetRemoteID := strings.TrimSpace(reaction.TargetRemoteMessageID)
	target, err := w.messages.GetMessageByRemote(
		ctx,
		accountID,
		conversation.ConversationID,
		targetRemoteID,
	)
	if err == nil {
		return target, true, nil
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Message{}, false, err
	}
	if platform != bridge.PlatformSignal {
		return sqlite.Message{}, false, nil
	}

	timestamp, err := strconv.ParseInt(targetRemoteID, 10, 64)
	if err != nil {
		return sqlite.Message{}, false, nil
	}
	alias := v2keys.SignalLocalAlias(remoteConversationID, timestamp)
	target, err = w.messages.GetMessageByRemote(
		ctx,
		accountID,
		conversation.ConversationID,
		alias,
	)
	if errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Message{}, false, nil
	}
	if err != nil {
		return sqlite.Message{}, false, err
	}
	return target, true, nil
}

func (w *Worker) countOrphanReaction(
	accountID string,
	inboxID string,
	reaction bridge.ReactionEvent,
) {
	w.counters.account(accountID).reactionsOrphaned.Add(1)
	w.logger.Debug().
		Str("account_id", accountID).
		Str("inbox_id", inboxID).
		Str("remote_message_id", reaction.TargetRemoteMessageID).
		Msg("Ignored reaction for missing message")
}

func reactionRemoteTargetKey(
	platform bridge.Platform,
	remoteConversationID string,
	remoteMessageID string,
) string {
	return v2keys.NormalizeRemoteConversationID(string(platform), remoteConversationID) +
		"\x1f" + strings.TrimSpace(remoteMessageID)
}

func (w *Worker) messageProjection(
	ctx context.Context,
	accountID string,
	platform bridge.Platform,
	inboxID string,
	event bridge.MessageEvent,
	routeEcho bool,
) (sqlite.MessageProjection, error) {
	direction, err := messageDirection(event.Direction)
	if err != nil {
		return sqlite.MessageProjection{}, err
	}
	remoteMessageID := strings.TrimSpace(event.RemoteMessageID)
	if remoteMessageID == "" {
		return sqlite.MessageProjection{}, fmt.Errorf("message remote ID is empty")
	}

	if routeEcho && strings.TrimSpace(event.ClientRequestID) != "" && direction == sqlite.MessageDirectionOutgoing {
		if w.echoes == nil {
			return sqlite.MessageProjection{}, fmt.Errorf("echo observer is unavailable")
		}
		outcome, err := w.echoes.ObserveTransportEcho(ctx, messaging.TransportEcho{
			AccountID:          accountID,
			TransportRequestID: event.ClientRequestID,
			RemoteMessageID:    remoteMessageID,
		})
		switch {
		case err != nil:
			// Echo reconciliation is best-effort enrichment of outbound send
			// state. A reconcile fault (for example a concurrent confirm by
			// the dispatcher) must never block durable delivery of the
			// inbound message itself, so count it and keep projecting.
			w.counters.account(accountID).echoErrors.Add(1)
			w.logger.Warn().
				Str("account_id", accountID).
				Str("transport_request_id", event.ClientRequestID).
				Err(err).
				Msg("ingest: transport echo reconcile failed; projecting anyway")
		case outcome == messaging.EchoEnriched:
			w.counters.account(accountID).echoEnriched.Add(1)
		case outcome == messaging.EchoNotFound:
			w.counters.account(accountID).echoNotFound.Add(1)
		case outcome == messaging.EchoNoop:
			w.counters.account(accountID).echoNoop.Add(1)
		default:
			w.counters.account(accountID).echoReconciled.Add(1)
		}
	}

	occurredAtMS := event.OccurredAt.UnixMilli()
	if occurredAtMS <= 0 {
		return sqlite.MessageProjection{}, fmt.Errorf("message occurrence time is not positive")
	}
	conversation, remoteConversationID, err := w.ensureMessageConversation(
		accountID,
		platform,
		event.RemoteConversationID,
		occurredAtMS,
	)
	if err != nil {
		return sqlite.MessageProjection{}, err
	}

	if platform == bridge.PlatformSignal && direction == sqlite.MessageDirectionOutgoing {
		remoteMessageID, err = w.signalOutgoingRemoteID(
			ctx,
			accountID,
			conversation.ConversationID,
			remoteConversationID,
			remoteMessageID,
		)
		if err != nil {
			return sqlite.MessageProjection{}, err
		}
	}

	var senderIdentityID *string
	// A transport may deliver a message whose sender carries only a display
	// name (group RCS puts the number in the conversation roster, not the
	// message). Legacy stores that as an empty sender rather than dropping the
	// message; the v2 path degrades the same way — project with a null sender
	// instead of quarantining real content on an unresolvable identity.
	if direction != sqlite.MessageDirectionOutgoing && !event.Sender.IsSelf && identityRaw(event.Sender) != "" {
		identity, identityErr := w.resolveIdentity(accountID, platform, event.Sender)
		if identityErr != nil {
			return sqlite.MessageProjection{}, identityErr
		}
		senderIdentityID = &identity.IdentityID
		if err := w.ensureDirectPeerParticipant(conversation, identity); err != nil {
			return sqlite.MessageProjection{}, err
		}
	} else if conversation.Kind == sqlite.ConversationKindDirect {
		if err := w.ensureDirectPeerFromRemoteID(
			accountID, platform, conversation, remoteConversationID,
		); err != nil {
			return sqlite.MessageProjection{}, err
		}
	}

	var replyToRemoteID *string
	if reply := strings.TrimSpace(event.ReplyToRemoteID); reply != "" {
		replyToRemoteID = &reply
	}
	attachments := make([]sqlite.MessageAttachment, 0, len(event.Attachments))
	for index, attachment := range event.Attachments {
		size := attachment.Size
		attachments = append(attachments, sqlite.MessageAttachment{
			RemoteID:  attachment.RemoteID,
			Ordinal:   int64(index),
			RemoteRef: attachment.RemoteRef,
			Filename:  attachment.Filename,
			MIME:      attachment.MIME,
			SizeBytes: &size,
			State:     "pending",
		})
	}

	return sqlite.MessageProjection{
		InboxID: inboxID,
		Message: sqlite.Message{
			MessageID:        v2keys.DeriveID("message", accountID, remoteConversationID+"\x1f"+remoteMessageID),
			ConversationID:   conversation.ConversationID,
			AccountID:        accountID,
			RemoteMessageID:  remoteMessageID,
			SenderIdentityID: senderIdentityID,
			Direction:        direction,
			Body:             event.Body,
			ReplyToRemoteID:  replyToRemoteID,
			State:            sqlite.MessageStateActive,
			OccurredAtMS:     occurredAtMS,
		},
		Attachments: attachments,
	}, nil
}

func (w *Worker) signalOutgoingRemoteID(
	ctx context.Context,
	accountID string,
	conversationID string,
	remoteConversationID string,
	remoteMessageID string,
) (string, error) {
	if _, err := w.messages.GetMessageByRemote(ctx, accountID, conversationID, remoteMessageID); err == nil {
		return remoteMessageID, nil
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return "", err
	}
	timestamp, err := strconv.ParseInt(remoteMessageID, 10, 64)
	if err != nil {
		return remoteMessageID, nil
	}
	alias := v2keys.SignalLocalAlias(remoteConversationID, timestamp)
	if _, err := w.messages.GetMessageByRemote(ctx, accountID, conversationID, alias); err == nil {
		return alias, nil
	} else if errors.Is(err, sqlite.ErrNotFound) {
		return remoteMessageID, nil
	} else {
		return "", err
	}
}

func (w *Worker) advanceReadCursor(
	ctx context.Context,
	accountID string,
	platform bridge.Platform,
	event bridge.ReceiptEvent,
) (bool, error) {
	conversation, _, err := w.existingConversation(accountID, platform, event.RemoteConversationID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	device, err := w.store.GetLocalInstallationDevice(ctx, accountID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	var lastReadMessageID *string
	for index := len(event.RemoteMessageIDs) - 1; index >= 0; index-- {
		remoteID := strings.TrimSpace(event.RemoteMessageIDs[index])
		if remoteID == "" {
			continue
		}
		message, getErr := w.messages.GetMessageByRemote(
			ctx,
			accountID,
			conversation.ConversationID,
			remoteID,
		)
		if errors.Is(getErr, sqlite.ErrNotFound) {
			break
		}
		if getErr != nil {
			return false, getErr
		}
		lastReadMessageID = &message.MessageID
		break
	}
	readAtMS := event.OccurredAt.UnixMilli()
	if readAtMS <= 0 {
		return false, fmt.Errorf("receipt occurrence time is not positive")
	}
	updatedAtMS, err := w.nowMS()
	if err != nil {
		return false, err
	}
	if err := w.store.UpsertReadCursor(sqlite.ReadCursor{
		AccountID:         accountID,
		DeviceID:          device.DeviceID,
		ConversationID:    conversation.ConversationID,
		LastReadMessageID: lastReadMessageID,
		LastReadAtMS:      readAtMS,
		UpdatedAtMS:       updatedAtMS,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (w *Worker) nowMS() (int64, error) {
	nowMS := w.now().UnixMilli()
	if nowMS <= 0 {
		return 0, fmt.Errorf("ingest worker clock is not positive")
	}
	return nowMS, nil
}

func conversationKind(value string, fallback sqlite.ConversationKind) (sqlite.ConversationKind, error) {
	switch kind := sqlite.ConversationKind(strings.TrimSpace(value)); kind {
	case "":
		return fallback, nil
	case sqlite.ConversationKindDirect,
		sqlite.ConversationKindGroup,
		sqlite.ConversationKindBroadcast,
		sqlite.ConversationKindSystem:
		return kind, nil
	default:
		return "", fmt.Errorf("invalid conversation kind %q", value)
	}
}

func participantRole(value string) (sqlite.ParticipantRole, error) {
	switch role := sqlite.ParticipantRole(strings.TrimSpace(value)); role {
	case "", sqlite.ParticipantRoleMember:
		return sqlite.ParticipantRoleMember, nil
	case sqlite.ParticipantRoleAdmin, sqlite.ParticipantRoleOwner, sqlite.ParticipantRoleUnknown:
		return role, nil
	default:
		return "", fmt.Errorf("invalid participant role %q", value)
	}
}

func messageDirection(value string) (sqlite.MessageDirection, error) {
	switch direction := sqlite.MessageDirection(strings.TrimSpace(value)); direction {
	case sqlite.MessageDirectionIncoming, sqlite.MessageDirectionOutgoing:
		return direction, nil
	default:
		return "", fmt.Errorf("invalid message direction %q", value)
	}
}

func optionalTrimmed(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
