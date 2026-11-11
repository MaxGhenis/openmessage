package whatsapplive_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/libsignal/ecc"
	"go.mau.fi/libsignal/groups"
	"go.mau.fi/libsignal/keys/identity"
	"go.mau.fi/libsignal/keys/prekey"
	"go.mau.fi/libsignal/protocol"
	"go.mau.fi/libsignal/session"
	"go.mau.fi/libsignal/util/optional"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waServerSync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/keys"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
	"modernc.org/sqlite"

	"github.com/maxghenis/openmessage/internal/bridge"
	whatsappadapter "github.com/maxghenis/openmessage/internal/bridgeadapters/whatsapp"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// These tests deliver real Signal ciphertext, encrypted by a second real
// Signal store, through whatsmeow's real message handler
// (HandleEncryptedMessage: the handler whatsmeow's handler queue runs with the
// context given to ConnectContext), the real bridge, lifecycle adapter and
// supervisor. Only the network dial is stubbed. At one point inside the
// handler the test retires the generation the way a dropped socket does
// (whatsmeow dispatches Disconnected), and holds the next generation back
// until the handler returns. It then redelivers the same stanza to the next
// generation, as WhatsApp's server does for a message whose receipt it never
// got, and checks the message reached v2 ingest.
//
// Invariant: a teardown at any point of a message's handling never loses the
// message. Every frame committed for it carries one dedupe key per message
// part (the inbox keeps one row per key), and the legacy store holds it.
//
// The message tests run twice: with the handler context the bridge hands
// whatsmeow (detached from teardown once the connection is up), and with a
// handler context that teardown cancels, which is how main behaved and what a
// teardown that overlaps ConnectContext returning still does.

// teardownTestTimeout bounds every wait; passing runs never approach it, but a
// heavily loaded machine under -race can stall the supervisor for seconds.
const teardownTestTimeout = 30 * time.Second

// teardownClock is a real clock whose short timers (the supervisor's backoff;
// every other supervisor timer in these tests is at least a minute) wait for
// an open gate, so a test can keep the next generation from starting.
type teardownClock struct{ gate *backoffGate }

type backoffGate struct {
	mu   sync.Mutex
	open chan struct{} // closed while the gate is open
}

func newBackoffGate() *backoffGate {
	open := make(chan struct{})
	close(open)
	return &backoffGate{open: open}
}

func (g *backoffGate) wait() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

func (g *backoffGate) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.open:
		g.open = make(chan struct{})
	default:
	}
}

func (g *backoffGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.open:
	default:
		close(g.open)
	}
}

func (c teardownClock) Now() time.Time { return time.Now() }

func (c teardownClock) NewTimer(delay time.Duration) bridge.Timer {
	if delay >= 30*time.Second {
		return teardownTimer{timer: time.NewTimer(delay)}
	}
	timer := &gatedTimer{fired: make(chan time.Time, 1), stop: make(chan struct{})}
	go func() {
		select {
		case <-time.After(delay):
		case <-timer.stop:
			return
		}
		select {
		case <-c.gate.wait():
		case <-timer.stop:
			return
		}
		if timer.done.CompareAndSwap(false, true) {
			timer.fired <- time.Now()
		}
	}()
	return timer
}

type teardownTimer struct{ timer *time.Timer }

func (t teardownTimer) C() <-chan time.Time { return t.timer.C }
func (t teardownTimer) Stop() bool          { return t.timer.Stop() }

type gatedTimer struct {
	fired    chan time.Time
	stop     chan struct{}
	stopOnce sync.Once
	done     atomic.Bool
}

func (t *gatedTimer) C() <-chan time.Time { return t.fired }

func (t *gatedTimer) Stop() bool {
	stopped := t.done.CompareAndSwap(false, true)
	t.stopOnce.Do(func() { close(t.stop) })
	return stopped
}

type teardownRandom struct{}

func (teardownRandom) Int63n(n int64) int64 { return n / 2 }

