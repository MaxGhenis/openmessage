package whatsapplive_test

import (
	"context"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.mau.fi/libsignal/ecc"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCert"
	"go.mau.fi/whatsmeow/proto/waWa6"
	"go.mau.fi/whatsmeow/socket"
	watypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/util/gcmutil"
	"go.mau.fi/whatsmeow/util/keys"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
	"google.golang.org/protobuf/proto"
)

// fakeWhatsApp is an in-process WhatsApp web server. whatsmeow dials it for
// real (through httpClient, whose connections to wss://web.whatsapp.com reach
// this server), completes the real Noise_XX handshake against a test
// certificate chain (whatsmeow.WACertPubKey), logs in, and then exchanges
// binary nodes over the real noise socket, so whatsmeow's handler queue,
// acks, receipts and resends all run unmodified.
//
// Like WhatsApp's server, it keeps every stanza it delivers until the client
// acknowledges it (an <ack> naming the stanza, or a delivery <receipt> for a
// message) and redelivers every unacknowledged stanza, in order, on the next
// connection. Every node the client sends is recorded with a sequence number
// taken from the shared clock, so a test can order acknowledgements against
// v2 commits stamped from the same clock.
type fakeWhatsApp struct {
	t      testing.TB
	server *httptest.Server
	clock  *atomic.Uint64
	ourLID watypes.JID

	static    *keys.KeyPair
	certChain []byte

	mu          sync.Mutex
	conn        *fakeConnection
	connections int
	pending     []fakePendingStanza
	received    []fakeReceived
	changed     chan struct{}
}

type fakePendingStanza struct {
	node *waBinary.Node
}

// fakeReceived is one node the client sent.
type fakeReceived struct {
	connection int
	seq        uint64
	node       *waBinary.Node
}

type fakeConnection struct {
	number int
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	writeMu      sync.Mutex
	writeKey     cipher.AEAD
	writeCounter uint32
	readKey      cipher.AEAD
	readCounter  uint32
}

// newFakeWhatsApp starts the server and makes whatsmeow trust its certificate
// for the rest of the test. ourLID is the LID the login hands the client.
func newFakeWhatsApp(t testing.TB, clock *atomic.Uint64, ourLID watypes.JID) *fakeWhatsApp {
	t.Helper()
	root := keys.NewKeyPair()
	intermediate := keys.NewKeyPair()
	f := &fakeWhatsApp{
		t:       t,
		clock:   clock,
		ourLID:  ourLID,
		static:  keys.NewKeyPair(),
		changed: make(chan struct{}),
	}
	f.certChain = fakeCertChain(t, root, intermediate, f.static)

	originalCertKey := whatsmeow.WACertPubKey
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	whatsmeow.WACertPubKey = *root.Pub
	t.Cleanup(func() {
		f.dropConnection()
		f.server.Close()
		whatsmeow.WACertPubKey = originalCertKey
	})
	return f
}

// httpClient dials this server for every connection, including whatsmeow's
// TLS websocket to web.whatsapp.com (socket.URL is a constant). The
// connection is plain TCP; the Noise handshake on top of it is real.
func (f *fakeWhatsApp) httpClient() *http.Client {
	address := strings.TrimPrefix(f.server.URL, "http://")
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", address)
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:          nil,
		DialContext:    dial,
		DialTLSContext: dial,
	}}
}

