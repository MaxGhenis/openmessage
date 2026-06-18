package app

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/db"
)

// mockGMClient implements GMClient for testing.
type mockGMClient struct {
	// conversations maps folder -> pages of conversations (each page is a slice).
	// The outer slice is pages; each page is a slice of conversations.
	conversations map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation

	// messages maps conversationID -> pages of messages.
	messages map[string][][]*gmproto.Message

	// contacts returned by ListContacts
	contacts []*gmproto.Contact

	// getOrCreateResults maps phone number -> conversation
	getOrCreateResults map[string]*gmproto.Conversation

	// Error injection
	listConvErrors  map[gmproto.ListConversationsRequest_Folder]error // folder -> error
	fetchMsgErrors  map[string]error                                  // convID -> error
	listContactsErr error
	getOrCreateErrs map[string]error // phone -> error

	afterFetchMessages func(conversationID string, pageIdx int)
	fetchCalls         map[string]int

	mu                    sync.Mutex
	participantThumbnails map[string][]byte
	contactThumbnails     map[string][]byte
	participantThumbCalls map[string]int
	contactThumbCalls     map[string]int
}

func (m *mockGMClient) ListConversationsWithCursor(count int, folder gmproto.ListConversationsRequest_Folder, cursor *gmproto.Cursor) (*gmproto.ListConversationsResponse, error) {
	if err, ok := m.listConvErrors[folder]; ok && err != nil {
		return nil, err
	}

	pages := m.conversations[folder]
	if len(pages) == 0 {
		return &gmproto.ListConversationsResponse{}, nil
	}

	// Determine which page based on cursor
	pageIdx := 0
	if cursor != nil && cursor.LastItemID != "" {
		// Parse page index from cursor ID (format: "page_N")
		fmt.Sscanf(cursor.LastItemID, "page_%d", &pageIdx)
	}

	if pageIdx >= len(pages) {
		return &gmproto.ListConversationsResponse{}, nil
	}

	resp := &gmproto.ListConversationsResponse{
		Conversations: pages[pageIdx],
	}

	// Set cursor for next page if there are more pages
	if pageIdx+1 < len(pages) {
		resp.Cursor = &gmproto.Cursor{
			LastItemID: fmt.Sprintf("page_%d", pageIdx+1),
		}
	}

	return resp, nil
}

func (m *mockGMClient) FetchMessages(conversationID string, count int64, cursor *gmproto.Cursor) (*gmproto.ListMessagesResponse, error) {
	if err, ok := m.fetchMsgErrors[conversationID]; ok && err != nil {
		return nil, err
	}

	pages := m.messages[conversationID]
	if len(pages) == 0 {
		return &gmproto.ListMessagesResponse{}, nil
	}

	pageIdx := 0
	if cursor != nil && cursor.LastItemID != "" {
		fmt.Sscanf(cursor.LastItemID, "msgpage_%d", &pageIdx)
	}

	if pageIdx >= len(pages) {
		return &gmproto.ListMessagesResponse{}, nil
	}
	if m.fetchCalls != nil {
		m.fetchCalls[conversationID]++
	}

	resp := &gmproto.ListMessagesResponse{
		Messages: pages[pageIdx],
	}

	if pageIdx+1 < len(pages) {
		resp.Cursor = &gmproto.Cursor{
			LastItemID: fmt.Sprintf("msgpage_%d", pageIdx+1),
		}
	}
	if m.afterFetchMessages != nil {
		m.afterFetchMessages(conversationID, pageIdx)
	}

	return resp, nil
}

func (m *mockGMClient) GetOrCreateConversation(req *gmproto.GetOrCreateConversationRequest) (*gmproto.GetOrCreateConversationResponse, error) {
	if len(req.Numbers) == 0 {
		return nil, fmt.Errorf("no numbers provided")
	}
	phone := req.Numbers[0].Number

	if err, ok := m.getOrCreateErrs[phone]; ok && err != nil {
		return nil, err
	}

	if conv, ok := m.getOrCreateResults[phone]; ok {
		return &gmproto.GetOrCreateConversationResponse{
			Conversation: conv,
		}, nil
	}

	return &gmproto.GetOrCreateConversationResponse{}, nil
}

