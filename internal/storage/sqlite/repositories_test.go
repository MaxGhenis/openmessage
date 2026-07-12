package sqlite

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

const repositoryTestTimeMS int64 = 1_800_000_000_000

func TestRepositoriesRoundTripRows(t *testing.T) {
	store := openRepositoryTestStore(t)

	remoteAccountID := "remote-account"
	account := Account{
		AccountID:       "account-a",
		BridgeKey:       "signal",
		RemoteAccountID: &remoteAccountID,
		DisplayName:     "Signal archive",
		Mode:            AccountModeArchive,
		Enabled:         false,
		ConfigJSON:      `{"region":"us"}`,
		CreatedAtMS:     repositoryTestTimeMS,
		UpdatedAtMS:     repositoryTestTimeMS + 1,
	}
	mustRepositoryWrite(t, "UpsertAccount", store.UpsertAccount(account))
	gotAccount, err := store.GetAccount(account.AccountID)
	mustRepositoryRead(t, "GetAccount", err)
	assertRepositoryEqual(t, "account", gotAccount, account)
	accounts, err := store.ListAccounts()
	mustRepositoryRead(t, "ListAccounts", err)
	assertRepositoryEqual(t, "accounts", accounts, []Account{account})

	remoteDeviceID := "remote-device"
	lastSeenAtMS := repositoryTestTimeMS + 2
	device := Device{
		DeviceID:       "device-a",
		AccountID:      account.AccountID,
		RemoteDeviceID: &remoteDeviceID,
		Kind:           DeviceKindRemoteEndpoint,
		DisplayName:    "Phone",
		State:          DeviceStateUnknown,
		IsCurrent:      true,
		LastSeenAtMS:   &lastSeenAtMS,
		CreatedAtMS:    repositoryTestTimeMS + 2,
		UpdatedAtMS:    repositoryTestTimeMS + 3,
	}
	mustRepositoryWrite(t, "UpsertDevice", store.UpsertDevice(device))
	gotDevice, err := store.GetDevice(device.DeviceID)
	mustRepositoryRead(t, "GetDevice", err)
	assertRepositoryEqual(t, "device", gotDevice, device)
	devices, err := store.ListDevices(account.AccountID)
	mustRepositoryRead(t, "ListDevices", err)
	assertRepositoryEqual(t, "devices", devices, []Device{device})

	identity := Identity{
		IdentityID:     "identity-a",
		AccountID:      account.AccountID,
		Kind:           IdentityKind("email"),
		CanonicalValue: "alice@example.com",
		RawValue:       "Alice <alice@example.com>",
		DisplayName:    "Alice",
		IsSelf:         true,
		MetadataJSON:   `{"verified":true}`,
		CreatedAtMS:    repositoryTestTimeMS + 4,
		UpdatedAtMS:    repositoryTestTimeMS + 5,
	}
	mustRepositoryWrite(t, "UpsertIdentity", store.UpsertIdentity(identity))
	gotIdentity, err := store.GetIdentity(identity.IdentityID)
	mustRepositoryRead(t, "GetIdentity", err)
	assertRepositoryEqual(t, "identity", gotIdentity, identity)
	canonicalIdentity, err := store.GetIdentityByCanonical(
		account.AccountID,
		identity.Kind,
		identity.CanonicalValue,
	)
	mustRepositoryRead(t, "GetIdentityByCanonical", err)
	assertRepositoryEqual(t, "canonical identity", canonicalIdentity, identity)
	identities, err := store.ListIdentities(account.AccountID)
	mustRepositoryRead(t, "ListIdentities", err)
	assertRepositoryEqual(t, "identities", identities, []Identity{identity})

	parent := Person{
		PersonID:    "person-parent",
		DisplayName: "Parent",
		SortName:    "Parent",
		CreatedAtMS: repositoryTestTimeMS + 6,
		UpdatedAtMS: repositoryTestTimeMS + 6,
	}
	mustRepositoryWrite(t, "CreatePerson parent", store.CreatePerson(parent))
	person := Person{
		PersonID:           "person-a",
		DisplayName:        "Alice",
		SortName:           "Example, Alice",
		MergedIntoPersonID: stringPointer(parent.PersonID),
		CreatedAtMS:        repositoryTestTimeMS + 7,
		UpdatedAtMS:        repositoryTestTimeMS + 8,
	}
	mustRepositoryWrite(t, "CreatePerson", store.CreatePerson(person))
	gotPerson, err := store.GetPerson(person.PersonID)
	mustRepositoryRead(t, "GetPerson", err)
	assertRepositoryEqual(t, "person", gotPerson, person)
	people, err := store.ListPeople()
	mustRepositoryRead(t, "ListPeople", err)
	assertRepositoryContains(t, "people", people, person)

	personIdentity := PersonIdentity{
		IdentityID: identity.IdentityID,
		PersonID:   person.PersonID,
		Provenance: IdentityProvenanceAddressBook,
		Confidence: 0.875,
		IsPrimary:  true,
		LinkedAtMS: repositoryTestTimeMS + 9,
	}
	mustRepositoryWrite(
		t,
		"LinkIdentityToPerson",
		store.LinkIdentityToPerson(personIdentity),
	)
	gotPersonIdentity, err := store.GetPersonIdentity(identity.IdentityID)
	mustRepositoryRead(t, "GetPersonIdentity", err)
	assertRepositoryEqual(t, "person identity", gotPersonIdentity, personIdentity)
	personIdentities, err := store.ListPersonIdentities(person.PersonID)
	mustRepositoryRead(t, "ListPersonIdentities", err)
	assertRepositoryEqual(
		t,
		"person identities",
		personIdentities,
		[]PersonIdentity{personIdentity},
	)
	identityPerson, err := store.GetPersonForIdentity(identity.IdentityID)
	mustRepositoryRead(t, "GetPersonForIdentity", err)
	assertRepositoryEqual(t, "person for identity", identityPerson, person)

	remoteRevision := "revision-7"
	archivedAtMS := repositoryTestTimeMS + 10
	conversation := Conversation{
		ConversationID:       "conversation-a",
		AccountID:            account.AccountID,
		RemoteConversationID: "remote-conversation-a",
		Kind:                 ConversationKindBroadcast,
		Title:                "Announcements",
		RemoteRevision:       &remoteRevision,
		NotificationMode:     NotificationModeMuted,
		IsFavorite:           true,
		ArchivedAtMS:         &archivedAtMS,
		LastMessageAtMS:      repositoryTestTimeMS + 11,
		MetadataJSON:         `{"color":"blue"}`,
		CreatedAtMS:          repositoryTestTimeMS + 10,
		UpdatedAtMS:          repositoryTestTimeMS + 12,
	}
	mustRepositoryWrite(t, "UpsertConversation", store.UpsertConversation(conversation))
	gotConversation, err := store.GetConversation(conversation.ConversationID)
	mustRepositoryRead(t, "GetConversation", err)
	assertRepositoryEqual(t, "conversation", gotConversation, conversation)
	conversations, err := store.ListConversationsByRecency(account.AccountID)
	mustRepositoryRead(t, "ListConversationsByRecency", err)
	assertRepositoryEqual(t, "conversations", conversations, []Conversation{conversation})

	joinedAtMS := repositoryTestTimeMS + 13
	leftAtMS := repositoryTestTimeMS + 14
	participant := ConversationParticipant{
		AccountID:      account.AccountID,
		ConversationID: conversation.ConversationID,
		IdentityID:     identity.IdentityID,
		Role:           ParticipantRoleOwner,
		DisplayName:    "Alice in announcements",
		IsActive:       false,
		JoinedAtMS:     &joinedAtMS,
		LeftAtMS:       &leftAtMS,
	}
	mustRepositoryWrite(
		t,
		"ReplaceConversationParticipants",
		store.ReplaceConversationParticipants(conversation.ConversationID, []ConversationParticipant{participant}),
	)
	participants, err := store.ListParticipants(conversation.ConversationID)
	mustRepositoryRead(t, "ListParticipants", err)
	assertRepositoryEqual(
		t,
		"conversation participants",
		participants,
		[]ConversationParticipant{participant},
	)
}

