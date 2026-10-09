package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"

	"github.com/maxghenis/openmessage/internal/client"
)

// stubPairing swaps the pairing indirections and gives each test a fresh
// registry, so handles never leak between tests.
func stubPairing(
	t *testing.T,
	start func(ctx context.Context, cli *client.Client, rawCookies string) (string, *libgm.PairingSession, error),
	finish func(ctx context.Context, cli *client.Client, session *libgm.PairingSession, sessionPath string) error,
) {
	t.Helper()
	origStart, origFinish, origNew, origRegistry := startGaiaPairing, finishGaiaPairing, newPairingClient, pairings
	startGaiaPairing = start
	finishGaiaPairing = finish
	newPairingClient = func(zerolog.Logger) *client.Client { return client.NewForPairing(zerolog.Nop()) }
	pairings = newPairingRegistry()
	t.Cleanup(func() {
		startGaiaPairing, finishGaiaPairing, newPairingClient, pairings = origStart, origFinish, origNew, origRegistry
	})
}

func callTool(t *testing.T, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return result
}

func TestStartGooglePairingReturnsEmojiAndHandle(t *testing.T) {
	a := testApp(t)
	stubPairing(t,
		func(context.Context, *client.Client, string) (string, *libgm.PairingSession, error) {
			return "🛋", &libgm.PairingSession{}, nil
		},
		func(context.Context, *client.Client, *libgm.PairingSession, string) error { return nil },
	)

	result := callTool(t, startGooglePairingHandler(a), map[string]any{"cookies": "SID=abc"})
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}

	payload := structuredMap(t, result)
	if payload["emoji"] != "🛋" {
		t.Errorf("expected emoji in payload, got %v", payload["emoji"])
	}
	if h, _ := payload["pairing_handle"].(string); h == "" {
		t.Error("expected a pairing_handle")
	}
	// The emoji must be in the text too — that is what the user is shown while
	// the tap is still pending.
	if text := result.Content[0].(mcp.TextContent).Text; !strings.Contains(text, "🛋") {
		t.Errorf("expected emoji in result text, got: %s", text)
	}
}

func TestStartGooglePairingRejectsEmptyCookies(t *testing.T) {
	a := testApp(t)
	called := false
	stubPairing(t,
		func(context.Context, *client.Client, string) (string, *libgm.PairingSession, error) {
			called = true
			return "", nil, nil
		},
		func(context.Context, *client.Client, *libgm.PairingSession, string) error { return nil },
	)

	result := callTool(t, startGooglePairingHandler(a), map[string]any{"cookies": "   "})
	if !result.IsError {
		t.Fatal("expected an error result for blank cookies")
	}
	if called {
		t.Error("pairing should not be attempted with blank cookies")
	}
}

func TestStartGooglePairingReportsFailure(t *testing.T) {
	a := testApp(t)
	stubPairing(t,
		func(context.Context, *client.Client, string) (string, *libgm.PairingSession, error) {
			return "", nil, errors.New("parse Google cookies: no cookies found")
		},
		func(context.Context, *client.Client, *libgm.PairingSession, string) error { return nil },
	)

	result := callTool(t, startGooglePairingHandler(a), map[string]any{"cookies": "garbage"})
	if !result.IsError {
		t.Fatal("expected an error result when starting fails")
	}
	if text := result.Content[0].(mcp.TextContent).Text; !strings.Contains(text, "no cookies found") {
		t.Errorf("expected underlying error in text, got: %s", text)
	}
}

func TestCompleteGooglePairingSuccess(t *testing.T) {
	a := testApp(t)
	a.DataDir = t.TempDir()
	a.SessionPath = a.DataDir + "/session.json"

	var gotSessionPath string
	stubPairing(t,
		func(context.Context, *client.Client, string) (string, *libgm.PairingSession, error) {
			return "🛋", &libgm.PairingSession{}, nil
		},
		func(_ context.Context, _ *client.Client, _ *libgm.PairingSession, sessionPath string) error {
			gotSessionPath = sessionPath
			return nil
		},
	)

	start := callTool(t, startGooglePairingHandler(a), map[string]any{"cookies": "SID=abc"})
	handle := structuredMap(t, start)["pairing_handle"].(string)

	result := callTool(t, completeGooglePairingHandler(a), map[string]any{"pairing_handle": handle})
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	if gotSessionPath != a.SessionPath {
		t.Errorf("expected session path %q, got %q", a.SessionPath, gotSessionPath)
	}
	if payload := structuredMap(t, result); payload["ok"] != true {
		t.Errorf("expected ok=true, got %v", payload["ok"])
	}
}