func (m *mockGMClient) ListContacts() (*gmproto.ListContactsResponse, error) {
	if m.listContactsErr != nil {
		return nil, m.listContactsErr
	}
	return &gmproto.ListContactsResponse{
		Contacts: m.contacts,
	}, nil
}

func (m *mockGMClient) GetParticipantThumbnail(participantIDs ...string) (*gmproto.GetThumbnailResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.participantThumbCalls == nil {
		m.participantThumbCalls = map[string]int{}
	}
	for _, id := range participantIDs {
		m.participantThumbCalls[id]++
	}
	return thumbnailResponseForIDs(participantIDs, m.participantThumbnails), nil
}

func (m *mockGMClient) GetContactThumbnail(contactIDs ...string) (*gmproto.GetThumbnailResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.contactThumbCalls == nil {
		m.contactThumbCalls = map[string]int{}
	}
	for _, id := range contactIDs {
		m.contactThumbCalls[id]++
	}
	return thumbnailResponseForIDs(contactIDs, m.contactThumbnails), nil
}

func thumbnailResponseForIDs(ids []string, images map[string][]byte) *gmproto.GetThumbnailResponse {
	resp := &gmproto.GetThumbnailResponse{}
	for _, id := range ids {
		if len(images[id]) == 0 {
			continue
		}
		resp.Thumbnail = append(resp.Thumbnail, &gmproto.GetThumbnailResponse_Thumbnail{
			Identifier: id,
			Data:       &gmproto.ThumbnailData{ImageBuffer: images[id]},
		})
	}
	return resp
}

// helper to make a proto conversation
func makeConv(id, name string) *gmproto.Conversation {
	return &gmproto.Conversation{
		ConversationID:       id,
		Name:                 name,
		LastMessageTimestamp: 1000000, // 1000ms
	}
}

// helper to make a proto message
func makeMsg(id, convID, body string, ts int64) *gmproto.Message {
	return &gmproto.Message{
		MessageID:      id,
		ConversationID: convID,
		Timestamp:      ts * 1000, // convert ms to µs
		MessageInfo: []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: body},
			},
		}},
	}
}

func newTestApp(t *testing.T, mock *mockGMClient) *App {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	a := &App{
		Store:    store,
		Logger:   zerolog.Nop(),
		gmClient: mock,
	}
	t.Cleanup(a.StopGoogleAvatarSync)
	return a
}

var testAvatarPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func TestSyncGoogleContactsQueuesAvatarFetch(t *testing.T) {
	mock := &mockGMClient{
		contacts: []*gmproto.Contact{{
			ParticipantID: "participant-1",
			ContactID:     "contact-1",
			Name:          "Alice",
			Number:        &gmproto.ContactNumber{Number: "(615) 555-0100"},
		}},
		contactThumbnails: map[string][]byte{"contact-1": testAvatarPNG},
	}
	a := newTestApp(t, mock)

	count, err := a.SyncGoogleContacts()
	if err != nil {
		t.Fatalf("SyncGoogleContacts() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("SyncGoogleContacts() count = %d, want 1", count)
	}

	var avatar *db.ContactAvatar
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		avatar, err = a.Store.GetContactAvatar("sms", "participant-1", "contact-1", "(615) 555-0100")
		if err != nil {
			t.Fatalf("GetContactAvatar() error = %v", err)
		}
		if avatar != nil && len(avatar.ImageData) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if avatar == nil || len(avatar.ImageData) == 0 {
		t.Fatal("Google contact avatar was not cached")
	}
	if avatar.MimeType != "image/png" {
		t.Fatalf("avatar.MimeType = %q, want image/png", avatar.MimeType)
	}

	mock.mu.Lock()
	contactCalls := mock.contactThumbCalls["contact-1"]
	participantCalls := mock.participantThumbCalls["participant-1"]
	mock.mu.Unlock()
	if contactCalls == 0 {
		t.Fatal("GetContactThumbnail(contact-1) was not called")
	}
	if participantCalls != 0 {
		t.Fatalf("GetParticipantThumbnail(participant-1) calls = %d, want 0", participantCalls)
	}
}

func TestStopGoogleAvatarSyncPreventsLaterQueue(t *testing.T) {
	mock := &mockGMClient{
		contactThumbnails: map[string][]byte{"contact-1": testAvatarPNG},
	}
	a := newTestApp(t, mock)
	a.StopGoogleAvatarSync()

	a.QueueGoogleAvatarCandidates([]db.ContactAvatarCandidate{{
		SourcePlatform: "sms",
		ContactID:      "contact-1",
		PhoneNumber:    "6155550100",
		Source:         "contacts",
	}})
	time.Sleep(25 * time.Millisecond)

	mock.mu.Lock()
	contactCalls := mock.contactThumbCalls["contact-1"]
	mock.mu.Unlock()
	if contactCalls != 0 {
		t.Fatalf("GetContactThumbnail(contact-1) calls after stop = %d, want 0", contactCalls)
	}
}

// --- Tests ---

func TestDeepBackfillSinglePageSingleFolder(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice"), makeConv("c2", "Bob"), makeConv("c3", "Charlie")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {{makeMsg("m1", "c1", "hi", 100)}},
			"c2": {{makeMsg("m2", "c2", "hey", 200)}},
			"c3": {{makeMsg("m3", "c3", "yo", 300)}},
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	convos, _ := a.Store.ListConversations(50)
	if len(convos) != 3 {
		t.Fatalf("got %d conversations, want 3", len(convos))
	}

	progress := a.GetBackfillProgress()
	if progress.ConversationsFound != 3 {
		t.Errorf("progress.ConversationsFound = %d, want 3", progress.ConversationsFound)
	}
	if progress.MessagesFound != 3 {
		t.Errorf("progress.MessagesFound = %d, want 3", progress.MessagesFound)
	}
	if progress.Phase != BackfillPhaseDone {
		t.Errorf("progress.Phase = %q, want %q", progress.Phase, BackfillPhaseDone)
	}
}

func TestDeepBackfillMultiPageSingleFolder(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice"), makeConv("c2", "Bob")},     // page 0
				{makeConv("c3", "Charlie"), makeConv("c4", "Diana")}, // page 1
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {{makeMsg("m1", "c1", "hi", 100)}},
			"c2": {{makeMsg("m2", "c2", "hey", 200)}},
			"c3": {{makeMsg("m3", "c3", "yo", 300)}},
			"c4": {{makeMsg("m4", "c4", "sup", 400)}},
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	convos, _ := a.Store.ListConversations(50)
	if len(convos) != 4 {
		t.Fatalf("got %d conversations, want 4 (2 pages)", len(convos))
	}

	progress := a.GetBackfillProgress()
	if progress.ConversationsFound != 4 {
		t.Errorf("progress.ConversationsFound = %d, want 4", progress.ConversationsFound)
	}
}

func TestDeepBackfillMultiFolder(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
			gmproto.ListConversationsRequest_ARCHIVE: {
				{makeConv("c2", "Bob")},
			},
			gmproto.ListConversationsRequest_SPAM_BLOCKED: {
				{makeConv("c3", "Charlie")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {{makeMsg("m1", "c1", "hi", 100)}},
			"c2": {{makeMsg("m2", "c2", "hey", 200)}},
			"c3": {{makeMsg("m3", "c3", "yo", 300)}},
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	convos, _ := a.Store.ListConversations(50)
	if len(convos) != 3 {
		t.Fatalf("got %d conversations, want 3 (one per folder)", len(convos))
	}

	progress := a.GetBackfillProgress()
	if progress.FoldersScanned != 3 {
		t.Errorf("progress.FoldersScanned = %d, want 3", progress.FoldersScanned)
	}
}

func TestDeepBackfillMultiPageMultiFolder(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
				{makeConv("c2", "Bob")},
			},
			gmproto.ListConversationsRequest_ARCHIVE: {
				{makeConv("c3", "Charlie")},
				{makeConv("c4", "Diana")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {{makeMsg("m1", "c1", "hi", 100)}},
			"c2": {{makeMsg("m2", "c2", "hey", 200)}},
			"c3": {{makeMsg("m3", "c3", "yo", 300)}},
			"c4": {{makeMsg("m4", "c4", "sup", 400)}},
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	convos, _ := a.Store.ListConversations(50)
	if len(convos) != 4 {
		t.Fatalf("got %d conversations, want 4", len(convos))
	}
}

func TestDeepBackfillMessagePagination(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {
				{makeMsg("m1", "c1", "page1-a", 100), makeMsg("m2", "c1", "page1-b", 200)},
				{makeMsg("m3", "c1", "page2-a", 300), makeMsg("m4", "c1", "page2-b", 400)},
				{makeMsg("m5", "c1", "page3-a", 500)},
			},
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	msgs, _ := a.Store.GetMessagesByConversation("c1", 100)
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5 (3 pages)", len(msgs))
	}

	progress := a.GetBackfillProgress()
	if progress.MessagesFound != 5 {
		t.Errorf("progress.MessagesFound = %d, want 5", progress.MessagesFound)
	}
}

func TestDeepBackfillContactDiscovery(t *testing.T) {
	t.Setenv("OPENMESSAGES_BACKFILL_DISCOVER_ORPHANS", "1")
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1":     {{makeMsg("m1", "c1", "hi", 100)}},
			"c-mary": {{makeMsg("m2", "c-mary", "old msg", 50)}},
		},
		contacts: []*gmproto.Contact{
			{
				Name:   "Mary",
				Number: &gmproto.ContactNumber{Number: "+14157934268"},
			},
		},
		getOrCreateResults: map[string]*gmproto.Conversation{
			"+14157934268": makeConv("c-mary", "Mary MacLeod"),
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	convos, _ := a.Store.ListConversations(50)
	if len(convos) != 2 {
		t.Fatalf("got %d conversations, want 2 (1 inbox + 1 contact)", len(convos))
	}

	progress := a.GetBackfillProgress()
	if progress.ContactsChecked != 1 {
		t.Errorf("progress.ContactsChecked = %d, want 1", progress.ContactsChecked)
	}
}

func TestDeepBackfillContactDiscoverySkipsAlreadySeen(t *testing.T) {
	t.Setenv("OPENMESSAGES_BACKFILL_DISCOVER_ORPHANS", "1")
	// Contact's phone maps to a conversation already found in INBOX
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {{makeMsg("m1", "c1", "hi", 100)}},
		},
		contacts: []*gmproto.Contact{
			{
				Name:   "Alice",
				Number: &gmproto.ContactNumber{Number: "+15551234567"},
			},
		},
		getOrCreateResults: map[string]*gmproto.Conversation{
			"+15551234567": makeConv("c1", "Alice"), // same conv ID as inbox
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	convos, _ := a.Store.ListConversations(50)
	if len(convos) != 1 {
		t.Fatalf("got %d conversations, want 1 (contact maps to existing)", len(convos))
	}
}

// TestDeepBackfillSkipsPhaseCWhenOptOut confirms Phase C does NOT run when
// the env var is unset. The mock has a contact whose phone would map to a
// brand-new conversation; without orphan discovery enabled, that contact
// must not be queried via GetOrCreateConversation, so the conversation
// must not appear in the store.
func TestDeepBackfillSkipsPhaseCWhenOptOut(t *testing.T) {
	// Explicitly empty (default behavior).
	t.Setenv("OPENMESSAGES_BACKFILL_DISCOVER_ORPHANS", "")
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1":     {{makeMsg("m1", "c1", "hi", 100)}},
			"c-mary": {{makeMsg("m2", "c-mary", "old msg", 50)}},
		},
		contacts: []*gmproto.Contact{
			{
				Name:   "Mary",
				Number: &gmproto.ContactNumber{Number: "+15555555555"},
			},
		},
		getOrCreateResults: map[string]*gmproto.Conversation{
			"+15555555555": makeConv("c-mary", "Mary"),
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	convos, _ := a.Store.ListConversations(50)
	if len(convos) != 1 {
		t.Fatalf("got %d conversations, want 1 (Phase C should be skipped)", len(convos))
	}
	progress := a.BackfillProgress.snapshot()
	if progress.ContactsChecked != 0 {
		t.Fatalf("got %d contacts checked, want 0 (Phase C should be skipped)", progress.ContactsChecked)
	}
}

func TestDeepBackfillFolderListError(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
			// ARCHIVE will error
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {{makeMsg("m1", "c1", "hi", 100)}},
		},
		listConvErrors: map[gmproto.ListConversationsRequest_Folder]error{
			gmproto.ListConversationsRequest_ARCHIVE: fmt.Errorf("server error"),
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	// INBOX should still work despite ARCHIVE failing
	convos, _ := a.Store.ListConversations(50)
	if len(convos) != 1 {
		t.Fatalf("got %d conversations, want 1 (INBOX should succeed despite ARCHIVE error)", len(convos))
	}

	progress := a.GetBackfillProgress()
	if progress.Errors < 1 {
		t.Errorf("expected at least 1 error, got %d", progress.Errors)
	}
}

func TestDeepBackfillMessageFetchError(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice"), makeConv("c2", "Bob")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c2": {{makeMsg("m2", "c2", "hey", 200)}},
		},
		fetchMsgErrors: map[string]error{
			"c1": fmt.Errorf("timeout"),
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	// c2's messages should still be fetched despite c1 error
	msgs, _ := a.Store.GetMessagesByConversation("c2", 100)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages for c2, want 1", len(msgs))
	}

	progress := a.GetBackfillProgress()
	if progress.Errors < 1 {
		t.Errorf("expected at least 1 error, got %d", progress.Errors)
	}
}

func TestDeepBackfillGetOrCreateError(t *testing.T) {
	t.Setenv("OPENMESSAGES_BACKFILL_DISCOVER_ORPHANS", "1")
	mock := &mockGMClient{
		contacts: []*gmproto.Contact{
			{
				Name:   "Bad Contact",
				Number: &gmproto.ContactNumber{Number: "+10000000000"},
			},
		},
		getOrCreateErrs: map[string]error{
			"+10000000000": fmt.Errorf("not found"),
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	progress := a.GetBackfillProgress()
	if progress.Errors < 1 {
		t.Errorf("expected at least 1 error for failed GetOrCreate, got %d", progress.Errors)
	}
	if progress.ContactsChecked != 1 {
		t.Errorf("expected 1 contact checked, got %d", progress.ContactsChecked)
	}
}

func TestDeepBackfillDedupSameConvoInMultipleFolders(t *testing.T) {
	// Same conversation appears in both INBOX and ARCHIVE
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
			gmproto.ListConversationsRequest_ARCHIVE: {
				{makeConv("c1", "Alice")}, // same ID
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {{makeMsg("m1", "c1", "hi", 100)}},
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	convos, _ := a.Store.ListConversations(50)
	if len(convos) != 1 {
		t.Fatalf("got %d conversations, want 1 (dedup same conv in INBOX+ARCHIVE)", len(convos))
	}

	// Messages should only be fetched once
	progress := a.GetBackfillProgress()
	if progress.MessagesFound != 1 {
		t.Errorf("messages fetched %d times, want 1 (dedup)", progress.MessagesFound)
	}
}

func TestDeepBackfillProgressCallback(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {{makeMsg("m1", "c1", "hi", 100)}},
		},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill()

	progress := a.GetBackfillProgress()
	if !progress.Running && progress.Phase != BackfillPhaseDone {
		t.Errorf("expected phase=%q after completion, got %q", BackfillPhaseDone, progress.Phase)
	}
	if progress.FoldersScanned != 3 { // always scans 3 folders
		t.Errorf("FoldersScanned = %d, want 3", progress.FoldersScanned)
	}
	if progress.ConversationsFound != 1 {
		t.Errorf("ConversationsFound = %d, want 1", progress.ConversationsFound)
	}
	if progress.MessagesFound != 1 {
		t.Errorf("MessagesFound = %d, want 1", progress.MessagesFound)
	}
}

func TestDeepBackfillEmptyFolders(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{},
	}

	a := newTestApp(t, mock)
	a.DeepBackfill() // should not panic or error

	progress := a.GetBackfillProgress()
	if progress.ConversationsFound != 0 {
		t.Errorf("ConversationsFound = %d, want 0", progress.ConversationsFound)
	}
	if progress.Phase != BackfillPhaseDone {
		t.Errorf("Phase = %q, want %q", progress.Phase, BackfillPhaseDone)
	}
}

func TestDeepBackfillPublishesChangeNotifications(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {
				{makeMsg("m1", "c1", "hello", 100)},
			},
		},
	}

	a := newTestApp(t, mock)
	var (
		conversationChanges int
		messagesChangedFor  string
	)
	a.OnConversationsChange = func() {
		conversationChanges++
	}
	a.OnMessagesChange = func(conversationID string) {
		messagesChangedFor = conversationID
	}

	a.DeepBackfill()

	if conversationChanges != 1 {
		t.Fatalf("conversation change callback count = %d, want 1", conversationChanges)
	}
	if messagesChangedFor != "" {
		t.Fatalf("messages change callback conversation = %q, want global refresh", messagesChangedFor)
	}
}

func TestDeepBackfillTargetedPhoneBackfill(t *testing.T) {
	mock := &mockGMClient{
		getOrCreateResults: map[string]*gmproto.Conversation{
			"+14157934268": makeConv("c-mary", "Mary MacLeod"),
		},
		messages: map[string][][]*gmproto.Message{
			"c-mary": {
				{makeMsg("m1", "c-mary", "old msg 1", 50), makeMsg("m2", "c-mary", "old msg 2", 60)},
				{makeMsg("m3", "c-mary", "old msg 3", 70)},
			},
		},
	}

	a := newTestApp(t, mock)
	var (
		conversationChanges int
		messagesChangedFor  string
	)
	a.OnConversationsChange = func() {
		conversationChanges++
	}
	a.OnMessagesChange = func(conversationID string) {
		messagesChangedFor = conversationID
	}
	err := a.BackfillConversationByPhone("+14157934268")
	if err != nil {
		t.Fatalf("BackfillConversationByPhone: %v", err)
	}

	convos, _ := a.Store.ListConversations(50)
	if len(convos) != 1 {
		t.Fatalf("got %d conversations, want 1", len(convos))
	}
	if convos[0].Name != "Mary MacLeod" {
		t.Errorf("conversation name = %q, want Mary MacLeod", convos[0].Name)
	}

	msgs, _ := a.Store.GetMessagesByConversation("c-mary", 100)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3 (2 pages)", len(msgs))
	}
	if conversationChanges != 1 {
		t.Fatalf("conversation change callback count = %d, want 1", conversationChanges)
	}
	if messagesChangedFor != "c-mary" {
		t.Fatalf("messages change callback conversation = %q, want c-mary", messagesChangedFor)
	}
}

