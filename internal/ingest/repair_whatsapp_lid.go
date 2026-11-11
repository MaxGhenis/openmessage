package ingest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/whatsapplid"
	"go.mau.fi/whatsmeow/types"
)

// WhatsApp LID repair: e164 identities whose digits are a LID.
//
// Before the legacy bridge and the WhatsApp Desktop importer stored a LID by
// its JID, both stored some LIDs as "+<lid digits>" (jidToPhone kept any
// all-digit user), and migration minted those as e164 identities. The digits
// alone cannot tell such an identity from a real phone number, so the repair
// acts only on evidence that keeps the JID's server: the outside records the
// caller reads (whatsapplid: the whatsmeow session store, WhatsApp Desktop)
// and the store's own LID identities and chats. An identity is rewritten only
// when a record names its digits as a LID and none names them as a phone
// number. It becomes the form v2 ingest gives a LID: the phone number a record
// pairs the LID with when exactly one is named, else the LID itself
// ("<digits>@lid"), merging into that identity when it already exists. Live
// ingest knows only whatsmeow's pairs, so a pair only Desktop records can leave
// live frames naming the LID until whatsmeow learns it.

// In-store evidence sources.
const (
	lidSourceStoreIdentity = "v2_lid_identity"
	lidSourceStoreChat     = "v2_chat"
	lidSourceStoreSelf     = "v2_own_number"
	lidSourceStoreNumber   = "v2_number_on_" // + the other account's platform
)

// Candidate verdicts. Only lidVerdictRewrite changes the store.
const (
	lidVerdictRewrite     = "rewrite"
	lidVerdictAlsoAPhone  = "also-a-phone-number"
	lidVerdictOwnIdentity = "own-identity"
	lidVerdictTargetIsOwn = "target-is-own-identity"
	lidVerdictOtherPerson = "target-links-another-person"
)

// WhatsAppLIDRepairOptions scopes a WhatsApp LID repair to one account.
// Evidence holds what outside records say (nil for none) and Sources reports
// how reading them went.
type WhatsAppLIDRepairOptions struct {
	AccountID string
	Evidence  *whatsapplid.Evidence
	Sources   []whatsapplid.SourceReport
}

// WhatsAppLIDRepairCandidate is one e164 identity some record names as a LID,
// and what the repair does with it.
type WhatsAppLIDRepairCandidate struct {
	IdentityID           string               `json:"identity_id"`
	CanonicalValue       string               `json:"canonical_value"`
	DisplayName          string               `json:"display_name,omitempty"`
	Verdict              string               `json:"verdict"`
	LIDSources           []string             `json:"lid_sources"`
	PhoneSources         []string             `json:"phone_sources,omitempty"`
	PairedNumbers        []string             `json:"paired_numbers,omitempty"`
	TargetKind           string               `json:"target_kind,omitempty"`
	TargetCanonicalValue string               `json:"target_canonical_value,omitempty"`
	TargetIdentityID     string               `json:"target_identity_id,omitempty"`
	TargetExists         bool                 `json:"target_exists,omitempty"`
	RawNames             []string             `json:"raw_names,omitempty"`
	Usage                sqlite.IdentityUsage `json:"usage"`
	Detail               string               `json:"detail,omitempty"`
}

// WhatsAppLIDRepairReport is a dry-run plan: ApplyWhatsAppLIDRepair applies
// Rewrites, in order.
type WhatsAppLIDRepairReport struct {
	AccountID        string                       `json:"account_id"`
	Sources          []whatsapplid.SourceReport   `json:"sources"`
	PhoneIdentities  int                          `json:"phone_identities"`
	Candidates       []WhatsAppLIDRepairCandidate `json:"candidates"`
	Rewrites         []sqlite.IdentityRewrite     `json:"rewrites"`
	ToNumber         int                          `json:"to_number"`
	ToLID            int                          `json:"to_lid"`
	MergedIntoTarget int                          `json:"merged_into_target"`
	Refused          int                          `json:"refused"`
}

