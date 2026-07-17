package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/whatsappmedia"
)

const scheduledMediaMaxBytes int64 = 128 << 20

const legacySnapshotSuffix = ".source"

// Options names the immutable source and the staged v2 destinations. Transform
// never publishes the staged paths; the CLI owns the final atomic rename.
type Options struct {
	SourcePath      string
	TempStorePath   string
	TempBlobPath    string
	TargetPath      string
	TargetStorePath string
	Check           bool
}

type accountSpec struct {
	Platform    string
	AccountID   string
	BridgeKey   string
	DisplayName string
	Mode        sqlite.AccountMode
}

var platformAccounts = map[string]accountSpec{
	"sms": {
		Platform: "sms", AccountID: "google-primary", BridgeKey: "google_messages",
		DisplayName: "Google Messages", Mode: sqlite.AccountModeLive,
	},
	"whatsapp": {
		Platform: "whatsapp", AccountID: "whatsapp-primary", BridgeKey: "whatsmeow",
		DisplayName: "WhatsApp", Mode: sqlite.AccountModeLive,
	},
	"signal": {
		Platform: "signal", AccountID: "signal-primary", BridgeKey: "signal_cli",
		DisplayName: "Signal", Mode: sqlite.AccountModeLive,
	},
	"gchat": {
		Platform: "gchat", AccountID: "gchat-archive", BridgeKey: "gchat",
		DisplayName: "Google Chat archive", Mode: sqlite.AccountModeArchive,
	},
	"imessage": {
		Platform: "imessage", AccountID: "imessage-archive", BridgeKey: "imessage",
		DisplayName: "iMessage archive", Mode: sqlite.AccountModeArchive,
	},
}

type identityKey struct {
	AccountID string
	Kind      string
	Canonical string
}

type identityCandidate struct {
	Key         identityKey
	Raw         string
	DisplayName string
	IsSelf      bool
	Priority    int
}

type participantPlan struct {
	Identity identityKey
	Name     string
}

type conversationPlan struct {
	Legacy       legacyConversation
	Account      accountSpec
	V2ID         string
	Participants []participantPlan
}

type unifiedIdentifier struct {
	Platform string `json:"platform"`
	Value    string `json:"value"`
}

type unifiedContactPlan struct {
	Legacy     legacyUnifiedContact
	PersonID   string
	Identities []identityKey
}

type legacyParticipant struct {
	Name      string `json:"name"`
	Number    string `json:"number"`
	Phone     string `json:"phone"`
	Email     string `json:"email"`
	ID        string `json:"id"`
	ContactID string `json:"contact_id"`
	IsMe      bool   `json:"is_me"`
	IsMeCamel bool   `json:"isMe"`
}

type expectedMessage struct {
	Legacy   legacyMessage
	Platform string
	Account  accountSpec
	V2ID     string
	RemoteID string
	MediaID  string
}

type scheduledBlob struct {
	OutboxID string
	Ref      blob.BlobRef
}

type transformState struct {
	baseTimestampMS int64
	accounts        map[string]accountSpec
	identities      map[identityKey]*identityCandidate
	conversations   map[string]*conversationPlan
	unified         []unifiedContactPlan

	contactIdentityKeys       map[identityKey]struct{}
	contactRowsMapped         int64
	expectedLinks             int64
	expectedParticipants      int64
	expectedHistory           map[string]expectedMessage
	expectedHistoryByPlatform map[string]map[string]struct{}
	scheduledMessages         map[string]struct{}
	expectedCursors           int64
	expectedPendingMedia      int64
	scheduledBlobs            []scheduledBlob
}