func TestDeepBackfillNilClient(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	a := &App{
		Store:  store,
		Logger: zerolog.Nop(),
		// No Client or gmClient — both nil
	}

	// Should not panic
	a.DeepBackfill()

	progress := a.GetBackfillProgress()
	if progress.Phase == BackfillPhaseDone {
		t.Errorf("expected early return (not phase=%q) when client is nil", BackfillPhaseDone)
	}
}

func TestBackfillConversationByPhoneNilClient(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	a := &App{
		Store:  store,
		Logger: zerolog.Nop(),
	}

	err = a.BackfillConversationByPhone("+14157934268")
	if err == nil {
		t.Fatal("expected error when client is nil")
	}
}

func TestBackfillStoresConversationsAndMessages(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	a := &App{
		Store:  store,
		Logger: zerolog.Nop(),
	}

	// Without a real client, Backfill should return an error
	err = a.Backfill()
	if err == nil {
		t.Fatal("expected error when client is nil")
	}
}

func TestBackfillReturnsBusyWhenAnotherBackfillIsRunning(t *testing.T) {
	a := &App{}
	if !a.beginBackfill() {
		t.Fatal("expected to acquire backfill guard")
	}
	defer a.endBackfill()

	err := a.Backfill()
	if err == nil {
		t.Fatal("expected backfill to fail while another run is active")
	}
	if err.Error() != "backfill already running" {
		t.Fatalf("backfill error = %q, want %q", err.Error(), "backfill already running")
	}
}

