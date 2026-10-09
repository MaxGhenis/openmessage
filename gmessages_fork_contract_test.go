package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

const (
	gmessagesModule            = "go.mau.fi/mautrix-gmessages"
	gmessagesFork              = "github.com/MaxGhenis/gmessages"
	minimumGMessagesFork       = "v0.2602.1-0.20261009120339-1055c990a942"
	minimumGMessagesForkTime   = "20261009120339"
	gmessagesAuthRetryContract = "libgm/longpoll auth-refresh network retry, payload-less response rejection and request timeout"
)

var pseudoVersionSuffix = regexp.MustCompile(`[.-]([0-9]{14})-[0-9a-f]{12}$`)

type moduleVersion struct {
	Path    string
	Version string
}

type goModFile struct {
	Require []moduleVersion
	Replace []struct {
		Old moduleVersion
		New moduleVersion
	}
}

func TestGMessagesForkContract(t *testing.T) {
	cmd := exec.Command("go", "mod", "edit", "-json", "go.mod")
	cmd.Env = envWithGOWorkOff()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("parse go.mod: %v\n%s", err, output)
	}

	var mod goModFile
	if err := json.Unmarshal(output, &mod); err != nil {
		t.Fatalf("decode go.mod: %v", err)
	}

	required := false
	for _, requirement := range mod.Require {
		if requirement.Path == gmessagesModule {
			required = true
			break
		}
	}
	if !required {
		t.Fatalf("%s must remain required so the fork replacement protects the %s", gmessagesModule, gmessagesAuthRetryContract)
	}

	var replacements []struct {
		Old moduleVersion
		New moduleVersion
	}
	for _, replacement := range mod.Replace {
		if replacement.Old.Path == gmessagesModule {
			replacements = append(replacements, replacement)
		}
	}
	if len(replacements) != 1 {
		t.Fatalf("%s must have exactly one fork replacement; found %d", gmessagesModule, len(replacements))
	}

	replacement := replacements[0]
	if replacement.Old.Version != "" {
		t.Fatalf("replacement for %s must be unversioned so future require versions cannot bypass it", gmessagesModule)
	}
	if replacement.New.Path != gmessagesFork {
		t.Fatalf("%s must be replaced by %s to preserve the %s; found %s", gmessagesModule, gmessagesFork, gmessagesAuthRetryContract, replacement.New.Path)
	}

	match := pseudoVersionSuffix.FindStringSubmatch(replacement.New.Version)
	if match == nil {
		t.Fatalf("%s must use a canonical pseudo-version that records its commit time and revision; found %q", gmessagesFork, replacement.New.Version)
	}
	pinnedTime, err := time.Parse("20060102150405", match[1])
	if err != nil {
		t.Fatalf("parse %s pseudo-version timestamp %q: %v", gmessagesFork, match[1], err)
	}
	minimumTime, err := time.Parse("20060102150405", minimumGMessagesForkTime)
	if err != nil {
		t.Fatalf("parse test minimum timestamp: %v", err)
	}
	if pinnedTime.Before(minimumTime) {
		t.Fatalf("%s pin %s predates known-good %s and can regress the %s", gmessagesFork, replacement.New.Version, minimumGMessagesFork, gmessagesAuthRetryContract)
	}
}

func envWithGOWorkOff() []string {
	env := os.Environ()
	for i := 0; i < len(env); {
		if strings.HasPrefix(env[i], "GOWORK=") {
			env = append(env[:i], env[i+1:]...)
			continue
		}
		i++
	}
	return append(env, "GOWORK=off")
}

// gmessagesAcceptanceTests are the pinned fork's own behavioural tests for its
// second carried patch. They drive synthetic frames through libgm's receive
// path, which OpenMessage can't reach from outside the package.
var gmessagesAcceptanceTests = []string{
	"TestAccountContainerOnlyFrameFailsInsteadOfReturningEmpty",
	"TestHeaderOnlyFrameFails",
	"TestHeaderOnlyFrameStillCompletesAListing",
	"TestTypedResponseSurfacesPayloadError",
	"TestRealResponseAfterPayloadlessFrameIsDelivered",
	"TestEncryptedEmptyPayloadIsALegitimateEmptyAnswer",
	"TestNonDataActionsKeepFirstFrameSemantics",
	"TestAcceptanceInvariantOverRandomFrameSequences",
}