// fakeCertChain signs an intermediate certificate with the root key whatsmeow
// trusts and a leaf for the server's static key with the intermediate, as
// verifyServerCert checks them.
func fakeCertChain(t testing.TB, root, intermediate, static *keys.KeyPair) []byte {
	t.Helper()
	now := uint64(time.Now().Unix())
	sign := func(signer *keys.KeyPair, details *waCert.CertChain_NoiseCertificate_Details) *waCert.CertChain_NoiseCertificate {
		raw, err := proto.Marshal(details)
		if err != nil {
			t.Fatalf("marshal certificate: %v", err)
		}
		signature := ecc.CalculateSignature(ecc.NewDjbECPrivateKey(*signer.Priv), raw)
		return &waCert.CertChain_NoiseCertificate{Details: raw, Signature: signature[:]}
	}
	chain, err := proto.Marshal(&waCert.CertChain{
		Intermediate: sign(root, &waCert.CertChain_NoiseCertificate_Details{
			Serial:       proto.Uint32(1),
			IssuerSerial: proto.Uint32(whatsmeow.WACertIssuerSerial),
			Key:          intermediate.Pub[:],
			NotBefore:    proto.Uint64(now - 3600),
			NotAfter:     proto.Uint64(now + 86400),
		}),
		Leaf: sign(intermediate, &waCert.CertChain_NoiseCertificate_Details{
			Serial:       proto.Uint32(2),
			IssuerSerial: proto.Uint32(1),
			Key:          static.Pub[:],
			NotBefore:    proto.Uint64(now - 3600),
			NotAfter:     proto.Uint64(now + 86400),
		}),
	})
	if err != nil {
		t.Fatalf("marshal certificate chain: %v", err)
	}
	return chain
}

// fakeNoise is the server's half of whatsmeow's NoiseHandshake
// (socket/noisehandshake.go), with its chaining key readable so the server
// can derive the transport keys.
type fakeNoise struct {
	hash    []byte
	salt    []byte
	key     cipher.AEAD
	counter uint32
}

func newFakeNoise() (*fakeNoise, error) {
	pattern := []byte(socket.NoiseStartPattern)
	hash := pattern
	if len(pattern) != 32 {
		sum := sha256.Sum256(pattern)
		hash = sum[:]
	}
	key, err := gcmutil.Prepare(hash)
	if err != nil {
		return nil, err
	}
	n := &fakeNoise{hash: hash, salt: hash, key: key}
	n.authenticate(socket.WAConnHeader)
	return n, nil
}

func (n *fakeNoise) authenticate(data []byte) {
	sum := sha256.Sum256(append(append([]byte{}, n.hash...), data...))
	n.hash = sum[:]
}

func fakeIV(counter uint32) []byte {
	iv := make([]byte, 12)
	binary.BigEndian.PutUint32(iv[8:], counter)
	return iv
}

func (n *fakeNoise) encrypt(plaintext []byte) []byte {
	ciphertext := n.key.Seal(nil, fakeIV(n.counter), plaintext, n.hash)
	n.counter++
	n.authenticate(ciphertext)
	return ciphertext
}

func (n *fakeNoise) decrypt(ciphertext []byte) ([]byte, error) {
	plaintext, err := n.key.Open(nil, fakeIV(n.counter), ciphertext, n.hash)
	n.counter++
	if err == nil {
		n.authenticate(ciphertext)
	}
	return plaintext, err
}

func fakeExpand(salt, data []byte) (write, read []byte, err error) {
	reader := hkdf.New(sha256.New, data, salt, nil)
	write = make([]byte, 32)
	read = make([]byte, 32)
	if _, err = io.ReadFull(reader, write); err == nil {
		_, err = io.ReadFull(reader, read)
	}
	return write, read, err
}

func (n *fakeNoise) mixSharedSecret(priv, pub [32]byte) error {
	secret, err := curve25519.X25519(priv[:], pub[:])
	if err != nil {
		return err
	}
	n.counter = 0
	write, read, err := fakeExpand(n.salt, secret)
	if err != nil {
		return err
	}
	n.salt = write
	n.key, err = gcmutil.Prepare(read)
	return err
}

func (f *fakeWhatsApp) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		f.t.Logf("fake WhatsApp: accept: %v", err)
		return
	}
	ws.SetReadLimit(socket.FrameMaxSize)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &fakeConnection{ws: ws, ctx: ctx, cancel: cancel}
	if err := f.handshake(conn); err != nil {
		f.t.Logf("fake WhatsApp: handshake: %v", err)
		_ = ws.CloseNow()
		return
	}
	f.attach(conn)
	defer f.detach(conn)
	for {
		node, err := conn.readNode()
		if err != nil {
			return
		}
		f.handleClientNode(conn, node)
	}
}

