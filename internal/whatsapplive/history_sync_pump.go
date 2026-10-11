package whatsapplive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	waHistorySync "go.mau.fi/whatsmeow/proto/waHistorySync"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// The history-sync pump works through the history-sync queue
// (history_sync_queue.go), oldest notification first, while the account is
// connected. For each notification it downloads the chunk, hands it to the
// bridge like any captured frame (committed to the observing generation, or
// held in the ingress spool for the next one), and only then sends the
// history-sync receipt, deletes the chunk's server copy and removes the row.
// A process that ends at any point leaves the row for the next run.
//
// One notification's failure never holds up the ones behind it. A download
// that cannot succeed (the server copy is gone, the chunk does not parse) is
// dropped at once; one that fails otherwise is retried with backoff and
// dropped after historySyncMaxAttempts. A drop sends the history-sync receipt
// anyway (whatsmeow's default handling sends it for every notification) and
// logs at error level. A failure caused by the connection going away counts
// no attempt: the next connection resumes the queue.

const (
	historySyncMaxAttempts    = 10
	historySyncRetryMin       = 30 * time.Second
	historySyncRetryMax       = time.Hour
	historySyncAttemptTimeout = 5 * time.Minute
)

var (
	// errHistorySyncUnreadable wraps a chunk whose bytes were fetched but do
	// not decompress or parse; fetching them again gives the same bytes.
	errHistorySyncUnreadable = errors.New("whatsapp history-sync chunk does not parse")
	// errHistorySyncNotApplied reports a chunk the bridge neither committed
	// nor held (v2 refused it and the spool could not take it, or the
	// client was replaced while it downloaded).
	errHistorySyncNotApplied = errors.New("whatsapp history-sync chunk neither committed nor held")
)

type historySyncPump struct {
	queue *historySyncQueue
	now   func() time.Time

	wakeCh chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
	// passMu admits one pass at a time, so notifications are attempted
	// oldest first and never twice at once.
	passMu sync.Mutex

	// The pump's effects on WhatsApp. Tests replace them.
	download    func(context.Context, *whatsmeow.Client, *waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error)
	sendReceipt func(context.Context, *whatsmeow.Client, watypes.MessageID) error
	deleteMedia func(context.Context, *whatsmeow.Client, *waE2E.HistorySyncNotification) error
}

func newHistorySyncPump(queue *historySyncQueue) *historySyncPump {
	return &historySyncPump{
		queue:    queue,
		now:      time.Now,
		wakeCh:   make(chan struct{}, 1),
		download: downloadHistorySyncChunk,
		sendReceipt: func(ctx context.Context, cli *whatsmeow.Client, id watypes.MessageID) error {
			return cli.SendProtocolMessageReceipt(ctx, id, watypes.ReceiptTypeHistorySync)
		},
		deleteMedia: func(ctx context.Context, cli *whatsmeow.Client, notification *waE2E.HistorySyncNotification) error {
			return cli.DeleteMedia(ctx, whatsmeow.MediaHistory, notification.GetDirectPath(), notification.GetFileEncSHA256(), notification.GetEncHandle())
		},
	}
}

// downloadHistorySyncChunk fetches a notification's compressed chunk (inline,
// or from WhatsApp's media server) and parses it with whatsmeow's
// DownloadHistorySync given those bytes inline, so the parse touches no
// network. Storage is synchronous: what the chunk carries for whatsmeow (LID
// mappings, push names, message secrets, the NCT salt) is stored before the
// chunk is handed on and its row removed. Parsing and storage are local and
// short, and run detached from ctx, as whatsmeow's own loop runs its storage
// (context.WithoutCancel): a Close that cancels ctx mid-storage must not
// leave a chunk that commits with its side data half stored.
func downloadHistorySyncChunk(ctx context.Context, cli *whatsmeow.Client, notification *waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error) {
	data := notification.InitialHistBootstrapInlinePayload
	if data == nil {
		var err error
		if data, err = cli.Download(ctx, notification); err != nil {
			return nil, err
		}
	}
	inline := proto.Clone(notification).(*waE2E.HistorySyncNotification)
	inline.InitialHistBootstrapInlinePayload = data
	chunk, err := cli.DownloadHistorySync(context.WithoutCancel(ctx), inline, true)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errHistorySyncUnreadable, err)
	}
	return chunk, nil
}

