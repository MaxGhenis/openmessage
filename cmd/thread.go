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
	"github.com/maxghenis/openmessage/internal/db"
)

// defaultThreadLimit is how many of the most recent messages a thread prints
// when --limit is not given. They are still printed oldest→newest.
const defaultThreadLimit = 50

// threadOptions captures the parsed flags for a thread read.
type threadOptions struct {
	Limit   int
	SinceMS int64
	UntilMS int64
}

// threadResult is the outcome of resolving a thread query against the store.
// Exactly one of Messages or Matches is populated:
//   - Messages: the target resolved to a single thread; print these chronologically.
//   - Matches: the query was ambiguous; print this disambiguation list instead.
type threadResult struct {
	Conversation *db.Conversation // the resolved conversation, when known (nil for phone-only resolution)
	Label        string           // human label for the thread header (name / number / id)
	Messages     []*db.Message    // oldest→newest; populated when a single thread resolved
	Matches      []*db.Conversation
}

// RunThread handles
// "openmessage thread <name|number|conversation_id> [--limit N] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json]".
//
// Unlike "read" (which is a text search and requires a query term), "thread"
// prints a whole conversation chronologically, resolved by contact name, phone
// number, or conversation_id. It only touches the OpenMessage store
// (messages.db in the data dir), so it needs no Full Disk Access. To include
// the latest iMessages, run "openmessage import imessage" first (that step does
// need Full Disk Access).
//
// Resolution order:
//  1. If the argument is an existing conversation_id, that thread is used.
//  2. Otherwise it is matched against conversation names, participants, and
//     sender names/numbers. A single match is printed; multiple matches print a
//     short disambiguation list (and exit 0 without dumping a thread).
//  3. If nothing matches by metadata but the argument looks like a phone number,
//     messages are gathered for that number across conversations.
func RunThread(logger zerolog.Logger, args ...string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		return fmt.Errorf("usage: openmessage thread <name|number|conversation_id> [--limit N] [--since YYYY-MM-DD] [--until YYYY-MM-DD] [--json]")
	}
	query := args[0]
	rest := args[1:]

	opts := threadOptions{Limit: defaultThreadLimit}
	if v := flagValue(rest, "--limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			opts.Limit = n
		}
	}
	var err error
	if opts.SinceMS, err = parseDayBound(flagValue(rest, "--since"), false); err != nil {
		return fmt.Errorf("--since: %w", err)
	}
	if opts.UntilMS, err = parseDayBound(flagValue(rest, "--until"), true); err != nil {
		return fmt.Errorf("--until: %w", err)
	}
	asJSON := hasFlag(rest, "--json")

	a, err := app.New(logger)
	if err != nil {
		return fmt.Errorf("init app: %w", err)
	}
	defer a.Close()

	res, err := resolveThread(a.Store, query, opts)
	if err != nil {
		return err
	}

	if asJSON {
		return writeThreadJSON(res)
	}
	printThread(query, res)
	return nil
}

// resolveThread turns a query (conversation_id, contact name, or phone number)
// into a threadResult. It performs no I/O beyond the supplied Store, which keeps
// it unit-testable against an in-memory store.
func resolveThread(store *db.Store, query string, opts threadOptions) (*threadResult, error) {
	if opts.Limit <= 0 {
		opts.Limit = defaultThreadLimit
	}

	// 1. Exact conversation_id.
	if conv, err := store.GetConversation(query); err == nil && conv != nil {
		msgs, err := loadConversationMessages(store, conv.ConversationID, opts)
		if err != nil {
			return nil, err
		}
		return &threadResult{Conversation: conv, Label: conversationLabel(conv), Messages: msgs}, nil
	}

	// 2. Resolve by metadata (name / participants / sender name or number).
	matches, err := store.SearchConversationsByMetadata(query, 25)
	if err != nil {
		return nil, fmt.Errorf("resolve conversation: %w", err)
	}
	switch {
	case len(matches) == 1:
		conv := matches[0]
		msgs, err := loadConversationMessages(store, conv.ConversationID, opts)
		if err != nil {
			return nil, err
		}
		return &threadResult{Conversation: conv, Label: conversationLabel(conv), Messages: msgs}, nil
	case len(matches) > 1:
		return &threadResult{Matches: matches}, nil
	}

	// 3. Nothing matched. (A phone number that has any messages always resolves
	//    in step 2, because SearchConversationsByMetadata matches on participants
	//    and sender_number; so there is no separate phone path to fall through
	//    to here — an unmatched query is simply an empty thread.)
	return &threadResult{}, nil
}