func TestUpsertsPreserveIDsAndCreationTimes(t *testing.T) {
	store := openRepositoryTestStore(t)

	account := Account{
		AccountID:   "account-a",
		BridgeKey:   "signal",
		DisplayName: "Before",
		Mode:        AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: repositoryTestTimeMS,
		UpdatedAtMS: repositoryTestTimeMS,
	}
	mustRepositoryWrite(t, "initial UpsertAccount", store.UpsertAccount(account))
	updatedAccount := account
	updatedAccount.DisplayName = "After"
	updatedAccount.Mode = AccountModeArchive
	updatedAccount.CreatedAtMS += 50
	updatedAccount.UpdatedAtMS += 100
	mustRepositoryWrite(t, "updated UpsertAccount", store.UpsertAccount(updatedAccount))
	updatedAccount.CreatedAtMS = account.CreatedAtMS
	gotAccount, err := store.GetAccount(account.AccountID)
	mustRepositoryRead(t, "GetAccount", err)
	assertRepositoryEqual(t, "updated account", gotAccount, updatedAccount)

	device := Device{
		DeviceID:    "device-a",
		AccountID:   account.AccountID,
		Kind:        DeviceKindLocalInstallation,
		DisplayName: "Before",
		State:       DeviceStateActive,
		IsCurrent:   true,
		CreatedAtMS: repositoryTestTimeMS + 1,
		UpdatedAtMS: repositoryTestTimeMS + 1,
	}
	mustRepositoryWrite(t, "initial UpsertDevice", store.UpsertDevice(device))
	updatedDevice := device
	updatedDevice.DisplayName = "After"
	updatedDevice.State = DeviceStateRevoked
	updatedDevice.CreatedAtMS += 50
	updatedDevice.UpdatedAtMS += 100
	mustRepositoryWrite(t, "updated UpsertDevice", store.UpsertDevice(updatedDevice))
	updatedDevice.CreatedAtMS = device.CreatedAtMS
	gotDevice, err := store.GetDevice(device.DeviceID)
	mustRepositoryRead(t, "GetDevice", err)
	assertRepositoryEqual(t, "updated device", gotDevice, updatedDevice)

	identity := Identity{
		IdentityID:     "identity-original",
		AccountID:      account.AccountID,
		Kind:           IdentityKind("e164"),
		CanonicalValue: "+15550000001",
		RawValue:       "+1 (555) 000-0001",
		DisplayName:    "Before",
		MetadataJSON:   `{}`,
		CreatedAtMS:    repositoryTestTimeMS + 2,
		UpdatedAtMS:    repositoryTestTimeMS + 2,
	}
	mustRepositoryWrite(t, "initial UpsertIdentity", store.UpsertIdentity(identity))
	updatedIdentity := identity
	updatedIdentity.IdentityID = "identity-discarded"
	updatedIdentity.RawValue = "+1 555 000 0001"
	updatedIdentity.DisplayName = "After"
	updatedIdentity.MetadataJSON = `{"source":"sync"}`
	updatedIdentity.CreatedAtMS += 50
	updatedIdentity.UpdatedAtMS += 100
	mustRepositoryWrite(t, "deduplicating UpsertIdentity", store.UpsertIdentity(updatedIdentity))
	updatedIdentity.IdentityID = identity.IdentityID
	updatedIdentity.CreatedAtMS = identity.CreatedAtMS
	gotIdentity, err := store.GetIdentityByCanonical(
		account.AccountID,
		identity.Kind,
		identity.CanonicalValue,
	)
	mustRepositoryRead(t, "GetIdentityByCanonical", err)
	assertRepositoryEqual(t, "deduplicated identity", gotIdentity, updatedIdentity)
	assertRepositoryNotFound(t, "discarded identity ID", func() error {
		_, err := store.GetIdentity("identity-discarded")
		return err
	})
	identities, err := store.ListIdentities(account.AccountID)
	mustRepositoryRead(t, "ListIdentities", err)
	if len(identities) != 1 {
		t.Fatalf("identities after natural-key upsert = %d, want 1", len(identities))
	}

	conversation := Conversation{
		ConversationID:       "conversation-original",
		AccountID:            account.AccountID,
		RemoteConversationID: "remote-conversation",
		Kind:                 ConversationKindDirect,
		Title:                "Before",
		NotificationMode:     NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          repositoryTestTimeMS + 3,
		UpdatedAtMS:          repositoryTestTimeMS + 3,
	}
	mustRepositoryWrite(t, "initial UpsertConversation", store.UpsertConversation(conversation))
	updatedConversation := conversation
	updatedConversation.ConversationID = "conversation-discarded"
	updatedConversation.Title = "After"
	updatedConversation.NotificationMode = NotificationModeMentions
	updatedConversation.LastMessageAtMS = repositoryTestTimeMS + 200
	updatedConversation.CreatedAtMS += 50
	updatedConversation.UpdatedAtMS += 100
	mustRepositoryWrite(
		t,
		"deduplicating UpsertConversation",
		store.UpsertConversation(updatedConversation),
	)
	updatedConversation.ConversationID = conversation.ConversationID
	updatedConversation.CreatedAtMS = conversation.CreatedAtMS
	gotConversation, err := store.GetConversation(conversation.ConversationID)
	mustRepositoryRead(t, "GetConversation", err)
	assertRepositoryEqual(t, "deduplicated conversation", gotConversation, updatedConversation)
	remoteConversation, err := store.GetConversationByRemote(
		conversation.AccountID,
		conversation.RemoteConversationID,
	)
	mustRepositoryRead(t, "GetConversationByRemote", err)
	assertRepositoryEqual(t, "remote conversation", remoteConversation, updatedConversation)
	assertRepositoryNotFound(t, "discarded conversation ID", func() error {
		_, err := store.GetConversation("conversation-discarded")
		return err
	})
	conversations, err := store.ListConversationsByRecency(account.AccountID)
	mustRepositoryRead(t, "ListConversationsByRecency", err)
	if len(conversations) != 1 {
		t.Fatalf("conversations after natural-key upsert = %d, want 1", len(conversations))
	}
}

