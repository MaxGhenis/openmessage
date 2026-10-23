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
	// RepairRetryFloor is the shortest wait before a failed credential repair
	// is tried again; zero means MinBackoff. Retries back off exponentially
	// from it, with jitter, up to max(MaxBackoff, RepairRetryFloor). Callers
	// whose repairer paces attempts set it to that pace, so a retry never
	// counts as an early request.
	RepairRetryFloor time.Duration
}

// RepairOutcome records how the newest credential-repair attempt went.
type RepairOutcome string

const (
	RepairOutcomeRunning   RepairOutcome = "running"
	RepairOutcomeSucceeded RepairOutcome = "succeeded"
	// RepairOutcomeRetrying: the attempt failed with a recoverable error and
	// the next attempt is due at Snapshot.RetryAt.
	RepairOutcomeRetrying RepairOutcome = "failed_retrying"
	// RepairOutcomeBlocked: the attempt failed with a terminal error, or no
	// repairer exists, and the supervisor parked in StateBlocked.
	RepairOutcomeBlocked RepairOutcome = "failed_blocked"
)

// Block reasons say why a supervisor entered StateBlocked.
const (
	// BlockReasonTerminalFailure: the generation failed with a terminal class
	// (reauth or upgrade required, misconfigured, unsupported).
	BlockReasonTerminalFailure = "terminal_failure"
	// BlockReasonRepeatedFailure: MaxSameFingerprint consecutive non-transient
	// failures with one fingerprint. For credentials_expired this means
	// repairs succeeded but the new credentials were rejected every time.
	BlockReasonRepeatedFailure = "repeated_failure"
	// BlockReasonRepairFailed: credential repair returned a terminal error.
	BlockReasonRepairFailed = "repair_failed"
	// BlockReasonRepairUnavailable: credentials expired and no repairer exists.
	BlockReasonRepairUnavailable = "repair_unavailable"
	// BlockReasonInvalidJitter: the random source broke its contract.
	BlockReasonInvalidJitter = "invalid_jitter_source"
	// BlockReasonPairFailed: pairing failed with a terminal class.
	BlockReasonPairFailed = "pair_failed"
)

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
	// BlockReason says why State is StateBlocked; it is empty in every other
	// state.
	BlockReason string
	// RepairAttempts counts credential-repair attempts in the current expiry
	// episode. It resets when a generation proves live again or a user
	// retries.
	RepairAttempts int
	// RepairFailures counts consecutive failed repair attempts. It drives the
	// retry backoff and lets status escalate a refresh that keeps failing
	// (Chrome signed out, say) while the retries go on. It resets when a
	// repair succeeds or the episode resets.
	RepairFailures int
	// The newest repair attempt. These survive the episode, so status can
	// show how the last repair went.
	LastRepairStartedAt  time.Time
	LastRepairFinishedAt time.Time
	LastRepairOutcome    RepairOutcome
	LastRepairError      string
}
