package web

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func cspHashOf(script string) string {
	sum := sha256.Sum256([]byte(script))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

func TestInlineScriptHashSources(t *testing.T) {
	tests := []struct {
		name string
		page string
		want []string
	}{
		{"one inline", "<p>x</p><script>let a = 1;</script>", []string{cspHashOf("let a = 1;")}},
		{"src script skipped", `<script src="/app.js"></script><script>b()</script>`, []string{cspHashOf("b()")}},
		{"src after other attributes", `<script defer src='/a.js'></script>`, nil},
		{"uppercase tags", "<SCRIPT type=\"module\">c()</SCRIPT >", []string{cspHashOf("c()")}},
		{"crlf normalized like the HTML parser", "<script>\r\nd()\r\ne()\r</script>", []string{cspHashOf("\nd()\ne()\n")}},
		{"raw text is not entity-decoded", "<script>x = '&amp;'</script>", []string{cspHashOf("x = '&amp;'")}},
		{"ends at the first closing tag", "<script>s = '</script>'</script>", []string{cspHashOf("s = '")}},
		{"tag name must end", "<scripts>no</scripts><script>yes</script>", []string{cspHashOf("yes")}},
		{"closing tag name must end", "<script>a</scripty>b</script>", []string{cspHashOf("a</scripty>b")}},
		{"data-src is not src", `<script data-src="x">f()</script>`, []string{cspHashOf("f()")}},
		{"unterminated script ignored", "<script>never closed", nil},
		{"two scripts in order", "<script>1</script><script>2</script>", []string{cspHashOf("1"), cspHashOf("2")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inlineScriptHashSources([]byte(tt.page))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func uiPage(t *testing.T) string {
	t.Helper()
	page, err := staticFS.ReadFile(uiPagePath)
	if err != nil {
		t.Fatal(err)
	}
	return string(page)
}

// TestUIScriptDirectiveAllowsOnlyTheHashedPageScript pins the policy: script
// runs only from same-origin files or as the page's own inline script,
// matched by hash. 'unsafe-inline' would let injected markup run code again.
func TestUIScriptDirectiveAllowsOnlyTheHashedPageScript(t *testing.T) {
	page := uiPage(t)
	if strings.Count(strings.ToLower(page), "<script") != 1 {
		t.Fatalf("index.html has %d script elements; the CSP hashes every inline one, but review any new script", strings.Count(strings.ToLower(page), "<script"))
	}
	start := strings.Index(page, "<script>") + len("<script>")
	end := strings.LastIndex(page, "</script>")
	want := "script-src 'self' " + cspHashOf(page[start:end])
	if scriptSrcDirective != want {
		t.Fatalf("script-src = %q, want %q", scriptSrcDirective, want)
	}

	a, err := NewControlAuth(t.TempDir(), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	h := ProtectLocalControl(a.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	for _, path := range []string{"/", "/api/status", "/mcp"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7007"+path, nil))
		csp := w.Header().Get("Content-Security-Policy")
		directives := map[string]string{}
		for _, part := range strings.Split(csp, ";") {
			fields := strings.Fields(part)
			if len(fields) > 0 {
				directives[fields[0]] = strings.Join(fields[1:], " ")
			}
		}
		if got := "script-src " + directives["script-src"]; got != want {
			t.Fatalf("%s script-src = %q, want %q", path, got, want)
		}
		if directives["script-src-attr"] != "'none'" {
			t.Fatalf("%s script-src-attr = %q, want 'none'", path, directives["script-src-attr"])
		}
		if directives["form-action"] != "'self'" {
			t.Fatalf("%s form-action = %q, want 'self'", path, directives["form-action"])
		}
		for _, banned := range []string{"'unsafe-inline'", "'unsafe-eval'", "'unsafe-hashes'", "'strict-dynamic'"} {
			if strings.Contains(directives["script-src"], banned) || strings.Contains(directives["default-src"], banned) {
				t.Fatalf("%s CSP allows %s: %s", path, banned, csp)
			}
		}
	}
}

// inlineHandlerAttr matches an event-handler attribute in markup or in a
// markup string inside the page script: on<event>= followed by a quote, an
// escaped quote or a template interpolation, or an unquoted value written
// directly after "=". A JS property assignment (el.onclick = fn) is preceded
// by "." and a default parameter (onEvict = null) has no quote, so neither
// matches.
var inlineHandlerAttr = regexp.MustCompile(`(?i)(?:^|[\s"'/])on[a-z]{3,}(?:\s*=\s*(?:"|'|\\"|\\'|\$\{)|=[a-z_$])`)

func TestUIHasNoInlineEventHandlersOrScriptURLs(t *testing.T) {
	for i, line := range strings.Split(uiPage(t), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		if loc := inlineHandlerAttr.FindStringIndex(line); loc != nil {
			t.Errorf("index.html:%d has an inline event handler (%q); attach it with addEventListener and capture ids in a closure", i+1, strings.TrimSpace(line[loc[0]:min(len(line), loc[1]+30)]))
		}
		lower := strings.ToLower(line)
		for _, banned := range []string{"javascript:", "new function(", "eval(", "srcdoc"} {
			if strings.Contains(lower, banned) {
				t.Errorf("index.html:%d contains %q, which the CSP refuses or which bypasses escaping", i+1, banned)
			}
		}
	}
}

func TestInlineHandlerLintCatchesTheFixedSinks(t *testing.T) {
	// The exact shapes this change removed must keep failing the lint.
	for _, line := range []string{
		"html += `<div class=\"msg-reply-preview\" onclick=\"scrollToMessage('${escapeHtml(id)}')\">`;",
		"html += `<button onclick=\"sendReaction('${safeMsgId}','${safeEmoji}')\">`;",
		"let html = '<input type=\"text\" oninput=\"filterEmojis(event, \\'' + jsArg + '\\')\">';",
		"html += `<img src=\"${mediaSrc}\" onerror=handleMediaError(this)>`;",
		"<body onload='boot()'>",
		"x = `<a ONCLICK=${handler}>`",
	} {
		if !inlineHandlerAttr.MatchString(line) {
			t.Errorf("lint misses %q", line)
		}
	}
	for _, line := range []string{
		"el.onclick = () => {};",
		"button.onload=fn;",
		"const once = true;",
		"if (connection === 'online') {",
		"function cache(limit, onEvict = null) {",
	} {
		if inlineHandlerAttr.MatchString(line) {
			t.Errorf("lint flags harmless %q", line)
		}
	}
}

func TestUILoginErrorQueryMatchesServer(t *testing.T) {
	if !strings.Contains(uiPage(t), "params.get('"+LoginErrorQuery+"') === 'invalid_token'") {
		t.Fatalf("index.html does not read the %q query that /auth/login redirects with", LoginErrorQuery)
	}
}