func (p *historySyncPump) wake() {
	if p == nil {
		return
	}
	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

func (b *Bridge) historySyncPump() *historySyncPump {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.historySync
}

func (b *Bridge) wakeHistorySync() {
	b.historySyncPump().wake()
}

// startHistorySyncPump runs the pump until stopHistorySyncPump. A pass runs
// when the pump is woken (a notification was queued, the account connected,
// a generation began observing ingress) and when a deferred attempt is due.
func (b *Bridge) startHistorySyncPump() {
	pump := b.historySyncPump()
	if pump == nil || pump.done != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	pump.cancel = cancel
	pump.done = make(chan struct{})
	go b.runHistorySyncPump(ctx, pump)
	if pump.queue.pending() > 0 {
		pump.wake()
	}
}

func (b *Bridge) runHistorySyncPump(ctx context.Context, pump *historySyncPump) {
	defer close(pump.done)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pump.wakeCh:
		case <-timer.C:
		}
		next := b.pumpHistorySyncContained(ctx, pump)
		timer.Stop()
		if !next.IsZero() {
			timer.Reset(max(next.Sub(pump.now()), 0))
		}
	}
}

// pumpHistorySyncContained runs one pass and contains a panic in it, so a
// bug in one notification's handling cannot stop the pump until the next
// run; the pass is retried later.
func (b *Bridge) pumpHistorySyncContained(ctx context.Context, pump *historySyncPump) (next time.Time) {
	defer func() {
		if recovered := recover(); recovered != nil {
			b.logger.Error().
				Str("panic", fmt.Sprint(recovered)).
				Msg("Recovered panic in the WhatsApp history-sync pump")
			next = pump.now().Add(historySyncRetryMin)
		}
	}()
	return b.pumpHistorySync(ctx)
}

// stopHistorySyncPump cancels the pump and waits for it to return. Close
// calls it before it closes the database the queue lives in.
func (b *Bridge) stopHistorySyncPump() {
	pump := b.historySyncPump()
	if pump == nil || pump.done == nil {
		return
	}
	pump.cancel()
	<-pump.done
}

// historySyncClient returns the client a pass works with, and false while
// the account is not connected (a pass then waits for Connected).
func (b *Bridge) historySyncClient() (*whatsmeow.Client, uint64, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	cli := b.client
	if !b.connected || cli == nil || cli.Store == nil || cli.Store.ID == nil {
		return nil, 0, false
	}
	return cli, b.clientGeneration, true
}

// pumpHistorySync makes one pass over the notifications that are due, oldest
// first, and returns when the next deferred one comes due (zero when none is
// deferred, or the pass stopped because the account is not connected, which
// Connected ends by waking the pump).
func (b *Bridge) pumpHistorySync(ctx context.Context) time.Time {
	pump := b.historySyncPump()
	if pump == nil {
		return time.Time{}
	}
	pump.passMu.Lock()
	defer pump.passMu.Unlock()
	if pump.queue.pending() == 0 {
		return time.Time{}
	}
	seqs, err := pump.queue.due(pump.now())
	if err != nil {
		b.logger.Warn().Err(err).Msg("Failed to list the WhatsApp history-sync queue")
		return pump.now().Add(historySyncRetryMin)
	}
	for _, seq := range seqs {
		if ctx.Err() != nil {
			return time.Time{}
		}
		cli, generation, ok := b.historySyncClient()
		if !ok {
			return time.Time{}
		}
		if stop := b.pumpHistorySyncRow(ctx, pump, cli, generation, seq); stop {
			if ctx.Err() != nil {
				return time.Time{}
			}
			return pump.now().Add(historySyncRetryMin)
		}
	}
	next, ok, err := pump.queue.nextAttempt(pump.now())
	if err != nil {
		b.logger.Warn().Err(err).Msg("Failed to read the WhatsApp history-sync queue")
		return pump.now().Add(historySyncRetryMin)
	}
	if !ok {
		return time.Time{}
	}
	return next
}

