package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
	"github.com/maxghenis/openmessage/internal/v2read"
)

func TestConversationMutationsAreVisibleToPrimaryReads(t *testing.T) {
	tests := []struct {
		name, path, body string
		assert           func(*testing.T, *db.Conversation)
	}{
		{"mute", "notification-mode", `{"notification_mode":"muted"}`, func(t *testing.T, c *db.Conversation) {
			if c.NotificationMode != "muted" {
				t.Fatalf("primary notification mode = %q", c.NotificationMode)
			}
		}},
		{"mentions only", "notification-mode", `{"notification_mode":"mentions"}`, func(t *testing.T, c *db.Conversation) {
			if c.NotificationMode != "mentions" {
				t.Fatalf("primary notification mode = %q", c.NotificationMode)
			}
		}},
		{"favorite", "favorite", `{"favorite":true}`, func(t *testing.T, c *db.Conversation) {
			if !c.IsFavorite {
				t.Fatal("primary conversation is not favorite")
			}
		}},
		{"archive", "tab", `{"tab":"archive"}`, func(t *testing.T, c *db.Conversation) {
			if c.Tab != db.TabArchive {
				t.Fatalf("primary tab = %q", c.Tab)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			legacy, v2, v2ID := seedMutationStores(t)
			handler := APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, APIOptions{
				Reads: v2read.New(v2), V2Primary: true,
				V2: &V2Options{V2Store: v2},
			})
			req := httptest.NewRequest(http.MethodPost, "/api/conversations/"+v2ID+"/"+tc.path, bytes.NewBufferString(tc.body))
			req.Host = "127.0.0.1:8080"
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var response db.Conversation
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.ConversationID != v2ID {
				t.Fatalf("response conversation ID = %q, want v2 ID %q", response.ConversationID, v2ID)
			}
			primary, err := v2read.New(v2).GetConversation(v2ID)
			if err != nil {
				t.Fatal(err)
			}
			tc.assert(t, primary)
			legacyConversation, err := legacy.GetConversation("legacy-a")
			if err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "mute":
				if legacyConversation.NotificationMode != "muted" {
					t.Fatalf("legacy = %+v", legacyConversation)
				}
			case "mentions only":
				if legacyConversation.NotificationMode != "mentions" {
					t.Fatalf("legacy = %+v", legacyConversation)
				}
			case "favorite":
				if !legacyConversation.IsFavorite {
					t.Fatalf("legacy = %+v", legacyConversation)
				}
			case "archive":
				if legacyConversation.Tab != db.TabArchive {
					t.Fatalf("legacy = %+v", legacyConversation)
				}
			}
		})
	}
}

func TestBulkMoveDualWritesArchiveAndUnarchive(t *testing.T) {
	legacy, v2, v2ID := seedMutationStores(t)
	handler := APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, APIOptions{V2Primary: true, V2: &V2Options{V2Store: v2}})
	for _, tab := range []string{"archive", ""} {
		body := `{"ids":["` + v2ID + `"],"tab":"` + tab + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/conversations/move", bytes.NewBufferString(body))
		req.Host = "127.0.0.1:8080"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		conversation, err := v2.GetConversation(v2ID)
		if err != nil {
			t.Fatal(err)
		}
		if tab == "archive" && conversation.ArchivedAtMS == nil {
			t.Fatalf("v2 conversation was not archived: %+v", conversation)
		}
		if tab == "" && conversation.ArchivedAtMS != nil {
			t.Fatalf("v2 conversation remains archived: %+v", conversation)
		}
	}
}