func awaitTeardownCondition(t *testing.T, description string, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(teardownTestTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Errorf("timed out waiting for %s", description)
			return false
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	return true
}

// teardownSink is the v2 ingest sink behind the supervisor's generation fence.
// It records every commit; the real inbox keeps one row per dedupe key.
type teardownSink struct {
	mu      sync.Mutex
	records []bridge.RawIngressRecord
}

func (s *teardownSink) AppendIngress(_ context.Context, record bridge.RawIngressRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	return nil
}

func (s *teardownSink) EmitEphemeral(context.Context, bridge.EphemeralEvent) error { return nil }

func (s *teardownSink) Beat(bridge.Generation, time.Time, string) {}

type committedText struct {
	generation bridge.Generation
	dedupeKey  string
}

// textFrames lists the committed message frames for one id whose message text
// is text.
func (s *teardownSink) textFrames(t *testing.T, id watypes.MessageID, text string) []committedText {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var frames []committedText
	for _, record := range s.records {
		if !strings.HasPrefix(record.DedupeKey, "msg:"+string(id)+":") {
			continue
		}
		var envelope struct {
			Kind     string `json:"kind"`
			ProtoB64 []byte `json:"proto_b64"`
		}
		if err := json.Unmarshal(record.Payload, &envelope); err != nil {
			t.Fatalf("decode ingress payload: %v", err)
		}
		var message waE2E.Message
		if err := proto.Unmarshal(envelope.ProtoB64, &message); err != nil {
			t.Fatalf("decode ingress message: %v", err)
		}
		if message.GetConversation() == text {
			frames = append(frames, committedText{generation: record.Generation, dedupeKey: record.DedupeKey})
		}
	}
	return frames
}

// teardownMode picks the handler context and the install shape a test runs
// with.
type teardownMode struct {
	// cancelOnTeardown makes retireGeneration cancel the handler context
	// awaitGeneration returned: main's behavior, and what a teardown that
	// overlaps ConnectContext returning still does.
	cancelOnTeardown bool
	// legacyOnly runs the supervisor with no v2 sink, the default install
	// (all v2 flags off): legacy messages.db is the only durable store.
	legacyOnly bool
}

func (m teardownMode) String() string {
	context := "handler context detached from teardown"
	if m.cancelOnTeardown {
		context = "handler context cancelled by teardown"
	}
	if m.legacyOnly {
		return context + ", legacy-only install"
	}
	return context + ", v2 ingest"
}

type teardownHarness struct {
	t                *testing.T
	cancelOnTeardown bool
	legacyOnly       bool
	cancelHandlers   context.CancelFunc
	host             *whatsapplive.Bridge
	cli              *whatsmeow.Client
	messages         *db.Store
	supervisor       *bridge.Supervisor
	sink             *teardownSink
	gate             *backoffGate
	dials            chan context.Context
	sessionPath      string
	ourJID           watypes.JID
	peerJID          watypes.JID
	peer             *store.Device
	groupJID         watypes.JID
}

func newTeardownHarness(t *testing.T, mode teardownMode) *teardownHarness {
	t.Helper()
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "whatsapp-session.db")
	ourJID := watypes.JID{User: "15551230000", Device: 7, Server: watypes.DefaultUserServer}
	whatsapplive.SeedDeviceForTeardownTest(t, sessionPath, ourJID)
	messages, err := db.New(filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	t.Cleanup(func() { _ = messages.Close() })
	logger := zerolog.Nop()
	if os.Getenv("OPENMESSAGE_TEARDOWN_TEST_LOG") != "" {
		logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
	}
	host, err := whatsapplive.New(sessionPath, messages, logger, whatsapplive.Callbacks{})
	if err != nil {
		t.Fatalf("whatsapplive.New(): %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })

	dials := make(chan context.Context, 8)
	whatsapplive.StubDialForTeardownTest(t, func(ctx context.Context, _ *whatsmeow.Client) error {
		dials <- ctx
		return nil
	})
	sink := &teardownSink{}
	gate := newBackoffGate()
	options := []bridge.SupervisorOption{}
	if !mode.legacyOnly {
		options = append(options, bridge.WithConnectionSink(sink))
	}
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
		options...,
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
	return &teardownHarness{
		t:                t,
		cancelOnTeardown: mode.cancelOnTeardown,
		legacyOnly:       mode.legacyOnly,
		host:             host,
		cli:              whatsapplive.ClientForTeardownTest(host),
		messages:         messages,
		supervisor:       supervisor,
		sink:             sink,
		gate:             gate,
		dials:            dials,
		sessionPath:      sessionPath,
		ourJID:           ourJID,
		peerJID:          peerJID,
		peer:             whatsapplive.NewPeerDeviceForTeardownTest(t, filepath.Join(dir, "peer-session.db"), peerJID),
		groupJID:         watypes.JID{User: "120363000000000001", Server: watypes.GroupServer},
	}
}

// awaitGeneration lets the next generation start, waits for the supervisor to
// dial, completes the login the way whatsmeow does (Connected), and returns
// the context whatsmeow runs this connection's node handlers with.
func (h *teardownHarness) awaitGeneration() (context.Context, bridge.Generation) {
	h.t.Helper()
	h.gate.release()
	var handlerCtx context.Context
	select {
	case handlerCtx = <-h.dials:
	case <-time.After(teardownTestTimeout):
		h.t.Fatal("the supervisor never dialed")
	}
	h.cli.DangerousInternals().DispatchEvent(&waevents.Connected{})
	if !awaitTeardownCondition(h.t, "the generation to come online", func() bool {
		return h.supervisor.Snapshot().State == bridge.StateOnline
	}) {
		h.t.FailNow()
	}
	if h.cancelOnTeardown {
		handlerCtx, h.cancelHandlers = context.WithCancel(handlerCtx)
	}
	return handlerCtx, h.supervisor.Snapshot().Generation
}

// retireGeneration ends the online generation the way a dropped socket does
// (whatsmeow dispatches Disconnected) and waits until the supervisor has
// joined it. The next generation stays held until awaitGeneration.
func (h *teardownHarness) retireGeneration() {
	h.gate.hold()
	h.cli.DangerousInternals().DispatchEvent(&waevents.Disconnected{})
	awaitTeardownCondition(h.t, "the generation to retire", func() bool {
		return h.supervisor.Snapshot().State == bridge.StateBackoff
	})
	if h.cancelHandlers != nil {
		h.cancelHandlers()
		h.cancelHandlers = nil
	}
}

// forEachTeardownMode runs a message test with the handler context the bridge
// hands whatsmeow and with one that teardown cancels, on a v2-ingest install
// and on a legacy-only one.
func forEachTeardownMode(t *testing.T, test func(t *testing.T, mode teardownMode)) {
	for _, legacyOnly := range []bool{false, true} {
		for _, cancelOnTeardown := range []bool{false, true} {
			mode := teardownMode{cancelOnTeardown: cancelOnTeardown, legacyOnly: legacyOnly}
			t.Run(mode.String(), func(t *testing.T) { test(t, mode) })
		}
	}
}

// startPeerSession builds the peer's outgoing session from a prekey bundle of
// our device, as the peer's client does after fetching our keys.
func (h *teardownHarness) startPeerSession() {
	h.t.Helper()
	ctx := context.Background()
	our := h.cli.Store
	oneTime, err := our.PreKeys.GenOnePreKey(ctx)
	if err != nil {
		h.t.Fatalf("GenOnePreKey(): %v", err)
	}
	bundle := prekey.NewBundle(
		our.RegistrationID, uint32(h.ourJID.Device),
		optional.NewOptionalUint32(oneTime.KeyID), our.SignedPreKey.KeyID,
		ecc.NewDjbECPublicKey(*oneTime.Pub), ecc.NewDjbECPublicKey(*our.SignedPreKey.Pub), *our.SignedPreKey.Signature,
		identity.NewKey(ecc.NewDjbECPublicKey(*our.IdentityKey.Pub)),
	)
	builder := session.NewBuilderFromSignal(h.peer, h.ourJID.SignalAddress(), store.SignalProtobufSerializer)
	if err := builder.ProcessBundle(ctx, bundle); err != nil {
		h.t.Fatalf("peer ProcessBundle(): %v", err)
	}
}

func padForTeardownTest(plaintext []byte) []byte {
	return append(plaintext, bytes.Repeat([]byte{5}, 5)...)
}

func marshalForTeardownTest(t *testing.T, message *waE2E.Message) []byte {
	t.Helper()
	plaintext, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	return padForTeardownTest(plaintext)
}

// encryptToUs encrypts one Signal message from the peer to our device.
func (h *teardownHarness) encryptToUs(message *waE2E.Message) waBinary.Node {
	h.t.Helper()
	builder := session.NewBuilderFromSignal(h.peer, h.ourJID.SignalAddress(), store.SignalProtobufSerializer)
	ciphertext, err := session.NewCipher(builder, h.ourJID.SignalAddress()).Encrypt(context.Background(), marshalForTeardownTest(h.t, message))
	if err != nil {
		h.t.Fatalf("peer Encrypt(): %v", err)
	}
	encType := "msg"
	if ciphertext.Type() == protocol.PREKEY_TYPE {
		encType = "pkmsg"
	}
	return waBinary.Node{Tag: "enc", Attrs: waBinary.Attrs{"v": "2", "type": encType}, Content: ciphertext.Serialize()}
}

func withSecret(text string) *waE2E.Message {
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return &waE2E.Message{
		Conversation:       proto.String(text),
		MessageContextInfo: &waE2E.MessageContextInfo{MessageSecret: secret},
	}
}

func stanza(attrs waBinary.Attrs, id watypes.MessageID, children ...waBinary.Node) *waBinary.Node {
	attrs["id"] = string(id)
	attrs["t"] = strconv.FormatInt(time.Now().Unix(), 10)
	attrs["type"] = "text"
	return &waBinary.Node{Tag: "message", Attrs: attrs, Content: children}
}

// peerMessage is a one-to-one text message from the peer.
func (h *teardownHarness) peerMessage(id watypes.MessageID, text string) *waBinary.Node {
	h.t.Helper()
	return stanza(waBinary.Attrs{"from": h.peerJID}, id, h.encryptToUs(withSecret(text)))
}

// peerGroupMessage is the peer's first message in a group: its sender key
// distribution message encrypted to our device, and the text encrypted with
// that sender key, in one stanza (as WhatsApp's server delivers it).
func (h *teardownHarness) peerGroupMessage(id watypes.MessageID, text string) *waBinary.Node {
	h.t.Helper()
	ctx := context.Background()
	senderKeyName := protocol.NewSenderKeyName(h.groupJID.String(), h.peerJID.SignalAddress())
	builder := groups.NewGroupSessionBuilder(h.peer, store.SignalProtobufSerializer)
	distribution, err := builder.Create(ctx, senderKeyName)
	if err != nil {
		h.t.Fatalf("peer sender key Create(): %v", err)
	}
	direct := h.encryptToUs(&waE2E.Message{
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{
			GroupID:                             proto.String(h.groupJID.String()),
			AxolotlSenderKeyDistributionMessage: distribution.Serialize(),
		},
	})
	encrypted, err := groups.NewGroupCipher(builder, senderKeyName, h.peer).Encrypt(ctx, marshalForTeardownTest(h.t, withSecret(text)))
	if err != nil {
		h.t.Fatalf("peer group Encrypt(): %v", err)
	}
	group := waBinary.Node{Tag: "enc", Attrs: waBinary.Attrs{"v": "2", "type": "skmsg"}, Content: encrypted.SignedSerialize()}
	return stanza(waBinary.Attrs{"from": h.groupJID, "participant": h.peerJID}, id, direct, group)
}

// replyToPeer sends one message from our device back to the peer, which the
// peer decrypts. After it, the peer's messages are plain "msg" stanzas instead
// of prekey messages.
func (h *teardownHarness) replyToPeer() {
	h.t.Helper()
	ctx := context.Background()
	ours := session.NewBuilderFromSignal(h.cli.Store, h.peerJID.SignalAddress(), store.SignalProtobufSerializer)
	ciphertext, err := session.NewCipher(ours, h.peerJID.SignalAddress()).Encrypt(ctx, marshalForTeardownTest(h.t, &waE2E.Message{Conversation: proto.String("reply")}))
	if err != nil {
		h.t.Fatalf("our Encrypt(): %v", err)
	}
	if ciphertext.Type() != protocol.WHISPER_TYPE {
		h.t.Fatalf("reply type = %d, want a plain Signal message", ciphertext.Type())
	}
	signalMessage, err := protocol.NewSignalMessageFromBytes(ciphertext.Serialize(), store.SignalProtobufSerializer.SignalMessage)
	if err != nil {
		h.t.Fatalf("parse reply: %v", err)
	}
	theirs := session.NewBuilderFromSignal(h.peer, h.ourJID.SignalAddress(), store.SignalProtobufSerializer)
	if _, err := session.NewCipher(theirs, h.ourJID.SignalAddress()).Decrypt(ctx, signalMessage); err != nil {
		h.t.Fatalf("peer Decrypt(reply): %v", err)
	}
}

// deliver runs whatsmeow's real handler for one message stanza with the given
// handler context, as whatsmeow's handler queue does.
func (h *teardownHarness) deliver(handlerCtx context.Context, node *waBinary.Node) {
	h.cli.DangerousInternals().HandleEncryptedMessage(handlerCtx, node)
}

// requireDelivered checks the invariant for one message part: the legacy
// store holds it, and on a v2-ingest install it reached v2 ingest with one
// dedupe key across every commit. It returns the generations that committed
// it (none on a legacy-only install).
func (h *teardownHarness) requireDelivered(id watypes.MessageID, text, why string) []bridge.Generation {
	h.t.Helper()
	if stored, err := h.messages.GetMessageByID("whatsapp:" + string(id)); err != nil || stored == nil || stored.Body != text {
		h.t.Fatalf("legacy store message %s = %+v, %v; want body %q (%s)", id, stored, err, text, why)
	}
	if h.legacyOnly {
		return nil
	}
	frames := h.sink.textFrames(h.t, id, text)
	if len(frames) == 0 {
		h.t.Fatalf("message %s never reached v2 ingest (%s)", id, why)
	}
	generations := make([]bridge.Generation, 0, len(frames))
	for _, frame := range frames {
		if frame.dedupeKey != frames[0].dedupeKey {
			h.t.Fatalf("message %s committed under dedupe keys %q and %q", id, frames[0].dedupeKey, frame.dedupeKey)
		}
		generations = append(generations, frame.generation)
	}
	return generations
}

// requireCommittedOnlyIn checks that every commit of a message part came from
// one generation.
func requireCommittedOnlyIn(t *testing.T, generations []bridge.Generation, want bridge.Generation) {
	t.Helper()
	for _, generation := range generations {
		if generation != want {
			t.Fatalf("message committed by generations %v, want only %d", generations, want)
		}
	}
}

type preKeyStoreHook struct {
	store.PreKeyStore
	beforeRemove func(context.Context)
}

func (h *preKeyStoreHook) RemovePreKey(ctx context.Context, id uint32) error {
	if h.beforeRemove != nil {
		h.beforeRemove(ctx)
	}
	return h.PreKeyStore.RemovePreKey(ctx, id)
}

type msgSecretStoreHook struct {
	store.MsgSecretStore
	beforePut func(context.Context)
}

func (h *msgSecretStoreHook) PutMessageSecret(ctx context.Context, chat, sender watypes.JID, id watypes.MessageID, secret []byte) error {
	if h.beforePut != nil {
		h.beforePut(ctx)
	}
	return h.MsgSecretStore.PutMessageSecret(ctx, chat, sender, id, secret)
}

// TestTeardownDuringPreKeyDecryptDoesNotLoseTheMessage retires the generation
// while libsignal is inside a prekey message's decrypt: after it has stored the
// advanced session and before it removes the one-time prekey. Before the fix,
// the cancelled removal failed the decrypt after the session advanced, and the
// redelivery failed with "received message with old counter", which whatsmeow
// ignores and acknowledges.
func TestTeardownDuringPreKeyDecryptDoesNotLoseTheMessage(t *testing.T) {
	forEachTeardownMode(t, func(t *testing.T, mode teardownMode) {
		h := newTeardownHarness(t, mode)
		first, _ := h.awaitGeneration()
		h.startPeerSession()
		node := h.peerMessage("3EB0PREKEYTEARDOWN", "first message from a new session")
		if typ := node.GetChildren()[0].Attrs["type"]; typ != "pkmsg" {
			t.Fatalf("first message type = %v, want pkmsg", typ)
		}

		var once sync.Once
		h.cli.Store.PreKeys = &preKeyStoreHook{
			PreKeyStore:  h.cli.Store.PreKeys,
			beforeRemove: func(context.Context) { once.Do(h.retireGeneration) },
		}
		h.deliver(first, node)

		second, generation := h.awaitGeneration()
		h.deliver(second, node)
		requireCommittedOnlyIn(t, h.requireDelivered("3EB0PREKEYTEARDOWN", "first message from a new session",
			"teardown landed between the session store and the prekey removal"), generation)
	})
}

// TestTeardownBeforeDispatchDoesNotLoseTheMessage retires the generation after
// the message decrypted and before whatsmeow dispatched it to the bridge (while
// whatsmeow stores the message secret). Before the fix, the bridge dropped the
// event because no generation was observing ingress, and whatsmeow, told the
// event was handled, never saw it again.
func TestTeardownBeforeDispatchDoesNotLoseTheMessage(t *testing.T) {
	forEachTeardownMode(t, func(t *testing.T, mode teardownMode) {
		h := newTeardownHarness(t, mode)
		first, _ := h.awaitGeneration()
		h.startPeerSession()
		node := h.peerMessage("3EB0DISPATCHTEARDOWN", "arrives while the generation retires")

		var once sync.Once
		h.cli.Store.MsgSecrets = &msgSecretStoreHook{
			MsgSecretStore: h.cli.Store.MsgSecrets,
			beforePut:      func(context.Context) { once.Do(h.retireGeneration) },
		}
		h.deliver(first, node)

		second, generation := h.awaitGeneration()
		h.deliver(second, node)
		requireCommittedOnlyIn(t, h.requireDelivered("3EB0DISPATCHTEARDOWN", "arrives while the generation retires",
			"decrypted, then dispatched after the generation's ingress closed"), generation)
	})
}

// TestTeardownBeforeDispatchAcknowledgesOnlyWhatV2Holds pins what whatsmeow
// is told for a message dispatched after the generation retired. On a
// v2-ingest install it is not handled: whatsmeow keeps the plaintext (and
// sends no receipt) for the next generation. On a legacy-only install there
// is no v2 to wait for: it is handled at once, whatsmeow clears the plaintext
// and acknowledges it as on main, and the legacy store already holds it.
func TestTeardownBeforeDispatchAcknowledgesOnlyWhatV2Holds(t *testing.T) {
	for _, mode := range []teardownMode{{}, {legacyOnly: true}} {
		t.Run(mode.String(), func(t *testing.T) {
			h := newTeardownHarness(t, mode)
			first, _ := h.awaitGeneration()
			h.startPeerSession()
			node := h.peerMessage("3EB0ACKTEARDOWN", "arrives as the generation retires")
			var once sync.Once
			h.cli.Store.MsgSecrets = &msgSecretStoreHook{
				MsgSecretStore: h.cli.Store.MsgSecrets,
				beforePut:      func(context.Context) { once.Do(h.retireGeneration) },
			}
			h.deliver(first, node)

			if stored, err := h.messages.GetMessageByID("whatsapp:3EB0ACKTEARDOWN"); err != nil || stored == nil {
				t.Fatalf("legacy store after the first delivery = %+v, %v; want the message", stored, err)
			}
			retained := whatsapplive.RetainedPlaintextsForTeardownTest(t, h.sessionPath)
			if mode.legacyOnly && retained != 0 {
				t.Fatalf("legacy-only install: whatsmeow kept %d plaintexts, want the message handled and cleared", retained)
			}
			if !mode.legacyOnly && retained != 1 {
				t.Fatalf("v2 install: whatsmeow kept %d plaintexts, want the uncommitted message kept for redelivery", retained)
			}

			second, _ := h.awaitGeneration()
			h.deliver(second, node)
			h.requireDelivered("3EB0ACKTEARDOWN", "arrives as the generation retires", "redelivered to the next generation")
			if retained := whatsapplive.RetainedPlaintextsForTeardownTest(t, h.sessionPath); retained != 0 {
				t.Fatalf("whatsmeow still holds %d plaintexts after the redelivery was handled", retained)
			}
		})
	}
}

// commitHook lets one test act inside SQLite's commit on a session store
// connection, through modernc's real driver.
var (
	commitHooksOnce sync.Once
	commitHook      atomic.Pointer[func(dsn string)]
)

func installCommitHooks() {
	commitHooksOnce.Do(func() {
		sqlite.RegisterConnectionHook(func(conn sqlite.ExecQuerierContext, dsn string) error {
			hookable, ok := conn.(interface{ RegisterCommitHook(sqlite.CommitHookFn) })
			if !ok {
				return nil
			}
			hookable.RegisterCommitHook(func() int32 {
				if hook := commitHook.Load(); hook != nil {
					(*hook)(dsn)
				}
				return 0
			})
			return nil
		})
	})
}

type sessionStoreHook struct {
	store.SessionStore
	onPut func(ctx context.Context, address string)
}

func (h *sessionStoreHook) PutSession(ctx context.Context, address string, raw []byte) error {
	if h.onPut != nil {
		h.onPut(ctx, address)
	}
	return h.SessionStore.PutSession(ctx, address, raw)
}

// TestTeardownWhileTheSessionCommitsDoesNotLoseTheMessage retires the
// generation while SQLite commits the advanced Signal session of an ordinary
// ("msg") message. modernc.org/sqlite reports ctx.Err() for a statement whose
// context ended while it ran even when the statement committed (stmt.exec's
// deferred done check), so before the fix libsignal saw a failed store after
// the ratchet had moved, and the redelivery failed with an old counter.
func TestTeardownWhileTheSessionCommitsDoesNotLoseTheMessage(t *testing.T) {
	forEachTeardownMode(t, func(t *testing.T, mode teardownMode) {
		installCommitHooks()
		h := newTeardownHarness(t, mode)
		first, _ := h.awaitGeneration()
		h.startPeerSession()
		h.deliver(first, h.peerMessage("3EB0SESSIONSETUP", "opens the session"))
		h.requireDelivered("3EB0SESSIONSETUP", "opens the session", "setup message")
		h.replyToPeer()
		node := h.peerMessage("3EB0COMMITTEARDOWN", "an ordinary message")
		if typ := node.GetChildren()[0].Attrs["type"]; typ != "msg" {
			t.Fatalf("message type = %v, want msg", typ)
		}

		peerAddress := h.peerJID.SignalAddress().String()
		var armed atomic.Bool
		var writeCtx atomic.Pointer[context.Context]
		h.cli.Store.Sessions = &sessionStoreHook{
			SessionStore: h.cli.Store.Sessions,
			onPut: func(ctx context.Context, address string) {
				if address == peerAddress {
					writeCtx.Store(&ctx)
					armed.Store(true)
				}
			},
		}
		fired := false
		hook := func(dsn string) {
			// The commit that carries the session write: the statement's own
			// autocommit, or the decryption transaction's commit.
			if !strings.Contains(dsn, "whatsapp-session.db") || !armed.CompareAndSwap(true, false) {
				return
			}
			fired = true
			h.retireGeneration()
			if ctx := *writeCtx.Load(); ctx.Err() != nil {
				// Let modernc's interruptOnDone goroutine observe the cancellation
				// while the commit is still in progress.
				time.Sleep(50 * time.Millisecond)
			}
		}
		commitHook.Store(&hook)
		t.Cleanup(func() { commitHook.Store(nil) })

		h.deliver(first, node)
		commitHook.Store(nil)
		if !fired {
			t.Fatal("the commit hook never saw the session write commit")
		}

		second, generation := h.awaitGeneration()
		h.deliver(second, node)
		requireCommittedOnlyIn(t, h.requireDelivered("3EB0COMMITTEARDOWN", "an ordinary message",
			"the session committed but the store reported the cancellation"), generation)
	})
}

// cutPoints wraps the session-store calls whatsmeow's message handler makes
// and retires the generation once, just before the cutAt-th call.
type cutPoints struct {
	mu    sync.Mutex
	armed bool
	calls []string
	cutAt int
	onCut func()
}

func (c *cutPoints) reach(name string) {
	c.mu.Lock()
	if !c.armed {
		c.mu.Unlock()
		return
	}
	index := len(c.calls)
	c.calls = append(c.calls, name)
	cut := index == c.cutAt
	c.mu.Unlock()
	if cut {
		c.onCut()
	}
}

func (c *cutPoints) arm(cutAt int, onCut func()) {
	c.mu.Lock()
	c.armed, c.calls, c.cutAt, c.onCut = true, nil, cutAt, onCut
	c.mu.Unlock()
}

func (c *cutPoints) disarm() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = false
	return append([]string(nil), c.calls...)
}

func (c *cutPoints) install(device *store.Device) {
	device.Identities = cutIdentities{device.Identities, c}
	device.Sessions = cutSessions{device.Sessions, c}
	device.PreKeys = cutPreKeys{device.PreKeys, c}
	device.SenderKeys = cutSenderKeys{device.SenderKeys, c}
	device.MsgSecrets = cutMsgSecrets{device.MsgSecrets, c}
	device.EventBuffer = cutEventBuffer{device.EventBuffer, c}
	device.LIDs = cutLIDs{device.LIDs, c}
}

type cutIdentities struct {
	store.IdentityStore
	c *cutPoints
}

func (s cutIdentities) PutIdentity(ctx context.Context, address string, key [32]byte) error {
	s.c.reach("Identities.PutIdentity")
	return s.IdentityStore.PutIdentity(ctx, address, key)
}

func (s cutIdentities) IsTrustedIdentity(ctx context.Context, address string, key [32]byte) (bool, error) {
	s.c.reach("Identities.IsTrustedIdentity")
	return s.IdentityStore.IsTrustedIdentity(ctx, address, key)
}

type cutSessions struct {
	store.SessionStore
	c *cutPoints
}

func (s cutSessions) GetSession(ctx context.Context, address string) ([]byte, error) {
	s.c.reach("Sessions.GetSession")
	return s.SessionStore.GetSession(ctx, address)
}

func (s cutSessions) HasSession(ctx context.Context, address string) (bool, error) {
	s.c.reach("Sessions.HasSession")
	return s.SessionStore.HasSession(ctx, address)
}

func (s cutSessions) PutSession(ctx context.Context, address string, raw []byte) error {
	s.c.reach("Sessions.PutSession")
	return s.SessionStore.PutSession(ctx, address, raw)
}

type cutPreKeys struct {
	store.PreKeyStore
	c *cutPoints
}

func (s cutPreKeys) GetPreKey(ctx context.Context, id uint32) (*keys.PreKey, error) {
	s.c.reach("PreKeys.GetPreKey")
	return s.PreKeyStore.GetPreKey(ctx, id)
}

func (s cutPreKeys) RemovePreKey(ctx context.Context, id uint32) error {
	s.c.reach("PreKeys.RemovePreKey")
	return s.PreKeyStore.RemovePreKey(ctx, id)
}

type cutSenderKeys struct {
	store.SenderKeyStore
	c *cutPoints
}

func (s cutSenderKeys) PutSenderKey(ctx context.Context, group, user string, raw []byte) error {
	s.c.reach("SenderKeys.PutSenderKey")
	return s.SenderKeyStore.PutSenderKey(ctx, group, user, raw)
}

func (s cutSenderKeys) GetSenderKey(ctx context.Context, group, user string) ([]byte, error) {
	s.c.reach("SenderKeys.GetSenderKey")
	return s.SenderKeyStore.GetSenderKey(ctx, group, user)
}

type cutMsgSecrets struct {
	store.MsgSecretStore
	c *cutPoints
}

func (s cutMsgSecrets) PutMessageSecret(ctx context.Context, chat, sender watypes.JID, id watypes.MessageID, secret []byte) error {
	s.c.reach("MsgSecrets.PutMessageSecret")
	return s.MsgSecretStore.PutMessageSecret(ctx, chat, sender, id, secret)
}

type cutEventBuffer struct {
	store.EventBuffer
	c *cutPoints
}

func (s cutEventBuffer) GetBufferedEvent(ctx context.Context, hash [32]byte) (*store.BufferedEvent, error) {
	s.c.reach("EventBuffer.GetBufferedEvent")
	return s.EventBuffer.GetBufferedEvent(ctx, hash)
}

func (s cutEventBuffer) PutBufferedEvent(ctx context.Context, hash [32]byte, plaintext []byte, serverTimestamp time.Time) error {
	s.c.reach("EventBuffer.PutBufferedEvent")
	return s.EventBuffer.PutBufferedEvent(ctx, hash, plaintext, serverTimestamp)
}

func (s cutEventBuffer) DoDecryptionTxn(ctx context.Context, fn func(context.Context) error) error {
	s.c.reach("EventBuffer.DoDecryptionTxn")
	return s.EventBuffer.DoDecryptionTxn(ctx, fn)
}

func (s cutEventBuffer) ClearBufferedEventPlaintext(ctx context.Context, hash [32]byte) error {
	s.c.reach("EventBuffer.ClearBufferedEventPlaintext")
	return s.EventBuffer.ClearBufferedEventPlaintext(ctx, hash)
}

type cutLIDs struct {
	store.LIDStore
	c *cutPoints
}

func (s cutLIDs) GetLIDForPN(ctx context.Context, pn watypes.JID) (watypes.JID, error) {
	s.c.reach("LIDs.GetLIDForPN")
	return s.LIDStore.GetLIDForPN(ctx, pn)
}

// teardownScenario prepares one message in the first generation and returns
// its stanza and the text each of its parts carries.
type teardownScenario struct {
	name    string
	prepare func(h *teardownHarness, first context.Context) (*waBinary.Node, watypes.MessageID, string)
}

var teardownScenarios = []teardownScenario{
	{
		name: "prekey message from a new session",
		prepare: func(h *teardownHarness, _ context.Context) (*waBinary.Node, watypes.MessageID, string) {
			h.startPeerSession()
			return h.peerMessage("3EB0CUTPKMSG", "prekey text"), "3EB0CUTPKMSG", "prekey text"
		},
	},
	{
		name: "ordinary message on an established session",
		prepare: func(h *teardownHarness, first context.Context) (*waBinary.Node, watypes.MessageID, string) {
			h.startPeerSession()
			h.deliver(first, h.peerMessage("3EB0CUTSETUP", "setup"))
			h.requireDelivered("3EB0CUTSETUP", "setup", "setup message")
			h.replyToPeer()
			return h.peerMessage("3EB0CUTMSG", "ordinary text"), "3EB0CUTMSG", "ordinary text"
		},
	},
	{
		name: "first group message with its sender key",
		prepare: func(h *teardownHarness, _ context.Context) (*waBinary.Node, watypes.MessageID, string) {
			h.startPeerSession()
			return h.peerGroupMessage("3EB0CUTSKMSG", "group text"), "3EB0CUTSKMSG", "group text"
		},
	},
}

// TestEveryTeardownPointDeliversTheMessage checks the invariant exhaustively
// at store-call granularity: for each scenario it records the session-store
// calls of an undisturbed delivery, then for every one of them retires the
// generation just before that call, redelivers the stanza to the next
// generation, and requires the message to arrive.
func TestEveryTeardownPointDeliversTheMessage(t *testing.T) {
	forEachTeardownMode(t, func(t *testing.T, mode teardownMode) {
		testEveryTeardownPoint(t, mode)
	})
}

func testEveryTeardownPoint(t *testing.T, mode teardownMode) {
	for _, scenario := range teardownScenarios {
		t.Run(scenario.name, func(t *testing.T) {
			h := newTeardownHarness(t, mode)
			first, _ := h.awaitGeneration()
			node, id, text := scenario.prepare(h, first)
			points := &cutPoints{}
			points.install(h.cli.Store)
			points.arm(-1, nil)
			h.deliver(first, node)
			calls := points.disarm()
			h.requireDelivered(id, text, "undisturbed delivery")
			if len(calls) == 0 {
				t.Fatal("the handler made no session-store calls")
			}
			t.Logf("%d cut points: %s", len(calls), strings.Join(calls, ", "))

			for cutAt, call := range calls {
				t.Run(fmt.Sprintf("%02d_before_%s", cutAt, call), func(t *testing.T) {
					h := newTeardownHarness(t, mode)
					first, _ := h.awaitGeneration()
					node, id, text := scenario.prepare(h, first)
					points := &cutPoints{}
					points.install(h.cli.Store)
					cut := false
					points.arm(cutAt, func() {
						cut = true
						h.retireGeneration()
					})
					h.deliver(first, node)
					points.disarm()
					if !cut {
						t.Fatalf("the handler never reached cut point %d (%s)", cutAt, call)
					}
					second, _ := h.awaitGeneration()
					h.deliver(second, node)
					h.requireDelivered(id, text, fmt.Sprintf("generation retired before %s", call))
				})
			}
		})
	}
}

type appStateStoreHook struct {
	store.AppStateStore
	afterPutVersion func()
}

func (h *appStateStoreHook) PutAppStateVersion(ctx context.Context, name string, version uint64, hash [128]byte) error {
	err := h.AppStateStore.PutAppStateVersion(ctx, name, version, hash)
	if err == nil && h.afterPutVersion != nil {
		h.afterPutVersion()
	}
	return err
}

// appStatePhone encodes the contact-list app-state collection the way the
// primary phone does, with its own copy of the collection in the peer's store.
type appStatePhone struct {
	proc  *appstate.Processor
	state appstate.HashState
	keyID []byte
}

func (h *teardownHarness) newAppStatePhone() *appStatePhone {
	h.t.Helper()
	ctx := context.Background()
	keyID := []byte{0, 0, 0x2a, 1}
	keyData := make([]byte, 32)
	_, _ = rand.Read(keyData)
	key := store.AppStateSyncKey{Data: keyData, Fingerprint: []byte{}, Timestamp: time.Now().UnixMilli()}
	for _, device := range []*store.Device{h.cli.Store, h.peer} {
		if err := device.AppStateKeys.PutAppStateSyncKey(ctx, keyID, key); err != nil {
			h.t.Fatalf("PutAppStateSyncKey(): %v", err)
		}
	}
	return &appStatePhone{proc: appstate.NewProcessor(h.peer, waLog.Noop), keyID: keyID}
}

// renameContact returns the server's patch list for one contact rename.
func (p *appStatePhone) renameContact(t *testing.T, contact watypes.JID, name string) *appstate.PatchList {
	t.Helper()
	ctx := context.Background()
	raw, err := p.proc.EncodePatch(ctx, p.keyID, p.state, appstate.PatchInfo{
		Type: appstate.WAPatchCriticalUnblockLow,
		Mutations: []appstate.MutationInfo{{
			Index:   []string{appstate.IndexContact, contact.String()},
			Version: 2,
			Value: &waSyncAction.SyncActionValue{ContactAction: &waSyncAction.ContactAction{
				FullName:  proto.String(name),
				FirstName: proto.String(name),
			}},
		}},
	})
	if err != nil {
		t.Fatalf("EncodePatch(): %v", err)
	}
	patchList := func() *appstate.PatchList {
		var patch waServerSync.SyncdPatch
		if err := proto.Unmarshal(raw, &patch); err != nil {
			t.Fatalf("unmarshal patch: %v", err)
		}
		patch.Version = &waServerSync.SyncdVersion{Version: proto.Uint64(p.state.Version + 1)}
		return &appstate.PatchList{Name: appstate.WAPatchCriticalUnblockLow, Patches: []*waServerSync.SyncdPatch{&patch}}
	}
	_, next, err := p.proc.DecodePatches(ctx, patchList(), p.state, true)
	if err != nil {
		t.Fatalf("the phone could not apply its own patch: %v", err)
	}
	served := patchList()
	p.state = next
	return served
}

// syncAppState applies one fetched patch list the way whatsmeow's
// fetchAppState does (appstate.go applyAppStatePatches): decode it against
// the stored state, which stores the new version and value MACs, then apply
// each mutation to the store (dispatchAppState).
func (h *teardownHarness) syncAppState(ctx context.Context, list *appstate.PatchList) error {
	version, hash, err := h.cli.Store.AppState.GetAppStateVersion(ctx, string(list.Name))
	if err != nil {
		return err
	}
	proc := appstate.NewProcessor(h.cli.Store, waLog.Noop)
	mutations, _, err := proc.DecodePatches(ctx, list, appstate.HashState{Version: version, Hash: hash}, true)
	if err != nil {
		return err
	}
	for _, mutation := range mutations {
		h.cli.DangerousInternals().DispatchAppState(ctx, list.Name, mutation, false)
	}
	return nil
}

// TestTeardownDuringAppStateSyncKeepsTheCollectionUsable retires the
// generation while whatsmeow stores an app-state patch: after it has written
// the collection's new version and before it writes the patch's value MACs
// (appstate/decode.go storeMACs). Before the handler context was detached from
// teardown, the cancelled MAC writes left the version advanced without its
// MACs: the patch's contact rename was never applied, and every later patch
// touching that contact failed with a mismatching LTHash, so the contact list
// (whatsmeow's contact store, which OpenMessage reads for display names)
// stopped syncing.
func TestTeardownDuringAppStateSyncKeepsTheCollectionUsable(t *testing.T) {
	h := newTeardownHarness(t, teardownMode{})
	first, _ := h.awaitGeneration()
	phone := h.newAppStatePhone()
	contact := watypes.JID{User: "15550001111", Server: watypes.DefaultUserServer}
	renamed := phone.renameContact(t, contact, "Alice Phone")

	var once sync.Once
	h.cli.Store.AppState = &appStateStoreHook{
		AppStateStore:   h.cli.Store.AppState,
		afterPutVersion: func() { once.Do(h.retireGeneration) },
	}
	firstErr := h.syncAppState(first, renamed)

	second, _ := h.awaitGeneration()
	if err := h.syncAppState(second, phone.renameContact(t, contact, "Alice Renamed")); err != nil {
		t.Fatalf("the next contact-list patch failed after a teardown during the previous one (that sync: %v): %v", firstErr, err)
	}
	info, err := h.cli.Store.Contacts.GetContact(context.Background(), contact)
	if err != nil {
		t.Fatalf("GetContact(): %v", err)
	}
	if info.FullName != "Alice Renamed" {
		t.Fatalf("contact name = %q, want the phone's latest rename (first sync: %v)", info.FullName, firstErr)
	}
}