// loadConversationMessages returns the most recent opts.Limit messages of a
// conversation, oldest→newest, honoring --since/--until.
//
// It fetches newest-first (optionally bounded above by --until), drops anything
// before --since, then reverses to chronological order. Taking the newest Limit
// first — rather than the oldest within the window — keeps the result correct
// for any window size without an arbitrary cap.
func loadConversationMessages(store *db.Store, conversationID string, opts threadOptions) ([]*db.Message, error) {
	var (
		msgs []*db.Message
		err  error
	)
	if opts.UntilMS > 0 {
		// Before() is exclusive (timestamp_ms < bound); +1ms makes --until
		// inclusive of the end-of-day boundary parseDayBound produced.
		msgs, err = store.GetMessagesByConversationBefore(conversationID, opts.UntilMS+1, "", opts.Limit)
	} else {
		msgs, err = store.GetMessagesByConversation(conversationID, opts.Limit)
	}
	if err != nil {
		return nil, fmt.Errorf("load thread %s: %w", conversationID, err)
	}
	if opts.SinceMS > 0 {
		kept := msgs[:0]
		for _, m := range msgs {
			if m.TimestampMS >= opts.SinceMS {
				kept = append(kept, m)
			}
		}
		msgs = kept
	}
	reverseMessages(msgs) // queries return newest-first; threads print oldest-first
	return msgs, nil
}

// reverseMessages flips a slice in place (used to turn newest-first query
// results into the oldest→newest order threads print in).
func reverseMessages(msgs []*db.Message) {
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
}

// conversationLabel picks the best human label for a conversation header,
// falling back from name to participants to the id.
func conversationLabel(c *db.Conversation) string {
	if c == nil {
		return ""
	}
	return firstNonEmpty(c.Name, c.Participants, c.ConversationID)
}

// senderLabel returns "me" for outgoing messages, otherwise the sender's name,
// then number, then the conversation id.
func senderLabel(m *db.Message) string {
	if m.IsFromMe {
		return "me"
	}
	return firstNonEmpty(m.SenderName, m.SenderNumber, m.ConversationID)
}

// messageBody renders a message body for a single text line: newlines collapsed
// to spaces, with a "[media]" placeholder when an attachment carries no text.
func messageBody(m *db.Message) string {
	body := strings.TrimSpace(m.Body)
	if body == "" && m.MediaID != "" {
		body = "[media]"
	}
	return strings.ReplaceAll(body, "\n", " ")
}

func printThread(query string, res *threadResult) {
	if len(res.Matches) > 0 {
		fmt.Printf("%q matches %d conversations. Re-run with a conversation_id:\n\n", query, len(res.Matches))
		for _, c := range res.Matches {
			name := firstNonEmpty(c.Name, c.Participants, "(no name)")
			platform := ""
			if c.SourcePlatform != "" && c.SourcePlatform != "sms" {
				platform = " [" + c.SourcePlatform + "]"
			}
			fmt.Printf("  %-14s  %s%s  (last: %s)\n",
				c.ConversationID, name, platform, fmtTS(c.LastMessageTS))
		}
		return
	}

	if len(res.Messages) == 0 {
		fmt.Printf("No thread found for %q.\n", query)
		return
	}

	header := res.Label
	if res.Conversation != nil && res.Conversation.SourcePlatform != "" && res.Conversation.SourcePlatform != "sms" {
		header += " [" + res.Conversation.SourcePlatform + "]"
	}
	fmt.Printf("Thread with %s — %d message(s), oldest first:\n\n", header, len(res.Messages))
	for _, m := range res.Messages {
		ts := time.UnixMilli(m.TimestampMS).Format("2006-01-02 15:04")
		fmt.Printf("%s  %s: %s\n", ts, senderLabel(m), messageBody(m))
	}
}

