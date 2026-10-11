package whatsapplive_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/libsignal/protocol"
	"go.mau.fi/libsignal/session"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/maxghenis/openmessage/internal/bridge"
	whatsappadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/whatsapp"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// These tests run whatsmeow's real connection against fakeWhatsApp: the real
// handshake, login, handler queue, acks, receipts and resends, over a real
// noise socket. A generation ends the way a network drop ends it: the server
// closes the socket, whatsmeow reports Disconnected, and the supervisor
// retires the generation. The server keeps every stanza the client has not
// acknowledged and redelivers it to the next connection, as WhatsApp's server
// does.
//
// Invariant: no stanza is acknowledged before its v2 ingress frame commits.
// A stanza whose frame was not committed stays unacknowledged, so the server
// redelivers it to a later generation, which commits it. An incoming retry
// receipt is acknowledged only after its resend went out, so a resend lost
// with the socket is redone on the next connection.

// orderedSink is the v2 ingest sink behind the supervisor's generation fence.
// It stamps every commit from the clock fakeWhatsApp stamps received nodes
// from, so a test can check that an acknowledgement followed the commit.
type orderedSink struct {
	clock *atomic.Uint64

	mu      sync.Mutex
	commits []orderedCommit
	changed chan struct{}
	// fail, when set, decides an append's error before it commits.
	fail atomic.Pointer[func(bridge.RawIngressRecord) error]
}

type orderedCommit struct {
	seq    uint64
	record bridge.RawIngressRecord
}

func newOrderedSink(clock *atomic.Uint64) *orderedSink {
	return &orderedSink{clock: clock, changed: make(chan struct{})}
}

