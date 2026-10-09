package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	utilcurl "go.mau.fi/util/curl"
)

// ParseGoogleCookiesInput accepts the three shapes people can realistically
// copy out of browser devtools and normalizes them to a cookie map:
//
//   - a JSON object of cookie name/value pairs,
//   - a full `curl ...` command (the "Copy as cURL" menu item, whose cookies
//     may arrive either as `-b '...'` or as `-H 'Cookie: ...'`),
//   - a raw `Cookie:` header value.
func ParseGoogleCookiesInput(raw string) (map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("no cookies provided")
	}

	var cookieMap map[string]string
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &cookieMap); err == nil && len(cookieMap) > 0 {
			return cookieMap, nil
		}
	}

	if strings.HasPrefix(trimmed, "curl ") {
		parsed, err := utilcurl.Parse(trimmed)
		if err != nil {
			return nil, fmt.Errorf("parse cURL command: %w", err)
		}
		if cookies, err := ParseCookieHeader(parsed.Header.Get("Cookie")); err == nil {
			return cookies, nil
		}
		return nil, fmt.Errorf("cURL command did not include a Cookie header")
	}

	return ParseCookieHeader(trimmed)
}

// ParseCookieHeader parses a `Cookie:` header value into a cookie map. The
// leading `Cookie:` is optional so a header copied verbatim also works.
func ParseCookieHeader(raw string) (map[string]string, error) {
	header := strings.TrimSpace(raw)
	if header == "" {
		return nil, fmt.Errorf("no cookie header provided")
	}
	if strings.HasPrefix(strings.ToLower(header), "cookie:") {
		header = strings.TrimSpace(header[len("cookie:"):])
	}
	req := &http.Request{Header: make(http.Header)}
	req.Header.Set("Cookie", header)
	parsed := map[string]string{}
	for _, cookie := range req.Cookies() {
		name := strings.TrimSpace(cookie.Name)
		if name == "" {
			continue
		}
		parsed[name] = cookie.Value
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("no cookies found")
	}
	return parsed, nil
}

// StartGaiaPairing performs the first half of Google Account pairing: it
// authenticates with the supplied cookies and asks Google to display a
// confirmation emoji on the phone. It returns that emoji plus the pairing
// session needed to finish.
//
// The returned session holds an ECDSA private key and is bound to cli, so it
// cannot be serialized or moved between processes — the same cli must be used
// to call FinishGaiaPairing.
func StartGaiaPairing(ctx context.Context, cli *Client, rawCookies string) (string, *libgm.PairingSession, error) {
	cookies, err := ParseGoogleCookiesInput(rawCookies)
	if err != nil {
		return "", nil, fmt.Errorf("parse Google cookies: %w", err)
	}
	cli.GM.AuthData.Cookies = cookies

	emoji, session, err := cli.GM.StartGaiaPairing(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("start google account pairing: %w", err)
	}
	return emoji, session, nil
}

// FinishGaiaPairing completes pairing once the user has tapped the emoji on
// their phone, and writes the resulting session to sessionPath. It blocks
// until the tap is confirmed, so callers should bound ctx.
func FinishGaiaPairing(ctx context.Context, cli *Client, session *libgm.PairingSession, sessionPath string) error {
	if _, err := cli.GM.FinishGaiaPairing(ctx, session); err != nil {
		return fmt.Errorf("finish google account pairing: %w", err)
	}

	sessionData, err := cli.SessionData()
	if err != nil {
		return fmt.Errorf("get session data: %w", err)
	}
	if err := SaveSession(sessionPath, sessionData); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}

// PairWithGoogleCookies runs both halves back to back, reporting the emoji via
// emojiCB in between. Suited to the CLI, where the emoji can be printed while
// the handshake is still in flight; request/response callers should use
// StartGaiaPairing and FinishGaiaPairing so the emoji can be delivered before
// the wait begins.
func PairWithGoogleCookies(ctx context.Context, cli *Client, sessionPath, rawCookies string, emojiCB func(emoji string)) error {
	emoji, session, err := StartGaiaPairing(ctx, cli, rawCookies)
	if err != nil {
		return err
	}
	if emojiCB != nil {
		emojiCB(emoji)
	}
	return FinishGaiaPairing(ctx, cli, session, sessionPath)
}
