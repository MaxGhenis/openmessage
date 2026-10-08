package app

import (
	"context"
	"errors"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// ErrGoogleHistoryClosed reports that the connection generation which fetched
// a piece of history has ended, so its v2 ingest no longer accepts it.
var ErrGoogleHistoryClosed = errors.New("google history ingress is closed")

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
}

// beginGoogleCatchUp captures the current Google client and its generation's
// history ingress under one lock, so history fetched with generation N's client
// can only ever be handed to generation N. It returns nil when no client is
// connected.
func (a *App) beginGoogleCatchUp(reason string) *googleCatchUp {
	if a.gmClient != nil {
		return &googleCatchUp{
			app:     a,
			gm:      a.gmClient,
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
		gm:      newRealGMClient(cli.GM),
		token:   cli.GM,
		reason:  reason,
		history: history,
	}
}

func (c *googleCatchUp) stillCurrent() bool {
	return c.app.backfillClientStillCurrent(c.token)
}

// storeConversation writes the legacy snapshot and offers it to v2. The legacy
// result is returned unchanged; a v2 hand-off failure never fails the catch-up.
func (c *googleCatchUp) storeConversation(conversation *gmproto.Conversation) error {
	err := c.app.storeConversation(conversation)
	if c.history != nil && conversation != nil {
		c.record(c.history.AppendHistoryConversation(context.Background(), conversation))
	}
	return err
}

// storeMessage writes the legacy row and offers the message to v2 together
// with the snapshot of the conversation it was fetched from (nil when the
// catch-up fetched messages without listing their conversation).
func (c *googleCatchUp) storeMessage(
	conversationID string,
	conversation *gmproto.Conversation,
	message *gmproto.Message,
) {
	c.app.storeMessage(message)
	if c.history != nil && message != nil {
		c.record(c.history.AppendHistoryMessage(context.Background(), conversationID, conversation, message))
	}
}

func (c *googleCatchUp) record(err error) {
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
		// The generation that fetched this history has ended; every later
		// hand-off would fail the same way. The legacy writes continue.
		c.history = nil
	}
	if !c.warned {
		c.warned = true
		c.app.Logger.Warn().
			Err(err).
			Str("reason", c.reason).
			Msg("Google catch-up could not hand fetched history to v2; the legacy store still has it")
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