func (s *orderedSink) AppendIngress(_ context.Context, record bridge.RawIngressRecord) error {
	if fail := s.fail.Load(); fail != nil {
		if err := (*fail)(record); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits = append(s.commits, orderedCommit{seq: s.clock.Add(1), record: record})
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

func (s *orderedSink) EmitEphemeral(context.Context, bridge.EphemeralEvent) error { return nil }

func (s *orderedSink) Beat(bridge.Generation, time.Time, string) {}

// matching returns the commits whose record matches.
func (s *orderedSink) matching(match func(bridge.RawIngressRecord) bool) []orderedCommit {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []orderedCommit
	for _, commit := range s.commits {
		if match(commit.record) {
			out = append(out, commit)
		}
	}
	return out
}

// messageFrame matches the frames of one message id.
func messageFrame(id watypes.MessageID) func(bridge.RawIngressRecord) bool {
	return func(record bridge.RawIngressRecord) bool {
		return strings.HasPrefix(record.DedupeKey, "msg:"+string(id)+":")
	}
}

// receiptFrame matches the receipt frames that name one message id.
func receiptFrame(id watypes.MessageID) func(bridge.RawIngressRecord) bool {
	return func(record bridge.RawIngressRecord) bool {
		if !strings.HasPrefix(record.DedupeKey, "rcpt:") {
			return false
		}
		var envelope struct {
			Kind       string   `json:"kind"`
			MessageIDs []string `json:"message_ids"`
		}
		if err := json.Unmarshal(record.Payload, &envelope); err != nil {
			return false
		}
		return envelope.Kind == "receipt" && slices.Contains(envelope.MessageIDs, string(id))
	}
}

// logCapture records the bridge's log lines (whatsmeow logs through it).
type logCapture struct {
	mu      sync.Mutex
	lines   []string
	changed chan struct{}
}

func newLogCapture() *logCapture { return &logCapture{changed: make(chan struct{})} }

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.lines = append(l.lines, string(p))
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
	return len(p), nil
}

// await waits until a logged line contains every fragment. It reports
// failure instead of stopping the test, so hooks may call it.
func (l *logCapture) await(t testing.TB, description string, fragments ...string) bool {
	t.Helper()
	deadline := time.After(teardownTestTimeout)
	for {
		l.mu.Lock()
		for _, line := range l.lines {
			matched := true
			for _, fragment := range fragments {
				if !strings.Contains(line, fragment) {
					matched = false
					break
				}
			}
			if matched {
				l.mu.Unlock()
				return true
			}
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			t.Errorf("timed out waiting for the log to show %s", description)
			return false
		}
	}
}

// lidStoreHook runs an armed callback before a LID-to-phone-number lookup,
// which the bridge makes while it builds a receipt frame for a LID chat.
type lidStoreHook struct {
	store.LIDStore
	before atomic.Pointer[func(context.Context, watypes.JID)]
}

func (h *lidStoreHook) GetPNForLID(ctx context.Context, lid watypes.JID) (watypes.JID, error) {
	if before := h.before.Load(); before != nil {
		(*before)(ctx, lid)
	}
	return h.LIDStore.GetPNForLID(ctx, lid)
}

// secretStoreHook runs an armed callback before whatsmeow stores a message's
// secret, which it does after acknowledging a newsletter message
// asynchronously and before dispatching it.
type secretStoreHook struct {
	store.MsgSecretStore
	before atomic.Pointer[func(context.Context, watypes.MessageID)]
}

func (h *secretStoreHook) PutMessageSecret(ctx context.Context, chat, sender watypes.JID, id watypes.MessageID, secret []byte) error {
	if before := h.before.Load(); before != nil {
		(*before)(ctx, id)
	}
	return h.MsgSecretStore.PutMessageSecret(ctx, chat, sender, id, secret)
}

type ackHarness struct {
	*teardownHarness
	server  *fakeWhatsApp
	clock   *atomic.Uint64
	commits *orderedSink
	logs    *logCapture
	lids    *lidStoreHook
	secrets *secretStoreHook
	peerLID watypes.JID

	// retryHook, when set, runs inside whatsmeow's handling of an incoming
	// retry receipt, just before it encrypts and sends the resend.
	retryHook atomic.Pointer[func(*waevents.Receipt) bool]
	// sentForRetry holds the messages whatsmeow finds for retry receipts
	// (GetMessageForRetry), by message id.
	sentForRetry sync.Map
}

// newAckHarness starts the real bridge, lifecycle adapter and supervisor
// against fakeWhatsApp. Every hook is installed before the client connects.
func newAckHarness(t *testing.T) *ackHarness {
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
	h := &ackHarness{
		clock:   new(atomic.Uint64),
		peerLID: watypes.JID{User: "200000000000002", Server: watypes.HiddenUserServer},
	}
	// The server starts first so its cleanup (dropping the connection) runs
	// after the client has been closed: whatsmeow races on its disconnect
	// handler when the two closes overlap.
	h.server = newFakeWhatsApp(t, h.clock, ourLID)
	logs := newLogCapture()
	h.logs = logs
	var out = zerolog.MultiLevelWriter(logs)
	if os.Getenv("OPENMESSAGE_TEARDOWN_TEST_LOG") != "" {
		out = zerolog.MultiLevelWriter(logs, zerolog.ConsoleWriter{Out: os.Stderr})
	}
	logger := zerolog.New(out).Level(zerolog.DebugLevel).With().Timestamp().Logger()
	host, err := whatsapplive.New(sessionPath, messages, logger, whatsapplive.Callbacks{})
	if err != nil {
		t.Fatalf("whatsapplive.New(): %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })

	cli := whatsapplive.ClientForTeardownTest(host)
	h.lids = &lidStoreHook{LIDStore: cli.Store.LIDs}
	cli.Store.LIDs = h.lids
	h.secrets = &secretStoreHook{MsgSecretStore: cli.Store.MsgSecrets}
	cli.Store.MsgSecrets = h.secrets
	cli.PreRetryCallback = func(receipt *waevents.Receipt, _ watypes.MessageID, _ int, _ *waE2E.Message) bool {
		if hook := h.retryHook.Load(); hook != nil {
			return (*hook)(receipt)
		}
		return true
	}
	cli.GetMessageForRetry = func(_, _ watypes.JID, id watypes.MessageID) *waE2E.Message {
		if message, ok := h.sentForRetry.Load(id); ok {
			return message.(*waE2E.Message)
		}
		return nil
	}

	cli.SetWebsocketHTTPClient(h.server.httpClient())
	dials := make(chan context.Context, 8)
	whatsapplive.StubDialForTeardownTest(t, func(ctx context.Context, cli *whatsmeow.Client) error {
		select {
		case dials <- ctx:
		default:
		}
		return cli.ConnectContext(ctx)
	})
	h.commits = newOrderedSink(h.clock)
	gate := newBackoffGate()
	supervisor, err := bridge.NewSupervisor(
		"whatsapp-primary",
		bridge.PlatformWhatsApp,
		whatsappadapter.New("whatsapp-primary", host),
		bridge.Policy{
			ConnectTimeout:     time.Minute,
			ProbeEvery:         time.Hour,
			ProbeTimeout:       time.Minute,
			LivenessTimeout:    time.Hour,
			MinBackoff:         20 * time.Millisecond,
			MaxBackoff:         100 * time.Millisecond,
			MaxSameFingerprint: 1000,
		},
		teardownClock{gate: gate},
		teardownRandom{},
		bridge.WithConnectionSink(h.commits),
	)
	if err != nil {
		t.Fatalf("NewSupervisor(): %v", err)
	}
	t.Cleanup(func() {
		gate.release()
		ctx, cancel := context.WithTimeout(context.Background(), teardownTestTimeout)
		defer cancel()
		_ = supervisor.Stop(ctx)
	})
	startCtx, cancelStart := context.WithTimeout(context.Background(), teardownTestTimeout)
	defer cancelStart()
	if err := supervisor.Start(startCtx, bridge.StartRequest{}); err != nil {
		t.Fatalf("Supervisor.Start(): %v", err)
	}

	peerJID := watypes.JID{User: "15559870000", Server: watypes.DefaultUserServer}
	h.teardownHarness = &teardownHarness{
		t:           t,
		host:        host,
		cli:         cli,
		messages:    messages,
		supervisor:  supervisor,
		gate:        gate,
		dials:       dials,
		sessionPath: sessionPath,
		ourJID:      ourJID,
		peerJID:     peerJID,
		peer:        whatsapplive.NewPeerDeviceForTeardownTest(t, filepath.Join(dir, "peer-session.db"), peerJID),
		groupJID:    watypes.JID{User: "120363000000000001", Server: watypes.GroupServer},
	}
	return h
}

// awaitOnline lets the next generation start and waits until it has logged
// in on the given connection and is online.
func (h *ackHarness) awaitOnline(connection int) bridge.Generation {
	h.t.Helper()
	h.gate.release()
	h.server.awaitConnections(h.t, connection)
	if !awaitTeardownCondition(h.t, "the generation to come online", func() bool {
		return h.supervisor.Snapshot().State == bridge.StateOnline
	}) {
		h.t.FailNow()
	}
	return h.supervisor.Snapshot().Generation
}

// dropAndRetire drops the connection the way the network does and waits
// until the supervisor has retired the generation. The next generation stays
// held until awaitOnline. Hooks call it, so it reports failure instead of
// stopping the test.
func (h *ackHarness) dropAndRetire() {
	h.gate.hold()
	h.server.dropConnection()
	awaitTeardownCondition(h.t, "the generation to retire", func() bool {
		return h.supervisor.Snapshot().State == bridge.StateBackoff
	})
}

// awaitAsyncAck places a teardown after an acknowledgement whatsmeow sends
// before it handles a stanza. With asynchronous acks whatsmeow sends it in a
// goroutine as handling starts; the teardown under test lands after it is on
// the wire. With synchronous acks there is no such acknowledgement.
func (h *ackHarness) awaitAsyncAck(stanza *waBinary.Node) {
	if h.cli.SynchronousAck {
		return
	}
	if _, ok := h.server.waitReceived(teardownTestTimeout, ackFor(stanza)); !ok {
		h.t.Errorf("whatsmeow never acknowledged %s %v asynchronously", stanza.Tag, stanza.Attrs["id"])
	}
}

func nowAttr() string { return strconv.FormatInt(time.Now().Unix(), 10) }

// requireCommittedBeforeAck checks the invariant for one stanza: it was
// committed in the given generation only, and the client acknowledged it only
// after the first commit.
func (h *ackHarness) requireCommittedBeforeAck(stanza *waBinary.Node, commits []orderedCommit, generation bridge.Generation) {
	h.t.Helper()
	if len(commits) == 0 {
		h.t.Fatalf("%s %v never reached v2 ingest", stanza.Tag, stanza.Attrs["id"])
	}
	for _, commit := range commits {
		if commit.record.Generation != generation {
			h.t.Fatalf("%s %v committed by generation %d, want only %d", stanza.Tag, stanza.Attrs["id"], commit.record.Generation, generation)
		}
		if commit.record.DedupeKey != commits[0].record.DedupeKey {
			h.t.Fatalf("%s %v committed under dedupe keys %q and %q", stanza.Tag, stanza.Attrs["id"], commits[0].record.DedupeKey, commit.record.DedupeKey)
		}
	}
	ack := h.server.awaitReceived(h.t, "the acknowledgement of "+stanza.Tag+" "+fakeAttr(stanza, "id"), ackFor(stanza))
	if ack.seq < commits[0].seq {
		h.t.Fatalf("%s %v acknowledged (seq %d) before its frame committed (seq %d)", stanza.Tag, stanza.Attrs["id"], ack.seq, commits[0].seq)
	}
}

// awaitSettled waits until a stanza's frame committed or the server no
// longer holds it (it was acknowledged, so it will never be redelivered),
// and returns the commits.
func (h *ackHarness) awaitSettled(stanza *waBinary.Node, match func(bridge.RawIngressRecord) bool) []orderedCommit {
	h.t.Helper()
	tag, id, typ := stanza.Tag, fakeAttr(stanza, "id"), fakeAttr(stanza, "type")
	var commits []orderedCommit
	awaitTeardownCondition(h.t, "the stanza to commit or be acknowledged", func() bool {
		commits = h.commits.matching(match)
		return len(commits) > 0 || !h.server.pendingStanza(tag, id, typ)
	})
	if len(commits) == 0 {
		h.t.Fatalf("%s %s was acknowledged and never reached v2 ingest; the server will not redeliver it", tag, id)
	}
	return commits
}

// TestReceiptRacingTeardownReachesTheNextGeneration delivers a delivery
// receipt and drops the connection while the bridge builds its frame. Before
// the fix whatsmeow acknowledged the receipt before handling it (asynchronous
// acks), the bridge dropped the frame because no generation was observing
// ingress, and the server never sent the receipt again.
func TestReceiptRacingTeardownReachesTheNextGeneration(t *testing.T) {
	h := newAckHarness(t)
	h.awaitOnline(1)

	const id = watypes.MessageID("3EB0RECEIPTRACE")
	// Our outgoing message, so the legacy store shows when the bridge has
	// finished with the first delivery (it marks the message delivered).
	if err := h.messages.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:" + string(id),
		ConversationID: "whatsapp:" + h.peerJID.String(),
		Body:           "sent before the receipt",
		TimestampMS:    time.Now().UnixMilli(),
		Status:         "sent",
		IsFromMe:       true,
		SourcePlatform: "whatsapp",
		SourceID:       string(id),
	}); err != nil {
		t.Fatalf("seed outgoing message: %v", err)
	}
	receipt := &waBinary.Node{Tag: "receipt", Attrs: waBinary.Attrs{
		"from": h.peerLID,
		"id":   string(id),
		"t":    nowAttr(),
	}}
	var once sync.Once
	before := func(_ context.Context, lid watypes.JID) {
		if lid.User == h.peerLID.User {
			once.Do(func() {
				h.awaitAsyncAck(receipt)
				h.dropAndRetire()
			})
		}
	}
	h.lids.before.Store(&before)

	h.server.deliver(receipt)
	awaitTeardownCondition(t, "the bridge to finish the first delivery", func() bool {
		stored, err := h.messages.GetMessageByID("whatsapp:" + string(id))
		return err == nil && stored != nil && stored.Status == "delivered"
	})

	second := h.awaitOnline(2)
	h.requireCommittedBeforeAck(receipt, h.awaitSettled(receipt, receiptFrame(id)), second)
}

// TestNewsletterMessageRacingTeardownReachesTheNextGeneration delivers a
// newsletter (channel) post, which WhatsApp sends unencrypted, and drops the
// connection after whatsmeow decoded it and before it dispatched it. Before
// the fix whatsmeow acknowledged the post before handling it, the bridge
// reported it not handled because no generation was observing ingress, and
// the post was lost from v2 and the legacy store.
func TestNewsletterMessageRacingTeardownReachesTheNextGeneration(t *testing.T) {
	h := newAckHarness(t)
	h.awaitOnline(1)

	const id = watypes.MessageID("NEWSLETTERRACE1")
	const text = "a channel post that arrives during a teardown"
	plaintext, err := proto.Marshal(withSecret(text))
	if err != nil {
		t.Fatalf("marshal newsletter post: %v", err)
	}
	post := &waBinary.Node{Tag: "message", Attrs: waBinary.Attrs{
		"from":      watypes.JID{User: "120363000000000099", Server: watypes.NewsletterServer},
		"id":        string(id),
		"t":         nowAttr(),
		"type":      "text",
		"server_id": "100",
	}, Content: []waBinary.Node{{Tag: "plaintext", Content: plaintext}}}
	var once sync.Once
	before := func(_ context.Context, secretFor watypes.MessageID) {
		if secretFor == id {
			once.Do(func() {
				h.awaitAsyncAck(post)
				h.dropAndRetire()
			})
		}
	}
	h.secrets.before.Store(&before)

	h.server.deliver(post)
	h.logs.await(t, "the bridge to report the post not committed",
		"WhatsApp message not committed to ingress", string(id))

	second := h.awaitOnline(2)
	h.requireCommittedBeforeAck(post, h.awaitSettled(post, messageFrame(id)), second)
	if stored, err := h.messages.GetMessageByID("whatsapp:" + string(id)); err != nil || stored == nil || stored.Body != text {
		t.Fatalf("legacy store post %s = %+v, %v; want body %q", id, stored, err, text)
	}
}

// TestRetryReceiptRacingTeardownResendsOnTheNextConnection delivers a retry
// receipt (the peer could not decrypt a message we sent) and drops the
// connection after whatsmeow found the message and before it sent the
// resend. Before the fix whatsmeow acknowledged the retry receipt before the
// resend, the resend failed on the closed socket, and the peer never got the
// message ("Waiting for this message").
func TestRetryReceiptRacingTeardownResendsOnTheNextConnection(t *testing.T) {
	h := newAckHarness(t)
	h.awaitOnline(1)

	// The peer opens a session and we decrypt its message, so we have a
	// session to encrypt the resend with.
	h.startPeerSession()
	opening := h.peerMessage("3EB0PEEROPENS", "hello from the peer")
	h.server.deliver(opening)
	h.server.awaitReceived(t, "the receipt for the peer's message", ackFor(opening))

	const id = watypes.MessageID("3EB0OURRESEND")
	const text = "the message the peer could not decrypt"
	h.sentForRetry.Store(id, &waE2E.Message{Conversation: proto.String(text)})
	registration := make([]byte, 4)
	binary.BigEndian.PutUint32(registration, h.peer.RegistrationID)
	retry := &waBinary.Node{Tag: "receipt", Attrs: waBinary.Attrs{
		"from": h.peerJID,
		"id":   string(id),
		"t":    nowAttr(),
		"type": string(watypes.ReceiptTypeRetry),
	}, Content: []waBinary.Node{
		{Tag: "retry", Attrs: waBinary.Attrs{"count": "1", "id": string(id), "t": nowAttr(), "v": "1"}},
		{Tag: "registration", Content: registration},
	}}
	var once sync.Once
	hook := func(receipt *waevents.Receipt) bool {
		if slices.Contains(receipt.MessageIDs, id) {
			once.Do(func() {
				h.awaitAsyncAck(retry)
				h.dropAndRetire()
			})
		}
		return true
	}
	h.retryHook.Store(&hook)

	h.server.deliver(retry)
	h.logs.await(t, "the first resend to fail on the closed socket",
		"Failed to handle retry receipt for", string(id))

	h.awaitOnline(2)
	isResend := func(received fakeReceived) bool {
		return received.node.Tag == "message" && fakeAttr(received.node, "id") == string(id)
	}
	var resends []fakeReceived
	awaitTeardownCondition(t, "the resend or the retry receipt's acknowledgement", func() bool {
		resends = h.server.receivedNodes(isResend)
		return len(resends) > 0 || !h.server.pendingStanza("receipt", string(id), string(watypes.ReceiptTypeRetry))
	})
	if len(resends) == 0 {
		t.Fatalf("retry receipt for %s was acknowledged and the resend never went out; the server will not redeliver it", id)
	}
	if got := h.peerDecrypts(resends[0].node); got != text {
		t.Fatalf("peer decrypted the resend as %q, want %q", got, text)
	}
	ack := h.server.awaitReceived(t, "the retry receipt's acknowledgement", ackFor(retry))
	if ack.seq < resends[0].seq {
		t.Fatalf("retry receipt acknowledged (seq %d) before its resend went out (seq %d)", ack.seq, resends[0].seq)
	}
}

// peerDecrypts decrypts a message stanza we sent to the peer with the peer's
// Signal store and returns its text.
func (h *ackHarness) peerDecrypts(stanza *waBinary.Node) string {
	h.t.Helper()
	enc, ok := stanza.GetOptionalChildByTag("enc")
	if !ok {
		h.t.Fatalf("resend %s has no enc child", stanza.Attrs["id"])
	}
	content, _ := enc.Content.([]byte)
	builder := session.NewBuilderFromSignal(h.peer, h.ourJID.SignalAddress(), store.SignalProtobufSerializer)
	cipher := session.NewCipher(builder, h.ourJID.SignalAddress())
	var padded []byte
	var err error
	switch fakeAttr(&enc, "type") {
	case "msg":
		var message *protocol.SignalMessage
		message, err = protocol.NewSignalMessageFromBytes(content, store.SignalProtobufSerializer.SignalMessage)
		if err == nil {
			padded, err = cipher.Decrypt(context.Background(), message)
		}
	case "pkmsg":
		var message *protocol.PreKeySignalMessage
		message, err = protocol.NewPreKeySignalMessageFromBytes(content, store.SignalProtobufSerializer.PreKeySignalMessage, store.SignalProtobufSerializer.SignalMessage)
		if err == nil {
			padded, err = cipher.DecryptMessage(context.Background(), message)
		}
	default:
		h.t.Fatalf("resend enc type %q", fakeAttr(&enc, "type"))
	}
	if err != nil {
		h.t.Fatalf("peer could not decrypt the resend: %v", err)
	}
	if len(padded) == 0 || int(padded[len(padded)-1]) > len(padded) {
		h.t.Fatalf("resend plaintext has bad padding")
	}
	var message waE2E.Message
	if err := proto.Unmarshal(padded[:len(padded)-int(padded[len(padded)-1])], &message); err != nil {
		h.t.Fatalf("unmarshal resend: %v", err)
	}
	return message.GetConversation()
}
