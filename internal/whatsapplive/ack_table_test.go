package whatsapplive_test

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// TestAckFollowsTheIngressCommit runs every kind of stanza whatsmeow
// acknowledges through the real bridge against fakeWhatsApp, once per
// outcome of the ingress observer, with the connection left open: only the
// bridge's handler status decides whether the stanza is acknowledged.
//
// Invariants, for every row:
//   - A stanza that carries an ingress frame (message, receipt, newsletter
//     post) is acknowledged iff its frame committed or no lifecycle adapter
//     owns ingress (a legacy-only process), and never before the commit.
//   - A stanza left unacknowledged is redelivered on the next connection,
//     where it commits once, under the dedupe key of the first attempt, and
//     only then is acknowledged.
//   - A stanza that carries no ingress frame (a group notification, a call)
//     is acknowledged after its handlers, whatever the observer does.
//   - An incoming retry receipt is acknowledged iff its resend went out,
//     whatever the observer does with its receipt frame.
//   - The legacy store takes a message only on a delivery reported handled;
//     a receipt's legacy status (an upgrade-only update) applies on every
//     delivery.
func TestAckFollowsTheIngressCommit(t *testing.T) {
	appendFailed := errors.New("append failed: disk I/O error")
	outcomes := []tableOutcome{
		{name: "committed", observe: true},
		{name: "append error", observe: true, err: appendFailed},
		{name: "stale generation", observe: true, err: bridge.ErrStaleGeneration},
		{name: "no observer while an adapter owns ingress", observe: true, unobserved: true},
		{name: "legacy-only process", observe: false},
	}
	for _, kind := range tableStanzaKinds() {
		for _, outcome := range outcomes {
			t.Run(kind.name+"/"+outcome.name, func(t *testing.T) {
				runAckTableRow(t, kind, outcome)
			})
		}
	}
}

type tableOutcome struct {
	name string
	// observe makes the process own ingress (ObserveIngress), as a process
	// running a lifecycle adapter does.
	observe bool
	// err is what the observer returns for the first delivery.
	err error
	// unobserved removes the observer before the first delivery, as a
	// retired generation does.
	unobserved bool
}

// redelivers reports whether a stanza carrying an ingress frame is left
// unacknowledged under this outcome.
func (o tableOutcome) redelivers() bool { return o.observe && (o.err != nil || o.unobserved) }

// tableKind is one kind of stanza.
type tableKind struct {
	name string
	// carriesFrame: the stanza's handling commits an ingress frame and its
	// ack follows the commit.
	carriesFrame bool
	// retry: an incoming retry receipt, acknowledged after its resend.
	retry bool
	// legacyMessage: the legacy store takes the stanza as a message.
	legacyMessage bool
	// legacyReceipt: the stanza upgrades the legacy status of our outgoing
	// message to this status.
	legacyReceipt string
	// frames is how many distinct frames (dedupe keys) one delivery commits.
	frames int
	// setup prepares the row and returns the stanza and the matcher for its
	// frames.
	setup func(r *tableRow) (*waBinary.Node, func(bridge.RawIngressRecord) bool)
}

// tableRow is one bridge, connected to its own fakeWhatsApp.
type tableRow struct {
	*teardownHarness
	server  *fakeWhatsApp
	clock   *atomic.Uint64
	logs    *logCapture
	outcome atomic.Pointer[error]

	mu      sync.Mutex
	commits []orderedCommit
	changed chan struct{}

	sentForRetry sync.Map
	sentinels    int
	// disconnects counts the Disconnected events whatsmeow dispatched; it
	// does so once it has finished with the socket.
	disconnects atomic.Int32
}

