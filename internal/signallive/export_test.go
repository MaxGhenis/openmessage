package signallive

import (
	"bytes"
	"context"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/rs/zerolog"
)

// CapturedIngressForTest is the capture-boundary value exposed to external
// integration tests without widening Bridge's production API.
type CapturedIngressForTest struct {
	Account             string
	Line                []byte
	ResolvedSource      string
	ResolvedDestination string
}

// CaptureAndProcessReceiveLineForTest uses the same private contact cache and
// receive path as the retained legacy handler, returning what its durable tee
// observed for an external decoder integration test.
func CaptureAndProcessReceiveLineForTest(
	store *db.Store,
	configDir string,
	contactByACI map[string]string,
	account string,
	line []byte,
) (CapturedIngressForTest, bool, error) {
	bridge := &Bridge{
		store:        store,
		logger:       zerolog.Nop(),
		configDir:    configDir,
		contactByACI: contactByACI,
	}
	var captured CapturedIngressForTest
	unregister := bridge.ObserveIngress(func(
		observedAccount string,
		observedLine []byte,
		resolvedSource string,
		resolvedDestination string,
	) {
		captured = CapturedIngressForTest{
			Account:             observedAccount,
			Line:                bytes.Clone(observedLine),
			ResolvedSource:      resolvedSource,
			ResolvedDestination: resolvedDestination,
		}
	})
	defer unregister()
	processed, err := bridge.processReceiveLine(account, line, false)
	return captured, processed, err
}

// SetRunSignalCLIForTest replaces the signal-cli runner (contact and group
// refreshes included) for an external test and returns the restore function.
func SetRunSignalCLIForTest(
	run func(ctx context.Context, configDir string, args ...string) ([]byte, error),
) func() {
	original := runSignalCLI
	runSignalCLI = run
	return func() { runSignalCLI = original }
}

// LegacyQuoteArgsForTest is the legacy-store quote lookup: what a reply quoting
// replyToID got before the v2 dispatcher described its target.
func LegacyQuoteArgsForTest(
	store *db.Store,
	contactByACI map[string]string,
	account string,
	replyToID string,
) ([]string, error) {
	bridge := &Bridge{store: store, logger: zerolog.Nop(), contactByACI: contactByACI}
	return bridge.signalQuoteArgs(replyToID, account)
}

// NewConnectedBridgeForTest is a Bridge that reports account as paired and
// connected, so the Signal adapter dispatches to it. Every signal-cli call goes
// through runSignalCLI (see SetRunSignalCLIForTest); store is the legacy store
// the retained legacy paths read.
func NewConnectedBridgeForTest(
	store *db.Store,
	configDir string,
	account string,
	contactByACI map[string]string,
) *Bridge {
	if contactByACI == nil {
		contactByACI = map[string]string{}
	}
	return &Bridge{
		store:        store,
		logger:       zerolog.Nop(),
		configDir:    configDir,
		account:      account,
		connected:    true,
		contactByACI: contactByACI,
	}
}

// LegacySendReactionForTest runs the legacy reaction path (SendReaction, which
// looks its target up in the legacy store by message ID) from a connected
// bridge over store, with whatever runSignalCLI is installed.
func LegacySendReactionForTest(
	store *db.Store,
	configDir string,
	account string,
	contactByACI map[string]string,
	conversationID, messageID, emoji, action string,
) error {
	return NewConnectedBridgeForTest(store, configDir, account, contactByACI).
		SendReaction(conversationID, messageID, emoji, action)
}

// ReplyQuoteArgsForTest is the quote SendTextRequest and SendMediaRequest
// build for reply.
func ReplyQuoteArgsForTest(
	store *db.Store,
	contactByACI map[string]string,
	account string,
	reply ReplyTarget,
) ([]string, error) {
	bridge := &Bridge{store: store, logger: zerolog.Nop(), contactByACI: contactByACI}
	return bridge.replyQuoteArgs(reply, account)
}
