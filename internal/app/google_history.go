package app

import (
	"context"
	"errors"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

var (
	// ErrGoogleHistoryClosed reports that the connection generation which
	// fetched a piece of history has ended, so its v2 ingest no longer accepts
	// it. The catch-up stops: its client is going away, and legacy rows
	// written past this point would never reach v2 (later reconciles stop at
	// the newest legacy message).
	ErrGoogleHistoryClosed = errors.New("google history ingress is closed")
	// ErrGoogleHistoryDisabled reports that no v2 ingest is running (a
	// legacy-only install). The catch-up continues for the legacy store and
	// stops offering history, without counting it as a failure.
	ErrGoogleHistoryDisabled = errors.New("google history ingress is disabled")
)

// GoogleHistoryIngress receives Google history that a catch-up (startup
// backfill, recent reconcile, deep or window backfill, phone backfill, pending
// media refresh) fetched on one connection generation's client, and hands it
// to that generation's v2 ingest. Without it, fetched history reaches only the
// legacy store, which v2-primary readers never see. Implementations fail with
// an error wrapping ErrGoogleHistoryClosed once their generation has ended.
type GoogleHistoryIngress interface {
	AppendHistoryConversation(ctx context.Context, conversation *gmproto.Conversation) error
	// AppendHistoryMessage hands over one message fetched from conversationID.
	// conversation is the snapshot the message was fetched under, or nil.
	AppendHistoryMessage(
		ctx context.Context,
		conversationID string,
		conversation *gmproto.Conversation,
		message *gmproto.Message,
	) error
}

// googleCatchUp is one catch-up's view of Google: the client it fetches with,
// the token that detects a client change, and the history ingress of the same
// generation, all captured together. Every fetched conversation and message is
// stored in the legacy store exactly as before and then offered to v2.
type googleCatchUp struct {
	app      *App
	gm       GMClient
	token    any
	reason   string
	history  GoogleHistoryIngress
	progress *BackfillProgress

	teed   int
	failed int
	warned bool
	closed bool
}

// beginGoogleCatchUp captures the current Google client and its generation's
// history ingress under one lock, so history fetched with generation N's client
// can only ever be handed to generation N. It returns nil when no client is
// connected.
//
// The catch-up's client stops it after its first unanswered request
// (failFastGMClient).
func (a *App) beginGoogleCatchUp(reason string) *googleCatchUp {
	if a.gmClient != nil {
		return &googleCatchUp{
			app:     a,
			gm:      newFailFastGMClient(a.gmClient),
			token:   a.gmClient,
			reason:  reason,
			history: a.gmHistory,
		}
	}
	a.clientMu.RLock()
	cli := a.Client
	var history GoogleHistoryIngress
	if generation := a.googleGeneration; generation != nil && cli != nil && generation.Client == cli {
		history = generation.history
	}
	a.clientMu.RUnlock()
	if cli == nil || cli.GM == nil {
		return nil
	}
	return &googleCatchUp{
		app:     a,
		gm:      newFailFastGMClient(newRealGMClient(cli.GM)),
		token:   cli.GM,
		reason:  reason,
		history: history,
	}
}

func (c *googleCatchUp) stillCurrent() bool {
	return !c.closed && c.app.backfillClientStillCurrent(c.token)
}

// shouldAbort is the deep/window/phone backfill abort check: the generation
// that fetched closed its history ingress, or the client changed (a nil token,
// as for a phone backfill, never aborts on a client change).
func (c *googleCatchUp) shouldAbort(phase string) bool {
	if c.closed {
		c.app.Logger.Warn().Str("phase", phase).Str("reason", c.reason).
			Msg("Google catch-up stopped because its connection generation ended")
		return true
	}
	return c.app.deepBackfillShouldAbort(c.token, phase)
}

// storeConversation offers the snapshot to v2 and writes it to the legacy
// store. v2 is offered first: if that reports the generation closed, nothing
// is stored, so the legacy store never holds an item this catch-up could not
// also hand to v2. Any other hand-off failure leaves the legacy write in
// place and never fails the catch-up; the legacy result is returned unchanged.
func (c *googleCatchUp) storeConversation(conversation *gmproto.Conversation) error {
	if c.closed {
		return ErrGoogleHistoryClosed
	}
	if c.history != nil && conversation != nil {
		c.record(c.history.AppendHistoryConversation(context.Background(), conversation))
		if c.closed {
			return ErrGoogleHistoryClosed
		}
	}
	return c.app.storeConversation(conversation)
}

// storeMessage offers the message to v2, together with the snapshot of the
// conversation it was fetched from (nil when the catch-up fetched messages
// without listing their conversation), and writes the legacy row. The order
// and the closed rule are storeConversation's.
func (c *googleCatchUp) storeMessage(
	conversationID string,
	conversation *gmproto.Conversation,
	message *gmproto.Message,
) {
	if c.closed {
		return
	}
	if c.history != nil && message != nil {
		c.record(c.history.AppendHistoryMessage(context.Background(), conversationID, conversation, message))
		if c.closed {
			return
		}
	}
	c.app.storeMessage(message)
}

func (c *googleCatchUp) record(err error) {
	if errors.Is(err, ErrGoogleHistoryDisabled) {
		c.history = nil
		return
	}
	if err == nil {
		c.teed++
		if c.progress != nil {
			c.progress.addHistory(1, 0)
		}
		return
	}
	c.failed++
	if c.progress != nil {
		c.progress.addHistory(0, 1)
	}
	if errors.Is(err, ErrGoogleHistoryClosed) {
		// The generation that fetched this history has ended. Stop the whole
		// catch-up (stillCurrent turns false and nothing more is stored): its
		// client is going away, and a message stored only in legacy would be
		// hidden from v2 for good, since later reconciles stop at the newest
		// legacy message. What a recent reconcile or startup backfill did not
		// store sits above what it stored, so the next generation's reconcile
		// fetches it; a deep or window backfill has to be run again.
		c.history = nil
		c.closed = true
		c.app.Logger.Info().
			Err(err).
			Str("reason", c.reason).
			Msg("Google catch-up stopped: its connection generation ended")
		return
	}
	if !c.warned {
		c.warned = true
		c.app.Logger.Warn().
			Err(err).
			Str("reason", c.reason).
			Msg("Google catch-up could not hand fetched history to v2; the catch-up still writes it to the legacy store")
	}
}

// finish logs how much fetched history reached v2 ingest.
func (c *googleCatchUp) finish() {
	if c == nil || (c.teed == 0 && c.failed == 0) {
		return
	}
	c.app.Logger.Info().
		Str("reason", c.reason).
		Int("history_teed", c.teed).
		Int("history_tee_failed", c.failed).
		Msg("Google catch-up handed fetched history to v2 ingest")
}