// Transform creates and validates a complete staged v2 store using only
// repository writers. A failed validation returns both the report and an
// error wrapping ErrValidation so callers can still emit the evidence.
func Transform(ctx context.Context, options Options) (report Report, returnErr error) {
	report = Report{
		Format:            ReportFormat,
		Check:             options.Check,
		TableCounts:       map[string]TableReconciliation{},
		PlatformCounts:    map[string]PlatformReconciliation{},
		Warnings:          []string{},
		MessageCollisions: []MessageCollision{},
		SampledHashes:     []SampledHash{},
		Target: TargetReport{
			Path: options.TargetPath, DatabasePath: options.TargetStorePath,
			Counts: map[string]int64{}, MigrationChecksums: []string{},
		},
	}
	if ctx == nil {
		return report, fmt.Errorf("%w: context is nil", ErrTransform)
	}
	for name, value := range map[string]string{
		"source path": options.SourcePath, "temporary store path": options.TempStorePath,
		"temporary blob path": options.TempBlobPath, "target store path": options.TargetStorePath,
	} {
		if strings.TrimSpace(value) == "" {
			return report, fmt.Errorf("%w: %s is empty", ErrTransform, name)
		}
	}
	if err := requireAbsent(options.TempStorePath); err != nil {
		return report, fmt.Errorf("%w: temporary store: %v", ErrTransform, err)
	}
	if err := requireAbsent(options.TempBlobPath); err != nil {
		return report, fmt.Errorf("%w: temporary blob store: %v", ErrTransform, err)
	}
	tempSourcePath := options.TempStorePath + legacySnapshotSuffix
	if err := requireAbsent(tempSourcePath); err != nil {
		return report, fmt.Errorf("%w: temporary legacy snapshot: %v", ErrTransform, err)
	}
	stageParent := filepath.Dir(options.TempStorePath)
	if err := os.MkdirAll(stageParent, 0o700); err != nil {
		return report, fmt.Errorf("%w: create staged store directory: %v", ErrTransform, err)
	}
	if err := os.Chmod(stageParent, 0o700); err != nil {
		return report, fmt.Errorf("%w: secure staged store directory: %v", ErrTransform, err)
	}

	dataset, sourceReport, err := loadLegacySource(ctx, options.SourcePath, tempSourcePath)
	report.Source = sourceReport
	if err != nil {
		return report, err
	}
	report.Dropped = dataset.dropped
	report.SignalLocalRows = dataset.signalLocalRows
	report.ReadState = ReadStateReport{
		LossyWarning:                 "legacy read state was lossy",
		ConversationsWithUnreadCount: dataset.unreadRows,
	}
	report.Schedule = scheduleReport(dataset)
	report.Media.LegacyAttachments = dataset.mediaRows
	appendRequiredWarnings(&report)

	state, err := buildTransformState(dataset, &report)
	if err != nil {
		return report, fmt.Errorf("%w: plan transform: %v", ErrSource, err)
	}

	target, err := sqlite.Open(options.TempStorePath)
	if err != nil {
		return report, fmt.Errorf("%w: initialize merged v2 schema: %v", ErrTransform, err)
	}
	targetOpen := true
	defer func() {
		if targetOpen {
			if closeErr := target.Close(); closeErr != nil && returnErr == nil {
				returnErr = fmt.Errorf("%w: close staged v2 store: %v", ErrTransform, closeErr)
			}
		}
	}()
	blobs, err := blob.New(options.TempBlobPath)
	if err != nil {
		return report, fmt.Errorf("%w: initialize staged blob store: %v", ErrTransform, err)
	}

	clockMS := state.baseTimestampMS
	now := func() time.Time { return time.UnixMilli(clockMS) }
	messageRepository, err := sqlite.NewMessageRepository(target, now)
	if err != nil {
		return report, fmt.Errorf("%w: initialize message importer: %v", ErrTransform, err)
	}
	outboxRepository, err := sqlite.NewOutboxRepository(target, now)
	if err != nil {
		return report, fmt.Errorf("%w: initialize outbox importer: %v", ErrTransform, err)
	}

	if err := writeAccounts(target, state); err != nil {
		return report, fmt.Errorf("%w: %v", ErrTransform, err)
	}
	if err := writeIdentities(target, state); err != nil {
		return report, fmt.Errorf("%w: %v", ErrTransform, err)
	}
	if err := writePeople(target, state); err != nil {
		return report, fmt.Errorf("%w: %v", ErrTransform, err)
	}
	if err := writeConversations(target, state); err != nil {
		return report, fmt.Errorf("%w: %v", ErrTransform, err)
	}
	if err := writeHistory(ctx, messageRepository, dataset, state, &clockMS, &report); err != nil {
		return report, fmt.Errorf("%w: %v", ErrTransform, err)
	}
	if err := writeScheduled(
		ctx, messageRepository, outboxRepository, blobs, dataset, state, &clockMS, &report,
	); err != nil {
		return report, fmt.Errorf("%w: %v", ErrTransform, err)
	}
	if err := writeReadCursors(target, dataset, state, &report); err != nil {
		return report, fmt.Errorf("%w: %v", ErrTransform, err)
	}

	storeID, err := target.StoreInstanceID()
	if err != nil {
		return report, fmt.Errorf("%w: read staged store ID: %v", ErrTransform, err)
	}
	report.Target.StoreInstanceID = storeID
	if err := target.Close(); err != nil {
		return report, fmt.Errorf("%w: close staged v2 store: %v", ErrTransform, err)
	}
	targetOpen = false
	if err := checkpointAndSyncSQLite(ctx, options.TempStorePath); err != nil {
		return report, fmt.Errorf("%w: finalize staged v2 database: %v", ErrTransform, err)
	}

	if err := validateTarget(ctx, options, dataset, state, &report); err != nil {
		return report, err
	}
	report.OK = true
	return report, nil
}

