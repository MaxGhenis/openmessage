package db

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Tapback is an iMessage-style reaction that arrived as SMS/RCS text, e.g.
// `Loved "see you then"`. iPhones send these as plain messages to Android, and
// we convert them into an emoji reaction on the message they refer to.
type Tapback struct {
	Emoji     string
	Quoted    string // the referenced message text (may be truncated with an ellipsis)
	Remove    bool   // true when the reaction was removed
	Truncated bool   // the quoted text ended with an ellipsis (iMessage truncates long quotes)
}

type tapbackPattern struct {
	re     *regexp.Regexp
	emoji  string
	remove bool
}

// Quote class matches both straight ("...") and iMessage's curly (“...”) quotes.
const tbQuoteOpen = "[\"“]"
const tbQuoteClose = "[\"”]"

func tbVerb(verb string) *regexp.Regexp {
	return regexp.MustCompile("(?s)^" + verb + " " + tbQuoteOpen + "(.+)" + tbQuoteClose + "$")
}

var tapbackPatterns = []tapbackPattern{
	{tbVerb("Loved"), "❤️", false},
	{tbVerb("Liked"), "👍", false},
	{tbVerb("Disliked"), "👎", false},
	{tbVerb("Laughed at"), "😂", false},
	{tbVerb("Emphasized"), "‼️", false},
	{tbVerb("Questioned"), "❓", false},
	{tbVerb("Removed a heart from"), "❤️", true},
	{tbVerb("Removed a like from"), "👍", true},
	{tbVerb("Removed a dislike from"), "👎", true},
	{tbVerb("Removed a laugh from"), "😂", true},
	{tbVerb("Removed an exclamation from"), "‼️", true},
	{tbVerb("Removed a question mark from"), "❓", true},
}

// Newer iOS sends the actual emoji: `Reacted 🎉 to "we did it"`.
var reactedPattern = regexp.MustCompile("(?s)^Reacted (.+?) to " + tbQuoteOpen + "(.+)" + tbQuoteClose + "$")

// ParseTapback detects an iMessage tapback in a message body and returns the
// emoji, the quoted (referenced) text, and whether it was a removal.
func ParseTapback(body string) (*Tapback, bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, false
	}
	for _, p := range tapbackPatterns {
		if m := p.re.FindStringSubmatch(body); m != nil {
			q := strings.TrimSpace(m[1])
			return &Tapback{Emoji: p.emoji, Quoted: q, Remove: p.remove, Truncated: isTruncatedQuote(q)}, true
		}
	}
	if m := reactedPattern.FindStringSubmatch(body); m != nil {
		q := strings.TrimSpace(m[2])
		return &Tapback{Emoji: strings.TrimSpace(m[1]), Quoted: q, Truncated: isTruncatedQuote(q)}, true
	}
	return nil, false
}

// isTruncatedQuote reports whether a tapback's quoted text was cut off by
// iMessage (which appends an ellipsis to long quotes). Only truncated quotes
// are matched by prefix; exact quotes must match a message in full.
func isTruncatedQuote(quoted string) bool {
	q := strings.TrimRight(quoted, " ")
	return strings.HasSuffix(q, "…") || strings.HasSuffix(q, "...")
}

// ApplyTapback detects whether m is a tapback and, if so, applies it as an
// emoji reaction on the message it refers to (instead of a standalone text).
// It returns true when handled — the caller should then NOT store m as a
// regular message. It returns false (so the caller stores m normally) when m
// isn't a tapback or no matching target message can be found.
func (s *Store) ApplyTapback(m *Message) (bool, error) {
	if m == nil {
		return false, nil
	}
	tb, ok := ParseTapback(m.Body)
	if !ok {
		return false, nil
	}
	target, err := s.findTapbackTarget(m.ConversationID, tb.Quoted, m.TimestampMS, tb.Truncated)
	if err != nil {
		return false, err
	}
	if target == nil {
		return false, nil
	}
	actor := strings.TrimSpace(m.SenderNumber)
	if m.IsFromMe {
		actor = "me"
	} else if actor == "" {
		actor = strings.TrimSpace(m.SenderName)
	}
	next, changed, err := mergeReaction(target.Reactions, tb.Emoji, actor, tb.Remove)
	if err != nil {
		return false, err
	}
	if changed {
		if _, err := s.db.Exec(`UPDATE messages SET reactions = ? WHERE message_id = ?`, next, target.MessageID); err != nil {
			return false, err
		}
	}
	return true, nil
}

