package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	moderncsqlite "modernc.org/sqlite"
)

const (
	sqliteConstraintCode           = 19
	sqliteConstraintForeignKeyCode = 787
	sqliteConstraintPrimaryKeyCode = 1555
)

type rowScanner interface {
	Scan(dest ...any) error
}

func mapConstraintError(err error) error {
	if !isSQLiteConstraint(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrConstraintViolation, err)
}

func isSQLiteConstraint(err error) bool {
	code, ok := sqliteErrorCode(err)
	return ok && code&0xff == sqliteConstraintCode
}

func isSQLiteErrorCode(err error, want int) bool {
	code, ok := sqliteErrorCode(err)
	return ok && code == want
}

func sqliteErrorCode(err error) (int, bool) {
	var sqliteErr *moderncsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return 0, false
	}
	return sqliteErr.Code(), true
}

func notFound(entity, id string) error {
	return fmt.Errorf("%s %q: %w", entity, id, ErrNotFound)
}

func collectRows[T any](rows *sql.Rows, scan func(rowScanner) (T, error)) ([]T, error) {
	defer rows.Close()

	items := make([]T, 0)
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

const accountColumns = `
	account_id,
	bridge_key,
	remote_account_id,
	display_name,
	mode,
	enabled,
	config_json,
	created_at_ms,
	updated_at_ms`

// UpsertAccount inserts an account or updates the row with the same account ID.
// The original creation timestamp is retained on update.
func (s *Store) UpsertAccount(account Account) error {
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO accounts (
			account_id,
			bridge_key,
			remote_account_id,
			display_name,
			mode,
			enabled,
			config_json,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			bridge_key = excluded.bridge_key,
			remote_account_id = excluded.remote_account_id,
			display_name = excluded.display_name,
			mode = excluded.mode,
			enabled = excluded.enabled,
			config_json = excluded.config_json,
			updated_at_ms = excluded.updated_at_ms
	`,
		account.AccountID,
		account.BridgeKey,
		account.RemoteAccountID,
		account.DisplayName,
		account.Mode,
		account.Enabled,
		account.ConfigJSON,
		account.CreatedAtMS,
		account.UpdatedAtMS,
	)
	if err != nil {
		return fmt.Errorf("upsert account %q: %w", account.AccountID, mapConstraintError(err))
	}
	return nil
}

// GetAccount returns the account with accountID.
func (s *Store) GetAccount(accountID string) (Account, error) {
	account, err := scanAccount(s.db.QueryRowContext(
		context.Background(),
		"SELECT "+accountColumns+" FROM accounts WHERE account_id = ?",
		accountID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, notFound("account", accountID)
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account %q: %w", accountID, err)
	}
	return account, nil
}

// ListAccounts returns all accounts in stable ID order.
func (s *Store) ListAccounts() ([]Account, error) {
	rows, err := s.db.QueryContext(
		context.Background(),
		"SELECT "+accountColumns+" FROM accounts ORDER BY account_id",
	)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	accounts, err := collectRows(rows, scanAccount)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return accounts, nil
}

func scanAccount(row rowScanner) (Account, error) {
	var account Account
	err := row.Scan(
		&account.AccountID,
		&account.BridgeKey,
		&account.RemoteAccountID,
		&account.DisplayName,
		&account.Mode,
		&account.Enabled,
		&account.ConfigJSON,
		&account.CreatedAtMS,
		&account.UpdatedAtMS,
	)
	return account, err
}

const deviceColumns = `
	device_id,
	account_id,
	remote_device_id,
	kind,
	display_name,
	state,
	is_current,
	last_seen_at_ms,
	created_at_ms,
	updated_at_ms`

// UpsertDevice inserts a device or updates the row with the same device ID.
// The original creation timestamp is retained on update.
func (s *Store) UpsertDevice(device Device) error {
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO devices (
			device_id,
			account_id,
			remote_device_id,
			kind,
			display_name,
			state,
			is_current,
			last_seen_at_ms,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			account_id = excluded.account_id,
			remote_device_id = excluded.remote_device_id,
			kind = excluded.kind,
			display_name = excluded.display_name,
			state = excluded.state,
			is_current = excluded.is_current,
			last_seen_at_ms = excluded.last_seen_at_ms,
			updated_at_ms = excluded.updated_at_ms
	`,
		device.DeviceID,
		device.AccountID,
		device.RemoteDeviceID,
		device.Kind,
		device.DisplayName,
		device.State,
		device.IsCurrent,
		device.LastSeenAtMS,
		device.CreatedAtMS,
		device.UpdatedAtMS,
	)
	if err != nil {
		return fmt.Errorf("upsert device %q: %w", device.DeviceID, mapConstraintError(err))
	}
	return nil
}

// GetDevice returns the device with deviceID.
func (s *Store) GetDevice(deviceID string) (Device, error) {
	device, err := scanDevice(s.db.QueryRowContext(
		context.Background(),
		"SELECT "+deviceColumns+" FROM devices WHERE device_id = ?",
		deviceID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, notFound("device", deviceID)
	}
	if err != nil {
		return Device{}, fmt.Errorf("get device %q: %w", deviceID, err)
	}
	return device, nil
}

// ListDevices returns the devices for an account in stable ID order.
func (s *Store) ListDevices(accountID string) ([]Device, error) {
	rows, err := s.db.QueryContext(
		context.Background(),
		"SELECT "+deviceColumns+" FROM devices WHERE account_id = ? ORDER BY device_id",
		accountID,
	)
	if err != nil {
		return nil, fmt.Errorf("list devices for account %q: %w", accountID, err)
	}
	devices, err := collectRows(rows, scanDevice)
	if err != nil {
		return nil, fmt.Errorf("list devices for account %q: %w", accountID, err)
	}
	return devices, nil
}

func scanDevice(row rowScanner) (Device, error) {
	var device Device
	err := row.Scan(
		&device.DeviceID,
		&device.AccountID,
		&device.RemoteDeviceID,
		&device.Kind,
		&device.DisplayName,
		&device.State,
		&device.IsCurrent,
		&device.LastSeenAtMS,
		&device.CreatedAtMS,
		&device.UpdatedAtMS,
	)
	return device, err
}

const identityColumns = `
	identity_id,
	account_id,
	kind,
	canonical_value,
	raw_value,
	display_name,
	is_self,
	metadata_json,
	created_at_ms,
	updated_at_ms`

// UpsertIdentity inserts an identity or updates the row with the same account,
// kind, and canonical value. A natural-key conflict retains the existing
// identity ID and creation timestamp, and writes nothing (updated_at_ms
// included) when the raw value, display name, self flag and metadata already
// match: updated_at_ms is the time of the last change, not the last refresh.
func (s *Store) UpsertIdentity(identity Identity) error {
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO identities (
			identity_id,
			account_id,
			kind,
			canonical_value,
			raw_value,
			display_name,
			is_self,
			metadata_json,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, kind, canonical_value) DO UPDATE SET
			raw_value = excluded.raw_value,
			display_name = excluded.display_name,
			is_self = excluded.is_self,
			metadata_json = excluded.metadata_json,
			updated_at_ms = excluded.updated_at_ms
		WHERE identities.raw_value IS NOT excluded.raw_value
		   OR identities.display_name IS NOT excluded.display_name
		   OR identities.is_self IS NOT excluded.is_self
		   OR identities.metadata_json IS NOT excluded.metadata_json
	`,
		identity.IdentityID,
		identity.AccountID,
		identity.Kind,
		identity.CanonicalValue,
		identity.RawValue,
		identity.DisplayName,
		identity.IsSelf,
		identity.MetadataJSON,
		identity.CreatedAtMS,
		identity.UpdatedAtMS,
	)
	if err != nil {
		return fmt.Errorf("upsert identity %q: %w", identity.IdentityID, mapConstraintError(err))
	}
	return nil
}

// GetIdentity returns the identity with identityID.
func (s *Store) GetIdentity(identityID string) (Identity, error) {
	identity, err := scanIdentity(s.db.QueryRowContext(
		context.Background(),
		"SELECT "+identityColumns+" FROM identities WHERE identity_id = ?",
		identityID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, notFound("identity", identityID)
	}
	if err != nil {
		return Identity{}, fmt.Errorf("get identity %q: %w", identityID, err)
	}
	return identity, nil
}

// GetIdentityByCanonical returns an identity by its account-scoped natural key.
func (s *Store) GetIdentityByCanonical(
	accountID string,
	kind IdentityKind,
	canonicalValue string,
) (Identity, error) {
	identity, err := scanIdentity(s.db.QueryRowContext(
		context.Background(),
		"SELECT "+identityColumns+`
		 FROM identities
		 WHERE account_id = ? AND kind = ? AND canonical_value = ?`,
		accountID,
		kind,
		canonicalValue,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, fmt.Errorf(
			"identity for account %q, kind %q, canonical value %q: %w",
			accountID,
			kind,
			canonicalValue,
			ErrNotFound,
		)
	}
	if err != nil {
		return Identity{}, fmt.Errorf(
			"get identity for account %q, kind %q, canonical value %q: %w",
			accountID,
			kind,
			canonicalValue,
			err,
		)
	}
	return identity, nil
}

// ListIdentities returns an account's identities in stable ID order.
func (s *Store) ListIdentities(accountID string) ([]Identity, error) {
	rows, err := s.db.QueryContext(
		context.Background(),
		"SELECT "+identityColumns+" FROM identities WHERE account_id = ? ORDER BY identity_id",
		accountID,
	)
	if err != nil {
		return nil, fmt.Errorf("list identities for account %q: %w", accountID, err)
	}
	identities, err := collectRows(rows, scanIdentity)
	if err != nil {
		return nil, fmt.Errorf("list identities for account %q: %w", accountID, err)
	}
	return identities, nil
}

func scanIdentity(row rowScanner) (Identity, error) {
	var identity Identity
	err := row.Scan(
		&identity.IdentityID,
		&identity.AccountID,
		&identity.Kind,
		&identity.CanonicalValue,
		&identity.RawValue,
		&identity.DisplayName,
		&identity.IsSelf,
		&identity.MetadataJSON,
		&identity.CreatedAtMS,
		&identity.UpdatedAtMS,
	)
	return identity, err
}

const personColumns = `
	person_id,
	display_name,
	sort_name,
	merged_into_person_id,
	created_at_ms,
	updated_at_ms`

// CreatePerson creates a person. Names are not used as a deduplication key.
func (s *Store) CreatePerson(person Person) error {
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO people (
			person_id,
			display_name,
			sort_name,
			merged_into_person_id,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?)
	`,
		person.PersonID,
		person.DisplayName,
		person.SortName,
		person.MergedIntoPersonID,
		person.CreatedAtMS,
		person.UpdatedAtMS,
	)
	if err != nil {
		return fmt.Errorf("create person %q: %w", person.PersonID, mapConstraintError(err))
	}
	return nil
}

// GetPerson returns the person with personID.
func (s *Store) GetPerson(personID string) (Person, error) {
	person, err := scanPerson(s.db.QueryRowContext(
		context.Background(),
		"SELECT "+personColumns+" FROM people WHERE person_id = ?",
		personID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Person{}, notFound("person", personID)
	}
	if err != nil {
		return Person{}, fmt.Errorf("get person %q: %w", personID, err)
	}
	return person, nil
}

// ListPeople returns all people in stable ID order.
func (s *Store) ListPeople() ([]Person, error) {
	rows, err := s.db.QueryContext(
		context.Background(),
		"SELECT "+personColumns+" FROM people ORDER BY person_id",
	)
	if err != nil {
		return nil, fmt.Errorf("list people: %w", err)
	}
	people, err := collectRows(rows, scanPerson)
	if err != nil {
		return nil, fmt.Errorf("list people: %w", err)
	}
	return people, nil
}

func scanPerson(row rowScanner) (Person, error) {
	var person Person
	err := row.Scan(
		&person.PersonID,
		&person.DisplayName,
		&person.SortName,
		&person.MergedIntoPersonID,
		&person.CreatedAtMS,
		&person.UpdatedAtMS,
	)
	return person, err
}

const personIdentityColumns = `
	identity_id,
	person_id,
	provenance,
	confidence,
	is_primary,
	linked_at_ms`

// LinkIdentityToPerson creates a one-owner identity link.
func (s *Store) LinkIdentityToPerson(link PersonIdentity) error {
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO person_identities (
			identity_id,
			person_id,
			provenance,
			confidence,
			is_primary,
			linked_at_ms
		) VALUES (?, ?, ?, ?, ?, ?)
	`,
		link.IdentityID,
		link.PersonID,
		link.Provenance,
		link.Confidence,
		link.IsPrimary,
		link.LinkedAtMS,
	)
	if err == nil {
		return nil
	}
	if isSQLiteErrorCode(err, sqliteConstraintPrimaryKeyCode) {
		return fmt.Errorf(
			"link identity %q to person %q: %w: %w: %w",
			link.IdentityID,
			link.PersonID,
			ErrDuplicateIdentityLink,
			ErrConstraintViolation,
			err,
		)
	}
	return fmt.Errorf(
		"link identity %q to person %q: %w",
		link.IdentityID,
		link.PersonID,
		mapConstraintError(err),
	)
}

// GetPersonIdentity returns the link owned by identityID.
func (s *Store) GetPersonIdentity(identityID string) (PersonIdentity, error) {
	link, err := scanPersonIdentity(s.db.QueryRowContext(
		context.Background(),
		"SELECT "+personIdentityColumns+" FROM person_identities WHERE identity_id = ?",
		identityID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PersonIdentity{}, notFound("person identity link", identityID)
	}
	if err != nil {
		return PersonIdentity{}, fmt.Errorf("get person identity link %q: %w", identityID, err)
	}
	return link, nil
}

// ListPersonIdentities returns a person's links in stable identity ID order.
func (s *Store) ListPersonIdentities(personID string) ([]PersonIdentity, error) {
	rows, err := s.db.QueryContext(
		context.Background(),
		"SELECT "+personIdentityColumns+`
		 FROM person_identities
		 WHERE person_id = ?
		 ORDER BY identity_id`,
		personID,
	)
	if err != nil {
		return nil, fmt.Errorf("list identity links for person %q: %w", personID, err)
	}
	links, err := collectRows(rows, scanPersonIdentity)
	if err != nil {
		return nil, fmt.Errorf("list identity links for person %q: %w", personID, err)
	}
	return links, nil
}

// GetPersonForIdentity resolves identityID to its linked person.
func (s *Store) GetPersonForIdentity(identityID string) (Person, error) {
	person, err := scanPerson(s.db.QueryRowContext(
		context.Background(),
		"SELECT "+prefixedPersonColumns("p")+`
		 FROM person_identities AS pi
		 JOIN people AS p ON p.person_id = pi.person_id
		 WHERE pi.identity_id = ?`,
		identityID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Person{}, notFound("person for identity", identityID)
	}
	if err != nil {
		return Person{}, fmt.Errorf("get person for identity %q: %w", identityID, err)
	}
	return person, nil
}

func scanPersonIdentity(row rowScanner) (PersonIdentity, error) {
	var link PersonIdentity
	err := row.Scan(
		&link.IdentityID,
		&link.PersonID,
		&link.Provenance,
		&link.Confidence,
		&link.IsPrimary,
		&link.LinkedAtMS,
	)
	return link, err
}

func prefixedPersonColumns(alias string) string {
	return alias + `.person_id,
	` + alias + `.display_name,
	` + alias + `.sort_name,
	` + alias + `.merged_into_person_id,
	` + alias + `.created_at_ms,
	` + alias + `.updated_at_ms`
}

const conversationColumns = `
	conversation_id,
	account_id,
	remote_conversation_id,
	kind,
	title,
	remote_revision,
	notification_mode,
	is_favorite,
	archived_at_ms,
	last_message_at_ms,
	metadata_json,
	created_at_ms,
	updated_at_ms`

// UpsertConversation inserts a conversation or updates the row with the same
// account and remote conversation ID. A natural-key conflict retains the
// existing conversation ID and creation timestamp, and writes nothing
// (updated_at_ms included) when every other column already matches.
func (s *Store) UpsertConversation(conversation Conversation) error {
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO conversations (
			conversation_id,
			account_id,
			remote_conversation_id,
			kind,
			title,
			remote_revision,
			notification_mode,
			is_favorite,
			archived_at_ms,
			last_message_at_ms,
			metadata_json,
			created_at_ms,
			updated_at_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id, remote_conversation_id) DO UPDATE SET
			kind = excluded.kind,
			title = excluded.title,
			remote_revision = excluded.remote_revision,
			notification_mode = excluded.notification_mode,
			is_favorite = excluded.is_favorite,
			archived_at_ms = excluded.archived_at_ms,
			last_message_at_ms = excluded.last_message_at_ms,
			metadata_json = excluded.metadata_json,
			updated_at_ms = excluded.updated_at_ms
		WHERE conversations.kind IS NOT excluded.kind
		   OR conversations.title IS NOT excluded.title
		   OR conversations.remote_revision IS NOT excluded.remote_revision
		   OR conversations.notification_mode IS NOT excluded.notification_mode
		   OR conversations.is_favorite IS NOT excluded.is_favorite
		   OR conversations.archived_at_ms IS NOT excluded.archived_at_ms
		   OR conversations.last_message_at_ms IS NOT excluded.last_message_at_ms
		   OR conversations.metadata_json IS NOT excluded.metadata_json
	`,
		conversation.ConversationID,
		conversation.AccountID,
		conversation.RemoteConversationID,
		conversation.Kind,
		conversation.Title,
		conversation.RemoteRevision,
		conversation.NotificationMode,
		conversation.IsFavorite,
		conversation.ArchivedAtMS,
		conversation.LastMessageAtMS,
		conversation.MetadataJSON,
		conversation.CreatedAtMS,
		conversation.UpdatedAtMS,
	)
	if err != nil {
		return fmt.Errorf(
			"upsert conversation %q: %w",
			conversation.ConversationID,
			mapConstraintError(err),
		)
	}
	return nil
}

// GetConversation returns the conversation with conversationID.
func (s *Store) GetConversation(conversationID string) (Conversation, error) {
	conversation, err := scanConversation(s.db.QueryRowContext(
		context.Background(),
		"SELECT "+conversationColumns+" FROM conversations WHERE conversation_id = ?",
		conversationID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, notFound("conversation", conversationID)
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("get conversation %q: %w", conversationID, err)
	}
	return conversation, nil
}

// GetConversationByRemote returns a conversation by the same account-scoped
// natural key used by UpsertConversation.
func (s *Store) GetConversationByRemote(
	accountID string,
	remoteConversationID string,
) (Conversation, error) {
	conversation, err := scanConversation(s.db.QueryRowContext(
		context.Background(),
		"SELECT "+conversationColumns+`
		 FROM conversations
		 WHERE account_id = ? AND remote_conversation_id = ?`,
		accountID,
		remoteConversationID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, fmt.Errorf(
			"conversation for account %q and remote ID %q: %w",
			accountID,
			remoteConversationID,
			ErrNotFound,
		)
	}
	if err != nil {
		return Conversation{}, fmt.Errorf(
			"get conversation for account %q and remote ID %q: %w",
			accountID,
			remoteConversationID,
			err,
		)
	}
	return conversation, nil
}

// ListConversationsByRecency returns an account's conversations from newest to
// oldest, with conversation ID as a deterministic tie-breaker.
func (s *Store) ListConversationsByRecency(accountID string) ([]Conversation, error) {
	rows, err := s.db.QueryContext(
		context.Background(),
		"SELECT "+conversationColumns+`
		 FROM conversations
		 WHERE account_id = ?
		 ORDER BY last_message_at_ms DESC, conversation_id`,
		accountID,
	)
	if err != nil {
		return nil, fmt.Errorf("list conversations for account %q by recency: %w", accountID, err)
	}
	conversations, err := collectRows(rows, scanConversation)
	if err != nil {
		return nil, fmt.Errorf("list conversations for account %q by recency: %w", accountID, err)
	}
	return conversations, nil
}

// ListConversationsByRecencyAllAccounts merges every account's recency list,
// orders the result deterministically, and returns at most limit rows.
func (s *Store) ListConversationsByRecencyAllAccounts(limit int) ([]Conversation, error) {
	if limit <= 0 {
		return []Conversation{}, nil
	}
	accounts, err := s.ListAccounts()
	if err != nil {
		return nil, fmt.Errorf("list conversations across accounts: %w", err)
	}

	conversations := make([]Conversation, 0)
	for _, account := range accounts {
		accountConversations, err := s.ListConversationsByRecency(account.AccountID)
		if err != nil {
			return nil, fmt.Errorf("list conversations across accounts: %w", err)
		}
		conversations = append(conversations, accountConversations...)
	}
	sort.Slice(conversations, func(i, j int) bool {
		if conversations[i].LastMessageAtMS != conversations[j].LastMessageAtMS {
			return conversations[i].LastMessageAtMS > conversations[j].LastMessageAtMS
		}
		return conversations[i].ConversationID < conversations[j].ConversationID
	})
	if len(conversations) > limit {
		conversations = conversations[:limit]
	}
	return conversations, nil
}

// SearchConversationsByName returns conversations whose title, or whose
// participants' display names or canonical addresses, contain query as a
// case-insensitive substring — newest first, at most limit rows. It is the
// bounded v2 counterpart of the legacy metadata search: the match runs in SQL
// so callers map only the hits instead of every conversation.
func (s *Store) SearchConversationsByName(query string, limit int) ([]Conversation, error) {
	if limit <= 0 || strings.TrimSpace(query) == "" {
		return []Conversation{}, nil
	}
	pattern := "%" + escapeLikePattern(query) + "%"
	rows, err := s.db.QueryContext(
		context.Background(),
		"SELECT "+conversationColumns+`
		 FROM conversations
		 WHERE title LIKE ? ESCAPE '\'
		    OR conversation_id IN (
		        SELECT cp.conversation_id
		        FROM conversation_participants cp
		        JOIN identities i ON i.identity_id = cp.identity_id
		        WHERE cp.display_name LIKE ? ESCAPE '\'
		           OR i.display_name LIKE ? ESCAPE '\'
		           OR i.canonical_value LIKE ? ESCAPE '\'
		    )
		 ORDER BY last_message_at_ms DESC, conversation_id
		 LIMIT ?`,
		pattern, pattern, pattern, pattern, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("search conversations by name: %w", err)
	}
	conversations, err := collectRows(rows, scanConversation)
	if err != nil {
		return nil, fmt.Errorf("search conversations by name: %w", err)
	}
	return conversations, nil
}

// escapeLikePattern makes user text match literally inside a LIKE pattern
// that declares '\' as its escape character.
func escapeLikePattern(text string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
}

func scanConversation(row rowScanner) (Conversation, error) {
	var conversation Conversation
	err := row.Scan(
		&conversation.ConversationID,
		&conversation.AccountID,
		&conversation.RemoteConversationID,
		&conversation.Kind,
		&conversation.Title,
		&conversation.RemoteRevision,
		&conversation.NotificationMode,
		&conversation.IsFavorite,
		&conversation.ArchivedAtMS,
		&conversation.LastMessageAtMS,
		&conversation.MetadataJSON,
		&conversation.CreatedAtMS,
		&conversation.UpdatedAtMS,
	)
	return conversation, err
}

const participantColumns = `
	account_id,
	conversation_id,
	identity_id,
	role,
	display_name,
	is_active,
	joined_at_ms,
	left_at_ms`

// ReplaceConversationParticipants atomically replaces every typed participant
// row for a conversation. Empty account and conversation IDs in an input row
// are filled from the target conversation; contradictory IDs are rejected.
// Only rows that differ are written (see SyncConversationParticipants).
func (s *Store) ReplaceConversationParticipants(
	conversationID string,
	participants []ConversationParticipant,
) error {
	_, err := s.SyncConversationParticipants(conversationID, participants, 0)
	return err
}

// SyncConversationParticipants makes a conversation's participant rows exactly
// participants, with ReplaceConversationParticipants' validation and errors,
// but writes only the difference: rows no longer listed are deleted, changed
// rows updated, new rows inserted, and a roster that already matches is read
// without taking the write lock. changed reports whether any row was written.
// When it was and touchedAtMS is positive, the conversation's updated_at_ms
// advances to at least touchedAtMS in the same transaction, so a roster change
// still moves the conversation's update time when its own row is unchanged.
func (s *Store) SyncConversationParticipants(
	conversationID string,
	participants []ConversationParticipant,
	touchedAtMS int64,
) (changed bool, err error) {
	ctx := context.Background()
	if s.participantRosterMatches(ctx, conversationID, participants) {
		return false, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("replace participants for conversation %q: begin transaction: %w", conversationID, err)
	}
	defer tx.Rollback()

	var conversationAccountID string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT account_id FROM conversations WHERE conversation_id = ?`,
		conversationID,
	).Scan(&conversationAccountID); errors.Is(err, sql.ErrNoRows) {
		return false, notFound("conversation", conversationID)
	} else if err != nil {
		return false, fmt.Errorf("replace participants for conversation %q: read account: %w", conversationID, err)
	}

	existing, err := listParticipantsByIdentity(ctx, tx, conversationID)
	if err != nil {
		return false, fmt.Errorf("replace participants for conversation %q: read existing rows: %w", conversationID, err)
	}
	listed := make(map[string]struct{}, len(participants))
	for _, participant := range participants {
		listed[participant.IdentityID] = struct{}{}
	}
	for identityID := range existing {
		if _, keep := listed[identityID]; keep {
			continue
		}
		if _, err := tx.ExecContext(
			ctx,
			`DELETE FROM conversation_participants WHERE conversation_id = ? AND identity_id = ?`,
			conversationID,
			identityID,
		); err != nil {
			return false, fmt.Errorf("replace participants for conversation %q: delete existing rows: %w", conversationID, err)
		}
		changed = true
	}

	seen := make(map[string]struct{}, len(participants))
	for i, participant := range participants {
		if participant.ConversationID != "" && participant.ConversationID != conversationID {
			return false, invalidParticipantError(
				nil,
				"participant %d names conversation %q while replacing %q",
				i,
				participant.ConversationID,
				conversationID,
			)
		}

		accountID := participant.AccountID
		if accountID == "" {
			accountID = conversationAccountID
		}
		if accountID != conversationAccountID {
			return false, invalidParticipantError(
				ErrCrossAccountParticipant,
				"participant %d account %q does not match conversation account %q",
				i,
				accountID,
				conversationAccountID,
			)
		}
		participant.AccountID = accountID
		participant.ConversationID = conversationID

		_, duplicate := seen[participant.IdentityID]
		seen[participant.IdentityID] = struct{}{}
		current, exists := existing[participant.IdentityID]
		var writeErr error
		switch {
		case !duplicate && exists && sameParticipantRow(current, participant):
			continue
		case !duplicate && exists:
			_, writeErr = tx.ExecContext(ctx, `
				UPDATE conversation_participants
				SET role = ?, display_name = ?, is_active = ?, joined_at_ms = ?, left_at_ms = ?
				WHERE conversation_id = ? AND identity_id = ?
			`,
				participant.Role,
				participant.DisplayName,
				participant.IsActive,
				participant.JoinedAtMS,
				participant.LeftAtMS,
				conversationID,
				participant.IdentityID,
			)
		default:
			// A new identity, or a repeat of one already written: the plain
			// insert fails on the primary key for a repeat, exactly as a
			// delete-and-reinsert replacement does.
			_, writeErr = tx.ExecContext(ctx, `
				INSERT INTO conversation_participants (
					account_id,
					conversation_id,
					identity_id,
					role,
					display_name,
					is_active,
					joined_at_ms,
					left_at_ms
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			`,
				accountID,
				conversationID,
				participant.IdentityID,
				participant.Role,
				participant.DisplayName,
				participant.IsActive,
				participant.JoinedAtMS,
				participant.LeftAtMS,
			)
		}
		if writeErr != nil {
			return false, participantWriteError(ctx, tx, conversationID, conversationAccountID, i, participant.IdentityID, writeErr)
		}
		changed = true
	}

	if !changed {
		return false, nil
	}
	if touchedAtMS > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversations
			SET updated_at_ms = ?
			WHERE conversation_id = ? AND updated_at_ms < ?
		`, touchedAtMS, conversationID, touchedAtMS); err != nil {
			return false, fmt.Errorf("replace participants for conversation %q: touch conversation: %w", conversationID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		if isSQLiteErrorCode(err, sqliteConstraintForeignKeyCode) {
			return false, fmt.Errorf(
				"%w: %w: commit participant replacement: %w",
				ErrInvalidConversationParticipant,
				ErrConstraintViolation,
				err,
			)
		}
		return false, fmt.Errorf("replace participants for conversation %q: commit: %w", conversationID, err)
	}
	return true, nil
}

// participantRosterMatches reports, without a write transaction, whether the
// conversation exists and its rows already equal a well-formed participants
// list. Anything else (a missing conversation, an input the transaction would
// reject, a read error, a difference) reports false and leaves the decision
// and any error to the transaction.
func (s *Store) participantRosterMatches(
	ctx context.Context,
	conversationID string,
	participants []ConversationParticipant,
) bool {
	// One statement, so the account and the roster come from one snapshot.
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.account_id, p.identity_id, p.role, p.display_name, p.is_active,
		       p.joined_at_ms, p.left_at_ms
		FROM conversations c
		LEFT JOIN conversation_participants p ON p.conversation_id = c.conversation_id
		WHERE c.conversation_id = ?
	`, conversationID)
	if err != nil {
		return false
	}
	conversationAccountID := ""
	existing := make(map[string]ConversationParticipant)
	found := false
	for rows.Next() {
		var identityID, role, displayName sql.NullString
		var isActive sql.NullBool
		var current ConversationParticipant
		if err := rows.Scan(
			&conversationAccountID,
			&identityID,
			&role,
			&displayName,
			&isActive,
			&current.JoinedAtMS,
			&current.LeftAtMS,
		); err != nil {
			_ = rows.Close()
			return false
		}
		found = true
		if !identityID.Valid {
			continue
		}
		current.AccountID = conversationAccountID
		current.ConversationID = conversationID
		current.IdentityID = identityID.String
		current.Role = ParticipantRole(role.String)
		current.DisplayName = displayName.String
		current.IsActive = isActive.Bool
		existing[current.IdentityID] = current
	}
	if rows.Err() != nil || rows.Close() != nil || !found || len(existing) != len(participants) {
		return false
	}
	for _, participant := range participants {
		if participant.ConversationID != "" && participant.ConversationID != conversationID {
			return false
		}
		if participant.AccountID != "" && participant.AccountID != conversationAccountID {
			return false
		}
		participant.AccountID = conversationAccountID
		participant.ConversationID = conversationID
		current, exists := existing[participant.IdentityID]
		if !exists || !sameParticipantRow(current, participant) {
			return false
		}
		// Each identity may match once; a repeat is an input error.
		delete(existing, participant.IdentityID)
	}
	return true
}

type participantQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listParticipantsByIdentity(
	ctx context.Context,
	queryer participantQueryer,
	conversationID string,
) (map[string]ConversationParticipant, error) {
	rows, err := queryer.QueryContext(ctx, "SELECT "+participantColumns+`
		FROM conversation_participants
		WHERE conversation_id = ?`,
		conversationID,
	)
	if err != nil {
		return nil, err
	}
	participants, err := collectRows(rows, scanConversationParticipant)
	if err != nil {
		return nil, err
	}
	byIdentity := make(map[string]ConversationParticipant, len(participants))
	for _, participant := range participants {
		byIdentity[participant.IdentityID] = participant
	}
	return byIdentity, nil
}

func sameParticipantRow(left, right ConversationParticipant) bool {
	return left.AccountID == right.AccountID &&
		left.ConversationID == right.ConversationID &&
		left.IdentityID == right.IdentityID &&
		left.Role == right.Role &&
		left.DisplayName == right.DisplayName &&
		left.IsActive == right.IsActive &&
		optionalInt64Equal(left.JoinedAtMS, right.JoinedAtMS) &&
		optionalInt64Equal(left.LeftAtMS, right.LeftAtMS)
}

func optionalInt64Equal(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// participantWriteError classifies a failed participant insert or update the
// way a replacement always has.
func participantWriteError(
	ctx context.Context,
	tx *sql.Tx,
	conversationID string,
	conversationAccountID string,
	index int,
	identityID string,
	err error,
) error {
	if isSQLiteErrorCode(err, sqliteConstraintForeignKeyCode) {
		specific, classifyErr := classifyParticipantForeignKey(
			ctx,
			tx,
			conversationAccountID,
			identityID,
		)
		if classifyErr != nil {
			return fmt.Errorf(
				"replace participants for conversation %q: classify participant %d constraint: %w",
				conversationID,
				index,
				classifyErr,
			)
		}
		return invalidParticipantConstraintError(
			specific,
			err,
			"insert participant %d identity %q",
			index,
			identityID,
		)
	}
	if isSQLiteConstraint(err) {
		return invalidParticipantConstraintError(
			nil,
			err,
			"insert participant %d identity %q",
			index,
			identityID,
		)
	}
	return fmt.Errorf(
		"replace participants for conversation %q: insert participant %d: %w",
		conversationID,
		index,
		err,
	)
}

// ListParticipants returns typed participant rows in stable identity ID order.
func (s *Store) ListParticipants(conversationID string) ([]ConversationParticipant, error) {
	rows, err := s.db.QueryContext(
		context.Background(),
		"SELECT "+participantColumns+`
		 FROM conversation_participants
		 WHERE conversation_id = ?
		 ORDER BY identity_id`,
		conversationID,
	)
	if err != nil {
		return nil, fmt.Errorf("list participants for conversation %q: %w", conversationID, err)
	}
	participants, err := collectRows(rows, scanConversationParticipant)
	if err != nil {
		return nil, fmt.Errorf("list participants for conversation %q: %w", conversationID, err)
	}
	return participants, nil
}

func scanConversationParticipant(row rowScanner) (ConversationParticipant, error) {
	var participant ConversationParticipant
	err := row.Scan(
		&participant.AccountID,
		&participant.ConversationID,
		&participant.IdentityID,
		&participant.Role,
		&participant.DisplayName,
		&participant.IsActive,
		&participant.JoinedAtMS,
		&participant.LeftAtMS,
	)
	return participant, err
}

func classifyParticipantForeignKey(
	ctx context.Context,
	tx *sql.Tx,
	conversationAccountID string,
	identityID string,
) (error, error) {
	var identityAccountID string
	err := tx.QueryRowContext(
		ctx,
		`SELECT account_id FROM identities WHERE identity_id = ?`,
		identityID,
	).Scan(&identityAccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOrphanParticipantIdentity, nil
	}
	if err != nil {
		return nil, err
	}
	if identityAccountID != conversationAccountID {
		return ErrCrossAccountParticipant, nil
	}
	return nil, nil
}

func invalidParticipantConstraintError(
	specific error,
	cause error,
	format string,
	args ...any,
) error {
	detail := fmt.Sprintf(format, args...)
	if specific == nil {
		return fmt.Errorf(
			"%w: %w: %s: %w",
			ErrInvalidConversationParticipant,
			ErrConstraintViolation,
			detail,
			cause,
		)
	}
	return fmt.Errorf(
		"%w: %w: %w: %s: %w",
		ErrInvalidConversationParticipant,
		ErrConstraintViolation,
		specific,
		detail,
		cause,
	)
}

func invalidParticipantError(specific error, format string, args ...any) error {
	detail := fmt.Sprintf(format, args...)
	if specific == nil {
		return fmt.Errorf(
			"%w: %w: %s",
			ErrInvalidConversationParticipant,
			ErrConstraintViolation,
			detail,
		)
	}
	return fmt.Errorf(
		"%w: %w: %w: %s",
		ErrInvalidConversationParticipant,
		ErrConstraintViolation,
		specific,
		detail,
	)
}