func TestStartDeepBackfillReturnsFalseWhenBackfillIsRunning(t *testing.T) {
	a := &App{}
	if !a.beginBackfill() {
		t.Fatal("expected to acquire backfill guard")
	}
	defer a.endBackfill()

	if a.StartDeepBackfill() {
		t.Fatal("deep backfill should not start while another backfill is active")
	}
}

func TestStartRecentReconcileReturnsFalseWhenBackfillIsRunning(t *testing.T) {
	a := &App{}
	a.backfillRunning.Store(true)
	defer a.backfillRunning.Store(false)

	if a.StartRecentReconcile("listen_recovered") {
		t.Fatal("recent reconcile should not start while another backfill is active")
	}
}

func TestRecentReconcileStoresRecentMessagesAndPublishesChanges(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {
				{makeMsg("m1", "c1", "newer hello", 100), makeMsg("m2", "c1", "newer follow-up", 200)},
			},
		},
	}

	a := newTestApp(t, mock)
	var (
		conversationChanges int
		messagesChangedFor  string
	)
	a.OnConversationsChange = func() {
		conversationChanges++
	}
	a.OnMessagesChange = func(conversationID string) {
		messagesChangedFor = conversationID
	}

	a.reconcileRecentConversations("listen_recovered")

	convos, err := a.Store.ListConversations(10)
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	if len(convos) != 1 {
		t.Fatalf("stored conversations = %d, want 1", len(convos))
	}

	msgs, err := a.Store.GetMessagesByConversation("c1", 10)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("stored messages = %d, want 2", len(msgs))
	}
	if conversationChanges != 1 {
		t.Fatalf("conversation change callback count = %d, want 1", conversationChanges)
	}
	if messagesChangedFor != "" {
		t.Fatalf("messages change callback conversation = %q, want global refresh", messagesChangedFor)
	}
}