// TestGMessagesForkRejectsPayloadlessResponses pins the fork's second carried
// patch. Without it, a phone that answers a pull with a frame lacking the
// encrypted payload (on 2026-10-07/08: one GAIA_1 frame holding only the
// field-11 account container, after the phone switched to Google-account
// pairing) reaches backfill and sends as an empty success.
//
// The API checks alone would stay green if a rebase kept the exported names
// but lost the rejection in receiveResponse, so this also runs the pinned
// fork's behavioural tests and requires each one to have run and passed.
func TestGMessagesForkRejectsPayloadlessResponses(t *testing.T) {
	for _, action := range []gmproto.ActionType{
		gmproto.ActionType_LIST_CONVERSATIONS,
		gmproto.ActionType_LIST_MESSAGES,
		gmproto.ActionType_GET_OR_CREATE_CONVERSATION,
		gmproto.ActionType_GET_CONVERSATION,
	} {
		if !libgm.ResponsePayloadRequired(action) {
			t.Errorf("fork must require a response payload for %s", action)
		}
	}
	if libgm.ResponsePayloadRequired(gmproto.ActionType_NOTIFY_DITTO_ACTIVITY) {
		t.Error("liveness pings must keep first-frame semantics")
	}
	var err error = &libgm.ResponsePayloadError{Action: gmproto.ActionType_LIST_CONVERSATIONS, AccountSwitch: true}
	if !errors.Is(err, libgm.ErrNoResponsePayload) {
		t.Error("ResponsePayloadError must match ErrNoResponsePayload")
	}

	runPinnedForkTests(t, gmessagesAcceptanceTests, "payload-less response patch")
}

// gmessagesTimeoutTests are the pinned fork's behavioural tests for its third
// carried patch: the request timeout and failing waiters on disconnect.
var gmessagesTimeoutTests = []string{
	"TestUnansweredRequestFailsWithPhoneNotResponding",
	"TestTimeoutAfterPayloadlessAnswerReportsPayloadError",
	"TestDisconnectFailsPendingRequests",
	"TestPublicMethodsTimeOut",
	"TestPublicMethodFailsOnDisconnect",
	"TestTerminalOutcomeIsTheFirstTerminalEvent",
	"TestTimeoutRacesCompleteExactlyOnce",
	"TestPingerIgnoresAbandonedPing",
	"TestGaiaPairingMessageEndsCleanly",
}

// TestGMessagesForkTimesOutUnansweredRequests pins the fork's third carried
// patch. Without it, a request the phone never answers blocks its caller
// until the process exits; a catch-up blocked that way holds the backfill
// guard, so every later backfill, recent reconcile and pending-media refresh
// is refused until a restart. OpenMessage's catch-ups also rely on the two
// sentinels to stop a run after its first unanswered request
// (failFastGMClient).
func TestGMessagesForkTimesOutUnansweredRequests(t *testing.T) {
	if libgm.DefaultRequestTimeout < time.Minute || libgm.DefaultRequestTimeout > 2*time.Minute {
		t.Errorf("DefaultRequestTimeout = %s, want 1-2 minutes", libgm.DefaultRequestTimeout)
	}
	cli := libgm.NewClient(libgm.NewAuthData(), nil, zerolog.Nop())
	if got := cli.RequestTimeout(); got != libgm.DefaultRequestTimeout {
		t.Errorf("a new client's request timeout is %s, want the default", got)
	}
	cli.SetRequestTimeout(90 * time.Second)
	if got := cli.RequestTimeout(); got != 90*time.Second {
		t.Errorf("SetRequestTimeout(90s) left %s", got)
	}
	for _, reason := range []error{libgm.ErrPhoneNotResponding, libgm.ErrConnectionClosed} {
		var err error = &libgm.UnansweredRequestError{Action: gmproto.ActionType_LIST_MESSAGES, Reason: reason}
		if !errors.Is(err, reason) {
			t.Errorf("UnansweredRequestError must match its reason %v", reason)
		}
	}

	runPinnedForkTests(t, gmessagesTimeoutTests, "request-timeout patch")
}

// runPinnedForkTests runs the named tests of the pinned fork's libgm package
// and requires each one to have run and passed: the API checks alone would
// stay green if a rebase kept the exported names but lost the behaviour.
func runPinnedForkTests(t *testing.T, names []string, patch string) {
	t.Helper()
	args := []string{"test", "-count=1", "-v", "-run", "^(" + strings.Join(names, "|") + ")$"}
	if modfile := os.Getenv("OPENMESSAGE_CONTRACT_MODFILE"); modfile != "" {
		// Lets a mutation check point the contract at a modified fork copy.
		// Run the outer test with -count=1: Go's test cache does not see
		// edits to that copy and would replay a stale pass.
		args = append(args, "-modfile="+modfile)
	}
	args = append(args, gmessagesModule+"/pkg/libgm")
	cmd := exec.Command("go", args...)
	cmd.Env = envWithGOWorkOff()
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("the pinned gmessages fork fails its %s tests: %v\n%s", patch, runErr, output)
	}
	for _, name := range names {
		if !strings.Contains(string(output), "--- PASS: "+name+" ") {
			t.Errorf("the pinned gmessages fork did not run and pass %s; a rebase may have dropped the %s\n%s", name, patch, output)
		}
	}
}
