package media

import (
	"net/http"
	"strings"
)

// Disposition controls whether media may render inline or must be downloaded.
type Disposition string

const (
	DispositionInline         Disposition = "inline"
	DispositionForcedDownload Disposition = "attachment"
)

// ServingDisposition returns inline only for a passive declared media type
// whose sniffed bytes are not active markup.
func ServingDisposition(declaredMIME string, sniff []byte) Disposition {
	if isPassiveInlineMIME(declaredMIME) && !isActiveMIME(http.DetectContentType(sniff)) {
		return DispositionInline
	}
	return DispositionForcedDownload
}

func normalizeMIME(mimeType string) string {
	m := strings.ToLower(strings.TrimSpace(mimeType))
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return m
}

// isPassiveInlineMIME reports whether a media type can be rendered inline in a
// browser with no risk of executing script. SVG and the HTML/XML family are
// excluded because they can carry <script>; anything not positively recognized
// is treated as unsafe and downloaded instead.
func isPassiveInlineMIME(mimeType string) bool {
	m := normalizeMIME(mimeType)
	switch {
	case m == "image/svg+xml":
		return false
	case strings.HasPrefix(m, "image/"),
		strings.HasPrefix(m, "audio/"),
		strings.HasPrefix(m, "video/"):
		return true
	case m == "application/pdf", m == "text/plain":
		return true
	default:
		return false
	}
}

// isActiveMIME reports whether a sniffed type could be interpreted as
// script-bearing markup. It rejects a payload declared as a passive type whose
// bytes actually sniff as active content (a spoofed image/png that is really
// HTML).
func isActiveMIME(mimeType string) bool {
	switch normalizeMIME(mimeType) {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "text/xml", "application/xml":
		return true
	}
	return false
}

// sanitizeMediaFilename strips path components, control characters, and
// header-breaking characters so a filename can be placed in a quoted
// Content-Disposition value. Non-ASCII bytes are replaced to keep the header
// well-formed; the result is never empty.
func sanitizeMediaFilename(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f, r == '"' || r == '\\':
			// drop
		case r > 0x7f:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "attachment"
	}
	return out
}
