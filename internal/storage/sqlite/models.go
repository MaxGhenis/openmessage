package sqlite

// AccountMode controls whether an account is connected live or imported from
// an archive.
type AccountMode string

const (
	AccountModeLive    AccountMode = "live"
	AccountModeArchive AccountMode = "archive"
)

// DeviceKind identifies the relationship of a device to this installation.
type DeviceKind string

const (
	DeviceKindLocalInstallation DeviceKind = "local_installation"
	DeviceKindRemoteEndpoint    DeviceKind = "remote_endpoint"
)

// DeviceState is the persisted lifecycle state of a device.
type DeviceState string

const (
	DeviceStateActive  DeviceState = "active"
	DeviceStateRevoked DeviceState = "revoked"
	DeviceStateUnknown DeviceState = "unknown"
)

// IdentityKind is deliberately open-ended so bridges can introduce identity
// kinds without a schema migration.
type IdentityKind string

// IdentityProvenance records how an identity was linked to a person.
type IdentityProvenance string

const (
	IdentityProvenanceExplicit    IdentityProvenance = "explicit"
	IdentityProvenanceAddressBook IdentityProvenance = "address_book"
	IdentityProvenanceBridgeAlias IdentityProvenance = "bridge_alias"
	IdentityProvenanceImport      IdentityProvenance = "import"
	IdentityProvenanceHeuristic   IdentityProvenance = "heuristic"
)

// ConversationKind is the persisted shape of a conversation.
type ConversationKind string

const (
	ConversationKindDirect    ConversationKind = "direct"
	ConversationKindGroup     ConversationKind = "group"
	ConversationKindBroadcast ConversationKind = "broadcast"
	ConversationKindSystem    ConversationKind = "system"
)

// NotificationMode is the persisted notification policy for a conversation.
type NotificationMode string

const (
	NotificationModeAll      NotificationMode = "all"
	NotificationModeMentions NotificationMode = "mentions"
	NotificationModeMuted    NotificationMode = "muted"
)

// ParticipantRole is a participant's role within a conversation.
type ParticipantRole string

const (
	ParticipantRoleMember  ParticipantRole = "member"
	ParticipantRoleAdmin   ParticipantRole = "admin"
	ParticipantRoleOwner   ParticipantRole = "owner"
	ParticipantRoleUnknown ParticipantRole = "unknown"
)

// Account mirrors one row in accounts.
type Account struct {
	AccountID       string
	BridgeKey       string
	RemoteAccountID *string
	DisplayName     string
	Mode            AccountMode
	Enabled         bool
	ConfigJSON      string
	CreatedAtMS     int64
	UpdatedAtMS     int64
}

// Device mirrors one row in devices.
type Device struct {
	DeviceID       string
	AccountID      string
	RemoteDeviceID *string
	Kind           DeviceKind
	DisplayName    string
	State          DeviceState
	IsCurrent      bool
	LastSeenAtMS   *int64
	CreatedAtMS    int64
	UpdatedAtMS    int64
}

// Identity mirrors one row in identities.
type Identity struct {
	IdentityID     string
	AccountID      string
	Kind           IdentityKind
	CanonicalValue string
	RawValue       string
	DisplayName    string
	IsSelf         bool
	MetadataJSON   string
	CreatedAtMS    int64
	UpdatedAtMS    int64
}

// Person mirrors one row in people. DisplayName is intentionally not unique.
type Person struct {
	PersonID           string
	DisplayName        string
	SortName           string
	MergedIntoPersonID *string
	CreatedAtMS        int64
	UpdatedAtMS        int64
}

// PersonIdentity mirrors one row in person_identities.
type PersonIdentity struct {
	IdentityID string
	PersonID   string
	Provenance IdentityProvenance
	Confidence float64
	IsPrimary  bool
	LinkedAtMS int64
}

// Conversation mirrors one row in conversations.
type Conversation struct {
	ConversationID       string
	AccountID            string
	RemoteConversationID string
	Kind                 ConversationKind
	Title                string
	RemoteRevision       *string
	NotificationMode     NotificationMode
	IsFavorite           bool
	ArchivedAtMS         *int64
	LastMessageAtMS      int64
	MetadataJSON         string
	CreatedAtMS          int64
	UpdatedAtMS          int64
}

// ConversationParticipant mirrors one row in conversation_participants.
// Participants are always represented as typed rows, never as JSON blobs.
type ConversationParticipant struct {
	AccountID      string
	ConversationID string
	IdentityID     string
	Role           ParticipantRole
	DisplayName    string
	IsActive       bool
	JoinedAtMS     *int64
	LeftAtMS       *int64
}