func TestIdentityLinksHaveOneOwnerAndPeopleNamesAreNotUnique(t *testing.T) {
	store := openRepositoryTestStore(t)
	account := repositoryTestAccount("account-a")
	mustRepositoryWrite(t, "UpsertAccount", store.UpsertAccount(account))
	identity := repositoryTestIdentity("identity-a", account.AccountID, "alice")
	mustRepositoryWrite(t, "UpsertIdentity", store.UpsertIdentity(identity))

	first := Person{
		PersonID:    "person-a",
		DisplayName: "Same Name",
		SortName:    "Name, Same",
		CreatedAtMS: repositoryTestTimeMS + 10,
		UpdatedAtMS: repositoryTestTimeMS + 10,
	}
	second := first
	second.PersonID = "person-b"
	second.CreatedAtMS++
	second.UpdatedAtMS++
	mustRepositoryWrite(t, "CreatePerson first", store.CreatePerson(first))
	mustRepositoryWrite(t, "CreatePerson second", store.CreatePerson(second))
	people, err := store.ListPeople()
	mustRepositoryRead(t, "ListPeople", err)
	if len(people) != 2 {
		t.Fatalf("same-name people = %d, want 2", len(people))
	}
	assertRepositoryContains(t, "same-name people", people, first)
	assertRepositoryContains(t, "same-name people", people, second)

	link := PersonIdentity{
		IdentityID: identity.IdentityID,
		PersonID:   first.PersonID,
		Provenance: IdentityProvenanceExplicit,
		Confidence: 1,
		IsPrimary:  true,
		LinkedAtMS: repositoryTestTimeMS + 20,
	}
	mustRepositoryWrite(t, "first LinkIdentityToPerson", store.LinkIdentityToPerson(link))
	secondLink := link
	secondLink.PersonID = second.PersonID
	secondLink.LinkedAtMS++
	err = store.LinkIdentityToPerson(secondLink)
	if !errors.Is(err, ErrDuplicateIdentityLink) {
		t.Fatalf("second LinkIdentityToPerson error = %v, want ErrDuplicateIdentityLink", err)
	}
	if !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("second LinkIdentityToPerson error = %v, want ErrConstraintViolation", err)
	}
	gotPerson, err := store.GetPersonForIdentity(identity.IdentityID)
	mustRepositoryRead(t, "GetPersonForIdentity", err)
	assertRepositoryEqual(t, "identity owner after rejected relink", gotPerson, first)
}