// pumpHistorySyncRow takes one notification one step as far as it can go. It
// returns true when the pass should stop: the connection went away, or the
// queue's database failed.
func (b *Bridge) pumpHistorySyncRow(ctx context.Context, pump *historySyncPump, cli *whatsmeow.Client, generation uint64, seq int64) (stop bool) {
	row, ok, err := pump.queue.load(seq)
	if errors.Is(err, errHistorySyncRowUnreadable) {
		b.dropHistorySync(ctx, pump, cli, row, err, true)
		return false
	}
	if err != nil {
		b.logger.Warn().Err(err).Msg("Failed to load a queued WhatsApp history-sync notification")
		return true
	}
	if !ok {
		return false
	}
	if own := cli.Store.ID.User; row.accountUser != own {
		// The account was unpaired and another one paired: the notification
		// names history this account cannot fetch, and the receipt is not
		// this account's to send.
		if err := pump.queue.remove(row.seq); err != nil {
			b.logger.Warn().Err(err).Msg("Failed to remove a WhatsApp history-sync notification of another account")
			return true
		}
		b.logger.Warn().
			Str("msg_id", string(row.messageID)).
			Msg("Dropped a queued WhatsApp history-sync notification received by another account")
		return false
	}

	if !row.committed {
		attemptCtx, cancel := context.WithTimeout(ctx, historySyncAttemptTimeout)
		chunk, err := b.downloadQueuedHistorySync(attemptCtx, pump, cli, row)
		cancel()
		if err != nil {
			return b.historySyncFailed(ctx, pump, cli, row, "download", err)
		}
		if !b.applyQueuedHistorySync(cli, generation, chunk) {
			// Back-pressure, not a failed attempt: v2 is refusing frames and
			// the spool is full, or the client changed. The row waits.
			if err := pump.queue.deferAttempt(row.seq, row.attempts, pump.now().Add(historySyncRetryMin), errHistorySyncNotApplied); err != nil {
				b.logger.Warn().Err(err).Msg("Failed to defer a WhatsApp history-sync notification")
				return true
			}
			b.logger.Warn().
				Str("msg_id", string(row.messageID)).
				Msg("WhatsApp history-sync chunk neither committed nor held; keeping its notification queued")
			return false
		}
		if err := pump.queue.markCommitted(row.seq, pump.now()); err != nil {
			// The chunk is committed or held; if the row stays uncommitted a
			// later pass commits it again under the same dedupe key.
			b.logger.Warn().Err(err).Str("msg_id", string(row.messageID)).Msg("Failed to mark a WhatsApp history-sync chunk committed")
		}
		row.committed = true
	}

	if err := pump.sendReceipt(ctx, cli, row.messageID); err != nil {
		return b.historySyncFailed(ctx, pump, cli, row, "receipt", err)
	}
	b.deleteHistorySyncMedia(ctx, pump, cli, row)
	if err := pump.queue.remove(row.seq); err != nil {
		b.logger.Warn().Err(err).Str("msg_id", string(row.messageID)).Msg("Failed to remove a finished WhatsApp history-sync notification")
		return true
	}
	b.logger.Info().
		Str("msg_id", string(row.messageID)).
		Str("sync_type", row.notification.GetSyncType().String()).
		Uint32("chunk_order", row.notification.GetChunkOrder()).
		Int("attempts", row.attempts+1).
		Msg("Committed a WhatsApp history-sync chunk")
	return false
}

