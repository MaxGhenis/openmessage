package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

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
// identity ID and creation timestamp.
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
// existing conversation ID and creation timestamp.
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

// SetConversationNotificationMode updates the local notification policy.
func (s *Store) SetConversationNotificationMode(ctx context.Context, conversationID string, mode NotificationMode) error {
	return s.updateConversationAttributes(ctx, conversationID,
		"notification_mode = ?, updated_at_ms = MAX(updated_at_ms + 1, ?)", mode, time.Now().UnixMilli())
}

// SetConversationFavorite updates the local favorite state.
func (s *Store) SetConversationFavorite(ctx context.Context, conversationID string, favorite bool) error {
	return s.updateConversationAttributes(ctx, conversationID,
		"is_favorite = ?, updated_at_ms = MAX(updated_at_ms + 1, ?)", favorite, time.Now().UnixMilli())
}

// SetConversationArchived updates the local archive timestamp. Nil restores
// the conversation to the inbox.
func (s *Store) SetConversationArchived(ctx context.Context, conversationID string, archivedAtMS *int64) error {
	return s.updateConversationAttributes(ctx, conversationID,
		"archived_at_ms = ?, updated_at_ms = MAX(updated_at_ms + 1, ?)", archivedAtMS, time.Now().UnixMilli())
}

func (s *Store) updateConversationAttributes(ctx context.Context, conversationID, assignments string, args ...any) error {
	args = append(args, conversationID)
	result, err := s.db.ExecContext(ctx, "UPDATE conversations SET "+assignments+" WHERE conversation_id = ?", args...)
	if err != nil {
		return fmt.Errorf("update conversation %q: %w", conversationID, mapConstraintError(err))
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update conversation %q rows affected: %w", conversationID, err)
	}
	if rows == 0 {
		return notFound("conversation", conversationID)
	}
	return nil
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
func (s *Store) ReplaceConversationParticipants(
	conversationID string,
	participants []ConversationParticipant,
) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace participants for conversation %q: begin transaction: %w", conversationID, err)
	}
	defer tx.Rollback()

	var conversationAccountID string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT account_id FROM conversations WHERE conversation_id = ?`,
		conversationID,
	).Scan(&conversationAccountID); errors.Is(err, sql.ErrNoRows) {
		return notFound("conversation", conversationID)
	} else if err != nil {
		return fmt.Errorf("replace participants for conversation %q: read account: %w", conversationID, err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM conversation_participants WHERE conversation_id = ?`,
		conversationID,
	); err != nil {
		return fmt.Errorf("replace participants for conversation %q: delete existing rows: %w", conversationID, err)
	}

	for i, participant := range participants {
		if participant.ConversationID != "" && participant.ConversationID != conversationID {
			return invalidParticipantError(
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
			return invalidParticipantError(
				ErrCrossAccountParticipant,
				"participant %d account %q does not match conversation account %q",
				i,
				accountID,
				conversationAccountID,
			)
		}

		if _, err := tx.ExecContext(ctx, `
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
		); err != nil {
			if isSQLiteErrorCode(err, sqliteConstraintForeignKeyCode) {
				specific, classifyErr := classifyParticipantForeignKey(
					ctx,
					tx,
					conversationAccountID,
					participant.IdentityID,
				)
				if classifyErr != nil {
					return fmt.Errorf(
						"replace participants for conversation %q: classify participant %d constraint: %w",
						conversationID,
						i,
						classifyErr,
					)
				}
				return invalidParticipantConstraintError(
					specific,
					err,
					"insert participant %d identity %q",
					i,
					participant.IdentityID,
				)
			}
			if isSQLiteConstraint(err) {
				return invalidParticipantConstraintError(
					nil,
					err,
					"insert participant %d identity %q",
					i,
					participant.IdentityID,
				)
			}
			return fmt.Errorf(
				"replace participants for conversation %q: insert participant %d: %w",
				conversationID,
				i,
				err,
			)
		}
	}

	if err := tx.Commit(); err != nil {
		if isSQLiteErrorCode(err, sqliteConstraintForeignKeyCode) {
			return fmt.Errorf(
				"%w: %w: commit participant replacement: %w",
				ErrInvalidConversationParticipant,
				ErrConstraintViolation,
				err,
			)
		}
		return fmt.Errorf("replace participants for conversation %q: commit: %w", conversationID, err)
	}
	return nil
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