func TestListConversationsByRecencyScopesOrdersAndBreaksTies(t *testing.T) {
	store := openRepositoryTestStore(t)
	accountA := repositoryTestAccount("account-a")
	accountB := repositoryTestAccount("account-b")
	accountB.BridgeKey = "whatsapp"
	mustRepositoryWrite(t, "UpsertAccount A", store.UpsertAccount(accountA))
	mustRepositoryWrite(t, "UpsertAccount B", store.UpsertAccount(accountB))

	conversation := func(id, accountID string, recency int64) Conversation {
		return Conversation{
			ConversationID:       id,
			AccountID:            accountID,
			RemoteConversationID: "remote-" + id,
			Kind:                 ConversationKindGroup,
			Title:                id,
			NotificationMode:     NotificationModeAll,
			LastMessageAtMS:      recency,
			MetadataJSON:         `{}`,
			CreatedAtMS:          repositoryTestTimeMS,
			UpdatedAtMS:          repositoryTestTimeMS,
		}
	}
	want := []Conversation{
		conversation("conversation-tie-a", accountA.AccountID, 300),
		conversation("conversation-tie-b", accountA.AccountID, 300),
		conversation("conversation-middle", accountA.AccountID, 200),
		conversation("conversation-old", accountA.AccountID, 100),
	}
	for _, item := range []Conversation{want[2], want[1], want[3], want[0]} {
		mustRepositoryWrite(t, "UpsertConversation", store.UpsertConversation(item))
	}
	mustRepositoryWrite(
		t,
		"UpsertConversation other account",
		store.UpsertConversation(conversation("conversation-other", accountB.AccountID, 1_000)),
	)

	got, err := store.ListConversationsByRecency(accountA.AccountID)
	mustRepositoryRead(t, "ListConversationsByRecency", err)
	assertRepositoryEqual(t, "recency ordered conversations", got, want)
}