// readFrame reads one length-prefixed frame. The client sends one frame per
// websocket message, the first prefixed with the WA connection header.
func (c *fakeConnection) readFrame(first bool) ([]byte, error) {
	_, data, err := c.ws.Read(c.ctx)
	if err != nil {
		return nil, err
	}
	if first {
		if len(data) < len(socket.WAConnHeader) || string(data[:len(socket.WAConnHeader)]) != string(socket.WAConnHeader) {
			return nil, fmt.Errorf("missing WA connection header")
		}
		data = data[len(socket.WAConnHeader):]
	}
	if len(data) < socket.FrameLengthSize {
		return nil, fmt.Errorf("short frame")
	}
	length := int(data[0])<<16 | int(data[1])<<8 | int(data[2])
	if len(data) != socket.FrameLengthSize+length {
		return nil, fmt.Errorf("frame length %d does not match message length %d", length, len(data)-socket.FrameLengthSize)
	}
	return data[socket.FrameLengthSize:], nil
}

func (c *fakeConnection) writeFrame(payload []byte) error {
	frame := make([]byte, socket.FrameLengthSize+len(payload))
	frame[0] = byte(len(payload) >> 16)
	frame[1] = byte(len(payload) >> 8)
	frame[2] = byte(len(payload))
	copy(frame[socket.FrameLengthSize:], payload)
	return c.ws.Write(c.ctx, websocket.MessageBinary, frame)
}

func (f *fakeWhatsApp) handshake(conn *fakeConnection) error {
	frame, err := conn.readFrame(true)
	if err != nil {
		return err
	}
	var hello waWa6.HandshakeMessage
	if err := proto.Unmarshal(frame, &hello); err != nil {
		return err
	}
	clientEphemeral := hello.GetClientHello().GetEphemeral()
	if len(clientEphemeral) != 32 {
		return fmt.Errorf("client ephemeral key has %d bytes", len(clientEphemeral))
	}
	clientEphemeralKey := *(*[32]byte)(clientEphemeral)

	noise, err := newFakeNoise()
	if err != nil {
		return err
	}
	noise.authenticate(clientEphemeral)
	serverEphemeral := keys.NewKeyPair()
	noise.authenticate(serverEphemeral.Pub[:])
	if err := noise.mixSharedSecret(*serverEphemeral.Priv, clientEphemeralKey); err != nil {
		return err
	}
	staticCiphertext := noise.encrypt(f.static.Pub[:])
	if err := noise.mixSharedSecret(*f.static.Priv, clientEphemeralKey); err != nil {
		return err
	}
	certificateCiphertext := noise.encrypt(f.certChain)
	serverHello, err := proto.Marshal(&waWa6.HandshakeMessage{ServerHello: &waWa6.HandshakeMessage_ServerHello{
		Ephemeral: serverEphemeral.Pub[:],
		Static:    staticCiphertext,
		Payload:   certificateCiphertext,
	}})
	if err != nil {
		return err
	}
	if err := conn.writeFrame(serverHello); err != nil {
		return err
	}

	frame, err = conn.readFrame(false)
	if err != nil {
		return err
	}
	var finish waWa6.HandshakeMessage
	if err := proto.Unmarshal(frame, &finish); err != nil {
		return err
	}
	clientStatic, err := noise.decrypt(finish.GetClientFinish().GetStatic())
	if err != nil {
		return fmt.Errorf("decrypt client static key: %w", err)
	}
	if len(clientStatic) != 32 {
		return fmt.Errorf("client static key has %d bytes", len(clientStatic))
	}
	if err := noise.mixSharedSecret(*serverEphemeral.Priv, *(*[32]byte)(clientStatic)); err != nil {
		return err
	}
	if _, err := noise.decrypt(finish.GetClientFinish().GetPayload()); err != nil {
		return fmt.Errorf("decrypt client payload: %w", err)
	}
	clientWrite, clientRead, err := fakeExpand(noise.salt, nil)
	if err != nil {
		return err
	}
	if conn.readKey, err = gcmutil.Prepare(clientWrite); err != nil {
		return err
	}
	conn.writeKey, err = gcmutil.Prepare(clientRead)
	return err
}

