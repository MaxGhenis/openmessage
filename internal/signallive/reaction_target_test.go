package signallive

import (
	"context"
	"errors"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/v2keys"
)

const (
	reactionTestAccount = "+15551230000"
	reactionTestSender  = "+15551234567"
	reactionTestACI     = "9f4b50e3-ebf2-413c-a856-161756a6161a"
	// An outbox transport request ID: 128 random bits in hex
	// (messaging.CryptoIDSource), the remote ID of an unconfirmed send.
	reactionTestRequestID = "0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	reactionTestLocalID   = "local:c88a3b1e5f4ec4fe43c3b85b7664cbe45b711735"
	errReactionTimestamp  = "signal reaction target timestamp is unavailable"
	errReactionAuthor     = "signal reaction target author is unavailable"
)

func TestReactionTargetArgsRules(t *testing.T) {
	incoming := v2keys.SignalIncomingSourceID("signal:"+reactionTestSender, reactionTestSender, 1700000000123)
	tests := []struct {
		name    string
		target  ReactionTarget
		account string
		want    []string
		wantErr string
	}{
		{
			name:   "incoming message: SHA-1 remote ID, timestamp from the occurred time",
			target: ReactionTarget{RemoteID: incoming, AuthorID: reactionTestSender, SentAt: time.UnixMilli(1700000000123)},
			want:   reactionArgs(reactionTestSender, "1700000000123"),
		},
		{
			name:   "incoming from an unresolved ACI keeps the ACI as author",
			target: ReactionTarget{RemoteID: incoming, AuthorID: " " + reactionTestACI + " ", SentAt: time.UnixMilli(1700000000124)},
			want:   reactionArgs(reactionTestACI, "1700000000124"),
		},
		{
			name:   "incoming message stored under this account's own address is still incoming",
			target: ReactionTarget{RemoteID: " " + incoming + " ", AuthorID: reactionTestAccount, SentAt: time.UnixMilli(1700000000123)},
			want:   reactionArgs(reactionTestAccount, "1700000000123"),
		},
		{
			name: "incoming Signal Desktop row stored under its received time",
			target: ReactionTarget{
				RemoteID: v2keys.SignalReceivedSourceID("signal:"+reactionTestSender, reactionTestSender, 1700000000500),
				AuthorID: reactionTestSender, SentAt: time.UnixMilli(1700000000500),
			},
			wantErr: errReactionTimestamp,
		},
		{
			name:    "incoming under a decimal remote ID, a form no Signal receiver writes",
			target:  ReactionTarget{RemoteID: "1700000000999", AuthorID: reactionTestSender, SentAt: time.UnixMilli(1700000000123)},
			wantErr: errReactionTimestamp,
		},
		{
			name:    "mirrored incoming message under its legacy ID",
			target:  ReactionTarget{RemoteID: "signal:" + incoming, AuthorID: reactionTestSender, SentAt: time.UnixMilli(1700000000123)},
			wantErr: errReactionTimestamp,
		},
		{
			name:    "incoming under an upper-case hash is not the receiver's form",
			target:  ReactionTarget{RemoteID: strings.ToUpper(incoming), AuthorID: reactionTestSender, SentAt: time.UnixMilli(1700000000123)},
			wantErr: errReactionTimestamp,
		},
		{
			name:   "own outbox send: the confirmed transport timestamp, not the submit time",
			target: ReactionTarget{RemoteID: "1700000000555", Outgoing: true, SentAt: time.UnixMilli(1700000000000)},
			want:   reactionArgs(reactionTestAccount, "1700000000555"),
		},
		{
			name:   "own send from the phone: remote ID and occurred time agree",
			target: ReactionTarget{RemoteID: " 1700000000777 ", Outgoing: true, SentAt: time.UnixMilli(1700000000777)},
			want:   reactionArgs(reactionTestAccount, "1700000000777"),
		},
		{
			name:   "own send is this account's whatever sender the store recorded",
			target: ReactionTarget{RemoteID: "1700000000556", AuthorID: reactionTestACI, Outgoing: true},
			want:   reactionArgs(reactionTestAccount, "1700000000556"),
		},
		{
			name:    "own send still on its outbox request ID",
			target:  ReactionTarget{RemoteID: reactionTestRequestID, Outgoing: true, SentAt: time.UnixMilli(1700000000000)},
			wantErr: errReactionTimestamp,
		},
		{
			name:    "own migrated message under a local alias",
			target:  ReactionTarget{RemoteID: reactionTestLocalID, Outgoing: true, SentAt: time.UnixMilli(1700000000321)},
			wantErr: errReactionTimestamp,
		},
		{
			name:    "own migrated placeholder for an edit",
			target:  ReactionTarget{RemoteID: "missing-edit:c88a3b1e5f4ec4fe43c3b85b7664cbe45b711735", Outgoing: true, SentAt: time.UnixMilli(1700000000322)},
			wantErr: errReactionTimestamp,
		},
		{
			name:    "own decimal remote ID past int64",
			target:  ReactionTarget{RemoteID: "99999999999999999999", Outgoing: true, SentAt: time.UnixMilli(1700000000322)},
			wantErr: errReactionTimestamp,
		},
		{
			name:    "own zero remote ID",
			target:  ReactionTarget{RemoteID: "0", Outgoing: true, SentAt: time.UnixMilli(1700000000323)},
			wantErr: errReactionTimestamp,
		},
		{
			name:    "incoming message with no stored sender",
			target:  ReactionTarget{RemoteID: incoming, SentAt: time.UnixMilli(1700000000123)},
			wantErr: errReactionAuthor,
		},
		{
			name:    "incoming message with no stored sender and a decimal remote ID",
			target:  ReactionTarget{RemoteID: "1700000000123", SentAt: time.UnixMilli(1700000000123)},
			wantErr: errReactionAuthor,
		},
		{
			name:    "mirrored incoming message with no stored sender",
			target:  ReactionTarget{RemoteID: "signal:1700000000123", SentAt: time.UnixMilli(1700000000123)},
			wantErr: errReactionAuthor,
		},
		{
			name:    "incoming message with no occurred time",
			target:  ReactionTarget{RemoteID: incoming, AuthorID: reactionTestSender},
			wantErr: errReactionTimestamp,
		},
		{
			name:    "an epoch occurred time is not a timestamp",
			target:  ReactionTarget{RemoteID: incoming, AuthorID: reactionTestSender, SentAt: time.UnixMilli(0)},
			wantErr: errReactionTimestamp,
		},
		{
			name:    "a negative occurred time is not a timestamp",
			target:  ReactionTarget{RemoteID: incoming, AuthorID: reactionTestSender, SentAt: time.UnixMilli(-5)},
			wantErr: errReactionTimestamp,
		},
		{
			name:    "own send with no account to name",
			target:  ReactionTarget{RemoteID: "1700000000555", Outgoing: true},
			account: " ",
			wantErr: errReactionAuthor,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			account := reactionTestAccount
			if tc.account != "" {
				account = tc.account
			}
			got, err := ReactionTargetArgs(tc.target, account)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr || got != nil {
					t.Fatalf("ReactionTargetArgs(%+v) = %q, %v; want error %q", tc.target, got, err, tc.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("ReactionTargetArgs(%+v) = %q, %v; want %q", tc.target, got, err, tc.want)
			}
		})
	}
}