func TestReplaceConversationParticipantsRejectsInvalidBatchesAtomically(t *testing.T) {
	store := openRepositoryTestStore(t)
	accountA := repositoryTestAccount("account-a")
	accountB := repositoryTestAccount("account-b")
	accountB.BridgeKey = "whatsapp"
	mustRepositoryWrite(t, "UpsertAccount A", store.UpsertAccount(accountA))
	mustRepositoryWrite(t, "UpsertAccount B", store.UpsertAccount(accountB))
	priorIdentity := repositoryTestIdentity("identity-prior", accountA.AccountID, "prior")
	validIdentity := repositoryTestIdentity("identity-valid", accountA.AccountID, "valid")
	crossAccountIdentity := repositoryTestIdentity("identity-cross", accountB.AccountID, "cross")
	for _, identity := range []Identity{priorIdentity, validIdentity, crossAccountIdentity} {
		mustRepositoryWrite(t, "UpsertIdentity", store.UpsertIdentity(identity))
	}
	conversation := Conversation{
		ConversationID:       "conversation-a",
		AccountID:            accountA.AccountID,
		RemoteConversationID: "remote-a",
		Kind:                 ConversationKindDirect,
		NotificationMode:     NotificationModeAll,
		MetadataJSON:         `{}`,
		CreatedAtMS:          repositoryTestTimeMS,
		UpdatedAtMS:          repositoryTestTimeMS,
	}
	mustRepositoryWrite(t, "UpsertConversation", store.UpsertConversation(conversation))
	prior := ConversationParticipant{
		AccountID:      accountA.AccountID,
		ConversationID: conversation.ConversationID,
		IdentityID:     priorIdentity.IdentityID,
		Role:           ParticipantRoleAdmin,
		DisplayName:    "Prior participant",
		IsActive:       true,
	}
	mustRepositoryWrite(
		t,
		"initial ReplaceConversationParticipants",
		store.ReplaceConversationParticipants(conversation.ConversationID, []ConversationParticipant{prior}),
	)
	valid := ConversationParticipant{
		AccountID:      accountA.AccountID,
		ConversationID: conversation.ConversationID,
		IdentityID:     validIdentity.IdentityID,
		Role:           ParticipantRoleMember,
		DisplayName:    "Valid participant",
		IsActive:       true,
	}

	tests := []struct {
		name string
		row  ConversationParticipant
		want error
	}{
		{
			name: "cross account identity",
			row: ConversationParticipant{
				AccountID:      accountA.AccountID,
				ConversationID: conversation.ConversationID,
				IdentityID:     crossAccountIdentity.IdentityID,
				Role:           ParticipantRoleMember,
				IsActive:       true,
			},
			want: ErrCrossAccountParticipant,
		},
		{
			name: "orphan identity",
			row: ConversationParticipant{
				AccountID:      accountA.AccountID,
				ConversationID: conversation.ConversationID,
				IdentityID:     "identity-missing",
				Role:           ParticipantRoleMember,
				IsActive:       true,
			},
			want: ErrOrphanParticipantIdentity,
		},
		{
			name: "mismatched conversation",
			row: ConversationParticipant{
				AccountID:      accountA.AccountID,
				ConversationID: "conversation-other",
				IdentityID:     validIdentity.IdentityID,
				Role:           ParticipantRoleMember,
				IsActive:       true,
			},
			want: ErrInvalidConversationParticipant,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := store.ReplaceConversationParticipants(
				conversation.ConversationID,
				[]ConversationParticipant{valid, test.row},
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("ReplaceConversationParticipants error = %v, want %v", err, test.want)
			}
			if !errors.Is(err, ErrInvalidConversationParticipant) {
				t.Fatalf("ReplaceConversationParticipants error = %v, want ErrInvalidConversationParticipant", err)
			}
			if !errors.Is(err, ErrConstraintViolation) {
				t.Fatalf("ReplaceConversationParticipants error = %v, want ErrConstraintViolation", err)
			}
			got, err := store.ListParticipants(conversation.ConversationID)
			mustRepositoryRead(t, "ListParticipants after rejected batch", err)
			assertRepositoryEqual(
				t,
				"participants after rejected batch",
				got,
				[]ConversationParticipant{prior},
			)
		})
	}

	mustRepositoryWrite(
		t,
		"empty ReplaceConversationParticipants",
		store.ReplaceConversationParticipants(conversation.ConversationID, nil),
	)
	got, err := store.ListParticipants(conversation.ConversationID)
	mustRepositoryRead(t, "ListParticipants after clear", err)
	if len(got) != 0 {
		t.Fatalf("participants after empty replace = %d, want 0", len(got))
	}
}

