package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2keys"
)

func TestE2EServerDoesNotRegisterTestOwnedAPIRoutes(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		source, readErr := os.ReadFile(name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), name, source, 0)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc") {
				return true
			}
			pattern, ok := call.Args[0].(*ast.BasicLit)
			if ok && pattern.Kind == token.STRING && isAPIRoutePattern(pattern.Value) {
				t.Errorf("%s: e2e-server directly registers test-owned API route %s", name, pattern.Value)
			}
			return true
		})
	}
}

func TestIsAPIRoutePatternHandlesMethodPrefix(t *testing.T) {
	for _, pattern := range []string{`"/api/send"`, `"POST /api/send"`} {
		if !isAPIRoutePattern(pattern) {
			t.Errorf("isAPIRoutePattern(%s) = false, want true", pattern)
		}
	}
}

func TestE2EServerReportsV2Primary(t *testing.T) {
	server, cleanup, err := newE2EServer(zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.Host = "127.0.0.1:7010"
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var status struct {
		V2Primary bool `json:"v2_primary"`
	}
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.V2Primary {
		t.Fatal("v2_primary = false, want true")
	}
}

func TestJordanFixtureHasOneSMSAndOneWhatsAppRoute(t *testing.T) {
	server, cleanup, err := newE2EServer(zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	request := httptest.NewRequest(http.MethodGet, "/api/conversations?limit=200", nil)
	request.Host = "127.0.0.1:7010"
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("conversations status = %d, body = %s", response.Code, response.Body.String())
	}
	var conversations []struct {
		Name           string `json:"Name"`
		Participants   string `json:"Participants"`
		SourcePlatform string `json:"source_platform"`
	}
	if err := json.NewDecoder(response.Body).Decode(&conversations); err != nil {
		t.Fatal(err)
	}
	routes := map[string]int{}
	for _, conversation := range conversations {
		if conversation.Name == "Jordan Rivera" && strings.Contains(conversation.Participants, "+14699991654") {
			routes[conversation.SourcePlatform]++
		}
	}
	if routes["sms"] != 1 || routes["whatsapp"] != 1 {
		t.Fatalf("aligned Jordan routes = %#v, want one SMS and one WhatsApp", routes)
	}
}

func isAPIRoutePattern(literal string) bool {
	pattern, err := strconv.Unquote(literal)
	if err != nil {
		return false
	}
	fields := strings.Fields(pattern)
	return len(fields) > 0 && strings.HasPrefix(fields[len(fields)-1], "/api")
}

func TestUncertainSubmitIsDurableAndIdempotent(t *testing.T) {
	server, cleanup, err := newE2EServer(zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	control := httptest.NewRequest(http.MethodPost, "/_e2e/bridges/google-primary/next-result", strings.NewReader(`{"next_result":"uncertain"}`))
	control.Header.Set("Content-Type", "application/json")
	controlResponse := httptest.NewRecorder()
	server.ServeHTTP(controlResponse, control)
	if controlResponse.Code != http.StatusOK {
		t.Fatalf("control status = %d, body = %s", controlResponse.Code, controlResponse.Body.String())
	}

	conversationID := v2keys.DeriveID("conversation", "google-primary", "conv1")
	body := []byte(fmt.Sprintf(`{"conversation_id":%q,"body":"durable uncertain","idempotency_key":"e2e-uncertain-dedup"}`, conversationID))
	first := submitRequest(t, server, body)
	if first.OutboxID == "" {
		t.Fatal("first submit returned an empty outbox_id")
	}

	repository, err := sqlite.NewOutboxRepository(server.v2Store, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		row, findErr := repository.FindByID(context.Background(), first.OutboxID)
		if findErr == nil && row.State == sqlite.OutboxUncertain {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox did not become uncertain: row=%+v err=%v", row, findErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	second := submitRequest(t, server, body)
	if second.OutboxID != first.OutboxID || !second.Deduplicated {
		t.Fatalf("repeat submit = %+v, want outbox_id %q and deduplicated", second, first.OutboxID)
	}
	if got := len(server.adapters["google-primary"].TextRequests()); got != 1 {
		t.Fatalf("dispatch count = %d, want 1", got)
	}
}

type submissionResponse struct {
	OutboxID     string `json:"outbox_id"`
	Deduplicated bool   `json:"deduplicated"`
}

func submitRequest(t *testing.T, server http.Handler, body []byte) submissionResponse {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/outbox/messages", bytes.NewReader(body))
	request.Host = "127.0.0.1:7010"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("submit status = %d, body = %s", response.Code, response.Body.String())
	}
	var decoded submissionResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}
