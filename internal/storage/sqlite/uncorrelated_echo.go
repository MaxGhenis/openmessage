package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Google Messages normally echoes a sent message with the TmpID it was sent
// with, and the ingest worker correlates that TmpID to the outbox row's
// transport request ID. On the live install the phone echoes every sent
// message with an empty TmpID (observed in raw inbox frames on 2026-10-16, on
// both the QR and the Google-account pairing), so nothing correlated the echo
// and it projected as a second copy of the sent message beside the local row.
//
// The functions here pair such echoes with their sends by content instead.
// Within one conversation and one body, sent-but-unbound text intents are
// aligned in time order with the outgoing messages this machine first saw
// near the moment each send went out (see pairEchoes).
//
// Both times are on this machine's clock. "Went out" is the intent's
// updated_at_ms: for an unbound confirmed or uncertain intent, that is when
// the dispatcher recorded the transport's answer. "First seen" is when the
// echo frame was received, which for a stored row is its created_at_ms. On
// the live store every one of 44 doubled sends had its echo first seen
// 0.28-0.79 s after the confirm. The echo's own timestamp comes from the
// phone's clock, which ran 0.6 s behind to 3.7 s ahead on the same sends, so
// it is not used. Measuring from the send rather than from the intent's
// creation keeps retried and scheduled sends matchable (one live send went
// out 47 minutes after it was created). Each pair then goes through the
// normal reconcile, so the local row takes the real remote ID and any echo
// row already projected is merged into it (repointLocalMessage).

const (
	// echoTypicalLagMS and echoLagSpreadMS describe when an echo is first
	// seen after its send went out: 0.28-0.79 s on all 44 doubled live sends.
	echoTypicalLagMS = int64(500)
	echoLagSpreadMS  = int64(1000)
	// echoUnpairedCost is what leaving a send or an echo unpaired costs in
	// pairEchoes. A pair costs echoPairCost, which grows with the square of
	// how far its lag is from typical, like a negative log-likelihood. So a
	// pair beats leaving both its send and its echo unpaired only while its
	// lag is within about -5.2 s to +6.2 s, and a lone echo is never taken
	// from the send it fits for a worse fit elsewhere.
	echoUnpairedCost = int64(8000)
	// echoMaxLagMS keeps the arithmetic small; any lag beyond it is far
	// outside what echoUnpairedCost allows anyway.
	echoMaxLagMS = int64(60 * 1000)
	// echoMaxStampSkewMS bounds how far an echo's own (phone) timestamp may be
	// from when this machine first saw it. A real echo is stamped as it is
	// sent (live store: -1.35 s to +3.3 s). An old message that arrives now (a
	// history catch-up inserting a months-old text, or a live re-delivery of
	// one when a reaction changes) is first seen now but stamped long ago, so
	// it is never an echo candidate, however well its arrival time fits.
	echoMaxStampSkewMS = int64(5 * 60 * 1000)
)

// EchoMaxLag is echoMaxLagMS as a duration: no echo first seen later than
// this after its send went out (the intent's updated_at_ms) is ever paired
// with that send by content. The messaging dispatcher stops treating a sent
// message as still awaiting its echo after it.
const EchoMaxLag = time.Duration(echoMaxLagMS) * time.Millisecond

// UncorrelatedEchoRequest is an outgoing message frame that names no
// transport request. SeenAtMS is when this machine received the frame;
// OccurredAtMS is the phone's stamp on the message. Media is whether the frame
// carries an attachment.
type UncorrelatedEchoRequest struct {
	AccountID            string
	RemoteConversationID string
	RemoteMessageID      string
	Body                 string
	Media                bool
	SeenAtMS             int64
	OccurredAtMS         int64
}

// Media sends pair the same way, with two differences that follow from how
// the Google adapter sends one (google.Adapter.SendMedia): the image goes out
// as a message of its own with no text, and a caption follows as a separate
// text message. So the echo of the image is an outgoing message with an
// attachment and no text, paired with the conversation's media sends whatever
// their caption; and the echo of a caption is an ordinary text echo, paired
// alongside the conversation's text sends of the same body. Pairing a caption
// moves it out of the sent message, where it showed until then, leaving it to
// the echo's own row, so the conversation reads as the phone shows it: the
// image, then the caption. All six media sends on the live store (none with
// a caption) had one such echo, first seen 0.27-0.59 s after the send went
// out.

