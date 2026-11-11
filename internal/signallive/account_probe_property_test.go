package signallive

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// signalCLINoiseLines are log lines of the shapes signal-cli writes to stderr.
// The probe reads CombinedOutput, so any of them can land around the JSON.
// Some embed an address inside prose, which must never count as an account.
// The JSON-shaped ones name no account and reach the line-by-line decode.
var signalCLINoiseLines = []string{
	"WARN  AccountHelper - Messages have been last received 8 days ago. The Signal protocol expects that incoming messages are regularly received.",
	"INFO  AccountHelper - The Signal protocol expects that incoming messages are regularly received.",
	"WARN  MultiAccountManager - Ignoring +15551230000: User is not registered. (NotRegisteredException)",
	"INFO  Manager - Starting up",
	"WARN  RefreshRecipientsJob - Failed to refresh recipients",
	"User +15557654321 is not registered.",
	"[]",
	`{"accounts":[]}`,
	`{"error":{"message":"User +15551230000 is not registered."}}`,
	`{"type":"x"}`,
	"",
	"   ",
}

// accountProbeOutput is one generated listAccounts buffer: the accounts the
// JSON payload names, the payload itself, and stderr noise on either side.
type accountProbeOutput struct {
	Accounts []string
	Payload  string
	Before   []string
	After    []string
}

func (accountProbeOutput) Generate(r *rand.Rand, _ int) reflect.Value {
	accounts := make([]string, 1+r.Intn(4))
	for i := range accounts {
		// Allow repeats so de-duplication is exercised too.
		accounts[i] = fmt.Sprintf("+1555%07d", r.Intn(8))
	}
	type entry struct {
		Number string `json:"number"`
	}
	entries := make([]entry, len(accounts))
	for i, account := range accounts {
		entries[i] = entry{Number: account}
	}
	var payload []byte
	if r.Intn(2) == 0 {
		payload, _ = json.Marshal(entries)
	} else {
		payload, _ = json.Marshal(map[string]any{"accounts": entries, "version": 2})
	}
	noise := func() []string {
		lines := make([]string, r.Intn(4))
		for i := range lines {
			lines[i] = signalCLINoiseLines[r.Intn(len(signalCLINoiseLines))]
		}
		return lines
	}
	return reflect.ValueOf(accountProbeOutput{
		Accounts: accounts,
		Payload:  string(payload),
		Before:   noise(),
		After:    noise(),
	})
}

func (o accountProbeOutput) buffer() []byte {
	lines := append(append(slices.Clone(o.Before), o.Payload), o.After...)
	return []byte(strings.Join(lines, "\n") + "\n")
}

func (o accountProbeOutput) want() []string {
	want := slices.Clone(o.Accounts)
	slices.Sort(want)
	return slices.Compact(want)
}

// Invariant: stderr noise around the payload never changes the result. The
// noisy buffer parses to exactly the accounts the clean payload names (sorted,
// de-duplicated), and agrees with parsing the payload alone.
func TestParseSignalAccountsIgnoresNoiseAroundPayloadProperty(t *testing.T) {
	property := func(o accountProbeOutput) bool {
		got := parseSignalAccounts(o.buffer())
		clean := parseSignalAccounts([]byte(o.Payload))
		return slices.Equal(got, o.want()) && slices.Equal(got, clean)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

// Invariant: noise alone names no account, including the addresses some
// signal-cli warnings mention in prose.
func TestParseSignalAccountsFindsNothingInNoiseOnlyProperty(t *testing.T) {
	property := func(o accountProbeOutput) bool {
		lines := append(slices.Clone(o.Before), o.After...)
		return len(parseSignalAccounts([]byte(strings.Join(lines, "\n")))) == 0
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 2000}); err != nil {
		t.Fatal(err)
	}
}

// signal-cli 0.14.9's real listAccounts output for a config whose only
// account was deregistered: the warning on stderr, then an empty JSON array.
func TestParseSignalAccountsDeregisteredAccountOutput(t *testing.T) {
	raw := []byte("WARN  MultiAccountManager - Ignoring +15551230000: User is not registered. (NotRegisteredException)\n[]\n")
	if got := parseSignalAccounts(raw); len(got) != 0 {
		t.Fatalf("parseSignalAccounts() = %#v, want no accounts", got)
	}
}