func TestPrimaryV2FailureSurfacesAfterLegacyWrite(t *testing.T) {
	legacy, v2, v2ID := seedMutationStores(t)
	missingID := v2keys.DeriveID("conversation", "google-primary", "legacy-missing-v2")
	if err := legacy.UpsertConversation(&db.Conversation{ConversationID: "legacy-missing-v2", Name: "Missing", SourcePlatform: "sms"}); err != nil {
		t.Fatal(err)
	}
	handler := APIHandlerWithOptions(legacy, nil, zerolog.Nop(), nil, APIOptions{V2Primary: true, V2: &V2Options{V2Store: v2}})
	req := httptest.NewRequest(http.MethodPost, "/api/conversations/legacy-missing-v2/favorite", bytes.NewBufferString(`{"favorite":true}`))
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	legacyConversation, err := legacy.GetConversation("legacy-missing-v2")
	if err != nil || !legacyConversation.IsFavorite {
		t.Fatalf("legacy-first write missing: conversation=%+v err=%v", legacyConversation, err)
	}
	if _, err := v2.GetConversation(missingID); err == nil {
		t.Fatal("unexpected v2 conversation")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/conversations/"+v2ID+"/favorite", bytes.NewBufferString(`{"favorite":true}`))
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	req = req.WithContext(ctx)
	req.Host = "127.0.0.1:8080"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("real v2 error status = %d, body = %s", rec.Code, rec.Body.String())
	}
	legacyConversation, err = legacy.GetConversation("legacy-a")
	if err != nil || !legacyConversation.IsFavorite {
		t.Fatalf("legacy-first write before real v2 error missing: conversation=%+v err=%v", legacyConversation, err)
	}
}

func TestResolveConversationMutationIDsUsesMigrationAccounts(t *testing.T) {
	tests := []struct {
		platform, accountID string
	}{
		{"whatsapp", "whatsapp-primary"},
		{"signal", "signal-primary"},
		{"gchat", "gchat-archive"},
		{"imessage", "imessage-archive"},
	}
	for _, tc := range tests {
		t.Run(tc.platform, func(t *testing.T) {
			legacy, v2, v2ID := seedMutationPlatformStores(t, tc.platform, tc.accountID)
			legacyID, derivedID, err := resolveConversationMutationIDs(legacy, &V2Options{V2Store: v2}, "legacy-a")
			if err != nil {
				t.Fatal(err)
			}
			if legacyID != "legacy-a" || derivedID != v2ID {
				t.Fatalf("legacy->v2 = (%q, %q), want (%q, %q)", legacyID, derivedID, "legacy-a", v2ID)
			}
			row, err := v2.GetConversation(derivedID)
			if err != nil {
				t.Fatal(err)
			}
			if row.AccountID != tc.accountID {
				t.Fatalf("derived account = %q, want %q", row.AccountID, tc.accountID)
			}
			resolvedLegacyID, resolvedV2ID, err := resolveConversationMutationIDs(legacy, &V2Options{V2Store: v2}, v2ID)
			if err != nil {
				t.Fatal(err)
			}
			if resolvedLegacyID != "legacy-a" || resolvedV2ID != v2ID {
				t.Fatalf("v2->legacy = (%q, %q), want (%q, %q)", resolvedLegacyID, resolvedV2ID, "legacy-a", v2ID)
			}
		})
	}
}

func seedMutationStores(t *testing.T) (*db.Store, *sqlite.Store, string) {
	return seedMutationPlatformStores(t, "sms", "google-primary")
}

func seedMutationPlatformStores(t *testing.T, platform, accountID string) (*db.Store, *sqlite.Store, string) {
	t.Helper()
	legacy, err := db.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := sqlite.Open(filepath.Join(t.TempDir(), "v2.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close(); _ = v2.Close() })
	if err := legacy.UpsertConversation(&db.Conversation{ConversationID: "legacy-a", Name: "Alice", SourcePlatform: platform, NotificationMode: db.NotificationModeAll}); err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000_000)
	if err := v2.UpsertAccount(sqlite.Account{AccountID: accountID, BridgeKey: platform, Mode: sqlite.AccountModeLive, Enabled: true, ConfigJSON: `{}`, CreatedAtMS: now, UpdatedAtMS: now}); err != nil {
		t.Fatal(err)
	}
	v2ID := v2keys.DeriveID("conversation", accountID, "legacy-a")
	if err := v2.UpsertConversation(sqlite.Conversation{ConversationID: v2ID, AccountID: accountID, RemoteConversationID: v2keys.NormalizeRemoteConversationID(platform, "legacy-a"), Kind: sqlite.ConversationKindDirect, Title: "Alice", NotificationMode: sqlite.NotificationModeAll, MetadataJSON: `{}`, CreatedAtMS: now, UpdatedAtMS: now}); err != nil {
		t.Fatal(err)
	}
	// Ensure the seeded primary projection is stale before every discriminator mutation.
	before, err := v2read.New(v2).GetConversation(v2ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(before)
	if before.IsFavorite || before.NotificationMode != "all" || before.Tab != "" {
		t.Fatalf("unexpected primary precondition: %s", encoded)
	}
	return legacy, v2, v2ID
}