// downloadQueuedHistorySync runs the pump's download and turns a panic into
// an error, so a chunk that crashes whatsmeow counts attempts and is
// eventually dropped instead of stopping the pump.
func (b *Bridge) downloadQueuedHistorySync(ctx context.Context, pump *historySyncPump, cli *whatsmeow.Client, row queuedHistorySync) (chunk *waHistorySync.HistorySync, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			chunk, err = nil, fmt.Errorf("history-sync download panicked: %v", recovered)
		}
	}()
	return pump.download(ctx, cli, row.notification)
}

// applyQueuedHistorySync hands a downloaded chunk to the bridge, as
// whatsmeow's loop dispatched one, and reports whether its frame was
// committed or held (applyHistorySync; the legacy store takes the chunk on
// its first application either way). The frame commits as Replayed: the pump may run long after the connection that
// delivered the notification (in a later generation, or a later run), so
// committing the chunk is no evidence about the current connection. The
// notification's own message was.
func (b *Bridge) applyQueuedHistorySync(cli *whatsmeow.Client, generation uint64, chunk *waHistorySync.HistorySync) bool {
	evt := &waevents.HistorySync{Data: chunk}
	b.mu.RLock()
	current := b.client == cli && b.clientGeneration == generation
	hook := b.beforeEventForTest
	b.mu.RUnlock()
	if hook != nil {
		hook(evt)
	}
	if !current {
		return false
	}
	return b.applyHistorySync(evt, true)
}

func (b *Bridge) deleteHistorySyncMedia(ctx context.Context, pump *historySyncPump, cli *whatsmeow.Client, row queuedHistorySync) {
	// A chunk carried inline has no server copy.
	if row.notification.GetDirectPath() == "" {
		return
	}
	if err := pump.deleteMedia(ctx, cli, row.notification); err != nil {
		b.logger.Warn().Err(err).Str("msg_id", string(row.messageID)).Msg("Failed to delete a WhatsApp history-sync chunk from the server")
	}
}

// historySyncFailed records a failed download or receipt and reports whether
// the pass should stop.
func (b *Bridge) historySyncFailed(ctx context.Context, pump *historySyncPump, cli *whatsmeow.Client, row queuedHistorySync, stage string, err error) (stop bool) {
	err = fmt.Errorf("%s: %w", stage, err)
	if historySyncConnectionGone(ctx, err) {
		return true
	}
	if stage == "download" && historySyncDownloadPermanent(err) {
		b.dropHistorySync(ctx, pump, cli, row, err, true)
		return false
	}
	attempts := row.attempts + 1
	if attempts >= historySyncMaxAttempts {
		// A notification whose receipt keeps failing is not sent one more.
		b.dropHistorySync(ctx, pump, cli, row, fmt.Errorf("gave up after %d attempts: %w", attempts, err), stage != "receipt")
		return false
	}
	next := pump.now().Add(historySyncBackoff(attempts))
	if deferErr := pump.queue.deferAttempt(row.seq, attempts, next, err); deferErr != nil {
		b.logger.Warn().Err(deferErr).Msg("Failed to defer a WhatsApp history-sync notification")
		return true
	}
	b.logger.Warn().
		Err(err).
		Str("msg_id", string(row.messageID)).
		Int("attempts", attempts).
		Time("next_attempt", next).
		Msg("WhatsApp history-sync notification failed; retrying later")
	return false
}

