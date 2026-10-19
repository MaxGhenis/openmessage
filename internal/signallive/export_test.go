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