func newTableRow(t *testing.T) *tableRow {
	t.Helper()
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "whatsapp-session.db")
	ourJID := watypes.JID{User: "15551230000", Device: 7, Server: watypes.DefaultUserServer}
	ourLID := watypes.JID{User: "100000000000001", Device: 7, Server: watypes.HiddenUserServer}
	whatsapplive.SeedDeviceForTeardownTest(t, sessionPath, ourJID)
	messages, err := db.New(filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = messages.Close() })
	r := &tableRow{clock: new(atomic.Uint64), logs: newLogCapture(), changed: make(chan struct{})}
	// The server starts first so the bridge's cleanup closes the client
	// before the server drops the connection: whatsmeow races on its
	// disconnect handler when the two closes overlap (NoiseSocket.Stop
	// writes FrameSocket.OnDisconnect without the socket's lock).
	r.server = newFakeWhatsApp(t, r.clock, ourLID)
	logger := zerolog.New(r.logs).Level(zerolog.DebugLevel)
	host, err := whatsapplive.New(sessionPath, messages, logger, whatsapplive.Callbacks{})
	if err != nil {
		t.Fatalf("whatsapplive.New(): %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	host.ObserveLifecycle(func(event whatsapplive.LifecycleEvent) {
		if _, ok := event.Raw.(*waevents.Disconnected); ok {
			r.disconnects.Add(1)
		}
	})
	cli := whatsapplive.ClientForTeardownTest(host)
	cli.GetMessageForRetry = func(_, _ watypes.JID, id watypes.MessageID) *waE2E.Message {
		if message, ok := r.sentForRetry.Load(id); ok {
			return message.(*waE2E.Message)
		}
		return nil
	}
	cli.SetWebsocketHTTPClient(r.server.httpClient())
	peerJID := watypes.JID{User: "15559870000", Server: watypes.DefaultUserServer}
	r.teardownHarness = &teardownHarness{
		t:           t,
		host:        host,
		cli:         cli,
		messages:    messages,
		sessionPath: sessionPath,
		ourJID:      ourJID,
		peerJID:     peerJID,
		peer:        whatsapplive.NewPeerDeviceForTeardownTest(t, filepath.Join(dir, "peer-session.db"), peerJID),
		groupJID:    watypes.JID{User: "120363000000000001", Server: watypes.GroupServer},
	}
	return r
}

// observe installs an ingress observer that returns the current outcome's
// error, and otherwise commits the frame, stamped from the server's clock.
func (r *tableRow) observe() func() {
	return r.host.ObserveIngress(func(frame whatsapplive.IngressFrame) error {
		if err := r.outcome.Load(); err != nil && *err != nil {
			return *err
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.commits = append(r.commits, orderedCommit{seq: r.clock.Add(1), record: bridge.RawIngressRecord{
			DedupeKey:  frame.DedupeKey,
			ReceivedAt: frame.ReceivedAt,
			Payload:    frame.Payload,
		}})
		close(r.changed)
		r.changed = make(chan struct{})
		return nil
	})
}

func (r *tableRow) matching(match func(bridge.RawIngressRecord) bool) []orderedCommit {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []orderedCommit
	for _, commit := range r.commits {
		if match(commit.record) {
			out = append(out, commit)
		}
	}
	return out
}

// connect logs in on the given connection and waits until the bridge reports
// the connection up.
func (r *tableRow) connect(connection int) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), teardownTestTimeout)
	defer cancel()
	if err := r.host.ConnectPaired(ctx); err != nil {
		r.t.Fatalf("ConnectPaired(): %v", err)
	}
	r.server.awaitConnections(r.t, connection)
	if !awaitTeardownCondition(r.t, "the bridge to report the connection up", func() bool {
		return r.host.Status().Connected
	}) {
		r.t.FailNow()
	}
}

// reconnect drops the connection the way the network does and logs in again
// once whatsmeow has reported the disconnect.
func (r *tableRow) reconnect(connection int) {
	r.t.Helper()
	before := r.disconnects.Load()
	r.server.dropConnection()
	if !awaitTeardownCondition(r.t, "whatsmeow to report the disconnect", func() bool {
		return r.disconnects.Load() > before
	}) {
		r.t.FailNow()
	}
	r.connect(connection)
}

// drain delivers a notification whatsmeow does not handle and waits for its
// acknowledgement. whatsmeow's handler queue runs one stanza at a time and
// (with synchronous acks) acknowledges a stanza before its handler returns,
// so every stanza delivered before it has been handled and acknowledged, or
// left unacknowledged, by then.
func (r *tableRow) drain() {
	r.t.Helper()
	r.sentinels++
	sentinel := &waBinary.Node{Tag: "notification", Attrs: waBinary.Attrs{
		"from": watypes.ServerJID,
		"id":   "SENTINEL" + strconv.Itoa(r.sentinels),
		"type": "openmessage_test_sentinel",
		"t":    nowAttr(),
	}}
	r.server.deliver(sentinel)
	r.server.awaitReceived(r.t, "the sentinel's acknowledgement", ackFor(sentinel))
}