func TestRepositoryErrorsAreTyped(t *testing.T) {
	store := openRepositoryTestStore(t)
	assertRepositoryNotFound(t, "missing account", func() error {
		_, err := store.GetAccount("missing-account")
		return err
	})

	invalid := repositoryTestAccount("account-invalid")
	invalid.Mode = AccountMode("invalid")
	if err := store.UpsertAccount(invalid); !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("UpsertAccount invalid mode error = %v, want ErrConstraintViolation", err)
	}
}

func openRepositoryTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite3"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	return store
}

func repositoryTestAccount(id string) Account {
	return Account{
		AccountID:   id,
		BridgeKey:   "signal",
		Mode:        AccountModeLive,
		Enabled:     true,
		ConfigJSON:  `{}`,
		CreatedAtMS: repositoryTestTimeMS,
		UpdatedAtMS: repositoryTestTimeMS,
	}
}

func repositoryTestIdentity(id, accountID, canonical string) Identity {
	return Identity{
		IdentityID:     id,
		AccountID:      accountID,
		Kind:           IdentityKind("test"),
		CanonicalValue: canonical,
		RawValue:       canonical,
		MetadataJSON:   `{}`,
		CreatedAtMS:    repositoryTestTimeMS,
		UpdatedAtMS:    repositoryTestTimeMS,
	}
}

func assertRepositoryNotFound(t *testing.T, name string, operation func() error) {
	t.Helper()
	if err := operation(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%s error = %v, want ErrNotFound", name, err)
	}
}

func mustRepositoryWrite(t *testing.T, name string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func mustRepositoryRead(t *testing.T, name string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func assertRepositoryEqual[T any](t *testing.T, name string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %#v, want %#v", name, got, want)
	}
}

func assertRepositoryContains[T any](t *testing.T, name string, got []T, want T) {
	t.Helper()
	for _, item := range got {
		if reflect.DeepEqual(item, want) {
			return
		}
	}
	t.Fatalf("%s = %#v, does not contain %#v", name, got, want)
}

func stringPointer(value string) *string {
	return &value
}