func buildTransformState(dataset legacyDataset, report *Report) (*transformState, error) {
	state := &transformState{
		baseTimestampMS: minLegacyTimestamp(dataset),
		accounts:        map[string]accountSpec{}, identities: map[identityKey]*identityCandidate{},
		conversations:             map[string]*conversationPlan{},
		contactIdentityKeys:       map[identityKey]struct{}{},
		expectedHistory:           map[string]expectedMessage{},
		expectedHistoryByPlatform: map[string]map[string]struct{}{},
		scheduledMessages:         map[string]struct{}{},
	}

	for _, legacy := range dataset.conversations {
		account, err := accountForPlatform(normalizeLegacyPlatform(legacy.Platform))
		if err != nil {
			return nil, err
		}
		state.accounts[account.Platform] = account
		plan := &conversationPlan{
			Legacy: legacy, Account: account,
			V2ID: deriveID("conversation", account.AccountID, legacy.ID),
		}
		var participants []legacyParticipant
		if err := json.Unmarshal([]byte(legacy.ParticipantsJSON), &participants); err != nil {
			return nil, fmt.Errorf("conversation %q participants JSON: %w", legacy.ID, err)
		}
		seen := map[identityKey]bool{}
		for index, participant := range participants {
			value := firstNonEmpty(
				participant.Number, participant.Phone, participant.Email,
				participant.ID, participant.ContactID,
			)
			if strings.TrimSpace(value) == "" {
				// Never key an identity by display name. A conversation-scoped
				// synthetic value preserves the participant without merging names.
				if strings.TrimSpace(participant.Name) == "" {
					continue
				}
				value = fmt.Sprintf("legacy-participant:%s:%d", legacy.ID, index)
			}
			key, err := addIdentity(
				state, account, value, participant.Name,
				participant.IsMe || participant.IsMeCamel || strings.EqualFold(strings.TrimSpace(value), "me"), 10,
			)
			if err != nil {
				return nil, fmt.Errorf("conversation %q participant %d: %w", legacy.ID, index, err)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			plan.Participants = append(plan.Participants, participantPlan{Identity: key, Name: participant.Name})
		}
		state.expectedParticipants += int64(len(plan.Participants))
		state.conversations[legacy.ID] = plan
	}

	for _, message := range dataset.messages {
		account, _ := accountForPlatform(normalizeLegacyPlatform(message.Platform))
		state.accounts[account.Platform] = account
		if strings.TrimSpace(message.SenderNumber) != "" {
			if _, err := addIdentity(
				state, account, message.SenderNumber, message.SenderName, message.IsFromMe, 20,
			); err != nil {
				return nil, fmt.Errorf("message %q sender: %w", message.ID, err)
			}
		}
	}

	if len(dataset.contacts) > 0 {
		account, _ := accountForPlatform("sms")
		state.accounts[account.Platform] = account
		for _, contact := range dataset.contacts {
			if strings.TrimSpace(contact.Number) == "" {
				report.Warnings = append(report.Warnings,
					fmt.Sprintf("contact %s had no number and could not produce an identity", contact.ID))
				continue
			}
			key, err := addIdentity(state, account, contact.Number, contact.Name, false, 30)
			if err != nil {
				return nil, fmt.Errorf("contact %q: %w", contact.ID, err)
			}
			state.contactIdentityKeys[key] = struct{}{}
			state.contactRowsMapped++
		}
	}

	identityOwners := map[identityKey]string{}
	for _, legacy := range dataset.unified {
		if strings.TrimSpace(legacy.ID) == "" {
			return nil, fmt.Errorf("unified contact has an empty ID")
		}
		plan := unifiedContactPlan{
			Legacy:   legacy,
			PersonID: deriveID("person", "", legacy.ID),
		}
		var identifiers []unifiedIdentifier
		if err := json.Unmarshal([]byte(legacy.IdentifiersJSON), &identifiers); err != nil {
			return nil, fmt.Errorf("unified contact %q identifiers JSON: %w", legacy.ID, err)
		}
		seen := map[identityKey]bool{}
		for index, identifier := range identifiers {
			if strings.TrimSpace(identifier.Value) == "" {
				return nil, fmt.Errorf("unified contact %q identifier %d is empty", legacy.ID, index)
			}
			account, err := accountForPlatform(normalizeLegacyPlatform(identifier.Platform))
			if err != nil {
				return nil, fmt.Errorf("unified contact %q identifier %d: %w", legacy.ID, index, err)
			}
			state.accounts[account.Platform] = account
			key, err := addIdentity(state, account, identifier.Value, legacy.DisplayName, false, 40)
			if err != nil {
				return nil, fmt.Errorf("unified contact %q identifier %d: %w", legacy.ID, index, err)
			}
			if seen[key] {
				continue
			}
			if owner, exists := identityOwners[key]; exists && owner != legacy.ID {
				return nil, fmt.Errorf(
					"explicit identity %q is owned by both unified contacts %q and %q",
					key.Canonical, owner, legacy.ID,
				)
			}
			identityOwners[key] = legacy.ID
			seen[key] = true
			plan.Identities = append(plan.Identities, key)
			state.expectedLinks++
		}
		state.unified = append(state.unified, plan)
	}
	return state, nil
}

func writeAccounts(target *sqlite.Store, state *transformState) error {
	platforms := sortedAccountPlatforms(state.accounts)
	for _, platform := range platforms {
		account := state.accounts[platform]
		if err := target.UpsertAccount(sqlite.Account{
			AccountID: account.AccountID, BridgeKey: account.BridgeKey,
			DisplayName: account.DisplayName, Mode: account.Mode, Enabled: true,
			ConfigJSON: "{}", CreatedAtMS: state.baseTimestampMS, UpdatedAtMS: state.baseTimestampMS,
		}); err != nil {
			return fmt.Errorf("upsert %s account: %w", platform, err)
		}
		deviceID := deriveID("device", account.AccountID, account.AccountID+"\x1flocal")
		if err := target.UpsertDevice(sqlite.Device{
			DeviceID: deviceID, AccountID: account.AccountID,
			Kind: sqlite.DeviceKindLocalInstallation, DisplayName: "OpenMessage",
			State: sqlite.DeviceStateActive, IsCurrent: true,
			CreatedAtMS: state.baseTimestampMS, UpdatedAtMS: state.baseTimestampMS,
		}); err != nil {
			return fmt.Errorf("upsert %s migration device: %w", platform, err)
		}
	}
	return nil
}

func writeIdentities(target *sqlite.Store, state *transformState) error {
	keys := make([]identityKey, 0, len(state.identities))
	for key := range state.identities {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return identityID(keys[i]) < identityID(keys[j]) })
	for _, key := range keys {
		candidate := state.identities[key]
		if err := target.UpsertIdentity(sqlite.Identity{
			IdentityID: identityID(key), AccountID: key.AccountID,
			Kind: sqlite.IdentityKind(key.Kind), CanonicalValue: key.Canonical,
			RawValue: candidate.Raw, DisplayName: candidate.DisplayName,
			IsSelf: candidate.IsSelf, MetadataJSON: "{}",
			CreatedAtMS: state.baseTimestampMS, UpdatedAtMS: state.baseTimestampMS,
		}); err != nil {
			return fmt.Errorf("upsert identity %q: %w", key.Canonical, err)
		}
	}
	return nil
}

