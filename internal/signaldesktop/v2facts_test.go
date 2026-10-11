package signaldesktop

import (
	"errors"
	"testing"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestAccountNumberComesFromV2Only(t *testing.T) {
	remote := func(value string) *string { return &value }
	self := func(kind, value string) sqlite.Identity {
		return sqlite.Identity{Kind: sqlite.IdentityKind(kind), CanonicalValue: value, IsSelf: true}
	}
	peer := sqlite.Identity{Kind: "e164", CanonicalValue: tPeerE164}
	for _, tc := range []struct {
		name       string
		account    sqlite.Account
		identities []sqlite.Identity
		want       string
		wantErr    bool
	}{
		{name: "remote account ID", account: sqlite.Account{RemoteAccountID: remote(tSelf)}, identities: []sqlite.Identity{self("e164", tThirdE164)}, want: tSelf},
		{name: "no remote ID: the self phone identity", identities: []sqlite.Identity{peer, self("e164", tSelf)}, want: tSelf},
		// Seen live: a long non-phone self "username" next to the phone one.
		{name: "non-phone self identities are ignored", identities: []sqlite.Identity{self("username", "Esynthetic-not-a-number"), self("e164", tSelf)}, want: tSelf},
		{name: "remote ID that is not a number falls back", account: sqlite.Account{RemoteAccountID: remote(tSelfACI)}, identities: []sqlite.Identity{self("e164", tSelf)}, want: tSelf},
		{name: "two own numbers", identities: []sqlite.Identity{self("e164", tSelf), self("e164", tThirdE164)}, wantErr: true},
		{name: "none", identities: []sqlite.Identity{peer}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AccountNumber(tc.account, tc.identities)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("AccountNumber = %q, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("AccountNumber = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := AccountNumber(sqlite.Account{}, nil); !errors.Is(err, ErrNoAccountNumber) {
		t.Fatalf("err = %v, want ErrNoAccountNumber", err)
	}
}
