package whatsapplive

import (
	"context"
	"database/sql"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Test hooks shared by this package's tests and the external whatsapplive_test
// package, which drives the real lifecycle adapter and supervisor against a
// real Bridge and real whatsmeow event handling.

// WhatsmeowClientForTest returns the bridge's current whatsmeow client.
func (b *Bridge) WhatsmeowClientForTest() *whatsmeow.Client {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.client
}

// StubConnectForTest replaces the paired-transport dial for the rest of the
// test.
func StubConnectForTest(t testing.TB, connect func(context.Context, *whatsmeow.Client) error) {
	t.Helper()
	original := connectClientContext
	connectClientContext = connect
	t.Cleanup(func() { connectClientContext = original })
}

// testADVIdentity fills the account fields whatsmeow's PutDevice writes; the
// session store only checks their sizes.
func testADVIdentity() *waAdv.ADVSignedDeviceIdentity {
	return &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{},
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
}

// SeedLinkedDeviceForTest stores a linked identity in a session store the way
// a completed pairing leaves it (Device.Save).
func SeedLinkedDeviceForTest(t testing.TB, sessionPath string, jid watypes.JID) {
	t.Helper()
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite", sessionStoreDSN(sessionPath), waLog.Noop)
	if err != nil {
		t.Fatalf("sqlstore.New(): %v", err)
	}
	defer container.Close()
	device := container.NewDevice()
	device.ID = &jid
	device.PushName = "OpenMessage test"
	device.Account = testADVIdentity()
	if err := device.Save(ctx); err != nil {
		t.Fatalf("save linked device %s: %v", jid, err)
	}
}

// LinkedDeviceJIDsForTest lists the identities a session store holds.
func LinkedDeviceJIDsForTest(t testing.TB, sessionPath string) []string {
	t.Helper()
	database, err := sql.Open("sqlite", sessionStoreReadOnlyDSN(sessionPath))
	if err != nil {
		t.Fatalf("open session store: %v", err)
	}
	defer database.Close()
	rows, err := database.Query(`SELECT jid FROM whatsmeow_device ORDER BY jid`)
	if err != nil {
		t.Fatalf("list linked devices: %v", err)
	}
	defer rows.Close()
	var jids []string
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			t.Fatalf("scan linked device: %v", err)
		}
		jids = append(jids, jid)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list linked devices: %v", err)
	}
	return jids
}

// emulatePairingSaveForTest does what whatsmeow's handlePair does to the
// current client before it dispatches PairSuccess: install the identity on the
// client's device and save it. It returns that device.
func emulatePairingSaveForTest(t testing.TB, b *Bridge, jid watypes.JID) *wastore.Device {
	t.Helper()
	b.mu.Lock()
	device := b.client.Store
	device.ID = &jid
	device.Account = testADVIdentity()
	err := device.Save(context.Background())
	b.mu.Unlock()
	if err != nil {
		t.Fatalf("save paired device %s: %v", jid, err)
	}
	return device
}

// emulatePairingForTest does what a successful whatsmeow pairing does to the
// current client (install the identity and save it) and then delivers
// PairSuccess through the bridge's event path.
func emulatePairingForTest(t testing.TB, b *Bridge, jid watypes.JID) {
	t.Helper()
	emulatePairingSaveForTest(t, b, jid)
	b.handleEvent(&waevents.PairSuccess{ID: jid})
}