func writePeople(target *sqlite.Store, state *transformState) error {
	for _, plan := range state.unified {
		if err := target.CreatePerson(sqlite.Person{
			PersonID: plan.PersonID, DisplayName: plan.Legacy.DisplayName,
			SortName:    strings.ToLower(strings.TrimSpace(plan.Legacy.DisplayName)),
			CreatedAtMS: state.baseTimestampMS, UpdatedAtMS: state.baseTimestampMS,
		}); err != nil {
			return fmt.Errorf("create person for unified contact %q: %w", plan.Legacy.ID, err)
		}
		for index, key := range plan.Identities {
			if err := target.LinkIdentityToPerson(sqlite.PersonIdentity{
				IdentityID: identityID(key), PersonID: plan.PersonID,
				Provenance: sqlite.IdentityProvenanceExplicit, Confidence: 1,
				IsPrimary: index == 0, LinkedAtMS: state.baseTimestampMS,
			}); err != nil {
				return fmt.Errorf("link unified contact %q identity %q: %w", plan.Legacy.ID, key.Canonical, err)
			}
		}
	}
	return nil
}

func writeConversations(target *sqlite.Store, state *transformState) error {
	ids := make([]string, 0, len(state.conversations))
	for id := range state.conversations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, legacyID := range ids {
		plan := state.conversations[legacyID]
		createdAt, lastMessageAt := conversationTimes(plan.Legacy, state.baseTimestampMS)
		kind := sqlite.ConversationKindDirect
		if plan.Legacy.IsGroup {
			kind = sqlite.ConversationKindGroup
		}
		notification := sqlite.NotificationMode(strings.ToLower(strings.TrimSpace(plan.Legacy.NotificationMode)))
		switch notification {
		case sqlite.NotificationModeAll, sqlite.NotificationModeMentions, sqlite.NotificationModeMuted:
		default:
			notification = sqlite.NotificationModeAll
		}
		var archivedAt *int64
		if strings.EqualFold(strings.TrimSpace(plan.Legacy.Tab), "archive") {
			value := lastMessageAt
			if value == 0 {
				value = createdAt
			}
			archivedAt = &value
		}
		if err := target.UpsertConversation(sqlite.Conversation{
			ConversationID: plan.V2ID, AccountID: plan.Account.AccountID,
			RemoteConversationID: normalizeRemoteConversationID(plan.Account.Platform, plan.Legacy.ID),
			Kind:                 kind, Title: plan.Legacy.Name, NotificationMode: notification,
			IsFavorite: plan.Legacy.IsFavorite, ArchivedAtMS: archivedAt,
			LastMessageAtMS: lastMessageAt, MetadataJSON: "{}",
			CreatedAtMS: createdAt, UpdatedAtMS: maxInt64(createdAt, lastMessageAt),
		}); err != nil {
			return fmt.Errorf("upsert conversation %q: %w", legacyID, err)
		}
		participants := make([]sqlite.ConversationParticipant, 0, len(plan.Participants))
		for _, participant := range plan.Participants {
			participants = append(participants, sqlite.ConversationParticipant{
				AccountID: plan.Account.AccountID, ConversationID: plan.V2ID,
				IdentityID: identityID(participant.Identity), Role: sqlite.ParticipantRoleMember,
				DisplayName: participant.Name, IsActive: true,
			})
		}
		if err := target.ReplaceConversationParticipants(plan.V2ID, participants); err != nil {
			return fmt.Errorf("replace conversation %q participants: %w", legacyID, err)
		}
	}
	return nil
}

