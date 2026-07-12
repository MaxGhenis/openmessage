package bridge

import "time"

// State is brought forward from the supervisor contract because Snapshot is
// part of the C2 Registry interface. C2 registrations begin stopped; C3 will
// supply live supervisor state and generations.
type State string

const (
	StateUnconfigured         State = "unconfigured"
	StateUnpaired             State = "unpaired"
	StateStopped              State = "stopped"
	StatePairing              State = "pairing"
	StateConnecting           State = "connecting"
	StateOnline               State = "online"
	StateDegraded             State = "degraded"
	StateBackoff              State = "backoff"
	StateRepairingCredentials State = "repairing_credentials"
	StateBlocked              State = "blocked"
	StateStopping             State = "stopping"
)

// Policy contains every time and retry decision made by Supervisor. All
// durations must be positive, MaxBackoff must be at least MinBackoff, and
// MaxSameFingerprint must be positive.
type Policy struct {
	ConnectTimeout     time.Duration
	ProbeEvery         time.Duration
	ProbeTimeout       time.Duration
	LivenessTimeout    time.Duration
	MinBackoff         time.Duration
	MaxBackoff         time.Duration
	MaxSameFingerprint int
}

type Snapshot struct {
	AccountID        string
	Platform         Platform
	State            State
	Generation       Generation
	LastEventAt      time.Time
	LastProbeOKAt    time.Time
	LivenessDeadline time.Time
	RetryAt          time.Time
	ErrorClass       FailureClass
	ErrorFingerprint string
}