// sendWentOutPredicate selects intents the transport accepted (or may have
// accepted). An uncertain intent the user already sent again is left alone:
// the resend owns the echo. It expects the outbox as o and the intent's local
// message as m.
const sendWentOutPredicate = `
	o.state IN ('confirmed', 'uncertain', 'store_failed')
	AND m.direction = 'outgoing'
	AND NOT (o.state = 'uncertain' AND EXISTS (
		SELECT 1 FROM outbox AS again WHERE again.send_again_of_outbox_id = o.outbox_id
	))
`

// unboundIntentPredicate selects sent intents whose local row still carries
// the transport request ID because no echo has bound it to the real remote ID.
const unboundIntentPredicate = `
	(o.result_remote_id IS NULL OR o.result_remote_id = o.transport_request_id)
	AND m.remote_message_id = o.transport_request_id
	AND ` + sendWentOutPredicate

// unboundTextIntentPredicate and unboundMediaIntentPredicate select the
// unbound sends of one kind.
const (
	unboundTextIntentPredicate  = `o.kind = 'text' AND ` + unboundIntentPredicate
	unboundMediaIntentPredicate = `o.kind = 'media' AND ` + unboundIntentPredicate
)

// unsplitCaptionPredicate selects sent media intents whose local row still
// shows the caption, bound or not: no echo of the caption has been paired yet.
const unsplitCaptionPredicate = `
	o.kind = 'media'
	AND m.body <> ''
	AND ` + sendWentOutPredicate

// mediaEchoPredicate and textEchoPredicate tell the two kinds of echo apart.
// They expect the echo's message as e.
const (
	mediaEchoPredicate = `trim(e.body) = ''
		AND EXISTS (SELECT 1 FROM message_attachments AS a WHERE a.message_id = e.message_id)`
	textEchoPredicate = `NOT EXISTS (SELECT 1 FROM message_attachments AS a WHERE a.message_id = e.message_id)`
)

// echoGroup is a set of sends and the echoes they can be paired with, all in
// one conversation: the media sends and the image echoes (media), or the text
// sends and unsplit captions that read body and the text echoes of body.
type echoGroup struct {
	conversationID string
	media          bool
	body           string
}

type echoIntent struct {
	transportRequestID string
	sentAtMS           int64
	// captionOf is the local message of a media send whose caption this
	// intent stands for, or empty for a send paired by its request ID.
	captionOf string
}

type echoCandidate struct {
	remoteMessageID string
	seenAtMS        int64
	// occurredAtMS is the phone's stamp; only an incoming frame carries it
	// here (stored rows are filtered in SQL).
	occurredAtMS int64
}

// stampFitsSeen reports whether a message's phone stamp is close enough to
// when it was first seen for it to be a fresh echo (see echoMaxStampSkewMS).
func stampFitsSeen(occurredAtMS, seenAtMS int64) bool {
	d := occurredAtMS - seenAtMS
	return d >= -echoMaxStampSkewMS && d <= echoMaxStampSkewMS
}

type echoPair struct {
	intent echoIntent
	echo   echoCandidate
}

// pairEchoes aligns intents with echoes in time order. Sends of one account
// go out one at a time and their echoes arrive in that order, so the k-th
// paired send takes the k-th paired echo: pairs never cross. Among such
// alignments it picks the one with the least total cost: a pair costs
// echoPairCost, and leaving an intent or an echo unpaired (a send whose echo
// has not arrived, a text typed on the phone) costs echoUnpairedCost. Closest-first pairing is not enough: two identical
// texts a second apart can each lie nearer the other's echo. It is pure and
// deterministic, so the live path and the repair share the rule.
func pairEchoes(intents []echoIntent, echoes []echoCandidate) []echoPair {
	sort.SliceStable(intents, func(i, j int) bool {
		if intents[i].sentAtMS != intents[j].sentAtMS {
			return intents[i].sentAtMS < intents[j].sentAtMS
		}
		return intents[i].transportRequestID < intents[j].transportRequestID
	})
	sort.SliceStable(echoes, func(i, j int) bool {
		if echoes[i].seenAtMS != echoes[j].seenAtMS {
			return echoes[i].seenAtMS < echoes[j].seenAtMS
		}
		return echoes[i].remoteMessageID < echoes[j].remoteMessageID
	})
	n, m := len(intents), len(echoes)
	const skip = echoUnpairedCost
	// cost[i][j] is the least cost of aligning intents[i:] with echoes[j:].
	cost := make([][]int64, n+1)
	for i := range cost {
		cost[i] = make([]int64, m+1)
	}
	for i := n; i >= 0; i-- {
		for j := m; j >= 0; j-- {
			switch {
			case i == n:
				cost[i][j] = int64(m-j) * skip
			case j == m:
				cost[i][j] = int64(n-i) * skip
			default:
				best := min(cost[i+1][j], cost[i][j+1]) + skip
				if c, ok := echoPairCost(intents[i], echoes[j]); ok {
					best = min(best, cost[i+1][j+1]+c)
				}
				cost[i][j] = best
			}
		}
	}
	var pairs []echoPair
	for i, j := 0, 0; i < n && j < m; {
		// Prefer a pair, then leaving the echo, on ties.
		if c, ok := echoPairCost(intents[i], echoes[j]); ok && cost[i][j] == cost[i+1][j+1]+c {
			pairs = append(pairs, echoPair{intent: intents[i], echo: echoes[j]})
			i, j = i+1, j+1
		} else if cost[i][j] == cost[i][j+1]+skip {
			j++
		} else {
			i++
		}
	}
	return pairs
}