func (r *tableRow) seedOutgoing(id watypes.MessageID, conversation watypes.JID) {
	r.t.Helper()
	if err := r.messages.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:" + string(id),
		ConversationID: "whatsapp:" + conversation.String(),
		Body:           "our message " + string(id),
		TimestampMS:    time.Now().UnixMilli(),
		Status:         "sent",
		IsFromMe:       true,
		SourcePlatform: "whatsapp",
		SourceID:       string(id),
	}); err != nil {
		r.t.Fatalf("seed outgoing message %s: %v", id, err)
	}
}

func tableStanzaKinds() []tableKind {
	return []tableKind{
		{
			name: "delivery receipt", carriesFrame: true, legacyReceipt: "delivered", frames: 1,
			setup: func(r *tableRow) (*waBinary.Node, func(bridge.RawIngressRecord) bool) {
				r.seedOutgoing("3EB0TABLEDELIVERED", r.peerJID)
				return &waBinary.Node{Tag: "receipt", Attrs: waBinary.Attrs{
					"from": r.peerJID, "id": "3EB0TABLEDELIVERED", "t": nowAttr(),
				}}, receiptFrame("3EB0TABLEDELIVERED")
			},
		},
		{
			name: "read receipt for several messages", carriesFrame: true, legacyReceipt: "read", frames: 1,
			setup: func(r *tableRow) (*waBinary.Node, func(bridge.RawIngressRecord) bool) {
				r.seedOutgoing("3EB0TABLEREADA", r.peerJID)
				return &waBinary.Node{Tag: "receipt", Attrs: waBinary.Attrs{
					"from": r.peerJID, "id": "3EB0TABLEREADA", "t": nowAttr(), "type": "read",
				}, Content: []waBinary.Node{{Tag: "list", Content: []waBinary.Node{
					{Tag: "item", Attrs: waBinary.Attrs{"id": "3EB0TABLEREADB"}},
				}}}}, receiptFrame("3EB0TABLEREADB")
			},
		},
		{
			name: "grouped read receipt", carriesFrame: true, legacyReceipt: "read", frames: 2,
			setup: func(r *tableRow) (*waBinary.Node, func(bridge.RawIngressRecord) bool) {
				r.seedOutgoing("3EB0TABLEGROUPED", r.groupJID)
				return &waBinary.Node{Tag: "receipt", Attrs: waBinary.Attrs{
					"from": r.groupJID, "id": "3EB0TABLEGROUPED", "t": nowAttr(), "type": "read",
				}, Content: []waBinary.Node{{Tag: "participants", Attrs: waBinary.Attrs{"key": "3EB0TABLEGROUPED"}, Content: []waBinary.Node{
					{Tag: "user", Attrs: waBinary.Attrs{"jid": r.peerJID, "t": nowAttr()}},
					{Tag: "user", Attrs: waBinary.Attrs{"jid": watypes.JID{User: "15550001111", Server: watypes.DefaultUserServer}, "t": nowAttr()}},
				}}}}, receiptFrame("3EB0TABLEGROUPED")
			},
		},
		{
			name: "newsletter post", carriesFrame: true, legacyMessage: true, frames: 1,
			setup: func(r *tableRow) (*waBinary.Node, func(bridge.RawIngressRecord) bool) {
				return newsletterStanza(r.t, "message", "NEWSLETTERTABLE1", "a channel post"), messageFrame("NEWSLETTERTABLE1")
			},
		},
		{
			name: "newsletter status update", carriesFrame: true, legacyMessage: true, frames: 1,
			setup: func(r *tableRow) (*waBinary.Node, func(bridge.RawIngressRecord) bool) {
				return newsletterStanza(r.t, "status", "NEWSLETTERTABLE2", "a channel status update"), messageFrame("NEWSLETTERTABLE2")
			},
		},
		{
			name: "encrypted message", carriesFrame: true, legacyMessage: true, frames: 1,
			setup: func(r *tableRow) (*waBinary.Node, func(bridge.RawIngressRecord) bool) {
				r.startPeerSession()
				return r.peerMessage("3EB0TABLEENCRYPTED", "an encrypted message"), messageFrame("3EB0TABLEENCRYPTED")
			},
		},
		{
			name: "group notification",
			setup: func(r *tableRow) (*waBinary.Node, func(bridge.RawIngressRecord) bool) {
				return &waBinary.Node{Tag: "notification", Attrs: waBinary.Attrs{
					"from": r.groupJID, "id": "GROUPNOTIFICATION1", "t": nowAttr(), "type": "w:gp2", "participant": r.peerJID,
				}, Content: []waBinary.Node{{Tag: "subject", Attrs: waBinary.Attrs{
					"subject": "renamed", "s_t": nowAttr(), "s_o": r.peerJID,
				}}}}, func(bridge.RawIngressRecord) bool { return false }
			},
		},
		{
			name: "call offer",
			setup: func(r *tableRow) (*waBinary.Node, func(bridge.RawIngressRecord) bool) {
				return &waBinary.Node{Tag: "call", Attrs: waBinary.Attrs{
					"from": r.peerJID, "id": "CALLSTANZA1", "t": nowAttr(),
				}, Content: []waBinary.Node{{Tag: "offer", Attrs: waBinary.Attrs{
					"call-id": "CALLID1", "call-creator": r.peerJID,
				}}}}, func(bridge.RawIngressRecord) bool { return false }
			},
		},
		{
			name: "incoming retry receipt", retry: true,
			setup: func(r *tableRow) (*waBinary.Node, func(bridge.RawIngressRecord) bool) {
				// The peer's first message gives us a session to resend with.
				r.startPeerSession()
				opening := r.peerMessage("3EB0TABLEOPENING", "the peer says hello")
				r.server.deliver(opening)
				r.server.awaitReceived(r.t, "the receipt for the peer's message", ackFor(opening))
				r.sentForRetry.Store(watypes.MessageID("3EB0TABLERETRY"), &waE2E.Message{Conversation: proto.String("resend me")})
				registration := make([]byte, 4)
				binary.BigEndian.PutUint32(registration, r.peer.RegistrationID)
				return &waBinary.Node{Tag: "receipt", Attrs: waBinary.Attrs{
					"from": r.peerJID, "id": "3EB0TABLERETRY", "t": nowAttr(), "type": string(watypes.ReceiptTypeRetry),
				}, Content: []waBinary.Node{
					{Tag: "retry", Attrs: waBinary.Attrs{"count": "1", "id": "3EB0TABLERETRY", "t": nowAttr(), "v": "1"}},
					{Tag: "registration", Content: registration},
				}}, receiptFrame("3EB0TABLERETRY")
			},
		},
	}
}