func TestRecentReconcilePagesUntilItCrossesLocalBoundary(t *testing.T) {
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {
				{makeMsg("m5", "c1", "latest", 500), makeMsg("m4", "c1", "newer", 400)},
				{makeMsg("m3", "c1", "middle", 300), makeMsg("m2", "c1", "older", 200), makeMsg("m1", "c1", "existing", 100)},
			},
		},
		fetchCalls: map[string]int{},
	}

	a := newTestApp(t, mock)
	if err := a.Store.UpsertConversation(&db.Conversation{
		ConversationID: "c1",
		Name:           "Alice",
		LastMessageTS:  100,
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if err := a.Store.UpsertMessage(&db.Message{
		MessageID:      "m1",
		ConversationID: "c1",
		Body:           "existing",
		TimestampMS:    100,
	}); err != nil {
		t.Fatalf("seed boundary message: %v", err)
	}

	a.reconcileRecentConversations("listen_recovered")

	if mock.fetchCalls["c1"] != 2 {
		t.Fatalf("fetch call count = %d, want 2 pages to cross the local boundary", mock.fetchCalls["c1"])
	}

	msgs, err := a.Store.GetMessagesByConversation("c1", 10)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("stored messages = %d, want 5 after paging recovery", len(msgs))
	}
	if msgs[0].MessageID != "m5" {
		t.Fatalf("most recent message = %q, want m5", msgs[0].MessageID)
	}
}