// echoPairCost is the cost of pairing a send with an echo:
// ((lag - typical) / spread)^2 / 2, scaled by 1000, where lag is how long
// after the send went out the echo was first seen. It reports false for a lag
// beyond echoMaxLagMS either way.
func echoPairCost(intent echoIntent, echo echoCandidate) (int64, bool) {
	lag := echo.seenAtMS - intent.sentAtMS
	if lag < -echoMaxLagMS || lag > echoMaxLagMS {
		return 0, false
	}
	off := lag - echoTypicalLagMS
	return off * off * 1000 / (2 * echoLagSpreadMS * echoLagSpreadMS), true
}

// reconcileEchoGroupTx pairs the sends of one echo group with the group's
// unclaimed echo rows, plus incoming (a frame not projected yet, of the
// group's kind), and acts on each pair: a send is reconciled to its echo, and
// a caption moves out of its sent message. It reports how many pairs changed
// durable state and whether incoming was one of them.
func (r *OutboxRepository) reconcileEchoGroupTx(
	ctx context.Context,
	tx *sql.Tx,
	accountID string,
	group echoGroup,
	incoming *echoCandidate,
) (int, bool, error) {
	intentQuery := `
		SELECT o.transport_request_id, o.updated_at_ms, ''
		FROM outbox AS o
		JOIN messages AS m ON m.message_id = o.local_message_id
		WHERE o.account_id = ?
		  AND m.conversation_id = ?
		  AND ` + unboundMediaIntentPredicate
	intentArgs := []any{accountID, group.conversationID}
	echoPredicate := mediaEchoPredicate
	if !group.media {
		intentQuery = `
			SELECT o.transport_request_id, o.updated_at_ms,
				CASE WHEN o.kind = 'media' THEN m.message_id ELSE '' END
			FROM outbox AS o
			JOIN messages AS m ON m.message_id = o.local_message_id
			WHERE o.account_id = ?
			  AND m.conversation_id = ?
			  AND m.body = ?
			  AND ((` + unboundTextIntentPredicate + `) OR (` + unsplitCaptionPredicate + `))`
		intentArgs = append(intentArgs, group.body)
		echoPredicate = `e.body = ? AND ` + textEchoPredicate
	}
	rows, err := tx.QueryContext(ctx, intentQuery, intentArgs...)
	if err != nil {
		return 0, false, fmt.Errorf("list unbound intents: %w", err)
	}
	var intents []echoIntent
	for rows.Next() {
		var intent echoIntent
		if err := rows.Scan(&intent.transportRequestID, &intent.sentAtMS, &intent.captionOf); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("scan unbound intent: %w", err)
		}
		intents = append(intents, intent)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, false, fmt.Errorf("list unbound intents: %w", err)
	}
	if len(intents) == 0 {
		return 0, false, nil
	}
	firstSent, lastSent := intents[0].sentAtMS, intents[0].sentAtMS
	for _, intent := range intents[1:] {
		firstSent, lastSent = min(firstSent, intent.sentAtMS), max(lastSent, intent.sentAtMS)
	}

	// An echo row is claimed once it is some intent's own local row: that is
	// what binding it does. The check is per conversation by construction,
	// so a remote ID Google reused in another thread blocks nothing here.
	// Bounding the phone stamp too lets the conversation-time index serve
	// this, and drops rows that were stored long after they were sent.
	echoArgs := []any{
		accountID, group.conversationID,
		firstSent - echoMaxLagMS - echoMaxStampSkewMS, lastSent + echoMaxLagMS + echoMaxStampSkewMS,
		firstSent - echoMaxLagMS, lastSent + echoMaxLagMS,
		echoMaxStampSkewMS, echoMaxStampSkewMS,
	}
	if !group.media {
		echoArgs = append(echoArgs, group.body)
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT e.remote_message_id, e.created_at_ms
		FROM messages AS e
		WHERE e.account_id = ?
		  AND e.conversation_id = ?
		  AND e.occurred_at_ms BETWEEN ? AND ?
		  AND e.created_at_ms BETWEEN ? AND ?
		  AND e.occurred_at_ms BETWEEN e.created_at_ms - ? AND e.created_at_ms + ?
		  AND e.direction = 'outgoing'
		  AND `+echoPredicate+`
		  AND NOT EXISTS (
		      SELECT 1 FROM outbox AS o WHERE o.local_message_id = e.message_id
		  )
	`, echoArgs...)
	if err != nil {
		return 0, false, fmt.Errorf("list echo rows: %w", err)
	}
	var echoes []echoCandidate
	incomingIsRow := false
	for rows.Next() {
		var echo echoCandidate
		if err := rows.Scan(&echo.remoteMessageID, &echo.seenAtMS); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("scan echo row: %w", err)
		}
		if incoming != nil && echo.remoteMessageID == incoming.remoteMessageID {
			incomingIsRow = true
		}
		echoes = append(echoes, echo)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, false, fmt.Errorf("list echo rows: %w", err)
	}
	if incoming != nil && !incomingIsRow && stampFitsSeen(incoming.occurredAtMS, incoming.seenAtMS) {
		// Not yet a row: either new, or a status re-delivery of a message
		// already bound to an intent (whose row then holds this remote ID).
		var bound int
		err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM messages
			WHERE account_id = ? AND conversation_id = ? AND remote_message_id = ?
		`, accountID, group.conversationID, incoming.remoteMessageID).Scan(&bound)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			echoes = append(echoes, *incoming)
		case err != nil:
			return 0, false, fmt.Errorf("read incoming echo row: %w", err)
		}
	}

	changed := 0
	incomingMatched := false
	for _, pair := range pairEchoes(intents, echoes) {
		applied, err := r.applyEchoPairTx(ctx, tx, accountID, group, pair)
		if err != nil {
			return 0, false, err
		}
		if applied {
			changed++
			if incoming != nil && pair.echo.remoteMessageID == incoming.remoteMessageID {
				incomingMatched = true
			}
		}
	}
	return changed, incomingMatched, nil
}

