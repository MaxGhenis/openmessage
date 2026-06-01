package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
)

// RunRead handles "openmessage read <query> [--limit N] [--phone NUMBER] [--json]".
//
// It searches the local message store for matching text and prints the results.
// Reading only touches the OpenMessage store (messages.db in the data dir), so it
// does not require Full Disk Access. To include the latest iMessages, run
// "openmessage import imessage" first (that step needs Full Disk Access).
func RunRead(logger zerolog.Logger, args ...string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		return fmt.Errorf("usage: openmessage read <query> [--limit N] [--phone NUMBER] [--json]")
	}
	query := args[0]
	rest := args[1:]

	limit := 20
	if v := flagValue(rest, "--limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	phone := flagValue(rest, "--phone")
	asJSON := hasFlag(rest, "--json")

	a, err := app.New(logger)
	if err != nil {
		return fmt.Errorf("init app: %w", err)
	}
	defer a.Close()

	msgs, err := a.Store.SearchMessages(query, phone, limit)
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