func writeHistory(
	ctx context.Context,
	repository *sqlite.MessageRepository,
	dataset legacyDataset,
	state *transformState,
	clockMS *int64,
	report *Report,
) error {
	remoteByLegacyID := make(map[string]string, len(dataset.messages))
	for _, message := range dataset.messages {
		remoteByLegacyID[message.ID] = deriveRemoteMessageID(message)
	}
	collisionGroups := map[string][]string{}
	for _, legacy := range dataset.messages {
		conversation := state.conversations[legacy.ConversationID]
		remoteID := deriveRemoteMessageID(legacy)
		messageID := deriveID(
			"message", conversation.Account.AccountID,
			legacy.ConversationID+"\x1f"+remoteID,
		)
		collisionKey := conversation.Account.AccountID + "\x1f" + conversation.V2ID + "\x1f" + remoteID
		collisionGroups[collisionKey] = append(collisionGroups[collisionKey], legacy.ID)
		state.expectedHistory[messageID] = expectedMessage{
			Legacy: legacy, Platform: conversation.Account.Platform, Account: conversation.Account,
			V2ID: messageID, RemoteID: remoteID, MediaID: legacy.MediaID,
		}
		if state.expectedHistoryByPlatform[conversation.Account.Platform] == nil {
			state.expectedHistoryByPlatform[conversation.Account.Platform] = map[string]struct{}{}
		}
		state.expectedHistoryByPlatform[conversation.Account.Platform][messageID] = struct{}{}

		var senderID *string
		if !legacy.IsFromMe && strings.TrimSpace(legacy.SenderNumber) != "" {
			key, err := identityKeyFor(conversation.Account, legacy.SenderNumber)
			if err != nil {
				return fmt.Errorf("message %q sender identity: %w", legacy.ID, err)
			}
			value := identityID(key)
			senderID = &value
		}
		var replyTo *string
		if strings.TrimSpace(legacy.ReplyToID) != "" {
			value := remoteByLegacyID[legacy.ReplyToID]
			if value == "" {
				value = stripPlatformPrefix(legacy.ReplyToID)
			}
			if strings.TrimSpace(value) != "" {
				replyTo = &value
			}
		}
		direction := sqlite.MessageDirectionIncoming
		if legacy.IsFromMe {
			direction = sqlite.MessageDirectionOutgoing
		}
		projection := sqlite.MessageProjection{Message: sqlite.Message{
			MessageID: messageID, ConversationID: conversation.V2ID,
			AccountID: conversation.Account.AccountID, RemoteMessageID: remoteID,
			SenderIdentityID: senderID, Direction: direction, Body: legacy.Body,
			ReplyToRemoteID: replyTo, State: sqlite.MessageStateActive,
			OccurredAtMS: legacy.TimestampMS,
		}}
		if strings.TrimSpace(legacy.MediaID) != "" {
			attachment, err := packLegacyAttachment(legacy, report)
			if err != nil {
				return fmt.Errorf("message %q attachment: %w", legacy.ID, err)
			}
			projection.Attachments = []sqlite.MessageAttachment{attachment}
		}
		*clockMS = legacy.TimestampMS
		if err := repository.ImportMessage(ctx, projection); err != nil {
			return fmt.Errorf("import message %q: %w", legacy.ID, err)
		}
	}

	for key, ids := range collisionGroups {
		if len(ids) < 2 {
			continue
		}
		parts := strings.Split(key, "\x1f")
		sort.Strings(ids)
		report.MessageCollisions = append(report.MessageCollisions, MessageCollision{
			AccountID: parts[0], ConversationID: legacyConversationIDForV2(state, parts[1]),
			RemoteMessageID: parts[2], LegacyMessageIDs: ids,
		})
	}
	sort.Slice(report.MessageCollisions, func(i, j int) bool {
		left, right := report.MessageCollisions[i], report.MessageCollisions[j]
		return left.AccountID+left.ConversationID+left.RemoteMessageID <
			right.AccountID+right.ConversationID+right.RemoteMessageID
	})
	return nil
}