func TestPendingMediaRefreshRetriesUntilMMSHydrates(t *testing.T) {
	placeholder := &gmproto.Message{
		MessageID:      "m1",
		ConversationID: "c1",
		Timestamp:      500 * 1000,
		Type:           3,
		MessageInfo: []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: "Image from phone"},
			},
		}},
	}
	hydrated := &gmproto.Message{
		MessageID:      "m1",
		ConversationID: "c1",
		Timestamp:      500 * 1000,
		Type:           2,
		MessageInfo: []*gmproto.MessageInfo{{
			Data: &gmproto.MessageInfo_MediaContent{
				MediaContent: &gmproto.MediaContent{
					MediaID:       "media-123",
					MimeType:      "image/jpeg",
					DecryptionKey: []byte{0x01, 0x02},
				},
			},
		}},
	}

	mock := &mockGMClient{
		messages: map[string][][]*gmproto.Message{
			"c1": {{placeholder}},
		},
		fetchCalls: map[string]int{},
	}
	mock.afterFetchMessages = func(conversationID string, pageIdx int) {
		if conversationID == "c1" && pageIdx == 0 && mock.fetchCalls[conversationID] == 1 {
			mock.messages[conversationID] = [][]*gmproto.Message{{hydrated}}
		}
	}

	a := newTestApp(t, mock)
	var changed []string
	a.OnMessagesChange = func(conversationID string) {
		changed = append(changed, conversationID)
	}

	a.refreshPendingMediaMessageWithSchedule("c1", "m1", []time.Duration{0, 0})

	msg, err := a.Store.GetMessageByID("m1")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if msg == nil {
		t.Fatal("expected stored message")
	}
	if msg.MediaID != "media-123" {
		t.Fatalf("media_id = %q, want media-123", msg.MediaID)
	}
	if msg.MimeType != "image/jpeg" {
		t.Fatalf("mime_type = %q, want image/jpeg", msg.MimeType)
	}
	if len(changed) != 2 {
		t.Fatalf("messages change callbacks = %d, want 2 attempts", len(changed))
	}
	if mock.fetchCalls["c1"] != 2 {
		t.Fatalf("fetch call count = %d, want 2", mock.fetchCalls["c1"])
	}
}

