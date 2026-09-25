package contactsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

type fakeCaller struct {
	t      *testing.T
	pages  []*mcp.CallToolResult
	tokens []string
	calls  int
	failAt int
}

func (f *fakeCaller) CallTool(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	i := f.calls
	f.calls++
	if f.failAt > 0 && f.calls == f.failAt {
		return nil, errors.New("secret upstream body")
	}
	if i >= len(f.pages) {
		f.t.Fatal("unexpected extra page")
	}
	a := r.GetArguments()
	if a["pageSize"] != 1000 || a["sortOrder"] != "LAST_MODIFIED_ASCENDING" || r.Params.Name != "contacts_list" {
		f.t.Fatalf("bad request: %+v", r.Params)
	}
	if i < len(f.tokens) {
		token, _ := a["pageToken"].(string)
		if token != f.tokens[i] {
			f.t.Fatalf("token %q", token)
		}
	}
	return f.pages[i], nil
}
func pageResult(n, total int, next string, offset int) *mcp.CallToolResult {
	p := page{TotalItems: &total, NextPageToken: next, Connections: []Person{}}
	for i := 0; i < n; i++ {
		p.Connections = append(p.Connections, Person{ResourceName: fmt.Sprintf("people/c%d", offset+i), Names: []Name{{DisplayName: fmt.Sprintf("Person %d", offset+i)}}})
	}
	return mcp.NewToolResultStructuredOnly(p)
}
func TestFetchPagesFullDirectory(t *testing.T) {
	f := &fakeCaller{t: t, pages: []*mcp.CallToolResult{pageResult(1000, 1002, "next", 0), pageResult(2, 1002, "", 1000)}, tokens: []string{"", "next"}}
	p, err := fetchPages(context.Background(), f)
	if err != nil || len(p) != 1002 || p[1001].DisplayName() != "Person 1001" {
		t.Fatalf("len=%d err=%v", len(p), err)
	}
}
func TestFetchPagesRejectsIncomplete(t *testing.T) {
	cases := map[string][]*mcp.CallToolResult{
		"missing tail":     {pageResult(50, 672, "", 0)},
		"repeated token":   {pageResult(1, 3, "next", 0), pageResult(1, 3, "next", 1)},
		"duplicate person": {pageResult(1, 2, "next", 0), pageResult(1, 2, "", 0)},
		"changing total":   {pageResult(1, 2, "next", 0), pageResult(1, 3, "", 1)},
		"invalid body":     {mcp.NewToolResultText(`{}`)},
		"error body":       {mcp.NewToolResultError("private detail must not escape")},
	}
	for name, pages := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := fetchPages(context.Background(), &fakeCaller{t: t, pages: pages})
			if err == nil || p != nil {
				t.Fatal("accepted partial list")
			}
			if strings.Contains(err.Error(), "private detail") {
				t.Fatal("leaked error")
			}
		})
	}
	f := &fakeCaller{t: t, pages: []*mcp.CallToolResult{pageResult(1, 2, "next", 0)}, failAt: 2}
	p, err := fetchPages(context.Background(), f)
	if err == nil || p != nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("failed fetch did not fail closed")
	}
}
func TestFetchEmptyAndTextResult(t *testing.T) {
	for _, r := range []*mcp.CallToolResult{pageResult(0, 0, "", 0), mcp.NewToolResultText(`{"connections":[],"totalPeople":0}`)} {
		p, err := fetchPages(context.Background(), &fakeCaller{t: t, pages: []*mcp.CallToolResult{r}})
		if err != nil || len(p) != 0 {
			t.Fatalf("%v %v", p, err)
		}
	}
}
func TestFetchMCPHTTPAndPrivateToken(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("test-bearer\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-bearer" {
			t.Error("missing token")
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "test", "version": "1"}}})
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/call":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": pageResult(2, 2, "", 0)})
		default:
			t.Errorf("unexpected method %s", req.Method)
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	p, err := Fetch(context.Background(), server.URL, tokenPath)
	if err != nil || len(p) != 2 {
		t.Fatalf("%d %v", len(p), err)
	}
	os.Chmod(tokenPath, 0644)
	if _, err = Fetch(context.Background(), server.URL, tokenPath); err == nil {
		t.Fatal("accepted public token")
	}
}
func TestFetchRejectsUnsafeURLs(t *testing.T) {
	for _, u := range []string{"http://example.com/mcp", "file:///tmp/a", "https://user:secret@example.com/mcp", "https://example.com/mcp?token=secret"} {
		if _, err := Fetch(context.Background(), u, ""); err == nil {
			t.Fatal(u)
		}
	}
}

func TestFetchDoesNotFollowRedirects(t *testing.T) {
	var followed bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed = true; w.WriteHeader(500) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	if _, err := Fetch(context.Background(), source.URL, ""); err == nil {
		t.Fatal("redirect accepted")
	}
	if followed {
		t.Fatal("followed connector redirect")
	}
}
