package app

import (
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// GMClient abstracts the libgm methods used by backfill so we can test with
// a mock implementation. The real implementation wraps *libgm.Client.
type GMClient interface {
	ListConversationsWithCursor(count int, folder gmproto.ListConversationsRequest_Folder, cursor *gmproto.Cursor) (*gmproto.ListConversationsResponse, error)
	FetchMessages(conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error)
	GetOrCreateConversation(req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error)
	ListContacts() (*gmproto.ListContactsResponse, error)
	GetParticipantThumbnail(participantIDs ...string) (*gmproto.GetThumbnailResponse, error)
	GetContactThumbnail(contactIDs ...string) (*gmproto.GetThumbnailResponse, error)
}

// realGMClient wraps *libgm.Client to implement GMClient.
type realGMClient struct {
	gm *libgm.Client
}

func newRealGMClient(gm *libgm.Client) GMClient {
	return &realGMClient{gm: gm}
}

func (r *realGMClient) ListConversationsWithCursor(count int, folder gmproto.ListConversationsRequest_Folder, cursor *gmproto.Cursor) (*gmproto.ListConversationsResponse, error) {
	return r.gm.ListConversationsWithCursor(count, folder, cursor)
}

func (r *realGMClient) FetchMessages(conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	return r.gm.FetchMessages(conversationID, count, cursor)
}

func (r *realGMClient) GetOrCreateConversation(req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error) {
	return r.gm.GetOrCreateConversation(req)
}

func (r *realGMClient) ListContacts() (*gmproto.ListContactsResponse, error) {
	return r.gm.ListContacts()
}

func (r *realGMClient) GetParticipantThumbnail(participantIDs ...string) (*gmproto.GetThumbnailResponse, error) {
	return r.gm.GetParticipantThumbnail(participantIDs...)
}

func (r *realGMClient) GetContactThumbnail(contactIDs ...string) (*gmproto.GetThumbnailResponse, error) {
	return r.gm.GetContactThumbnail(contactIDs...)
}
