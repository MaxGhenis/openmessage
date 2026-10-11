package whatsapplive

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	watypes "go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Hooks for the external whatsapplive_test package's teardown tests, which
// drive whatsmeow's real decrypt handlers through the real lifecycle adapter
// and supervisor while a generation is torn down.

// ClientForTeardownTest returns the bridge's current whatsmeow client.
func ClientForTeardownTest(b *Bridge) *whatsmeow.Client {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.client
}

// StubDialForTeardownTest replaces whatsmeow's ConnectContext (the network
// dial) for the rest of the test. Everything the bridge does around the dial
// still runs.
func StubDialForTeardownTest(t testing.TB, connect func(context.Context, *whatsmeow.Client) error) {
	t.Helper()
	original := connectClientContext
	connectClientContext = connect
	t.Cleanup(func() { connectClientContext = original })
}

// TeardownTestAccount fills the account fields whatsmeow's PutDevice writes;
// the session store only checks their sizes.
func TeardownTestAccount() *waAdv.ADVSignedDeviceIdentity {
	return &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{},
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
}

// SeedDeviceForTeardownTest stores a linked identity, with real identity,
// signed prekey and registration keys, the way a completed pairing leaves it.
func SeedDeviceForTeardownTest(t testing.TB, sessionPath string, jid watypes.JID) {
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
	device.Account = TeardownTestAccount()
	if err := device.Save(ctx); err != nil {
		t.Fatalf("save linked device %s: %v", jid, err)
	}
}

// NewPeerDeviceForTeardownTest opens a second, unrelated session store and
// returns a saved device in it: a remote WhatsApp device with its own real
// Signal stores.
func NewPeerDeviceForTeardownTest(t testing.TB, path string, jid watypes.JID) *store.Device {
	t.Helper()
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite", sessionStoreDSN(path), waLog.Noop)
	if err != nil {
		t.Fatalf("sqlstore.New(peer): %v", err)
	}
	t.Cleanup(func() { _ = container.Close() })
	device := container.NewDevice()
	device.ID = &jid
	device.Account = TeardownTestAccount()
	if err := device.Save(ctx); err != nil {
		t.Fatalf("save peer device %s: %v", jid, err)
	}
	return device
}