func TestDeepBackfillStopsWhenClientChanges(t *testing.T) {
	replacement := &mockGMClient{}
	mock := &mockGMClient{
		conversations: map[gmproto.ListConversationsRequest_Folder][][]*gmproto.Conversation{
			gmproto.ListConversationsRequest_INBOX: {
				{makeConv("c1", "Alice")},
			},
		},
		messages: map[string][][]*gmproto.Message{
			"c1": {
				{makeMsg("m1", "c1", "page1-a", 100), makeMsg("m2", "c1", "page1-b", 200)},
				{makeMsg("m3", "c1", "page2-a", 300)},
			},
		},
		fetchCalls: map[string]int{},
	}

	a := newTestApp(t, mock)
	mock.afterFetchMessages = func(conversationID string, pageIdx int) {
		if conversationID == "c1" && pageIdx == 0 {
			a.gmClient = replacement
		}
	}

	a.DeepBackfill()

	msgs, err := a.Store.GetMessagesByConversation("c1", 100)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d stored messages, want 2 from the first page only", len(msgs))
	}
	if mock.fetchCalls["c1"] != 1 {
		t.Fatalf("fetch call count = %d, want 1 before abort", mock.fetchCalls["c1"])
	}

	progress := a.GetBackfillProgress()
	if progress.MessagesFound != 2 {
		t.Fatalf("messages found = %d, want 2 before abort", progress.MessagesFound)
	}
	if progress.Phase != BackfillPhaseDone {
		t.Fatalf("phase = %q, want %q", progress.Phase, BackfillPhaseDone)
	}
}

func TestBackfillPopulatesDB(t *testing.T) {
	// Verify that after backfill stores conversations, they're queryable
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Manually insert a conversation as if backfill ran
	store.UpsertConversation(&db.Conversation{
		ConversationID: "c1",
		Name:           "Alice",
		LastMessageTS:  1000,
	})
	store.UpsertMessage(&db.Message{
		MessageID:      "m1",
		ConversationID: "c1",
		Body:           "Hello from backfill",
		TimestampMS:    1000,
		SenderName:     "Alice",
	})

	convos, err := store.ListConversations(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(convos) != 1 {
		t.Fatalf("got %d conversations, want 1", len(convos))
	}
	if convos[0].Name != "Alice" {
		t.Fatalf("got name %q, want Alice", convos[0].Name)
	}

	msgs, err := store.GetMessagesByConversation("c1", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "Hello from backfill" {
		t.Fatalf("got body %q", msgs[0].Body)
	}
}

func TestOrphanContactDiscoveryEnabled(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want bool
	}{
		{"unset", "", false},
		{"empty", "", false},
		{"explicit zero", "0", false},
		{"explicit false", "false", false},
		{"no", "no", false},
		{"off", "off", false},
		{"one", "1", true},
		{"true", "true", true},
		{"True mixed case", "True", true},
		{"YES upper", "YES", true},
		{"on padded", "  on  ", true},
		{"unrecognized", "maybe", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENMESSAGES_BACKFILL_DISCOVER_ORPHANS", tc.env)
			if got := orphanContactDiscoveryEnabled(); got != tc.want {
				t.Fatalf("orphanContactDiscoveryEnabled() = %v with env=%q, want %v", got, tc.env, tc.want)
			}
		})
	}
}