// threadMessageJSON is the per-message shape emitted by `thread --json`. It
// surfaces a parsed local timestamp and a resolved sender label alongside the
// raw fields, so callers do not have to re-derive them.
type threadMessageJSON struct {
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	TimestampMS    int64  `json:"timestamp_ms"`
	Timestamp      string `json:"timestamp"`
	IsFromMe       bool   `json:"is_from_me"`
	Sender         string `json:"sender"`
	Body           string `json:"body"`
	SourcePlatform string `json:"source_platform,omitempty"`
}

func writeThreadJSON(res *threadResult) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if len(res.Matches) > 0 {
		type matchJSON struct {
			ConversationID string `json:"conversation_id"`
			Name           string `json:"name,omitempty"`
			LastMessageTS  int64  `json:"last_message_ts"`
			LastMessage    string `json:"last_message,omitempty"`
			SourcePlatform string `json:"source_platform,omitempty"`
		}
		out := struct {
			Matches []matchJSON `json:"matches"`
		}{}
		for _, c := range res.Matches {
			mj := matchJSON{
				ConversationID: c.ConversationID,
				Name:           c.Name,
				LastMessageTS:  c.LastMessageTS,
				SourcePlatform: c.SourcePlatform,
			}
			if c.LastMessageTS > 0 {
				mj.LastMessage = time.UnixMilli(c.LastMessageTS).Format(time.RFC3339)
			}
			out.Matches = append(out.Matches, mj)
		}
		return enc.Encode(out)
	}

	msgs := make([]threadMessageJSON, 0, len(res.Messages))
	for _, m := range res.Messages {
		mj := threadMessageJSON{
			MessageID:      m.MessageID,
			ConversationID: m.ConversationID,
			TimestampMS:    m.TimestampMS,
			IsFromMe:       m.IsFromMe,
			Sender:         senderLabel(m),
			Body:           m.Body,
			SourcePlatform: m.SourcePlatform,
		}
		if m.TimestampMS > 0 {
			mj.Timestamp = time.UnixMilli(m.TimestampMS).Format(time.RFC3339)
		}
		msgs = append(msgs, mj)
	}
	out := struct {
		ConversationID string              `json:"conversation_id,omitempty"`
		Label          string              `json:"label,omitempty"`
		Count          int                 `json:"count"`
		Messages       []threadMessageJSON `json:"messages"`
	}{Label: res.Label, Count: len(msgs), Messages: msgs}
	if res.Conversation != nil {
		out.ConversationID = res.Conversation.ConversationID
	}
	return enc.Encode(out)
}

// RunThreads handles "openmessage threads [--limit N] [--json]": a quick list of
// the most recent conversations across all platforms, newest activity first.
// It is the companion lookup for "thread" — run it to find a conversation_id or
// the exact name to pass. Read-only; no Full Disk Access required.
func RunThreads(logger zerolog.Logger, args ...string) error {
	limit := 30
	if v := flagValue(args, "--limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	asJSON := hasFlag(args, "--json")

	a, err := app.New(logger)
	if err != nil {
		return fmt.Errorf("init app: %w", err)
	}
	defer a.Close()

	convs, err := a.Store.ListConversations(limit)
	if err != nil {
		return fmt.Errorf("list conversations: %w", err)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(convs)
	}

	if len(convs) == 0 {
		fmt.Println("No conversations stored yet.")
		return nil
	}

	fmt.Printf("%d most recent conversation(s):\n\n", len(convs))
	for _, c := range convs {
		name := firstNonEmpty(c.Name, c.Participants, "(no name)")
		platform := ""
		if c.SourcePlatform != "" && c.SourcePlatform != "sms" {
			platform = " [" + c.SourcePlatform + "]"
		}
		fmt.Printf("  %-14s  %s%s  (last: %s)\n",
			c.ConversationID, name, platform, fmtTS(c.LastMessageTS))
	}
	fmt.Printf("\nRead one with: openmessage thread <conversation_id>\n")
	return nil
}