// PlanWhatsAppLIDRepair inspects every e164 identity of a WhatsApp account
// and plans a rewrite for each one the evidence shows is a LID. The plan is
// pure: nothing is written until ApplyWhatsAppLIDRepair.
func PlanWhatsAppLIDRepair(
	ctx context.Context,
	store *sqlite.Store,
	opts WhatsAppLIDRepairOptions,
) (WhatsAppLIDRepairReport, error) {
	if store == nil {
		return WhatsAppLIDRepairReport{}, errors.New("plan whatsapp lid repair: store is nil")
	}
	accountID := strings.TrimSpace(opts.AccountID)
	if accountID == "" {
		return WhatsAppLIDRepairReport{}, errors.New("plan whatsapp lid repair: account is empty")
	}
	account, err := store.GetAccount(accountID)
	if err != nil {
		return WhatsAppLIDRepairReport{}, fmt.Errorf("plan whatsapp lid repair: %w", err)
	}
	if platformForBridgeKey(account.BridgeKey) != bridge.PlatformWhatsApp {
		return WhatsAppLIDRepairReport{}, fmt.Errorf("plan whatsapp lid repair: account %q is a %s account", accountID, account.BridgeKey)
	}
	report := WhatsAppLIDRepairReport{
		AccountID:  accountID,
		Sources:    append([]whatsapplid.SourceReport(nil), opts.Sources...),
		Candidates: []WhatsAppLIDRepairCandidate{},
		Rewrites:   []sqlite.IdentityRewrite{},
	}

	evidence := whatsapplid.NewEvidence()
	evidence.Merge(opts.Evidence)
	identities, err := addStoreLIDEvidence(store, accountID, evidence)
	if err != nil {
		return WhatsAppLIDRepairReport{}, fmt.Errorf("plan whatsapp lid repair: %w", err)
	}

	byKey := make(map[v2keys.Identity]sqlite.Identity, len(identities))
	for _, identity := range identities {
		byKey[v2keys.Identity{AccountID: accountID, Kind: string(identity.Kind), Canonical: identity.CanonicalValue}] = identity
	}
	// planned holds targets an earlier rewrite in this plan creates, so a
	// second LID paired with the same number is reported as a merge.
	planned := map[v2keys.Identity]string{}
	// people holds the person each target links to once the rewrites planned
	// so far are applied ("" for none), read from the store on first use.
	people := map[v2keys.Identity]string{}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].CanonicalValue != identities[j].CanonicalValue {
			return identities[i].CanonicalValue < identities[j].CanonicalValue
		}
		return identities[i].IdentityID < identities[j].IdentityID
	})
	for _, identity := range identities {
		if identity.Kind != "e164" {
			continue
		}
		report.PhoneIdentities++
		digits := strings.TrimPrefix(identity.CanonicalValue, "+")
		if !evidence.IsLID(digits) {
			continue
		}
		candidate, rewrite, err := planLIDCandidate(store, accountID, identity, digits, evidence, byKey, planned, people)
		if err != nil {
			return WhatsAppLIDRepairReport{}, fmt.Errorf("plan whatsapp lid repair: identity %q: %w", identity.IdentityID, err)
		}
		report.Candidates = append(report.Candidates, candidate)
		if rewrite == nil {
			report.Refused++
			continue
		}
		report.Rewrites = append(report.Rewrites, *rewrite)
		if rewrite.ToKind == "e164" {
			report.ToNumber++
		} else {
			report.ToLID++
		}
		if candidate.TargetExists {
			report.MergedIntoTarget++
		}
		planned[v2keys.Identity{AccountID: accountID, Kind: string(rewrite.ToKind), Canonical: rewrite.ToCanonicalValue}] = rewrite.ToIdentityID
	}
	return report, nil
}