func TestCompleteGooglePairingRejectsUnknownHandle(t *testing.T) {
	a := testApp(t)
	stubPairing(t,
		func(context.Context, *client.Client, string) (string, *libgm.PairingSession, error) {
			return "🛋", &libgm.PairingSession{}, nil
		},
		func(context.Context, *client.Client, *libgm.PairingSession, string) error { return nil },
	)

	result := callTool(t, completeGooglePairingHandler(a), map[string]any{"pairing_handle": "nope"})
	if !result.IsError {
		t.Fatal("expected an error result for an unknown handle")
	}
	if text := result.Content[0].(mcp.TextContent).Text; !strings.Contains(text, "start_google_pairing") {
		t.Errorf("expected recovery instructions, got: %s", text)
	}
}

// A handle is single-use: reusing one must not silently re-run a spent session.
func TestCompleteGooglePairingHandleIsSingleUse(t *testing.T) {
	a := testApp(t)
	a.SessionPath = t.TempDir() + "/session.json"
	stubPairing(t,
		func(context.Context, *client.Client, string) (string, *libgm.PairingSession, error) {
			return "🛋", &libgm.PairingSession{}, nil
		},
		func(context.Context, *client.Client, *libgm.PairingSession, string) error { return nil },
	)

	start := callTool(t, startGooglePairingHandler(a), map[string]any{"cookies": "SID=abc"})
	handle := structuredMap(t, start)["pairing_handle"].(string)

	if first := callTool(t, completeGooglePairingHandler(a), map[string]any{"pairing_handle": handle}); first.IsError {
		t.Fatalf("first completion should succeed: %v", first.Content)
	}
	second := callTool(t, completeGooglePairingHandler(a), map[string]any{"pairing_handle": handle})
	if !second.IsError {
		t.Error("expected reusing a handle to fail")
	}
}

// The timeout message must name the emoji, so a user who missed the prompt
// knows what they were looking for.
func TestCompleteGooglePairingTimeoutReportsEmoji(t *testing.T) {
	a := testApp(t)
	a.SessionPath = t.TempDir() + "/session.json"
	stubPairing(t,
		func(context.Context, *client.Client, string) (string, *libgm.PairingSession, error) {
			return "🚀", &libgm.PairingSession{}, nil
		},
		func(ctx context.Context, _ *client.Client, _ *libgm.PairingSession, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		},
	)

	start := callTool(t, startGooglePairingHandler(a), map[string]any{"cookies": "SID=abc"})
	handle := structuredMap(t, start)["pairing_handle"].(string)

	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"pairing_handle": handle}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := completeGooglePairingHandler(a)(ctx, req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result when the tap is not confirmed")
	}
	if text := result.Content[0].(mcp.TextContent).Text; !strings.Contains(text, "🚀") {
		t.Errorf("expected the emoji in the timeout message, got: %s", text)
	}
}

func TestPairingRegistryEvictsExpired(t *testing.T) {
	r := newPairingRegistry()
	now := time.Now()
	r.nowFn = func() time.Time { return now }

	handle := r.add(&pendingPairing{cli: client.NewForPairing(zerolog.Nop()), started: now})
	now = now.Add(pairingSessionTTL + time.Second)

	if _, ok := r.take(handle); ok {
		t.Error("expected an expired pairing to be evicted")
	}
}

func TestParseGoogleCookiesInputShapes(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"json object", `{"SID":"abc","HSID":"def"}`},
		{"raw cookie header", "SID=abc; HSID=def"},
		{"cookie header with name", "Cookie: SID=abc; HSID=def"},
		{"curl with -H", `curl 'https://messages.google.com/' -H 'Cookie: SID=abc; HSID=def'`},
		{"curl with -b", `curl 'https://messages.google.com/' -b 'SID=abc; HSID=def'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cookies, err := client.ParseGoogleCookiesInput(tc.input)
			if err != nil {
				t.Fatalf("parse %s: %v", tc.name, err)
			}
			if cookies["SID"] != "abc" || cookies["HSID"] != "def" {
				t.Errorf("unexpected cookies for %s: %v", tc.name, cookies)
			}
		})
	}
}

func TestParseGoogleCookiesInputRejectsGarbage(t *testing.T) {
	for _, input := range []string{"", "   ", "not cookies at all"} {
		if _, err := client.ParseGoogleCookiesInput(input); err == nil {
			t.Errorf("expected an error for %q", input)
		}
	}
}