func newsletterStanza(t testing.TB, tag, id, text string) *waBinary.Node {
	t.Helper()
	plaintext, err := proto.Marshal(&waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		t.Fatalf("marshal newsletter content: %v", err)
	}
	return &waBinary.Node{Tag: tag, Attrs: waBinary.Attrs{
		"from":      watypes.JID{User: "120363000000000099", Server: watypes.NewsletterServer},
		"id":        id,
		"t":         nowAttr(),
		"type":      "text",
		"server_id": "100",
	}, Content: []waBinary.Node{{Tag: "plaintext", Content: plaintext}}}
}

func runAckTableRow(t *testing.T, kind tableKind, outcome tableOutcome) {
	r := newTableRow(t)
	var removeObserver func()
	if outcome.observe {
		removeObserver = r.observe()
	}
	r.connect(1)
	stanza, frames := kind.setup(r)
	id := fakeAttr(stanza, "id")

	if outcome.unobserved {
		removeObserver()
	}
	if outcome.err != nil {
		err := outcome.err
		r.outcome.Store(&err)
	}
	r.server.deliver(stanza)
	if kind.retry {
		// whatsmeow handles a retry receipt apart from its handler queue and
		// acknowledges it after the resend, whatever the observer did with
		// the receipt's frame.
		resend := r.server.awaitReceived(t, "the resend", func(received fakeReceived) bool {
			return received.node.Tag == "message" && fakeAttr(received.node, "id") == id
		})
		ack := r.server.awaitReceived(t, "the retry receipt's acknowledgement", ackFor(stanza))
		if ack.seq < resend.seq {
			t.Fatalf("retry receipt acknowledged (seq %d) before its resend (seq %d)", ack.seq, resend.seq)
		}
		return
	}
	r.drain()

	acked := len(r.server.receivedNodes(ackFor(stanza))) > 0
	committed := r.matching(frames)
	expectRedelivery := kind.carriesFrame && outcome.redelivers()
	switch {
	case expectRedelivery && acked:
		t.Fatalf("%s acknowledged although its frame was not committed (%s)", kind.name, outcome.name)
	case !expectRedelivery && !acked:
		t.Fatalf("%s left unacknowledged (%s)", kind.name, outcome.name)
	case expectRedelivery && len(committed) > 0:
		t.Fatalf("%s committed %d frames under outcome %q", kind.name, len(committed), outcome.name)
	}
	if kind.carriesFrame && !expectRedelivery && outcome.observe {
		requireFramesBeforeAck(t, r, stanza, committed, kind.frames)
	}
	if kind.carriesFrame && !outcome.observe && len(committed) > 0 {
		t.Fatalf("legacy-only process committed %d frames", len(committed))
	}
	firstCommitKeys := dedupeKeys(committed)
	requireLegacy(t, r, kind, id, !expectRedelivery)

	if !expectRedelivery {
		return
	}
	// The next connection gets the stanza again, and this time the frame
	// commits.
	r.outcome.Store(nil)
	if outcome.unobserved {
		removeObserver = r.observe()
	}
	r.reconnect(2)
	r.drain()
	if !r.server.pendingStanzaNode(stanza) && len(r.server.receivedNodes(ackFor(stanza))) == 0 {
		t.Fatalf("%s neither redelivered nor acknowledged", kind.name)
	}
	committed = r.matching(frames)
	requireFramesBeforeAck(t, r, stanza, committed, kind.frames)
	if len(firstCommitKeys) > 0 && !slices.Equal(firstCommitKeys, dedupeKeys(committed)) {
		t.Fatalf("redelivery committed under %v, first delivery under %v", dedupeKeys(committed), firstCommitKeys)
	}
	requireLegacy(t, r, kind, id, true)
	_ = removeObserver
}