func writeScheduled(
	ctx context.Context,
	messages *sqlite.MessageRepository,
	outbox *sqlite.OutboxRepository,
	blobs *blob.BlobStore,
	dataset legacyDataset,
	state *transformState,
	clockMS *int64,
	report *Report,
) error {
	remoteByLegacyID := make(map[string]string, len(dataset.messages))
	for _, message := range dataset.messages {
		remoteByLegacyID[message.ID] = deriveRemoteMessageID(message)
	}
	for _, scheduled := range dataset.scheduled {
		conversation := state.conversations[scheduled.ConversationID]
		switch scheduled.Status {
		case "sent", "failed", "canceled":
			continue
		case "pending", "sending":
		default:
			return fmt.Errorf("scheduled message %q has unsupported status %q", scheduled.ID, scheduled.Status)
		}
		createdAt := scheduled.CreatedAtMS
		if createdAt <= 0 {
			createdAt = state.baseTimestampMS
		}
		requestID := deriveID("transport_request", conversation.Account.AccountID, scheduled.ID)
		localMessageID := deriveID(
			"message", conversation.Account.AccountID,
			scheduled.ConversationID+"\x1f"+requestID,
		)
		var replyTo *string
		if strings.TrimSpace(scheduled.ReplyToID) != "" {
			value := remoteByLegacyID[scheduled.ReplyToID]
			if value == "" {
				value = stripPlatformPrefix(scheduled.ReplyToID)
			}
			if strings.TrimSpace(value) != "" {
				replyTo = &value
			}
		}
		message := sqlite.Message{
			MessageID: localMessageID, ConversationID: conversation.V2ID,
			AccountID: conversation.Account.AccountID, RemoteMessageID: requestID,
			Direction: sqlite.MessageDirectionOutgoing, Body: scheduled.Body,
			ReplyToRemoteID: replyTo, State: sqlite.MessageStateActive,
			OccurredAtMS: createdAt,
		}
		*clockMS = createdAt
		state.scheduledMessages[localMessageID] = struct{}{}
		if scheduled.Status == "sending" {
			if err := messages.ImportMessage(ctx, sqlite.MessageProjection{Message: message}); err != nil {
				return fmt.Errorf("import ambiguous sending message %q: %w", scheduled.ID, err)
			}
			continue
		}

		item := sqlite.NewOutboxItem{
			OutboxID:  deriveID("outbox", conversation.Account.AccountID, scheduled.ID),
			AccountID: conversation.Account.AccountID, ConversationID: conversation.V2ID,
			IdempotencyKey: scheduled.ID, LocalMessageID: localMessageID,
			TransportRequestID: requestID, ScheduledForMS: scheduled.SendAtMS,
		}
		if strings.TrimSpace(scheduled.MediaFilename) == "" {
			payloadHash, err := textPayloadHash(scheduled.Body, scheduled.ReplyToID)
			if err != nil {
				return fmt.Errorf("hash scheduled text %q: %w", scheduled.ID, err)
			}
			item.Kind = sqlite.OutboxKindText
			item.Operation = "send_text"
			item.PayloadHash = payloadHash
			if _, disposition, err := outbox.EnqueueOutgoingMessage(ctx, item, message); err != nil {
				return fmt.Errorf("enqueue scheduled text %q: %w", scheduled.ID, err)
			} else if disposition != sqlite.EnqueueInserted {
				return fmt.Errorf("enqueue scheduled text %q returned disposition %q", scheduled.ID, disposition)
			}
			continue
		}

		mime := strings.TrimSpace(scheduled.MediaMIME)
		if mime == "" {
			mime = "application/octet-stream"
		}
		ref, err := blobs.Put(ctx, bytes.NewReader(scheduled.MediaData), mime, scheduledMediaMaxBytes)
		if err != nil {
			return fmt.Errorf("store scheduled media %q: %w", scheduled.ID, err)
		}
		if ref.Size <= 0 {
			return fmt.Errorf("scheduled media %q is empty", scheduled.ID)
		}
		payloadHash, err := mediaPayloadHash(
			ref.Hash, ref.Size, mime, scheduled.MediaFilename, scheduled.Body, scheduled.ReplyToID,
		)
		if err != nil {
			return fmt.Errorf("hash scheduled media %q: %w", scheduled.ID, err)
		}
		item.Kind = sqlite.OutboxKindMedia
		item.Operation = "send_media"
		item.PayloadHash = payloadHash
		if _, disposition, err := outbox.EnqueueOutgoingMediaMessage(
			ctx, item, message, sqlite.OutboxAttachment{
				BlobHash: ref.Hash, SizeBytes: ref.Size, MIME: mime,
				Filename: scheduled.MediaFilename,
			},
		); err != nil {
			return fmt.Errorf("enqueue scheduled media %q: %w", scheduled.ID, err)
		} else if disposition != sqlite.EnqueueInserted {
			return fmt.Errorf("enqueue scheduled media %q returned disposition %q", scheduled.ID, disposition)
		}
		state.expectedPendingMedia++
		state.scheduledBlobs = append(state.scheduledBlobs, scheduledBlob{
			OutboxID: item.OutboxID,
			Ref:      ref,
		})
	}
	return nil
}

func writeReadCursors(
	target *sqlite.Store,
	dataset legacyDataset,
	state *transformState,
	report *Report,
) error {
	type messagePosition struct {
		ID string
		At int64
	}
	byConversation := map[string][]messagePosition{}
	for _, expected := range state.expectedHistory {
		byConversation[expected.Legacy.ConversationID] = append(
			byConversation[expected.Legacy.ConversationID],
			messagePosition{ID: expected.V2ID, At: expected.Legacy.TimestampMS},
		)
	}
	ids := make([]string, 0, len(state.conversations))
	for id := range state.conversations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, legacyID := range ids {
		conversation := state.conversations[legacyID]
		positions := byConversation[legacyID]
		sort.Slice(positions, func(i, j int) bool {
			if positions[i].At != positions[j].At {
				return positions[i].At < positions[j].At
			}
			return positions[i].ID < positions[j].ID
		})
		unread := conversation.Legacy.UnreadCount
		if unread < 0 {
			unread = 0
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("conversation %s had a negative unread_count; treated as zero", legacyID))
		}
		var lastReadID *string
		lastReadAt := int64(0)
		index := int64(len(positions)) - unread - 1
		if index >= 0 && index < int64(len(positions)) {
			value := positions[index].ID
			lastReadID = &value
			lastReadAt = positions[index].At
		}
		deviceID := deriveID(
			"device", conversation.Account.AccountID,
			conversation.Account.AccountID+"\x1flocal",
		)
		updatedAt := state.baseTimestampMS
		if len(positions) > 0 {
			updatedAt = maxInt64(updatedAt, positions[len(positions)-1].At)
		}
		if err := target.UpsertReadCursor(sqlite.ReadCursor{
			AccountID: conversation.Account.AccountID, DeviceID: deviceID,
			ConversationID: conversation.V2ID, LastReadMessageID: lastReadID,
			LastReadAtMS: lastReadAt, UpdatedAtMS: updatedAt,
		}); err != nil {
			return fmt.Errorf("upsert read cursor for conversation %q: %w", legacyID, err)
		}
		state.expectedCursors++
	}
	report.ReadState.CursorsCreated = state.expectedCursors
	return nil
}

