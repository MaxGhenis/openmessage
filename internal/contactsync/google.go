// Package contactsync reads a complete address book from a Google Contacts MCP
// server. Google Messages' LIST_CONTACTS response is only a small suggestion list.
package contactsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

type Value struct {
	Value string `json:"value"`
	Type  string `json:"type,omitempty"`
}
type Name struct {
	DisplayName string `json:"displayName"`
}
type Organization struct {
	Name  string `json:"name"`
	Title string `json:"title,omitempty"`
}
type Person struct {
	ResourceName   string         `json:"resourceName"`
	Names          []Name         `json:"names,omitempty"`
	PhoneNumbers   []Value        `json:"phoneNumbers,omitempty"`
	EmailAddresses []Value        `json:"emailAddresses,omitempty"`
	Organizations  []Organization `json:"organizations,omitempty"`
}

func (p Person) DisplayName() string {
	for _, n := range p.Names {
		if s := strings.TrimSpace(n.DisplayName); s != "" {
			return s
		}
	}
	for _, o := range p.Organizations {
		if s := strings.TrimSpace(o.Name); s != "" {
			return s
		}
	}
	return ""
}

type page struct {
	Connections   []Person `json:"connections"`
	NextPageToken string   `json:"nextPageToken"`
	TotalItems    *int     `json:"totalItems"`
	TotalPeople   *int     `json:"totalPeople"`
}

type toolCaller interface {
	CallTool(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
}

// Fetch follows every page and fails closed on incomplete or inconsistent lists.
// Tokens belong to the connector; OpenMessage never reads Google OAuth cookies.
func Fetch(ctx context.Context, endpoint, tokenFile string) ([]Person, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid Google Contacts MCP URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, errors.New("Google Contacts MCP requires HTTPS or a loopback IP URL")
	}
	headers := map[string]string{}
	if tokenFile != "" {
		f, err := os.Open(tokenFile)
		if err != nil {
			return nil, errors.New("cannot read Google Contacts MCP token file")
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
			return nil, errors.New("Google Contacts MCP token file must be private (0600)")
		}
		b, err := io.ReadAll(io.LimitReader(f, 8193))
		if err != nil || len(b) > 8192 {
			return nil, errors.New("invalid Google Contacts MCP token file")
		}
		token := strings.TrimSpace(string(b))
		if token == "" || strings.ContainsAny(token, "\r\n") {
			return nil, errors.New("invalid Google Contacts MCP token file")
		}
		headers["Authorization"] = "Bearer " + token
	}
	hc := &http.Client{Timeout: 30 * time.Second, Transport: limitedTransport{}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects disabled") }}
	cli, err := mcpclient.NewStreamableHttpClient(endpoint, transport.WithHTTPHeaders(headers), transport.WithHTTPBasicClient(hc))
	if err != nil {
		return nil, errors.New("cannot configure Google Contacts MCP client")
	}
	if err = cli.Start(ctx); err != nil {
		return nil, errors.New("cannot start Google Contacts MCP client")
	}
	defer cli.Close()
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "openmessage-contacts", Version: "1"}
	if _, err = cli.Initialize(ctx, req); err != nil {
		return nil, errors.New("Google Contacts MCP initialization failed; check connector authorization")
	}
	return fetchPages(ctx, cli)
}

type limitedTransport struct{}

func (limitedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		resp.Body = &limitedBody{Reader: io.LimitReader(resp.Body, 8<<20), Closer: resp.Body}
	}
	return resp, err
}

type limitedBody struct {
	io.Reader
	io.Closer
}

func fetchPages(ctx context.Context, cli toolCaller) ([]Person, error) {
	people := []Person{}
	seenIDs := map[string]bool{}
	seenTokens := map[string]bool{}
	token := ""
	total := -1
	for n := 0; n < 100; n++ {
		req := mcp.CallToolRequest{}
		req.Params.Name = "contacts_list"
		args := map[string]any{"pageSize": 1000, "sortOrder": "LAST_MODIFIED_ASCENDING"}
		if token != "" {
			args["pageToken"] = token
		}
		req.Params.Arguments = args
		result, err := cli.CallTool(ctx, req)
		if err != nil || result == nil || result.IsError {
			return nil, errors.New("Google Contacts MCP listing failed; previous directory retained")
		}
		var raw []byte
		if result.StructuredContent != nil {
			raw, err = json.Marshal(result.StructuredContent)
		} else {
			for _, c := range result.Content {
				if t, ok := c.(mcp.TextContent); ok {
					raw = []byte(t.Text)
					break
				}
			}
		}
		var p page
		if err != nil || len(raw) == 0 || json.Unmarshal(raw, &p) != nil {
			return nil, errors.New("invalid Google Contacts MCP listing")
		}
		t := p.TotalItems
		if t == nil {
			t = p.TotalPeople
		}
		if t != nil {
			if *t < 0 || (total >= 0 && total != *t) {
				return nil, errors.New("Google Contacts listing changed during pagination; retry required")
			}
			total = *t
		}
		for _, person := range p.Connections {
			if !strings.HasPrefix(person.ResourceName, "people/") || seenIDs[person.ResourceName] {
				return nil, errors.New("invalid or repeated Google Contacts resource")
			}
			seenIDs[person.ResourceName] = true
			people = append(people, person)
		}
		if p.NextPageToken == "" {
			if total < 0 || len(people) != total {
				return nil, fmt.Errorf("incomplete Google Contacts listing: received %d, expected %d", len(people), total)
			}
			return people, nil
		}
		if len(p.Connections) == 0 || seenTokens[p.NextPageToken] {
			return nil, errors.New("Google Contacts pagination did not advance")
		}
		seenTokens[p.NextPageToken] = true
		token = p.NextPageToken
	}
	return nil, errors.New("Google Contacts pagination limit exceeded")
}
