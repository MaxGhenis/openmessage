package ingest

import (
	"math/rand"
	"reflect"
	"strconv"
	"testing"
	"testing/quick"

	"go.mau.fi/whatsmeow/types"

	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/whatsapplid"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
)

// Three paths name a WhatsApp sender: v2 live ingest (whatsAppIdentity), the
// legacy bridge (whatsapplive.StoredAddress, which migration turns into an
// identity) and the WhatsApp Desktop importer (whatsapplid's Address with no
// pairs). For any sender's JID all three must reach the same v2 identity, or
// one person splits in two after cutover; before the fix the legacy and
// importer paths named a LID "+<digits>" where live ingest names it
// "<digits>@lid".

type differentialJID struct{ JID types.JID }

func (differentialJID) Generate(r *rand.Rand, _ int) reflect.Value {
	servers := []string{types.DefaultUserServer, types.HiddenUserServer, types.HostedServer, types.HostedLIDServer, types.BotServer, types.NewsletterServer}
	user := strconv.FormatInt(1+r.Int63n(1_000_000_000_000_000), 10)
	jid := types.NewJID(user, servers[r.Intn(len(servers))])
	if r.Intn(3) == 0 {
		jid.Device = uint16(1 + r.Intn(20))
	}
	return reflect.ValueOf(differentialJID{JID: jid})
}

func TestWhatsAppPersonIdentityAgreesAcrossIngestPaths(t *testing.T) {
	key := func(raw string) v2keys.Identity {
		identity, err := v2keys.IdentityKey("whatsapp-primary", "whatsapp", raw)
		if err != nil {
			t.Fatalf("IdentityKey(%q): %v", raw, err)
		}
		return identity
	}
	check := func(input differentialJID) bool {
		live := key(whatsAppIdentity(input.JID.String(), "", false).Raw)
		legacy := key(whatsapplive.StoredAddress(input.JID))
		if live != legacy {
			t.Errorf("%s: live ingest %+v, legacy bridge %+v", input.JID, live, legacy)
			return false
		}
		if imported := key(whatsapplid.NewEvidence().Address(input.JID.String())); live != imported {
			t.Errorf("%s: live ingest %+v, Desktop import %+v", input.JID, live, imported)
			return false
		}
		return true
	}
	if err := quick.Check(check, &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(20261021))}); err != nil {
		t.Fatal(err)
	}
}