func packLegacyAttachment(message legacyMessage, report *Report) (sqlite.MessageAttachment, error) {
	attachment := sqlite.MessageAttachment{
		Ordinal: 0, RemoteID: message.MediaID, MIME: strings.TrimSpace(message.MIME),
		State: "pending",
	}
	if attachment.MIME == "" {
		attachment.MIME = "application/octet-stream"
	}
	platform := normalizeLegacyPlatform(message.Platform)
	var payload any
	switch platform {
	case "sms":
		payload = struct {
			Version       int    `json:"v"`
			MediaID       string `json:"media_id"`
			DecryptionKey string `json:"decryption_key"`
		}{1, message.MediaID, message.DecryptionKey}
	case "whatsapp":
		if !whatsappmedia.IsLocalMediaRef(message.MediaID) {
			report.Media.WhatsAppUnresolvable++
			return attachment, nil
		}
		if _, err := whatsappmedia.DecodeLocalMediaRef(message.MediaID); err != nil {
			report.Media.WhatsAppUnresolvable++
			return attachment, nil
		}
		payload = struct {
			Version  int    `json:"v"`
			Kind     string `json:"kind"`
			LocalRef string `json:"local_ref"`
		}{1, "local", message.MediaID}
	case "signal":
		const (
			remotePrefix = "signalatt:"
			localPrefix  = "signallocal:"
		)
		switch {
		case strings.HasPrefix(message.MediaID, localPrefix):
			encoded := strings.TrimSpace(strings.TrimPrefix(message.MediaID, localPrefix))
			decoded, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil || strings.TrimSpace(string(decoded)) == "" {
				// Symmetric with the WhatsApp unresolvable path: one corrupt
				// attachment ref in years of history must not abort the whole
				// migration. The message migrates; the attachment stays pending
				// with an empty remote_ref and is counted as unresolvable.
				report.Media.SignalUnresolvable++
				return attachment, nil
			}
			report.Media.SignalLocalAttachments++
			payload = struct {
				Version int    `json:"v"`
				Kind    string `json:"kind"`
				Path    string `json:"path"`
			}{1, "local", strings.TrimSpace(string(decoded))}
		default:
			attachmentID := strings.TrimSpace(message.MediaID)
			if strings.HasPrefix(attachmentID, remotePrefix) {
				attachmentID = strings.TrimSpace(strings.TrimPrefix(attachmentID, remotePrefix))
			}
			if attachmentID == "" {
				report.Media.SignalUnresolvable++
				return attachment, nil
			}
			payload = struct {
				Version        int    `json:"v"`
				Kind           string `json:"kind"`
				AttachmentID   string `json:"att_id"`
				ConversationID string `json:"conversation_id"`
			}{1, "remote", attachmentID, message.ConversationID}
		}
	case "gchat", "imessage":
		report.Media.UnsupportedArchiveAttachments++
		return attachment, nil
	default:
		return attachment, fmt.Errorf("unsupported platform %q", platform)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return attachment, err
	}
	attachment.RemoteRef = encoded
	return attachment, nil
}

func addIdentity(
	state *transformState,
	account accountSpec,
	raw string,
	displayName string,
	isSelf bool,
	priority int,
) (identityKey, error) {
	key, err := identityKeyFor(account, raw)
	if err != nil {
		return identityKey{}, err
	}
	candidate, exists := state.identities[key]
	if !exists {
		state.identities[key] = &identityCandidate{
			Key: key, Raw: strings.TrimSpace(raw), DisplayName: strings.TrimSpace(displayName),
			IsSelf: isSelf, Priority: priority,
		}
		return key, nil
	}
	candidate.IsSelf = candidate.IsSelf || isSelf
	if strings.TrimSpace(displayName) != "" && (candidate.DisplayName == "" || priority > candidate.Priority) {
		candidate.DisplayName = strings.TrimSpace(displayName)
		candidate.Priority = priority
	}
	return key, nil
}

var signalACI = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func identityKeyFor(account accountSpec, raw string) (identityKey, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return identityKey{}, fmt.Errorf("identity value is empty")
	}
	kind := "username"
	canonical := value
	switch {
	case strings.HasPrefix(value, "+"):
		var digits strings.Builder
		for _, character := range value[1:] {
			if character >= '0' && character <= '9' {
				digits.WriteRune(character)
			}
		}
		if digits.Len() == 0 {
			return identityKey{}, fmt.Errorf("invalid E.164 identity %q", value)
		}
		kind = "e164"
		canonical = "+" + digits.String()
	case account.Platform == "signal" && signalACI.MatchString(value):
		kind = "signal_aci"
		canonical = strings.ToLower(value)
	case strings.Contains(value, "@") && account.Platform == "whatsapp":
		kind = "jid"
		canonical = strings.ToLower(value)
	case strings.Contains(value, "@"):
		kind = "email"
		canonical = strings.ToLower(value)
	case strings.HasPrefix(value, "legacy-participant:"):
		kind = "legacy_participant"
	}
	return identityKey{AccountID: account.AccountID, Kind: kind, Canonical: canonical}, nil
}

func identityID(key identityKey) string {
	return deriveID("identity", key.AccountID, key.Kind+"\x1f"+key.Canonical)
}

func deriveID(entity, accountID, naturalKey string) string {
	sum := sha256.Sum256([]byte(entity + "\x1f" + accountID + "\x1f" + naturalKey))
	return hex.EncodeToString(sum[:])[:32]
}

func deriveRemoteMessageID(message legacyMessage) string {
	if message.SourceID != "" {
		return message.SourceID
	}
	return stripPlatformPrefix(message.ID)
}