// addStoreLIDEvidence adds what the store itself records with a server: the
// WhatsApp account's LID identities and its chats' JIDs, its own numbers, and
// the e164 identities of every account on another platform (a number someone
// texts from is a phone number). Another WhatsApp account's e164 identities
// say nothing: they may hold LIDs the same way. It returns the account's
// identities.
func addStoreLIDEvidence(store *sqlite.Store, accountID string, evidence *whatsapplid.Evidence) ([]sqlite.Identity, error) {
	accounts, err := store.ListAccounts()
	if err != nil {
		return nil, err
	}
	var own []sqlite.Identity
	for _, account := range accounts {
		identities, err := store.ListIdentities(account.AccountID)
		if err != nil {
			return nil, err
		}
		if account.AccountID != accountID {
			platform := platformForBridgeKey(account.BridgeKey)
			if platform == bridge.PlatformWhatsApp {
				continue
			}
			source := lidSourceStoreNumber + string(platform)
			for _, identity := range identities {
				if identity.Kind == "e164" {
					evidence.AddPhone(strings.TrimPrefix(identity.CanonicalValue, "+"), source)
				}
			}
			continue
		}
		own = identities
		for _, identity := range identities {
			switch {
			case identity.Kind == "jid":
				evidence.AddJID(identity.CanonicalValue, lidSourceStoreIdentity)
			case identity.Kind == "e164" && identity.IsSelf:
				evidence.AddPhone(strings.TrimPrefix(identity.CanonicalValue, "+"), lidSourceStoreSelf)
			}
		}
	}
	conversations, err := store.ListConversationsByRecency(accountID)
	if err != nil {
		return nil, err
	}
	for _, conversation := range conversations {
		evidence.AddJID(strings.TrimPrefix(conversation.RemoteConversationID, "whatsapp:"), lidSourceStoreChat)
	}
	return own, nil
}

