package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// The web UI is one page with one inline <script>. The CSP allows exactly
// that script by its SHA-256 instead of 'unsafe-inline', so markup that a
// missed escaping bug lets into the page cannot run: inline event handlers
// (onclick=, onerror=, ...) and injected <script> elements are both refused.
// The hash is computed from the embedded page at startup, so editing
// index.html never needs a hand-updated header.

// uiPagePath is the embedded page whose inline scripts the CSP allows.
const uiPagePath = "static/index.html"

var scriptSrcDirective = buildScriptSrcDirective()

func buildScriptSrcDirective() string {
	page, err := staticFS.ReadFile(uiPagePath)
	if err != nil {
		panic(fmt.Sprintf("read embedded %s: %v", uiPagePath, err))
	}
	sources := append([]string{"'self'"}, inlineScriptHashSources(page)...)
	return "script-src " + strings.Join(sources, " ")
}

// inlineScriptHashSources returns a CSP hash source ('sha256-...') for each
// inline script element in page, in document order. Script elements with a
// src attribute load from 'self' and are skipped. The hash covers the
// element's text exactly as a browser sees it: HTML input preprocessing turns
// CRLF and lone CR into LF before the script text exists, and script text is
// raw (no entity decoding), ending at the first "</script" that is followed
// by whitespace, "/" or ">".
func inlineScriptHashSources(page []byte) []string {
	page = bytes.ReplaceAll(page, []byte("\r\n"), []byte("\n"))
	page = bytes.ReplaceAll(page, []byte("\r"), []byte("\n"))
	lower := bytes.ToLower(page)
	var sources []string
	for offset := 0; ; {
		start := indexScriptTag(lower, offset, "<script")
		if start < 0 {
			return sources
		}
		tagEnd := bytes.IndexByte(lower[start:], '>')
		if tagEnd < 0 {
			return sources
		}
		tagEnd += start
		bodyStart := tagEnd + 1
		bodyEnd := indexScriptTag(lower, bodyStart, "</script")
		if bodyEnd < 0 {
			return sources
		}
		if !scriptTagHasSrc(string(lower[start:tagEnd])) {
			sum := sha256.Sum256(page[bodyStart:bodyEnd])
			sources = append(sources, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
		offset = bodyEnd + len("</script")
	}
}

// indexScriptTag finds needle ("<script" or "</script") at or after offset
// where it is a whole tag name, followed by whitespace, "/" or ">".
func indexScriptTag(lower []byte, offset int, needle string) int {
	for offset < len(lower) {
		i := bytes.Index(lower[offset:], []byte(needle))
		if i < 0 {
			return -1
		}
		i += offset
		next := i + len(needle)
		if next < len(lower) {
			switch lower[next] {
			case ' ', '\t', '\n', '\f', '/', '>':
				return i
			}
		}
		offset = next
	}
	return -1
}

func scriptTagHasSrc(tag string) bool {
	for _, field := range strings.FieldsFunc(tag, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\f' || r == '/'
	}) {
		if field == "src" || strings.HasPrefix(field, "src=") {
			return true
		}
	}
	return false
}