func stripPlatformPrefix(value string) string {
	for _, prefix := range []string{"whatsapp:", "signal:", "gchat:", "imessage:"} {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return value
}

func normalizeLegacyPlatform(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "sms", "google", "google_messages":
		return "sms"
	case "wa", "whatsapp", "whatsmeow":
		return "whatsapp"
	case "signal", "signal_cli":
		return "signal"
	case "gchat", "google_chat":
		return "gchat"
	case "imessage", "apple_messages":
		return "imessage"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func accountForPlatform(platform string) (accountSpec, error) {
	account, ok := platformAccounts[normalizeLegacyPlatform(platform)]
	if !ok {
		return accountSpec{}, fmt.Errorf("unsupported legacy source_platform %q", platform)
	}
	return account, nil
}

func normalizeRemoteConversationID(platform, legacyID string) string {
	value := strings.TrimSpace(legacyID)
	if platform != "signal" {
		return value
	}
	if strings.HasPrefix(value, "signal-group:") {
		return "signal-group:" + strings.TrimSpace(strings.TrimPrefix(value, "signal-group:"))
	}
	if strings.HasPrefix(value, "signal:") {
		return "signal:" + strings.TrimSpace(strings.TrimPrefix(value, "signal:"))
	}
	return value
}

func minLegacyTimestamp(dataset legacyDataset) int64 {
	minimum := int64(0)
	consider := func(value int64) {
		if value > 0 && (minimum == 0 || value < minimum) {
			minimum = value
		}
	}
	for _, conversation := range dataset.conversations {
		consider(conversation.LastMessageMS)
	}
	for _, message := range dataset.messages {
		consider(message.TimestampMS)
	}
	for _, scheduled := range dataset.scheduled {
		consider(scheduled.CreatedAtMS)
		consider(scheduled.SendAtMS)
	}
	if minimum == 0 {
		return 1
	}
	return minimum
}

func conversationTimes(conversation legacyConversation, base int64) (int64, int64) {
	created := base
	last := conversation.LastMessageMS
	if last < 0 {
		last = 0
	}
	return created, last
}

func scheduleReport(dataset legacyDataset) ScheduleReport {
	return ScheduleReport{
		LegacyTotal: int64(len(dataset.scheduled)), ByStatus: dataset.scheduleByStatus,
		PendingToOutbox:    dataset.scheduleByStatus["pending"],
		SkippedSent:        dataset.scheduleByStatus["sent"],
		SkippedCanceled:    dataset.scheduleByStatus["canceled"],
		SkippedFailed:      dataset.scheduleByStatus["failed"],
		AmbiguousSending:   dataset.scheduleByStatus["sending"],
		OptimisticOnlyRows: dataset.scheduleByStatus["sending"],
	}
}

func appendRequiredWarnings(report *Report) {
	report.Warnings = append(report.Warnings, report.ReadState.LossyWarning)
	if count := report.Dropped.ReactionsBearingMessages; count > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d messages with reactions were counted and dropped: merged v2 has no reaction read model", count))
	}
	if count := report.Dropped.TranscriptBearingMessages; count > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d messages with transcripts were counted and dropped: merged v2 has no transcript columns", count))
	}
	if count := report.Dropped.ContactAvatars; count > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d contact avatars were counted and dropped", count))
	}
	if count := report.Dropped.Drafts; count > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d drafts were counted and dropped", count))
	}
	if count := report.Dropped.ContactMetaCRM; count > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("IMPORTANT: %d contact_meta CRM records (tags/cadence/summaries) were counted and dropped; export them before deleting the legacy store", count))
	}
	if count := report.Dropped.OutgoingSendKeys; count > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d legacy outgoing_send_keys tombstones were counted and dropped", count))
	}
	if count := report.Dropped.Tabs; count > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d custom tabs were counted and dropped", count))
	}
	if count := report.Schedule.AmbiguousSending; count > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d ambiguous in-flight scheduled sends were not auto-resent; optimistic messages were preserved for verify/SendAgain", count))
	}
}

func textPayloadHash(body, replyToMessageID string) (string, error) {
	payload, err := json.Marshal(struct {
		Body             string `json:"body"`
		ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	}{body, replyToMessageID})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func mediaPayloadHash(
	blobHash string,
	size int64,
	mime string,
	filename string,
	caption string,
	replyToMessageID string,
) (string, error) {
	payload, err := json.Marshal(struct {
		BlobHash         string `json:"blob_hash"`
		Size             int64  `json:"size"`
		MIME             string `json:"mime"`
		Filename         string `json:"filename"`
		Caption          string `json:"caption,omitempty"`
		ReplyToMessageID string `json:"reply_to_message_id,omitempty"`
	}{blobHash, size, mime, filename, caption, replyToMessageID})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func sortedAccountPlatforms(accounts map[string]accountSpec) []string {
	platforms := make([]string, 0, len(accounts))
	for platform := range accounts {
		platforms = append(platforms, platform)
	}
	sort.Slice(platforms, func(i, j int) bool {
		return accounts[platforms[i]].AccountID < accounts[platforms[j]].AccountID
	})
	return platforms
}

func legacyConversationIDForV2(state *transformState, v2ID string) string {
	for legacyID, conversation := range state.conversations {
		if conversation.V2ID == v2ID {
			return legacyID
		}
	}
	return v2ID
}

func requireAbsent(path string) error {
	_, err := os.Lstat(path)
	if err == nil {
		return fmt.Errorf("path already exists: %s", path)
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