func planLIDCandidate(
	store *sqlite.Store,
	accountID string,
	identity sqlite.Identity,
	digits string,
	evidence *whatsapplid.Evidence,
	byKey map[v2keys.Identity]sqlite.Identity,
	planned map[v2keys.Identity]string,
	people map[v2keys.Identity]string,
) (WhatsAppLIDRepairCandidate, *sqlite.IdentityRewrite, error) {
	usage, err := store.IdentityUsage(accountID, identity.IdentityID)
	if err != nil {
		return WhatsAppLIDRepairCandidate{}, nil, err
	}
	candidate := WhatsAppLIDRepairCandidate{
		IdentityID:     identity.IdentityID,
		CanonicalValue: identity.CanonicalValue,
		DisplayName:    identity.DisplayName,
		LIDSources:     evidence.LIDSources(digits),
		PhoneSources:   evidence.PhoneSources(digits),
		PairedNumbers:  evidence.PhonesForLID(digits),
		Usage:          usage,
	}
	if identity.IsSelf {
		candidate.Verdict = lidVerdictOwnIdentity
		candidate.Detail = "this account's own identity is never rewritten"
		return candidate, nil, nil
	}
	if len(candidate.PhoneSources) > 0 {
		candidate.Verdict = lidVerdictAlsoAPhone
		candidate.Detail = "records name these digits both as a LID and as a phone number"
		return candidate, nil, nil
	}

	targetJID := digits + "@" + types.HiddenUserServer
	if number, ok := evidence.PhoneForLID(digits); ok {
		targetJID = number + "@" + types.DefaultUserServer
	} else if len(candidate.PairedNumbers) > 0 {
		candidate.Detail = "records pair the LID with numbers that disagree or are LIDs too; kept as the LID"
	}
	raw := whatsAppIdentity(targetJID, "", false).Raw
	key, err := v2keys.IdentityKey(accountID, string(bridge.PlatformWhatsApp), raw)
	if err != nil {
		return WhatsAppLIDRepairCandidate{}, nil, fmt.Errorf("derive the identity of %q: %w", targetJID, err)
	}
	candidate.TargetKind = key.Kind
	candidate.TargetCanonicalValue = key.Canonical
	targetID := v2keys.DeriveID("identity", accountID, key.Kind+"\x1f"+key.Canonical)
	if existing, ok := byKey[key]; ok {
		targetID = existing.IdentityID
		candidate.TargetExists = true
		if existing.IsSelf {
			candidate.Verdict = lidVerdictTargetIsOwn
			candidate.TargetIdentityID = targetID
			candidate.Detail = "the LID's number is this account's own"
			return candidate, nil, nil
		}
	} else if id, ok := planned[key]; ok {
		targetID = id
		candidate.TargetExists = true
	}
	candidate.TargetIdentityID = targetID
	targetPerson, known := people[key]
	if !known && candidate.TargetExists && planned[key] == "" {
		link, err := store.GetPersonIdentity(targetID)
		switch {
		case errors.Is(err, sqlite.ErrNotFound):
		case err != nil:
			return WhatsAppLIDRepairCandidate{}, nil, err
		default:
			targetPerson = link.PersonID
		}
	}
	if usage.PersonID != "" && targetPerson != "" && targetPerson != usage.PersonID {
		candidate.Verdict = lidVerdictOtherPerson
		candidate.Detail = fmt.Sprintf("linked to person %s, the target to person %s", usage.PersonID, targetPerson)
		return candidate, nil, nil
	}
	if targetPerson == "" {
		targetPerson = usage.PersonID
	}
	people[key] = targetPerson

	// The names that only repeat the LID's digits, on either identity: the
	// target may carry one too (a pre-fix copy of the same mistake).
	names := append([]string{identity.DisplayName}, usage.ParticipantNames...)
	names = append(names, usage.DirectTitles...)
	if existing, ok := byKey[key]; ok {
		targetUsage, err := store.IdentityUsage(accountID, existing.IdentityID)
		if err != nil {
			return WhatsAppLIDRepairCandidate{}, nil, err
		}
		names = append(names, existing.DisplayName)
		names = append(names, targetUsage.ParticipantNames...)
		names = append(names, targetUsage.DirectTitles...)
	}
	seen := map[string]struct{}{}
	for _, name := range names {
		if _, dup := seen[name]; dup || !isRawUserName(name, digits) {
			continue
		}
		seen[name] = struct{}{}
		candidate.RawNames = append(candidate.RawNames, name)
	}
	sort.Strings(candidate.RawNames)
	candidate.Verdict = lidVerdictRewrite
	return candidate, &sqlite.IdentityRewrite{
		FromIdentityID:     identity.IdentityID,
		FromKind:           identity.Kind,
		FromCanonicalValue: identity.CanonicalValue,
		ToIdentityID:       targetID,
		ToKind:             sqlite.IdentityKind(key.Kind),
		ToCanonicalValue:   key.Canonical,
		ToRawValue:         raw,
		RawNames:           candidate.RawNames,
	}, nil
}

// isRawUserName reports whether name only repeats a user part: "+<digits>",
// "<digits>@lid" or the digits, with or without phone-number spacing.
func isRawUserName(name, digits string) bool {
	value := strings.TrimSpace(name)
	if value == "" || digits == "" {
		return false
	}
	if strings.HasSuffix(strings.ToLower(value), "@"+types.HiddenUserServer) {
		value = value[:len(value)-len(types.HiddenUserServer)-1]
	}
	value = strings.TrimPrefix(value, "+")
	var kept strings.Builder
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			kept.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.' || r == ' ':
		default:
			return false
		}
	}
	return kept.String() == digits
}

// ApplyWhatsAppLIDRepair applies a plan atomically.
func ApplyWhatsAppLIDRepair(
	ctx context.Context,
	store *sqlite.Store,
	report WhatsAppLIDRepairReport,
	now time.Time,
) error {
	if len(report.Rewrites) == 0 {
		return nil
	}
	return store.ApplyIdentityRewrites(ctx, report.AccountID, report.Rewrites, now.UnixMilli())
}
