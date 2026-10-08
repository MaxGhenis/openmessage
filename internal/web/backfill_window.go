package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const backfillSinceFormats = "an RFC 3339 time, a YYYY-MM-DD local date, or Unix milliseconds"

// parseBackfillSince reads the optional window start of POST /api/backfill.
// An empty body (the original deep-backfill call) reports windowed=false. A
// JSON body {"since": ...} asks for a window backfill from that instant and
// accepts an RFC 3339 time, a YYYY-MM-DD local date, or Unix milliseconds
// (as a number or a string). Unknown fields are rejected so a typo cannot
// silently turn a window request into a full deep backfill.
func parseBackfillSince(r *http.Request, now time.Time) (time.Time, bool, error) {
	if r.Body == nil {
		return time.Time{}, false, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read request body: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return time.Time{}, false, nil
	}
	var request struct {
		Since json.RawMessage `json:"since"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return time.Time{}, false, fmt.Errorf("invalid JSON: %w", err)
	}
	raw := bytes.TrimSpace(request.Since)
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false, nil
	}

	since, err := parseBackfillSinceValue(raw)
	if err != nil {
		return time.Time{}, false, err
	}
	if since.UnixMilli() <= 0 {
		return time.Time{}, false, errors.New("since must be after the Unix epoch")
	}
	if since.After(now) {
		return time.Time{}, false, errors.New("since is in the future")
	}
	return since, true, nil
}

func parseBackfillSinceValue(raw json.RawMessage) (time.Time, error) {
	var milliseconds int64
	if err := json.Unmarshal(raw, &milliseconds); err == nil {
		return time.UnixMilli(milliseconds), nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return time.Time{}, fmt.Errorf("since must be %s", backfillSinceFormats)
	}
	text = strings.TrimSpace(text)
	if parsed, err := time.Parse(time.RFC3339, text); err == nil {
		return parsed, nil
	}
	if parsed, err := time.ParseInLocation("2006-01-02", text, time.Local); err == nil {
		return parsed, nil
	}
	if milliseconds, err := strconv.ParseInt(text, 10, 64); err == nil {
		return time.UnixMilli(milliseconds), nil
	}
	return time.Time{}, fmt.Errorf("since %q must be %s", text, backfillSinceFormats)
}
