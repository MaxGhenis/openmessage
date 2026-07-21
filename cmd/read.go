package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
	"github.com/maxghenis/openmessage/internal/v2read"
)

type commandReadSession struct {
	Reads     readsource.ReadSource
	DataDir   string
	StorePath string
	close     func()
}

func (s *commandReadSession) Close() {
	if s != nil && s.close != nil {
		s.close()
	}
}

func openCommandReadSource(
	logger zerolog.Logger,
	banner io.Writer,
) (*commandReadSession, error) {
	dataDir := app.DefaultDataDir()
	mode, err := resolveV2RuntimeMode(app.DemoMode(), dataDir)
	if err != nil {
		return nil, err
	}
	if mode.Primary {
		storePath := filepath.Join(dataDir, "v2", "store.sqlite3")
		store, err := sqlite.Open(storePath)
		if err != nil {
			return nil, fmt.Errorf("open v2 read store %q: %w", storePath, err)
		}
		if banner != nil {
			fmt.Fprintln(banner, "reading v2 store")
		}
		return &commandReadSession{
			Reads:     v2read.New(store),
			DataDir:   dataDir,
			StorePath: storePath,
			close: func() {
				_ = store.Close()
			},
		}, nil
	}

	// NewClient, not New: read and status are advertised as read-only
	// commands, and the daemon owns the startup repair sweeps — a one-shot
	// reader must not write to the live store it shares with the running app.
	a, err := app.NewClient(logger)
	if err != nil {
		return nil, fmt.Errorf("init app: %w", err)
	}
	return &commandReadSession{
		Reads:     a.Store,
		DataDir:   a.DataDir,
		StorePath: filepath.Join(a.DataDir, "messages.db"),
		close:     a.Close,
	}, nil
}

// RunRead handles "openmessage read <query> [--limit N] [--phone NUMBER] [--json]".
//
// It searches the local message store for matching text and prints the results.
// Reading only touches the OpenMessage store (messages.db in the data dir), so it
// does not require Full Disk Access. To include the latest iMessages, run
// "openmessage import imessage" first (that step needs Full Disk Access).
func RunRead(logger zerolog.Logger, args ...string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		return fmt.Errorf("usage: openmessage read <query> [--limit N] [--phone NUMBER] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json]")
	}
	query := args[0]
	rest := args[1:]

	limit := 20
	if v := flagValue(rest, "--limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	sinceMS, err := parseDayBound(flagValue(rest, "--since"), false)
	if err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	untilMS, err := parseDayBound(flagValue(rest, "--until"), true)
	if err != nil {
		return fmt.Errorf("--until: %w", err)
	}
	filter := db.SearchFilter{
		Phone:   flagValue(rest, "--phone"),
		SinceMS: sinceMS,
		UntilMS: untilMS,
		Limit:   limit,
	}
	asJSON := hasFlag(rest, "--json")

	session, err := openCommandReadSource(logger, os.Stderr)
	if err != nil {
		return err
	}
	defer session.Close()

	msgs, err := session.Reads.SearchMessagesFiltered(query, filter)
	if err != nil {
		return fmt.Errorf("search failed: %w", err)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(msgs)
	}

	if len(msgs) == 0 {
		fmt.Printf("No messages found matching %q.\n", query)
		return nil
	}

	fmt.Printf("Found %d message(s) matching %q:\n\n", len(msgs), query)
	for _, m := range msgs {
		ts := time.UnixMilli(m.TimestampMS).Format("2006-01-02 15:04")
		dir := "<-" // received
		if m.IsFromMe {
			dir = "->" // sent
		}
		sender := firstNonEmpty(m.SenderName, m.SenderNumber, m.ConversationID)
		body := strings.TrimSpace(m.Body)
		if body == "" && m.MediaID != "" {
			body = "[media]"
		}
		platform := ""
		if m.SourcePlatform != "" && m.SourcePlatform != "sms" {
			platform = " [" + m.SourcePlatform + "]"
		}
		fmt.Printf("%s  %s %s%s\n    %s\n",
			ts, dir, sender, platform, strings.ReplaceAll(body, "\n", " "))
	}
	return nil
}

// firstNonEmpty returns the first non-blank string, or "" if all are blank.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// parseDayBound parses a --since/--until value into a millisecond timestamp.
// It accepts a bare date (YYYY-MM-DD), a date-time, or RFC3339, all in local
// time. An empty string returns 0 (no bound). When endOfDay is true, a bare
// date resolves to 23:59:59.999 so that "--until 2026-05-28" includes all of
// that day rather than stopping at midnight.
func parseDayBound(s string, endOfDay bool) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		if endOfDay {
			t = t.Add(24*time.Hour - time.Millisecond)
		}
		return t.UnixMilli(), nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("invalid date %q (use YYYY-MM-DD)", s)
}