// findTapbackTarget locates the message a tapback refers to: the most recent
// message in the conversation (at or before the tapback) whose body equals the
// quoted text, or — for truncated quotes — starts with it.
func (s *Store) findTapbackTarget(conversationID, quoted string, beforeTS int64, truncated bool) (*Message, error) {
	quoted = strings.TrimSpace(quoted)
	// iMessage truncates long quotes with an ellipsis; for those, strip the
	// trailing ellipsis/period so the remaining text can prefix-match the
	// original. A non-truncated quote must match the body exactly, including a
	// trailing period — otherwise a tapback on `see you then.` leaks through as
	// the literal text `Loved "see you then."` instead of a reaction.
	if truncated {
		quoted = strings.TrimRight(quoted, ". …")
	}
	if quoted == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT message_id, body, reactions FROM messages
		 WHERE conversation_id = ? AND timestamp_ms <= ? AND TRIM(body) != ''
		 ORDER BY timestamp_ms DESC LIMIT 200`,
		conversationID, beforeTS,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, body, reactions string
		if err := rows.Scan(&id, &body, &reactions); err != nil {
			return nil, err
		}
		b := strings.TrimSpace(body)
		// Require an exact match for non-truncated quotes; only iMessage-
		// truncated quotes (ellipsis) may match by prefix. This avoids silently
		// converting a real typed message like `Loved "hello"` into a reaction
		// just because some earlier message happened to start with "hello".
		if b == quoted || (truncated && strings.HasPrefix(b, quoted)) {
			return &Message{MessageID: id, Body: body, Reactions: reactions}, nil
		}
	}
	return nil, rows.Err()
}

type tapbackReaction struct {
	Emoji  string   `json:"emoji"`
	Count  int      `json:"count"`
	Actors []string `json:"actors,omitempty"`
}

// mergeReaction adds or removes an actor's reaction for an emoji in the stored
// reactions JSON ([{emoji,count,actors}]). It's idempotent: re-adding the same
// actor/emoji is a no-op.
func mergeReaction(existingJSON, emoji, actor string, remove bool) (string, bool, error) {
	existingJSON = strings.TrimSpace(existingJSON)
	var reactions []tapbackReaction
	if existingJSON != "" && existingJSON != "null" {
		_ = json.Unmarshal([]byte(existingJSON), &reactions)
	}
	changed := false
	if remove {
		for i := range reactions {
			if reactions[i].Emoji != emoji {
				continue
			}
			if idx := indexOfString(reactions[i].Actors, actor); idx >= 0 {
				reactions[i].Actors = append(reactions[i].Actors[:idx], reactions[i].Actors[idx+1:]...)
				if reactions[i].Count > 0 {
					reactions[i].Count--
				}
				changed = true
			}
		}
	} else {
		found := false
		for i := range reactions {
			if reactions[i].Emoji != emoji {
				continue
			}
			found = true
			if indexOfString(reactions[i].Actors, actor) < 0 {
				reactions[i].Actors = append(reactions[i].Actors, actor)
				reactions[i].Count++
				changed = true
			}
			break
		}
		if !found {
			reactions = append(reactions, tapbackReaction{Emoji: emoji, Count: 1, Actors: []string{actor}})
			changed = true
		}
	}
	if !changed {
		return existingJSON, false, nil
	}
	compacted := make([]tapbackReaction, 0, len(reactions))
	for _, r := range reactions {
		if r.Count > 0 && strings.TrimSpace(r.Emoji) != "" {
			compacted = append(compacted, r)
		}
	}
	if len(compacted) == 0 {
		return "", true, nil
	}
	b, err := json.Marshal(compacted)
	if err != nil {
		return existingJSON, false, err
	}
	return string(b), true, nil
}

// RepairTapbacks converts already-stored tapback text messages into reactions
// on their referenced messages and removes the standalone tapback rows. Returns
// the number converted.
func (s *Store) RepairTapbacks() (int, error) {
	rows, err := s.db.Query(`
		SELECT message_id, conversation_id, sender_name, sender_number, body, timestamp_ms, is_from_me
		FROM messages
		WHERE body LIKE 'Loved %' OR body LIKE 'Liked %' OR body LIKE 'Disliked %'
		   OR body LIKE 'Laughed at %' OR body LIKE 'Emphasized %' OR body LIKE 'Questioned %'
		   OR body LIKE 'Removed a %' OR body LIKE 'Reacted %'
	`)
	if err != nil {
		return 0, err
	}
	var candidates []*Message
	for rows.Next() {
		m := &Message{}
		if err := rows.Scan(&m.MessageID, &m.ConversationID, &m.SenderName, &m.SenderNumber, &m.Body, &m.TimestampMS, &m.IsFromMe); err != nil {
			rows.Close()
			return 0, err
		}
		if _, ok := ParseTapback(m.Body); ok {
			candidates = append(candidates, m)
		}
	}
	rows.Close()

	count := 0
	for _, m := range candidates {
		applied, err := s.ApplyTapback(m)
		if err != nil || !applied {
			continue
		}
		if err := s.DeleteMessageByID(m.MessageID); err == nil {
			count++
		}
	}
	return count, nil
}

func indexOfString(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return -1
}