// dropHistorySync gives up on a notification. The receipt it sends (when
// asked to) is the one whatsmeow's default handling sends for every
// notification; the server copy of a committed chunk is deleted as usual.
func (b *Bridge) dropHistorySync(ctx context.Context, pump *historySyncPump, cli *whatsmeow.Client, row queuedHistorySync, reason error, receipt bool) {
	if receipt && row.messageID != "" {
		if err := pump.sendReceipt(ctx, cli, row.messageID); err != nil {
			b.logger.Warn().Err(err).Str("msg_id", string(row.messageID)).Msg("Failed to send the receipt of a dropped WhatsApp history-sync notification")
		}
	}
	if row.committed {
		b.deleteHistorySyncMedia(ctx, pump, cli, row)
	}
	if err := pump.queue.remove(row.seq); err != nil {
		b.logger.Warn().Err(err).Str("msg_id", string(row.messageID)).Msg("Failed to remove a dropped WhatsApp history-sync notification")
		return
	}
	event := b.logger.Error().
		Err(reason).
		Str("msg_id", string(row.messageID)).
		Int("attempts", row.attempts).
		Bool("committed", row.committed)
	if row.notification != nil {
		event = event.
			Str("sync_type", row.notification.GetSyncType().String()).
			Uint32("chunk_order", row.notification.GetChunkOrder())
	}
	event.Msg("Dropped a WhatsApp history-sync notification")
}

// historySyncConnectionGone reports a failure that says nothing about the
// notification: the pump is stopping, or the connection went away. It
// counts no attempt; the next connection resumes the queue.
func historySyncConnectionGone(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, whatsmeow.ErrNotConnected) ||
		errors.Is(err, whatsmeow.ErrIQDisconnected) ||
		errors.Is(err, whatsmeow.ErrNotLoggedIn) ||
		errors.Is(err, whatsmeow.ErrClientIsNil)
}

// historySyncDownloadPermanent reports a download that cannot succeed later:
// the server no longer has the chunk, the notification names none, or the
// fetched bytes do not parse.
func historySyncDownloadPermanent(err error) bool {
	return errors.Is(err, errHistorySyncUnreadable) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) ||
		errors.Is(err, whatsmeow.ErrNoURLPresent)
}

// historySyncBackoff is the wait before attempt attempts+1: 30 seconds,
// doubling, at most an hour.
func historySyncBackoff(attempts int) time.Duration {
	delay := historySyncRetryMin
	for i := 1; i < attempts && delay < historySyncRetryMax; i++ {
		delay *= 2
	}
	return min(delay, historySyncRetryMax)
}

// historySyncNotification returns the notification whatsmeow would have
// queued for this message (handleProtocolMessage via processProtocolParts):
// a protocol message our own account sent, unwrapped from at most a
// device-sent envelope.
func historySyncNotification(evt *waevents.Message) *waE2E.HistorySyncNotification {
	if evt == nil || !evt.Info.IsFromMe {
		return nil
	}
	message := evt.RawMessage
	if message == nil {
		message = evt.Message
	}
	if inner := message.GetDeviceSentMessage().GetMessage(); inner != nil {
		message = inner
	}
	return message.GetProtocolMessage().GetHistorySyncNotification()
}

// queueHistorySyncNotification stores the history-sync notification a
// message carries, if any, and wakes the pump. A non-nil error means it was
// not stored, and the message must be reported not handled so whatsmeow
// keeps it for redelivery.
func (b *Bridge) queueHistorySyncNotification(evt *waevents.Message) error {
	notification := historySyncNotification(evt)
	if notification == nil {
		return nil
	}
	b.mu.RLock()
	pump := b.historySync
	var account string
	if b.client != nil && b.client.Store != nil && b.client.Store.ID != nil {
		account = b.client.Store.ID.User
	}
	b.mu.RUnlock()
	if pump == nil {
		return errHistorySyncQueueUnavailable
	}
	if err := pump.queue.enqueue(evt.Info.ID, account, notification, time.Now()); err != nil {
		return err
	}
	b.logger.Debug().
		Str("msg_id", string(evt.Info.ID)).
		Str("sync_type", notification.GetSyncType().String()).
		Uint32("chunk_order", notification.GetChunkOrder()).
		Bool("inline", notification.InitialHistBootstrapInlinePayload != nil).
		Int64("queued", pump.queue.pending()).
		Msg("Queued a WhatsApp history-sync notification")
	pump.wake()
	return nil
}
