package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	defaultGIFSearchQuery         = "popular"
	maxGIFSearchResults           = 48
	defaultGIFSearchLimit         = 24
	klipyPreferredSizeLimit       = 3_000_000
	maxGIFPreviewBytes      int64 = 8 << 20
	maxGIFSendBytes         int64 = 24 << 20
)

var (
	errGIFProviderNotConfigured = errors.New("GIF search is not configured; set OPENMESSAGES_KLIPY_API_KEY")
	klipySearchEndpoint         = "https://api.klipy.com/v2/search"
	gifHTTPClient               = &http.Client{Timeout: 15 * time.Second}
)

type gifSearchResult struct {
	ID         string `json:"id,omitempty"`
	Title      string `json:"title,omitempty"`
	PreviewURL string `json:"preview_url"`
	URL        string `json:"url"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	MimeType   string `json:"mime_type"`
}

type klipySearchResponse struct {
	Results []klipyResult `json:"results"`
}

type klipyResult struct {
	ID           string                      `json:"id"`
	Title        string                      `json:"title"`
	Content      string                      `json:"content_description"`
	MediaFormats map[string]klipyMediaFormat `json:"media_formats"`
}

type klipyMediaFormat struct {
	URL  string `json:"url"`
	Size int64  `json:"size"`
	Dims []int  `json:"dims"`
}

func searchKlipyGIFs(ctx context.Context, query string, limit, page int) ([]gifSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		query = defaultGIFSearchQuery
	}
	if limit <= 0 {
		limit = defaultGIFSearchLimit
	}
	if limit > maxGIFSearchResults {
		limit = maxGIFSearchResults
	}
	if page <= 0 {
		page = 1
	}
	apiKey := klipyAPIKey()
	if apiKey == "" {
		return nil, errGIFProviderNotConfigured
	}

	endpoint, err := url.Parse(klipySearchEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parse GIF endpoint: %w", err)
	}
	params := endpoint.Query()
	params.Set("q", query)
	params.Set("key", apiKey)
	params.Set("page", strconv.Itoa(page))
	params.Set("per_page", strconv.Itoa(limit))
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := gifHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search GIFs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("search GIFs: provider returned %d", resp.StatusCode)
	}

	var payload klipySearchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode GIF search: %w", err)
	}

	results := make([]gifSearchResult, 0, min(limit, len(payload.Results)))
	for _, item := range payload.Results {
		result, ok := parseKlipyGIFResult(item)
		if !ok {
			continue
		}
		results = append(results, result)
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

func parseKlipyGIFResult(item klipyResult) (gifSearchResult, bool) {
	formats := item.MediaFormats
	if len(formats) == 0 {
		return gifSearchResult{}, false
	}

	preview, ok := firstKlipyFormat(formats, "tinygif", "nanogif", "mediumgif", "gif", "webp")
	if !ok || strings.TrimSpace(preview.URL) == "" {
		return gifSearchResult{}, false
	}

	full, ok := formats["gif"]
	mimeType := "image/gif"
	if !ok || strings.TrimSpace(full.URL) == "" {
		full, ok = firstKlipyFormat(formats, "mediumgif", "webp", "tinygif", "nanogif")
		if !ok || strings.TrimSpace(full.URL) == "" {
			return gifSearchResult{}, false
		}
		if strings.Contains(strings.ToLower(full.URL), ".webp") {
			mimeType = "image/webp"
		}
	}

	if medium, ok := formats["mediumgif"]; ok && strings.TrimSpace(medium.URL) != "" && full.Size > klipyPreferredSizeLimit {
		full = medium
		mimeType = "image/gif"
	}
	if webp, ok := formats["webp"]; ok && strings.TrimSpace(webp.URL) != "" && (full.Size == 0 || webp.Size == 0 || webp.Size < full.Size) {
		full = webp
		mimeType = "image/webp"
	}

	width, height := klipyDimensions(full)
	if width == 0 || height == 0 {
		width, height = klipyDimensions(preview)
	}

	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = strings.TrimSpace(item.Content)
	}

	return gifSearchResult{
		ID:         strings.TrimSpace(item.ID),
		Title:      title,
		PreviewURL: strings.TrimSpace(preview.URL),
		URL:        strings.TrimSpace(full.URL),
		Width:      width,
		Height:     height,
		MimeType:   mimeType,
	}, true
}

func firstKlipyFormat(formats map[string]klipyMediaFormat, names ...string) (klipyMediaFormat, bool) {
	for _, name := range names {
		format, ok := formats[name]
		if ok && strings.TrimSpace(format.URL) != "" {
			return format, true
		}
	}
	return klipyMediaFormat{}, false
}

func klipyDimensions(format klipyMediaFormat) (int, int) {
	if len(format.Dims) < 2 || format.Dims[0] <= 0 || format.Dims[1] <= 0 {
		return 0, 0
	}
	return format.Dims[0], format.Dims[1]
}

func klipyAPIKey() string {
	if key := strings.TrimSpace(os.Getenv("OPENMESSAGES_KLIPY_API_KEY")); key != "" {
		return key
	}
	if key := strings.TrimSpace(os.Getenv("KLIPY_API_KEY")); key != "" {
		return key
	}
	return ""
}

func proxyGIFPreviewURL(rawURL string) string {
	return "/api/gifs/preview?url=" + url.QueryEscape(rawURL)
}

func downloadGIFMedia(ctx context.Context, rawURL string, limit int64) ([]byte, string, string, error) {
	parsed, err := validateKlipyMediaURL(rawURL)
	if err != nil {
		return nil, "", "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", "", err
	}
	resp, err := gifHTTPClient.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("download GIF: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("download GIF: provider returned %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", "", fmt.Errorf("read GIF: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, "", "", fmt.Errorf("GIF too large (limit %d MB)", limit>>20)
	}

	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if idx := strings.Index(contentType, ";"); idx >= 0 {
		contentType = strings.TrimSpace(contentType[:idx])
	}
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = mime.TypeByExtension(path.Ext(parsed.Path))
	}
	if contentType == "" {
		contentType = "image/gif"
	}
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return nil, "", "", errors.New("GIF provider returned non-image content")
	}

	filename := gifFilename(parsed, contentType)
	return data, filename, contentType, nil
}

func validateKlipyMediaURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid GIF URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return nil, errors.New("GIF URL must use HTTPS")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "klipy.com" && !strings.HasSuffix(host, ".klipy.com") {
		return nil, errors.New("GIF URL host is not allowed")
	}
	return parsed, nil
}

func gifFilename(parsed *url.URL, mimeType string) string {
	name := strings.TrimSpace(path.Base(parsed.Path))
	if name == "." || name == "/" || name == "" {
		name = "openmessage-gif"
	}
	if ext := path.Ext(name); ext == "" {
		switch strings.ToLower(mimeType) {
		case "image/webp":
			name += ".webp"
		case "image/png":
			name += ".png"
		case "image/jpeg":
			name += ".jpg"
		default:
			name += ".gif"
		}
	}
	if decoded, err := url.PathUnescape(name); err == nil && strings.TrimSpace(decoded) != "" {
		name = decoded
	}
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', 0:
			return -1
		default:
			return r
		}
	}, name)
	if name == "" {
		return "openmessage-gif.gif"
	}
	return name
}
