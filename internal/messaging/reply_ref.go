package messaging

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// replyRefForLease describes the message a text or media send replies to.
//
// The outbox message stores only the quoted message's remote ID, as it stood
// when the reply was submitted. A transport that quotes by author and sent
// time rather than by remote ID (Signal) needs the stored message itself, so
// the ref carries its author, occurred time, body and first attachment.
//
// The quoted message is the one the conversation holds under that remote ID.
// When the reply was submitted while the quoted message was an outgoing one
// still waiting for its transport ID, the stored ID is that message's
// transport request ID. Confirming the send has since moved the message to
// the transport's ID, so the outbox row carrying the request ID leads back to
// it, and the ref names its current remote ID.
//
// A quoted message the store does not hold, or an outgoing one the transport
// has not accepted yet, yields a bare ref (RemoteID only), which is what every
// reply carried before. Store errors fail the attempt before the transport is
// called, like the other pre-call loads.
func (s *MessageService) replyRefForLease(
	ctx context.Context,
	item sqlite.OutboxItem,
	message sqlite.Message,
) (*bridge.MessageRef, string, error) {
	if message.ReplyToRemoteID == nil {
		return nil, "", nil
	}
	remoteID := *message.ReplyToRemoteID
	target, found, err := s.replyTarget(ctx, item, remoteID)
	if err != nil {
		return nil, "load_reply_target", err
	}
	if !found {
		return &bridge.MessageRef{RemoteID: remoteID}, "", nil
	}
	awaiting, err := s.awaitingTransportID(ctx, item, target)
	if err != nil {
		return nil, "load_reply_target", err
	}
	if awaiting {
		return &bridge.MessageRef{RemoteID: target.RemoteMessageID}, "", nil
	}
	authorID, err := s.messageAuthorID(item, target)
	if err != nil {
		return nil, "load_reply_target_author", err
	}
	mime, hasAttachment, err := s.messages.FirstAttachmentMIME(ctx, target.MessageID)
	if err != nil {
		return nil, "load_reply_target_attachment", err
	}
	return &bridge.MessageRef{
		RemoteID:       target.RemoteMessageID,
		AuthorID:       authorID,
		Outgoing:       target.Direction == sqlite.MessageDirectionOutgoing,
		SentAt:         time.UnixMilli(target.OccurredAtMS),
		Text:           target.Body,
		HasAttachment:  hasAttachment,
		AttachmentMIME: mime,
	}, "", nil
}

// replyTarget finds the message a reply's stored remote ID names in the
// outbox item's conversation: the message held under that remote ID, else the
// local message of the outbox row whose transport request ID it is.
func (s *MessageService) replyTarget(
	ctx context.Context,
	item sqlite.OutboxItem,
	remoteID string,
) (sqlite.Message, bool, error) {
	target, err := s.messages.GetMessageByRemote(ctx, item.AccountID, item.ConversationID, remoteID)
	if err == nil {
		return target, true, nil
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Message{}, false, err
	}

	row, err := s.outbox.FindByTransportRequestID(ctx, item.AccountID, remoteID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Message{}, false, nil
	}
	if err != nil {
		return sqlite.Message{}, false, err
	}
	if row.LocalMessageID == nil || row.ConversationID != item.ConversationID {
		return sqlite.Message{}, false, nil
	}
	target, err = s.messages.GetMessage(ctx, *row.LocalMessageID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return sqlite.Message{}, false, nil
	}
	if err != nil {
		return sqlite.Message{}, false, err
	}
	if target.AccountID != item.AccountID || target.ConversationID != item.ConversationID {
		return sqlite.Message{}, false, nil
	}
	return target, true, nil
}

// awaitingTransportID reports whether target is an outgoing message whose
// remote ID is still the transport request ID of its own outbox row, so the
// transport has not given it an ID (Signal: a sent timestamp) yet.
func (s *MessageService) awaitingTransportID(
	ctx context.Context,
	item sqlite.OutboxItem,
	target sqlite.Message,
) (bool, error) {
	if target.Direction != sqlite.MessageDirectionOutgoing {
		return false, nil
	}
	row, err := s.outbox.FindByTransportRequestID(ctx, item.AccountID, target.RemoteMessageID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return row.LocalMessageID != nil && *row.LocalMessageID == target.MessageID, nil
}

// messageAuthorID returns the canonical identity of a stored message's
// author. A nil sender marks an outgoing message authored by self, so an empty
// transport-neutral AuthorID preserves that distinction for adapter shims.
func (s *MessageService) messageAuthorID(
	item sqlite.OutboxItem,
	target sqlite.Message,
) (string, error) {
	if target.SenderIdentityID == nil {
		return "", nil
	}
	identity, err := s.store.GetIdentity(*target.SenderIdentityID)
	if err != nil {
		return "", err
	}
	if identity.AccountID != item.AccountID {
		return "", fmt.Errorf(
			"target author %q belongs to account %q",
			identity.IdentityID,
			identity.AccountID,
		)
	}
	return identity.CanonicalValue, nil
}
