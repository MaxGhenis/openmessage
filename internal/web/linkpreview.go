package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/net/html"
)

var (
	ErrInvalidLinkPreviewURL = errors.New("invalid link preview url")
	ErrBlockedLinkPreviewURL = errors.New("link preview url is blocked")
	ErrNoLinkPreview         = errors.New("no link preview available")
)

type LinkPreview struct {
	URL         string `json:"url,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	ImageURL    string `json:"image_url,omitempty"`
	SiteName    string `json:"site_name,omitempty"`
	Domain      string `json:"domain,omitempty"`
}

type LinkPreviewFetcher func(ctx context.Context, rawURL string) (*LinkPreview, error)

type linkPreviewCacheEntry struct {
	preview   *LinkPreview
	expiresAt time.Time
	touchedAt time.Time
}

type LinkPreviewService struct {
	logger            zerolog.Logger
	client            *http.Client
	ttl               time.Duration
	maxEntries        int
	allowPrivateHosts bool

	mu    sync.Mutex
	cache map[string]linkPreviewCacheEntry
}

func NewLinkPreviewService(logger zerolog.Logger) *LinkPreviewService {
	s := &LinkPreviewService{
		logger:     logger,
		ttl:        6 * time.Hour,
		maxEntries: 256,
		cache:      make(map[string]linkPreviewCacheEntry),
	}
	s.client = &http.Client{
		Timeout: 6 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
		Transport: &http.Transport{
			// Validate AND connect to the same resolved IP, closing the
			// resolve-then-fetch TOCTOU (DNS-rebinding) gap where a hostname
			// could resolve to a public IP for the guard check and a private
			// IP for the actual connection. This covers the initial request
			// and every redirect in one place.
			DialContext:           s.guardedDialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
	return s
}

// guardedDialContext resolves the target host, rejects it if any resolved
// address is private/loopback (unless allowPrivateHosts), and dials the exact
// IP it validated — so there is no second, unchecked DNS resolution. TLS still
// verifies against the original hostname (the transport sets ServerName from
// the request URL, not the dialed address).
func (s *LinkPreviewService) guardedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve preview host: %w", err)
	}
	if !s.allowPrivateHosts {
		for _, ip := range ips {
			if isPrivatePreviewIP(ip.IP) {
				return nil, ErrBlockedLinkPreviewURL
			}
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no addresses for preview host %q", host)
	}
	return nil, lastErr
}

func (s *LinkPreviewService) Fetch(ctx context.Context, rawURL string) (*LinkPreview, error) {
	normalizedURL, parsedURL, err := normalizeLinkPreviewURL(rawURL)
	if err != nil {
		return nil, err
	}
	if !s.allowPrivateHosts {
		if err := ensurePublicPreviewHost(ctx, parsedURL); err != nil {
			return nil, err
		}
	}

	if preview := s.cached(normalizedURL); preview != nil {
		return preview, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalizedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidLinkPreviewURL, err)
	}
	req.Header.Set("User-Agent", "OpenMessage/1.0 (+https://openmessage.ai)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch link preview: %w", err)
	}
	defer resp.Body.Close()

	if resp.Request != nil && resp.Request.URL != nil && !s.allowPrivateHosts {
		if err := ensurePublicPreviewHost(ctx, resp.Request.URL); err != nil {
			return nil, err
		}
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("fetch link preview: unexpected status %d", resp.StatusCode)
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "text/html") && !strings.Contains(contentType, "application/xhtml+xml") {
		return nil, ErrNoLinkPreview
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read link preview: %w", err)
	}

	finalURL := parsedURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL
	}

	preview, err := extractLinkPreview(body, finalURL)
	if err != nil {
		return nil, err
	}
	preview.URL = finalURL.String()
	preview.Domain = finalURL.Hostname()
	if preview.SiteName == "" {
		preview.SiteName = prettifyPreviewHost(finalURL.Hostname())
	}

	s.store(normalizedURL, preview)
	return cloneLinkPreview(preview), nil
}

func (s *LinkPreviewService) cached(rawURL string) *LinkPreview {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.cache[rawURL]
	if !ok {
		return nil
	}
	if time.Now().After(entry.expiresAt) {
		delete(s.cache, rawURL)
		return nil
	}
	entry.touchedAt = time.Now()
	s.cache[rawURL] = entry
	return cloneLinkPreview(entry.preview)
}

func (s *LinkPreviewService) store(rawURL string, preview *LinkPreview) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.pruneExpiredLocked(now)
	s.cache[rawURL] = linkPreviewCacheEntry{
		preview:   cloneLinkPreview(preview),
		expiresAt: now.Add(s.ttl),
		touchedAt: now,
	}
	s.evictIfNeededLocked()
}

func (s *LinkPreviewService) pruneExpiredLocked(now time.Time) {
	for rawURL, entry := range s.cache {
		if now.After(entry.expiresAt) {
			delete(s.cache, rawURL)
		}
	}
}

func (s *LinkPreviewService) evictIfNeededLocked() {
	if s.maxEntries <= 0 {
		return
	}
	for len(s.cache) > s.maxEntries {
		oldestURL := ""
		var oldestTouched time.Time
		for rawURL, entry := range s.cache {
			if oldestURL == "" || entry.touchedAt.Before(oldestTouched) {
				oldestURL = rawURL
				oldestTouched = entry.touchedAt
			}
		}
		if oldestURL == "" {
			return
		}
		delete(s.cache, oldestURL)
	}
}

func cloneLinkPreview(preview *LinkPreview) *LinkPreview {
	if preview == nil {
		return nil
	}
	copy := *preview
	return &copy
}

func normalizeLinkPreviewURL(raw string) (string, *url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil, ErrInvalidLinkPreviewURL
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInvalidLinkPreviewURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", nil, ErrInvalidLinkPreviewURL
	}
	if parsed.Hostname() == "" {
		return "", nil, ErrInvalidLinkPreviewURL
	}
	parsed.Fragment = ""
	return parsed.String(), parsed, nil
}

func ensurePublicPreviewHost(ctx context.Context, target *url.URL) error {
	host := strings.ToLower(target.Hostname())
	if host == "" {
		return ErrInvalidLinkPreviewURL
	}
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return ErrBlockedLinkPreviewURL
	}

	if ip := net.ParseIP(host); ip != nil {
		if isPrivatePreviewIP(ip) {
			return ErrBlockedLinkPreviewURL
		}
		return nil
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve preview host: %w", err)
	}
	for _, addr := range addrs {
		if isPrivatePreviewIP(addr.IP) {
			return ErrBlockedLinkPreviewURL
		}
	}
	return nil
}

func isPrivatePreviewIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified()
}

func extractLinkPreview(body []byte, baseURL *url.URL) (*LinkPreview, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parse link preview: %w", err)
	}

	meta := map[string]string{}
	title := ""
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch strings.ToLower(node.Data) {
			case "title":
				if title == "" {
					title = collapsePreviewWhitespace(textContent(node))
				}
			case "meta":
				key := ""
				content := ""
				for _, attr := range node.Attr {
					switch strings.ToLower(attr.Key) {
					case "property", "name", "itemprop":
						if key == "" {
							key = strings.ToLower(strings.TrimSpace(attr.Val))
						}
					case "content":
						content = collapsePreviewWhitespace(attr.Val)
					}
				}
				if key != "" && content != "" {
					if _, exists := meta[key]; !exists {
						meta[key] = content
					}
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)

	preview := &LinkPreview{
		Title:       firstNonEmpty(meta["og:title"], meta["twitter:title"], meta["title"], title),
		Description: firstNonEmpty(meta["og:description"], meta["twitter:description"], meta["description"]),
		ImageURL:    resolvePreviewURL(baseURL, firstNonEmpty(meta["og:image"], meta["twitter:image"], meta["twitter:image:src"], meta["image"])),
		SiteName:    firstNonEmpty(meta["og:site_name"], meta["application-name"]),
	}

	preview.Title = truncatePreviewText(preview.Title, 180)
	preview.Description = truncatePreviewText(preview.Description, 320)
	if preview.Title == "" && preview.Description == "" && preview.ImageURL == "" && preview.SiteName == "" {
		return nil, ErrNoLinkPreview
	}
	return preview, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func collapsePreviewWhitespace(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func truncatePreviewText(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	if limit <= 1 {
		return value[:limit]
	}
	return strings.TrimSpace(value[:limit-1]) + "…"
}

func resolvePreviewURL(baseURL *url.URL, raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "//") {
		if baseURL == nil || baseURL.Scheme == "" {
			return "https:" + trimmed
		}
		return baseURL.Scheme + ":" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	if parsed.IsAbs() {
		// Only http(s) image URLs are allowed through. og:image is
		// attacker-controlled (it comes from the linked page's meta tags), so
		// reject data:/javascript:/file:/etc. before it reaches an <img src>.
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return ""
		}
		return parsed.String()
	}
	if baseURL == nil {
		return ""
	}
	return baseURL.ResolveReference(parsed).String()
}

func textContent(node *html.Node) string {
	if node == nil {
		return ""
	}
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			builder.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return builder.String()
}

func prettifyPreviewHost(host string) string {
	trimmed := strings.ToLower(strings.TrimSpace(host))
	trimmed = strings.TrimPrefix(trimmed, "www.")
	trimmed = strings.TrimSpace(trimmed)
	trimmed = strings.TrimSuffix(trimmed, path.Ext(trimmed))
	if trimmed == "" {
		return host
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) == 0 {
		return host
	}
	label := strings.ReplaceAll(parts[0], "-", " ")
	if label == "" {
		return host
	}
	return strings.ToUpper(label[:1]) + label[1:]
}
