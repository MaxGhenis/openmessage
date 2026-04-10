package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestLinkPreviewServiceFetchParsesMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
<html>
  <head>
    <title>Ignored Title</title>
    <meta property="og:title" content="Preview Title">
    <meta property="og:description" content="Preview Description">
    <meta property="og:image" content="/card.png">
    <meta property="og:site_name" content="Preview Site">
  </head>
  <body>Hello</body>
</html>`))
	}))
	defer srv.Close()

	service := NewLinkPreviewService(zerolog.Nop())
	service.allowPrivateHosts = true

	preview, err := service.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Title != "Preview Title" {
		t.Fatalf("got title %q", preview.Title)
	}
	if preview.Description != "Preview Description" {
		t.Fatalf("got description %q", preview.Description)
	}
	if preview.SiteName != "Preview Site" {
		t.Fatalf("got site name %q", preview.SiteName)
	}
	if preview.ImageURL != srv.URL+"/card.png" {
		t.Fatalf("got image URL %q", preview.ImageURL)
	}
}

func TestLinkPreviewServiceBlocksPrivateHostsByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	service := NewLinkPreviewService(zerolog.Nop())
	_, err := service.Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrBlockedLinkPreviewURL) {
		t.Fatalf("got err %v, want ErrBlockedLinkPreviewURL", err)
	}
}

func TestLinkPreviewServiceEvictsLeastRecentlyUsedEntries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta property="og:title" content="%s"></head></html>`, r.URL.Path)
	}))
	defer srv.Close()

	service := NewLinkPreviewService(zerolog.Nop())
	service.allowPrivateHosts = true
	service.maxEntries = 2
	service.ttl = time.Hour

	urlA := srv.URL + "/a"
	urlB := srv.URL + "/b"
	urlC := srv.URL + "/c"

	if _, err := service.Fetch(context.Background(), urlA); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Fetch(context.Background(), urlB); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Fetch(context.Background(), urlA); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Fetch(context.Background(), urlC); err != nil {
		t.Fatal(err)
	}

	if len(service.cache) != 2 {
		t.Fatalf("cache size = %d, want 2", len(service.cache))
	}
	if _, ok := service.cache[urlA]; !ok {
		t.Fatalf("expected %s to remain cached", urlA)
	}
	if _, ok := service.cache[urlC]; !ok {
		t.Fatalf("expected %s to remain cached", urlC)
	}
	if _, ok := service.cache[urlB]; ok {
		t.Fatalf("expected %s to be evicted", urlB)
	}
}