func (c *fakeConnection) readNode() (*waBinary.Node, error) {
	ciphertext, err := c.readFrame(false)
	if err != nil {
		return nil, err
	}
	plaintext, err := c.readKey.Open(nil, fakeIV(c.readCounter), ciphertext, nil)
	c.readCounter++
	if err != nil {
		return nil, fmt.Errorf("decrypt client frame: %w", err)
	}
	unpacked, err := waBinary.Unpack(plaintext)
	if err != nil {
		return nil, err
	}
	return waBinary.Unmarshal(unpacked)
}

func (c *fakeConnection) sendNode(node waBinary.Node) error {
	payload, err := waBinary.Marshal(node)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ciphertext := c.writeKey.Seal(nil, fakeIV(c.writeCounter), payload, nil)
	c.writeCounter++
	return c.writeFrame(ciphertext)
}

// attach logs the new connection in and then redelivers, in order, every
// stanza the client has not acknowledged.
func (f *fakeWhatsApp) attach(conn *fakeConnection) {
	f.mu.Lock()
	f.connections++
	conn.number = f.connections
	f.conn = conn
	redeliver := make([]*waBinary.Node, 0, len(f.pending))
	for _, stanza := range f.pending {
		redeliver = append(redeliver, stanza.node)
	}
	f.broadcastLocked()
	f.mu.Unlock()

	if err := conn.sendNode(waBinary.Node{Tag: "success", Attrs: waBinary.Attrs{
		"t":   strconv.FormatInt(time.Now().Unix(), 10),
		"lid": f.ourLID,
	}}); err != nil {
		f.t.Logf("fake WhatsApp: send success: %v", err)
		return
	}
	for _, node := range redeliver {
		if err := conn.sendNode(*node); err != nil {
			f.t.Logf("fake WhatsApp: redeliver %s %v: %v", node.Tag, node.Attrs["id"], err)
			return
		}
	}
}

func (f *fakeWhatsApp) detach(conn *fakeConnection) {
	f.mu.Lock()
	if f.conn == conn {
		f.conn = nil
	}
	f.broadcastLocked()
	f.mu.Unlock()
}

func (f *fakeWhatsApp) broadcastLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

// deliver sends one stanza to the client now if it is connected, and keeps it
// until the client acknowledges it.
func (f *fakeWhatsApp) deliver(node *waBinary.Node) {
	f.mu.Lock()
	f.pending = append(f.pending, fakePendingStanza{node: node})
	conn := f.conn
	f.broadcastLocked()
	f.mu.Unlock()
	if conn == nil {
		return
	}
	if err := conn.sendNode(*node); err != nil {
		f.t.Logf("fake WhatsApp: deliver %s %v: %v", node.Tag, node.Attrs["id"], err)
	}
}

// dropConnection closes the current connection abruptly, as a network drop
// does. Stanzas the client has not acknowledged stay queued.
func (f *fakeWhatsApp) dropConnection() {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn != nil {
		_ = conn.ws.CloseNow()
		conn.cancel()
	}
}

func fakeAttr(node *waBinary.Node, key string) string {
	switch value := node.Attrs[key].(type) {
	case string:
		return value
	case watypes.JID:
		return value.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(value)
	}
}

// acknowledges reports whether a node the client sent acknowledges a stanza
// the server delivered: an <ack> names the stanza's tag, id and (for
// non-message stanzas) type; a delivery receipt names a message's id.
func acknowledges(sent, delivered *waBinary.Node) bool {
	if fakeAttr(sent, "id") != fakeAttr(delivered, "id") {
		return false
	}
	switch sent.Tag {
	case "ack":
		if fakeAttr(sent, "class") != delivered.Tag {
			return false
		}
		return delivered.Tag == "message" || fakeAttr(sent, "type") == fakeAttr(delivered, "type")
	case "receipt":
		return delivered.Tag == "message" && fakeAttr(sent, "type") != string(watypes.ReceiptTypeRetry)
	default:
		return false
	}
}

