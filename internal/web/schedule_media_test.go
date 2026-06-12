package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

func newScheduleMediaServer(t *testing.T) (*httptest.Server, *db.Store) {
	t.Helper()
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.UpsertConversation(&db.Conversation{ConversationID: "c1", SourcePlatform: "sms"}); err != nil {
		t.Fatal(err)
	}
	h := APIHandlerWithOptions(store, nil, zerolog.Nop(), nil, APIOptions{})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, store
}

func postScheduleMedia(t *testing.T, url string, fields map[string]string, filename string, fileData []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if filename != "" {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
		h.Set("Content-Type", "image/png")
		fw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write(fileData)
	}
	mw.Close()
	resp, err := http.Post(url+"/api/schedule-media", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestScheduleMedia_CreatesPendingWithBlob(t *testing.T) {
	srv, store := newScheduleMediaServer(t)
	sendAt := time.Now().Add(2 * time.Hour).UnixMilli()
	blob := []byte{0x89, 0x50, 0x4e, 0x47, 5, 5, 5}

	resp := postScheduleMedia(t, srv.URL, map[string]string{
		"conversation_id": "c1",
		"caption":         "hello pic",
		"send_at":         strconv.FormatInt(sendAt, 10),
	}, "pic.png", blob)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var sm db.ScheduledMessage
	if err := json.NewDecoder(resp.Body).Decode(&sm); err != nil {
		t.Fatal(err)
	}
	if sm.MediaFilename != "pic.png" || sm.Body != "hello pic" {
		t.Errorf("response metadata wrong: %+v", sm)
	}

	// The blob persisted and is loadable for the scheduler.
	data, err := store.GetScheduledMediaData(sm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, blob) {
		t.Errorf("stored blob mismatch: %v", data)
	}
	list, _ := store.ListScheduledMessages("c1")
	if len(list) != 1 || list[0].MediaMime != "image/png" {
		t.Errorf("list wrong: %+v", list)
	}
}

func TestScheduleMedia_RejectsMissingFile(t *testing.T) {
	srv, _ := newScheduleMediaServer(t)
	sendAt := time.Now().Add(2 * time.Hour).UnixMilli()
	resp := postScheduleMedia(t, srv.URL, map[string]string{
		"conversation_id": "c1",
		"send_at":         strconv.FormatInt(sendAt, 10),
	}, "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestScheduleMedia_RejectsPastTime(t *testing.T) {
	srv, _ := newScheduleMediaServer(t)
	past := time.Now().Add(-time.Hour).UnixMilli()
	resp := postScheduleMedia(t, srv.URL, map[string]string{
		"conversation_id": "c1",
		"send_at":         strconv.FormatInt(past, 10),
	}, "pic.png", []byte{1, 2, 3})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestScheduleMedia_RejectsUnknownConversation(t *testing.T) {
	srv, _ := newScheduleMediaServer(t)
	sendAt := time.Now().Add(2 * time.Hour).UnixMilli()
	resp := postScheduleMedia(t, srv.URL, map[string]string{
		"conversation_id": "nope",
		"send_at":         strconv.FormatInt(sendAt, 10),
	}, "pic.png", []byte{1, 2, 3})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