// TestReactionTargetArgsInvariants checks, over seeded random targets, against
// an independently written oracle:
//
//   - Shape: success is exactly "-a <non-empty author> -t <positive decimal>".
//   - Own message: the author is the account, and the timestamp is the
//     RemoteID when it is a positive int64 decimal. Any other RemoteID (a
//     request ID, a "local:" or "missing-edit:" alias, a SHA-1) fails as
//     unavailable whatever SentAt holds.
//   - Incoming message: the author is the trimmed AuthorID and the timestamp
//     is SentAt, and only when RemoteID is 40 lowercase hex digits, the form a
//     Signal receiver gives a message it keyed by its sent timestamp. No
//     AuthorID fails as author unavailable; any other RemoteID (a decimal, a
//     "received:" marker, a legacy "signal:" ID) or a SentAt that is zero or
//     not positive fails as timestamp unavailable.
//   - So -t is never a RemoteID that is not a decimal of an own message, never
//     the stored time of a message whose ID does not vouch for it, and -a is
//     never the account for an incoming message that names no sender.
//   - Agreement with quotes: wherever a reaction names a message, QuoteArgs
//     names the same author and timestamp for it.
//   - Determinism.
func TestReactionTargetArgsInvariants(t *testing.T) {
	random := rand.New(rand.NewSource(20261010))
	authors := []string{"", " ", reactionTestAccount, " " + reactionTestAccount, reactionTestSender, reactionTestACI}
	remoteIDs := []string{
		"", "0", "-17", "+17", "1700000000555", " 1700000000556 ", "99999999999999999999",
		"9223372036854775807", "9223372036854775808", "signal:1700000000557",
		"f48818f15483f503bb133d92360ca8e2fbd8287e", " 36c1e3741b6432157a0af5fe136a306fd941ee21 ",
		"0000000000000000000000000000000000000000", "c88a3b1e5f4ec4fe43c3b85b7664cbe45b711735",
		"F48818F15483F503BB133D92360CA8E2FBD8287E", "f48818f15483f503bb133d92360ca8e2fbd8287", "f48818f15483f503bb133d92360ca8e2fbd8287e0",
		"received:f48818f15483f503bb133d92360ca8e2fbd8287e", "signal:f48818f15483f503bb133d92360ca8e2fbd8287e",
		reactionTestLocalID, reactionTestRequestID,
		"missing-edit:c88a3b1e5f4ec4fe43c3b85b7664cbe45b711735", "17000000005x5",
	}
	identity := func(value string) string { return value }
	ownSuccesses, incomingSuccesses, authorFailures, timestampFailures := 0, 0, 0, 0
	for iteration := 0; iteration < 5000; iteration++ {
		target := ReactionTarget{
			RemoteID: remoteIDs[random.Intn(len(remoteIDs))],
			AuthorID: authors[random.Intn(len(authors))],
			Outgoing: random.Intn(2) == 0,
		}
		switch random.Intn(4) {
		case 0:
		case 1:
			target.SentAt = time.UnixMilli(-random.Int63n(1_000_000))
		default:
			target.SentAt = time.UnixMilli(1 + random.Int63n(2_000_000_000_000))
		}

		got, err := ReactionTargetArgs(target, reactionTestAccount)
		again, againErr := ReactionTargetArgs(target, reactionTestAccount)
		if !slices.Equal(got, again) || errorText(err) != errorText(againErr) {
			t.Fatalf("iteration %d: ReactionTargetArgs(%+v) is not deterministic", iteration, target)
		}

		var wantAuthor string
		var wantTimestamp int64
		if target.Outgoing {
			wantAuthor = reactionTestAccount
			if trimmed := strings.TrimSpace(target.RemoteID); trimmed != "" && strings.Trim(trimmed, "0123456789") == "" {
				if parsed, parseErr := strconv.ParseInt(trimmed, 10, 64); parseErr == nil {
					wantTimestamp = parsed
				}
			}
		} else {
			wantAuthor = strings.TrimSpace(target.AuthorID)
			trimmed := strings.TrimSpace(target.RemoteID)
			receiverForm := len(trimmed) == 40 && strings.Trim(trimmed, "0123456789abcdef") == ""
			if receiverForm && !target.SentAt.IsZero() {
				wantTimestamp = target.SentAt.UnixMilli()
			}
		}
		switch {
		case wantAuthor == "":
			authorFailures++
			if err == nil || err.Error() != errReactionAuthor || got != nil {
				t.Fatalf("iteration %d: ReactionTargetArgs(%+v) = %q, %v; want unavailable author", iteration, target, got, err)
			}
			continue
		case wantTimestamp <= 0:
			timestampFailures++
			if err == nil || err.Error() != errReactionTimestamp || got != nil {
				t.Fatalf("iteration %d: ReactionTargetArgs(%+v) = %q, %v; want unavailable timestamp", iteration, target, got, err)
			}
			continue
		}
		if target.Outgoing {
			ownSuccesses++
		} else {
			incomingSuccesses++
		}
		want := reactionArgs(wantAuthor, strconv.FormatInt(wantTimestamp, 10))
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("iteration %d: ReactionTargetArgs(%+v) = %q, %v; want %q", iteration, target, got, err, want)
		}

		// The quote of the same stored message. QuoteArgs has no direction: it
		// reads an empty author as this account, which is how an own message
		// reaches it. An incoming message stored under this account's own
		// address is the one shape it reads differently, so it is left out.
		quoted := ReplyTarget{RemoteID: target.RemoteID, AuthorID: target.AuthorID, SentAt: target.SentAt, Text: "quoted"}
		if target.Outgoing {
			quoted.AuthorID = ""
		} else if wantAuthor == reactionTestAccount {
			continue
		}
		quote, quoteErr := QuoteArgs(quoted, reactionTestAccount, identity)
		if quoteErr != nil || len(quote) != 6 || quote[1] != got[3] || quote[3] != got[1] {
			t.Fatalf("iteration %d: QuoteArgs(%+v) = %q, %v; the reaction targets %q", iteration, quoted, quote, quoteErr, got)
		}
	}
	if ownSuccesses < 150 || incomingSuccesses < 150 || authorFailures < 200 || timestampFailures < 500 {
		t.Fatalf("generator covered %d own and %d incoming successes, %d author failures, %d timestamp failures",
			ownSuccesses, incomingSuccesses, authorFailures, timestampFailures)
	}
}