func (f *fakeWhatsApp) handleClientNode(conn *fakeConnection, node *waBinary.Node) {
	f.mu.Lock()
	f.received = append(f.received, fakeReceived{connection: conn.number, seq: f.clock.Add(1), node: node})
	kept := f.pending[:0]
	for _, stanza := range f.pending {
		if !acknowledges(node, stanza.node) {
			kept = append(kept, stanza)
		}
	}
	f.pending = kept
	f.broadcastLocked()
	f.mu.Unlock()

	switch node.Tag {
	case "iq":
		f.answerIQ(conn, node)
	case "message":
		// The server acknowledges every message the client sends; SendMessage
		// waits for this.
		_ = conn.sendNode(waBinary.Node{Tag: "ack", Attrs: waBinary.Attrs{
			"class": "message",
			"id":    fakeAttr(node, "id"),
			"from":  node.Attrs["to"],
			"t":     strconv.FormatInt(time.Now().Unix(), 10),
		}})
	}
}

// answerIQ answers the IQs a login and a connected client send: the prekey
// count with a full count, everything else with an empty result.
func (f *fakeWhatsApp) answerIQ(conn *fakeConnection, node *waBinary.Node) {
	if fakeAttr(node, "type") == "result" || fakeAttr(node, "type") == "error" {
		return
	}
	var content []waBinary.Node
	if fakeAttr(node, "xmlns") == "encrypt" && fakeAttr(node, "type") == "get" {
		if _, ok := node.GetOptionalChildByTag("count"); ok {
			content = []waBinary.Node{{Tag: "count", Attrs: waBinary.Attrs{"value": "50"}}}
		}
	}
	_ = conn.sendNode(waBinary.Node{Tag: "iq", Attrs: waBinary.Attrs{
		"id":   fakeAttr(node, "id"),
		"type": "result",
		"from": watypes.ServerJID,
	}, Content: content})
}

// receivedNodes returns every node the client sent that matches.
func (f *fakeWhatsApp) receivedNodes(match func(fakeReceived) bool) []fakeReceived {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeReceived
	for _, received := range f.received {
		if match(received) {
			out = append(out, received)
		}
	}
	return out
}

// pendingStanza reports whether a delivered stanza is still unacknowledged.
func (f *fakeWhatsApp) pendingStanza(tag, id, typ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, stanza := range f.pending {
		if stanza.node.Tag == tag && fakeAttr(stanza.node, "id") == id && fakeAttr(stanza.node, "type") == typ {
			return true
		}
	}
	return false
}

// waitReceived waits up to timeout for the client to send a matching node.
// Hooks running on whatsmeow's goroutines use it; it never stops the test.
func (f *fakeWhatsApp) waitReceived(timeout time.Duration, match func(fakeReceived) bool) (fakeReceived, bool) {
	deadline := time.After(timeout)
	for {
		f.mu.Lock()
		for _, received := range f.received {
			if match(received) {
				f.mu.Unlock()
				return received, true
			}
		}
		changed := f.changed
		f.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			return fakeReceived{}, false
		}
	}
}

// awaitReceived waits until the client has sent a matching node.
func (f *fakeWhatsApp) awaitReceived(t testing.TB, description string, match func(fakeReceived) bool) fakeReceived {
	t.Helper()
	received, ok := f.waitReceived(teardownTestTimeout, match)
	if !ok {
		t.Fatalf("timed out waiting for the client to send %s", description)
	}
	return received
}

// awaitConnections waits until the client has logged in n times.
func (f *fakeWhatsApp) awaitConnections(t testing.TB, n int) {
	t.Helper()
	deadline := time.After(teardownTestTimeout)
	for {
		f.mu.Lock()
		connected := f.connections >= n && f.conn != nil && f.conn.number == n
		changed := f.changed
		f.mu.Unlock()
		if connected {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("timed out waiting for connection %d", n)
		}
	}
}

// ackFor matches the client's acknowledgement of one delivered stanza.
func ackFor(delivered *waBinary.Node) func(fakeReceived) bool {
	return func(received fakeReceived) bool { return acknowledges(received.node, delivered) }
}
