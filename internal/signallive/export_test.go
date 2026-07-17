package signallive

import (
	"bytes"

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