func dedupeKeys(commits []orderedCommit) []string {
	keys := make([]string, 0, len(commits))
	for _, commit := range commits {
		if !slices.Contains(keys, commit.record.DedupeKey) {
			keys = append(keys, commit.record.DedupeKey)
		}
	}
	slices.Sort(keys)
	return keys
}

// requireFramesBeforeAck checks that a delivery committed the expected frames
// once each and that the stanza's acknowledgement followed every commit.
func requireFramesBeforeAck(t *testing.T, r *tableRow, stanza *waBinary.Node, committed []orderedCommit, frames int) {
	t.Helper()
	if keys := dedupeKeys(committed); len(keys) != frames {
		t.Fatalf("%s committed %d distinct frames %v, want %d", stanza.Tag, len(keys), keys, frames)
	}
	if len(committed) != frames {
		t.Fatalf("%s committed %d frames for %d dedupe keys", stanza.Tag, len(committed), frames)
	}
	acks := r.server.receivedNodes(ackFor(stanza))
	if len(acks) == 0 {
		t.Fatalf("%s %v committed but never acknowledged", stanza.Tag, stanza.Attrs["id"])
	}
	for _, commit := range committed {
		if acks[0].seq < commit.seq {
			t.Fatalf("%s %v acknowledged (seq %d) before frame %s committed (seq %d)",
				stanza.Tag, stanza.Attrs["id"], acks[0].seq, commit.record.DedupeKey, commit.seq)
		}
	}
}

// requireLegacy checks the legacy store after a delivery: a message is there
// iff a delivery was reported handled; a receipt's status upgrade applies on
// every delivery.
func requireLegacy(t *testing.T, r *tableRow, kind tableKind, id string, handled bool) {
	t.Helper()
	stored, err := r.messages.GetMessageByID("whatsapp:" + id)
	if err != nil {
		t.Fatalf("legacy GetMessageByID(%s): %v", id, err)
	}
	switch {
	case kind.legacyMessage && handled && stored == nil:
		t.Fatalf("legacy store missing %s after a handled delivery", id)
	case kind.legacyMessage && !handled && stored != nil:
		t.Fatalf("legacy store took %s from a delivery reported not handled", id)
	case kind.legacyReceipt != "" && (stored == nil || stored.Status != kind.legacyReceipt):
		t.Fatalf("legacy status of %s = %+v, want %q", id, stored, kind.legacyReceipt)
	}
}

// pendingStanzaNode reports whether the server still holds this stanza.
func (f *fakeWhatsApp) pendingStanzaNode(node *waBinary.Node) bool {
	return f.pendingStanza(node.Tag, fakeAttr(node, "id"), fakeAttr(node, "type"))
}
