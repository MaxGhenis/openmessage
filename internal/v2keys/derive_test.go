package v2keys

import (
	"reflect"
	"testing"
)

func TestDeriveIDMigrationFixtureGoldens(t *testing.T) {
	t.Parallel()

	// These values were produced by migration's pre-extraction deriveID. The
	// corpus spans every account and derived entity used by the live fixtures.
	tests := []struct {
		name       string
		entity     string
		accountID  string
		naturalKey string
		want       string
	}{
		{name: "google account", entity: "account", naturalKey: "google-primary", want: "1ead391d67780be44786c24a64c24c24"},
		{name: "google conversation", entity: "conversation", accountID: "google-primary", naturalKey: "sms-conversation", want: "e5abadc3f4832c388c253f721477bd69"},
		{name: "google identity", entity: "identity", accountID: "google-primary", naturalKey: "e164\x1f+15550100001", want: "04d918d802930bf35edc293767526613"},
		{name: "google message", entity: "message", accountID: "google-primary", naturalKey: "sms-conversation\x1fgoogle-server-1", want: "5b66e34164c6cc052fd1dd40eb5975be"},
		{name: "google device", entity: "device", accountID: "google-primary", naturalKey: "google-primary\x1flocal", want: "9bcc134365b6f21de496ec1693b68421"},
		{name: "whatsapp account", entity: "account", naturalKey: "whatsapp-primary", want: "6b76048039141e9bd397ca32918b093b"},
		{name: "whatsapp conversation", entity: "conversation", accountID: "whatsapp-primary", naturalKey: "whatsapp:fixture-group@g.us", want: "21126fe0e64728af81ec3524454e59bc"},
		{name: "whatsapp identity", entity: "identity", accountID: "whatsapp-primary", naturalKey: "jid\x1f15550100002@s.whatsapp.net", want: "3e6c1c89010c1ddb057ce4a17ac39d7c"},
		{name: "whatsapp message", entity: "message", accountID: "whatsapp-primary", naturalKey: "whatsapp:fixture-group@g.us\x1fwa-source-1", want: "9c657e0bffa05e3baa7299982adb5742"},
		{name: "whatsapp device", entity: "device", accountID: "whatsapp-primary", naturalKey: "whatsapp-primary\x1flocal", want: "1aa97228aad2f8a40a7eea579625d579"},
		{name: "signal account", entity: "account", naturalKey: "signal-primary", want: "8eaaaab3cf32c276b14075f8d9531788"},
		{name: "signal conversation", entity: "conversation", accountID: "signal-primary", naturalKey: "signal:+16505550100", want: "e7167f0610029446f589ea5373ead528"},
		{name: "signal e164 identity", entity: "identity", accountID: "signal-primary", naturalKey: "e164\x1f+16505550100", want: "0e611895ca81bcc570dfe8754f0c55da"},
		{name: "signal aci identity", entity: "identity", accountID: "signal-primary", naturalKey: "signal_aci\x1f11111111-2222-3333-4444-555555555555", want: "3e1fff6f5b7bd9c20293dde8f8c8a60d"},
		{name: "signal incoming message", entity: "message", accountID: "signal-primary", naturalKey: "signal:+16505550100\x1f1700000001000", want: "ad474018087651496559b5faa428554a"},
		{name: "signal local message", entity: "message", accountID: "signal-primary", naturalKey: "signal:+16505550100\x1flocal:abc123", want: "eff6af5e8d89176e528cfb0adf9f25a8"},
		{name: "signal device", entity: "device", accountID: "signal-primary", naturalKey: "signal-primary\x1flocal", want: "f5725a0b516450efeab65b0ccdc9d041"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DeriveID(test.entity, test.accountID, test.naturalKey); got != test.want {
				t.Fatalf("DeriveID(%q, %q, %q) = %q, want pre-extraction golden %q", test.entity, test.accountID, test.naturalKey, got, test.want)
			}
		})
	}
}

func TestIdentityKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		accountID string
		platform  string
		raw       string
		want      Identity
		wantErr   string
	}{
		{name: "empty", accountID: "google-primary", platform: "sms", raw: " \t", wantErr: "identity value is empty"},
		{name: "formatted e164", accountID: "google-primary", platform: "sms", raw: " +1 (555) 010-0001 ", want: Identity{AccountID: "google-primary", Kind: "e164", Canonical: "+15550100001"}},
		{name: "invalid e164", accountID: "signal-primary", platform: "signal", raw: "+---", wantErr: `invalid E.164 identity "+---"`},
		{name: "signal aci", accountID: "signal-primary", platform: "signal", raw: " ABCDEF12-3456-7890-ABCD-EF1234567890 ", want: Identity{AccountID: "signal-primary", Kind: "signal_aci", Canonical: "abcdef12-3456-7890-abcd-ef1234567890"}},
		{name: "signal platform comparison remains exact", accountID: "signal-primary", platform: "Signal", raw: "ABCDEF12-3456-7890-ABCD-EF1234567890", want: Identity{AccountID: "signal-primary", Kind: "username", Canonical: "ABCDEF12-3456-7890-ABCD-EF1234567890"}},
		{name: "whatsapp jid", accountID: "whatsapp-primary", platform: "whatsapp", raw: " USER@S.WHATSAPP.NET ", want: Identity{AccountID: "whatsapp-primary", Kind: "jid", Canonical: "user@s.whatsapp.net"}},
		{name: "email", accountID: "gchat-archive", platform: "gchat", raw: " Friend@Example.COM ", want: Identity{AccountID: "gchat-archive", Kind: "email", Canonical: "friend@example.com"}},
		{name: "legacy participant", accountID: "imessage-archive", platform: "imessage", raw: "legacy-participant:42", want: Identity{AccountID: "imessage-archive", Kind: "legacy_participant", Canonical: "legacy-participant:42"}},
		{name: "username preserves case", accountID: "signal-primary", platform: "signal", raw: " MixedCaseHandle ", want: Identity{AccountID: "signal-primary", Kind: "username", Canonical: "MixedCaseHandle"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := IdentityKey(test.accountID, test.platform, test.raw)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("IdentityKey() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("IdentityKey() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("IdentityKey() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestNormalizeRemoteConversationID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, platform, legacyID, want string
	}{
		{name: "google trims outer whitespace", platform: "sms", legacyID: "  sms-conversation  ", want: "sms-conversation"},
		{name: "whatsapp preserves canonical form", platform: "whatsapp", legacyID: "  whatsapp:15550100002@s.whatsapp.net  ", want: "whatsapp:15550100002@s.whatsapp.net"},
		{name: "signal address", platform: "signal", legacyID: " signal:  +16505550100  ", want: "signal:+16505550100"},
		{name: "signal group", platform: "signal", legacyID: " signal-group:  group-id  ", want: "signal-group:group-id"},
		{name: "signal unprefixed", platform: "signal", legacyID: "  unprefixed  ", want: "unprefixed"},
		{name: "platform comparison remains exact", platform: "Signal", legacyID: " signal:  +16505550100  ", want: "signal:  +16505550100"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := NormalizeRemoteConversationID(test.platform, test.legacyID); got != test.want {
				t.Fatalf("NormalizeRemoteConversationID(%q, %q) = %q, want %q", test.platform, test.legacyID, got, test.want)
			}
		})
	}
}

func TestSignalIncomingSourceIDSignallivePin(t *testing.T) {
	t.Parallel()

	// The expected SHA-1 strings were captured from signallive's original
	// signalIncomingSourceID implementation. Keep that package untouched: this
	// table is the cross-package pin for its (conversation, source, timestamp)
	// algorithm.
	tests := []struct {
		name           string
		conversationID string
		source         string
		timestamp      int64
		want           string
	}{
		{name: "direct e164", conversationID: "signal:+16505550100", source: "+16505550100", timestamp: 1_700_000_001_000, want: "47b5e94ed638811b34db7cb7c6a8c946e60c8af9"},
		{name: "group aci", conversationID: "signal-group:group-fixture", source: "11111111-2222-3333-4444-555555555555", timestamp: 1_700_000_002_000, want: "f5c371a6b529e426f63e6c9fea72447837705c1e"},
		{name: "trims fields", conversationID: "  signal:+16505550100  ", source: "  +16505550100  ", timestamp: 1_700_000_001_000, want: "47b5e94ed638811b34db7cb7c6a8c946e60c8af9"},
		{name: "zero timestamp", conversationID: "signal:+15550000000", source: "source-aci", timestamp: 0, want: "24a8f375364d0fb84e43e655d091de8c0c40d4c5"},
		{name: "signed timestamp", conversationID: "signal-group: spaced group ", source: " sender ", timestamp: -42, want: "31c28133a5d453dc70fbc87fd577ed7540ef4a31"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := SignalIncomingSourceID(test.conversationID, test.source, test.timestamp); got != test.want {
				t.Fatalf("SignalIncomingSourceID(%q, %q, %d) = %q, want signallive pin %q", test.conversationID, test.source, test.timestamp, got, test.want)
			}
		})
	}
}

func TestSignalLocalAlias(t *testing.T) {
	t.Parallel()

	const want = "local:053fa0da59a9c2dee79cc0a2b18e4599ad080bf8"
	if got := SignalLocalAlias("signal:+16505550100", 1_700_000_006_000); got != want {
		t.Fatalf("SignalLocalAlias() = %q, want %q", got, want)
	}
}