// TestSendReactionRequestTargetsTheDescribedMessage checks the signal-cli argv
// SendReactionRequest runs for each kind of target the v2 dispatcher hands it,
// and that a target it cannot name fails before signal-cli runs, with an
// error the adapter reports as not dispatched.
func TestSendReactionRequestTargetsTheDescribedMessage(t *testing.T) {
	incoming := v2keys.SignalIncomingSourceID("signal:"+reactionTestSender, reactionTestSender, 1700000000123)
	tests := []struct {
		name         string
		conversation string
		target       ReactionTarget
		action       string
		want         []string
		wantErr      string
	}{
		{
			name:         "incoming message in a 1:1 conversation",
			conversation: "signal:" + reactionTestSender,
			target:       ReactionTarget{RemoteID: incoming, AuthorID: reactionTestSender, SentAt: time.UnixMilli(1700000000123)},
			action:       "add",
			want: []string{
				"-a", reactionTestAccount, "sendReaction", "-e", "👍",
				"-a", reactionTestSender, "-t", "1700000000123", reactionTestSender,
			},
		},
		{
			name:         "incoming message from an ACI in a group, removed",
			conversation: "signal-group:test-group",
			target:       ReactionTarget{RemoteID: incoming, AuthorID: reactionTestACI, SentAt: time.UnixMilli(1700000000124)},
			action:       "remove",
			want: []string{
				"-a", reactionTestAccount, "sendReaction", "-e", "👍",
				"-a", reactionTestACI, "-t", "1700000000124", "-r", "--group-id", "test-group",
			},
		},
		{
			name:         "own confirmed outbox send",
			conversation: "signal:" + reactionTestSender,
			target:       ReactionTarget{RemoteID: "1700000000555", Outgoing: true, SentAt: time.UnixMilli(1700000000000)},
			action:       "switch",
			want: []string{
				"-a", reactionTestAccount, "sendReaction", "-e", "👍",
				"-a", reactionTestAccount, "-t", "1700000000555", reactionTestSender,
			},
		},
		{
			name:         "own send still on its outbox request ID",
			conversation: "signal:" + reactionTestSender,
			target:       ReactionTarget{RemoteID: reactionTestRequestID, Outgoing: true, SentAt: time.UnixMilli(1700000000000)},
			action:       "add",
			wantErr:      errReactionTimestamp,
		},
		{
			name:         "own migrated message under a local alias",
			conversation: "signal:" + reactionTestSender,
			target:       ReactionTarget{RemoteID: reactionTestLocalID, Outgoing: true, SentAt: time.UnixMilli(1700000000321)},
			action:       "add",
			wantErr:      errReactionTimestamp,
		},
		{
			name:         "incoming message with no stored sender",
			conversation: "signal-group:test-group",
			target:       ReactionTarget{RemoteID: "1700000000123", SentAt: time.UnixMilli(1700000000123)},
			action:       "add",
			wantErr:      errReactionAuthor,
		},
		{
			name:         "incoming message with no occurred time",
			conversation: "signal:" + reactionTestSender,
			target:       ReactionTarget{RemoteID: incoming, AuthorID: reactionTestSender},
			action:       "add",
			wantErr:      errReactionTimestamp,
		},
		{
			name:         "incoming Signal Desktop row stored under its received time",
			conversation: "signal:" + reactionTestSender,
			target: ReactionTarget{
				RemoteID: v2keys.SignalReceivedSourceID("signal:"+reactionTestSender, reactionTestSender, 1700000000500),
				AuthorID: reactionTestSender, SentAt: time.UnixMilli(1700000000500),
			},
			action:  "add",
			wantErr: errReactionTimestamp,
		},
	}

	originalRun := runSignalCLI
	t.Cleanup(func() { runSignalCLI = originalRun })
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bridge := &Bridge{
				account:   reactionTestAccount,
				connected: true,
				configDir: t.TempDir(),
				logger:    zerolog.Nop(),
			}
			var calls [][]string
			runSignalCLI = func(_ context.Context, _ string, args ...string) ([]byte, error) {
				calls = append(calls, slices.Clone(args))
				return []byte("ok"), nil
			}

			err := bridge.SendReactionRequest(tc.conversation, tc.target, "👍", tc.action)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr || IsCommandError(err) || IsSendNotDispatchedError(err) {
					t.Fatalf("SendReactionRequest() = %v (%T); want plain pre-call error %q", err, err, tc.wantErr)
				}
				if len(calls) != 0 {
					t.Fatalf("signal-cli calls = %q, want none for a target it cannot name", calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("SendReactionRequest(): %v", err)
			}
			if len(calls) != 1 || !slices.Equal(calls[0], tc.want) {
				t.Fatalf("signal-cli calls = %q, want one %q", calls, tc.want)
			}
		})
	}
}

func TestSendReactionRequestStillRequiresATargetRemoteID(t *testing.T) {
	bridge := &Bridge{account: reactionTestAccount, connected: true, configDir: t.TempDir(), logger: zerolog.Nop()}
	originalRun := runSignalCLI
	t.Cleanup(func() { runSignalCLI = originalRun })
	runSignalCLI = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("signal-cli must not run")
	}
	err := bridge.SendReactionRequest(
		"signal:"+reactionTestSender,
		ReactionTarget{RemoteID: " ", AuthorID: reactionTestSender, SentAt: time.UnixMilli(1700000000123)},
		"👍",
		"add",
	)
	if err == nil || err.Error() != "signal target message is required" {
		t.Fatalf("SendReactionRequest(no remote ID) = %v, want the required-target error", err)
	}
}

func reactionArgs(author, timestamp string) []string {
	return []string{"-a", author, "-t", timestamp}
}