// applyEchoPairTx acts on one pair and reports whether it changed durable
// state. A send is reconciled to its echo, which merges any echo row into the
// send's local row (repointLocalMessage). A caption is cleared from its media
// send's local row; the echo keeps its own row, or projects as one.
func (r *OutboxRepository) applyEchoPairTx(
	ctx context.Context,
	tx *sql.Tx,
	accountID string,
	group echoGroup,
	pair echoPair,
) (bool, error) {
	if pair.intent.captionOf == "" {
		outcome, err := r.reconcileConfirmTx(ctx, tx, ReconcileRequest{
			AccountID:          accountID,
			TransportRequestID: pair.intent.transportRequestID,
			ResultRemoteID:     pair.echo.remoteMessageID,
		})
		if err != nil {
			return false, err
		}
		return outcome == ReconcileOutcomeReconciled || outcome == ReconcileOutcomeEnriched, nil
	}
	nowMS, err := r.nowMS("split caption")
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE messages
		SET body = '',
			updated_at_ms = MAX(updated_at_ms, ?)
		WHERE message_id = ? AND body = ?
	`, nowMS, pair.intent.captionOf, group.body)
	if err != nil {
		return false, fmt.Errorf("split caption from message %q: %w", pair.intent.captionOf, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("split caption from message %q: read rows affected: %w", pair.intent.captionOf, err)
	}
	return affected == 1, nil
}

// ReconcileUncorrelatedEcho pairs the sends in an echo's group (see
// echoGroup) with their echoes, counting the echo itself (see pairEchoes),
// and acts on each pair. It returns Enriched or Reconciled when the echo was
// paired with a send, Noop when other pairs changed (an earlier echo row
// merged) or nothing applied, and NotFound when the conversation has no
// unbound send the echo could belong to (a message sent from the phone
// itself). An echo with both an attachment and text is never one of a send
// from here, which sends a caption as a message of its own, and is NotFound.
func (r *OutboxRepository) ReconcileUncorrelatedEcho(
	ctx context.Context,
	req UncorrelatedEchoRequest,
) (ReconcileOutcome, error) {
	checks := []struct {
		name  string
		value string
	}{
		{name: "account ID", value: req.AccountID},
		{name: "remote conversation ID", value: req.RemoteConversationID},
		{name: "remote message ID", value: req.RemoteMessageID},
	}
	if !req.Media {
		checks = append(checks, struct {
			name  string
			value string
		}{name: "body", value: req.Body})
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return "", fmt.Errorf("reconcile uncorrelated echo: %s is empty", check.name)
		}
	}
	if req.SeenAtMS <= 0 || req.OccurredAtMS <= 0 {
		return "", fmt.Errorf("reconcile uncorrelated echo: receipt or occurrence time is not positive")
	}
	if req.Media && strings.TrimSpace(req.Body) != "" {
		return ReconcileOutcomeNotFound, nil
	}

	// Most outgoing frames (sent from the phone, or status re-deliveries of a
	// bound send) have no unbound send to pair with: answer them from a read,
	// without taking the write lock.
	findQuery := `
		SELECT m.conversation_id
		FROM outbox AS o
		JOIN messages AS m ON m.message_id = o.local_message_id
		JOIN conversations AS c ON c.conversation_id = m.conversation_id
		WHERE o.account_id = ?
		  AND c.remote_conversation_id = ?
		  AND ` + unboundMediaIntentPredicate + `
		LIMIT 1`
	findArgs := []any{req.AccountID, req.RemoteConversationID}
	if !req.Media {
		findQuery = `
			SELECT m.conversation_id
			FROM outbox AS o
			JOIN messages AS m ON m.message_id = o.local_message_id
			JOIN conversations AS c ON c.conversation_id = m.conversation_id
			WHERE o.account_id = ?
			  AND c.remote_conversation_id = ?
			  AND m.body = ?
			  AND ((` + unboundTextIntentPredicate + `) OR (` + unsplitCaptionPredicate + `))
			LIMIT 1`
		findArgs = append(findArgs, req.Body)
	}
	group := echoGroup{media: req.Media}
	if !req.Media {
		group.body = req.Body
	}
	err := r.store.db.QueryRowContext(ctx, findQuery, findArgs...).Scan(&group.conversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return ReconcileOutcomeNotFound, nil
	}
	if err != nil {
		return "", fmt.Errorf("reconcile uncorrelated echo: find unbound sends: %w", err)
	}

	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("reconcile uncorrelated echo: begin transaction: %w", err)
	}
	defer tx.Rollback()
	changed, incomingMatched, err := r.reconcileEchoGroupTx(ctx, tx, req.AccountID, group, &echoCandidate{
		remoteMessageID: req.RemoteMessageID,
		seenAtMS:        req.SeenAtMS,
		occurredAtMS:    req.OccurredAtMS,
	})
	if err != nil {
		return "", fmt.Errorf("reconcile uncorrelated echo: %w", err)
	}
	if changed > 0 {
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("reconcile uncorrelated echo: commit: %w", err)
		}
	}
	if incomingMatched {
		return ReconcileOutcomeEnriched, nil
	}
	return ReconcileOutcomeNoop, nil
}

// EchoDuplicateRepairReport counts what RepairUncorrelatedEchoDuplicates saw.
type EchoDuplicateRepairReport struct {
	// Attached is the number of sent media messages given the image they sent
	// (see BackfillOutgoingMediaAttachments).
	Attached int
	// Groups is the number of echo groups with unbound sends or unsplit
	// captions.
	Groups int
	// Merged is the number of sends paired with their echo, captions
	// included.
	Merged int
	// Failed is the number of groups whose repair failed and rolled back.
	Failed int
}

// RepairUncorrelatedEchoDuplicates pairs every Google conversation's unbound
// sends and unsplit captions with their echo rows, exactly as the live path
// does (see pairEchoes), and acts on each pair, merging an echo row into its
// send's local row. Sent media messages first get the image they sent
// (BackfillOutgoingMediaAttachments), so that a merge keeps it rather than the
// echo's copy. Each echo group is repaired in its own transaction; a group
// that fails is counted and skipped so it cannot block the rest. It is
// idempotent: a repaired send is bound, and a split caption gone from its
// send, so neither is selected again.
func (r *OutboxRepository) RepairUncorrelatedEchoDuplicates(ctx context.Context) (EchoDuplicateRepairReport, error) {
	var report EchoDuplicateRepairReport
	attached, err := r.BackfillOutgoingMediaAttachments(ctx)
	if err != nil {
		return report, fmt.Errorf("repair echo duplicates: %w", err)
	}
	report.Attached = attached

	rows, err := r.store.db.QueryContext(ctx, `
		SELECT DISTINCT o.account_id, m.conversation_id, 0, m.body
		FROM outbox AS o
		JOIN accounts AS a ON a.account_id = o.account_id
		JOIN messages AS m ON m.message_id = o.local_message_id
		WHERE a.bridge_key = 'google_messages'
		  AND m.body <> ''
		  AND ((`+unboundTextIntentPredicate+`) OR (`+unsplitCaptionPredicate+`))
		UNION
		SELECT DISTINCT o.account_id, m.conversation_id, 1, ''
		FROM outbox AS o
		JOIN accounts AS a ON a.account_id = o.account_id
		JOIN messages AS m ON m.message_id = o.local_message_id
		WHERE a.bridge_key = 'google_messages'
		  AND `+unboundMediaIntentPredicate)
	if err != nil {
		return report, fmt.Errorf("repair echo duplicates: list groups: %w", err)
	}
	type accountGroup struct {
		accountID string
		group     echoGroup
	}
	var groups []accountGroup
	for rows.Next() {
		var g accountGroup
		if err := rows.Scan(&g.accountID, &g.group.conversationID, &g.group.media, &g.group.body); err != nil {
			rows.Close()
			return report, fmt.Errorf("repair echo duplicates: scan group: %w", err)
		}
		groups = append(groups, g)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return report, fmt.Errorf("repair echo duplicates: list groups: %w", err)
	}
	report.Groups = len(groups)

	var errs []error
	for _, g := range groups {
		merged, err := r.repairEchoGroup(ctx, g.accountID, g.group)
		if err != nil {
			report.Failed++
			errs = append(errs, err)
			continue
		}
		report.Merged += merged
	}
	return report, errors.Join(errs...)
}

func (r *OutboxRepository) repairEchoGroup(ctx context.Context, accountID string, group echoGroup) (int, error) {
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("repair echo duplicates in %q: begin transaction: %w", group.conversationID, err)
	}
	defer tx.Rollback()
	changed, _, err := r.reconcileEchoGroupTx(ctx, tx, accountID, group, nil)
	if err != nil {
		return 0, fmt.Errorf("repair echo duplicates in %q: %w", group.conversationID, err)
	}
	if changed > 0 {
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("repair echo duplicates in %q: commit: %w", group.conversationID, err)
		}
	}
	return changed, nil
}

// BackfillOutgoingMediaAttachments gives each sent media message that has no
// attachment the image its send carried, as a new send gets it when it is
// queued (insertOutgoingMessageAttachment). Before that, a sent image showed
// as an empty message beside its echo. It is idempotent and reports how many
// messages it changed.
func (r *OutboxRepository) BackfillOutgoingMediaAttachments(ctx context.Context) (int, error) {
	nowMS, err := r.nowMS("backfill sent media attachments")
	if err != nil {
		return 0, err
	}
	result, err := r.store.db.ExecContext(ctx, `
		INSERT INTO message_attachments (
			message_id,
			ordinal,
			filename,
			mime,
			size_bytes,
			state,
			blob_hash,
			created_at_ms,
			updated_at_ms
		)
		SELECT m.message_id, 0, oa.filename, oa.mime, oa.size_bytes, 'downloaded', oa.blob_hash, ?, ?
		FROM outbox AS o
		JOIN outbox_attachments AS oa ON oa.outbox_id = o.outbox_id AND oa.ordinal = 0
		JOIN messages AS m ON m.message_id = o.local_message_id
		WHERE o.kind = 'media'
		  AND m.direction = 'outgoing'
		  AND NOT EXISTS (
			SELECT 1 FROM message_attachments AS a WHERE a.message_id = m.message_id
		  )
	`, nowMS, nowMS)
	if err != nil {
		return 0, fmt.Errorf("backfill sent media attachments: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("backfill sent media attachments: read rows affected: %w", err)
	}
	return int(affected), nil
}
