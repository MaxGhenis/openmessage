package db

import (
	"encoding/json"
	"sort"
	"strings"
)

// NativeReaction is one reaction row from Messages' chat.db: an
// associated_message_type of 2000–2006 (add) or 3000–3006 (remove) aimed at
// another message. Unlike an SMS/RCS tapback, it carries its kind, actor and
// target natively, so nothing is matched by quoted text.
type NativeReaction struct {
	Date  int64 // chat.db message.date
	RowID int64 // chat.db message.ROWID, which breaks ties in Date
	Add   bool
	Kind  int    // associated_message_type % 1000: 0 love … 5 question, 6 custom emoji
	Emoji string // for Kind 6, the custom emoji (associated_message_emoji)
	Actor string // "me", else the sender's handle
}

// nativeTapbackEmoji is the emoji for kinds 0–5, the same ones the SMS/RCS
// tapback text patterns map to.
var nativeTapbackEmoji = [...]string{"❤️", "👍", "👎", "😂", "‼️", "❓"}

// NativeTapbackKind maps an associated_message_type to its reaction: kind,
// whether it adds, and its emoji. ok is false for every other type,
// including 0 (an ordinary message) and stickers (1000, 2007, 3007), and for
// kind 6 without an emoji, which nothing could display.
func NativeTapbackKind(assocType int64, emoji string) (kind int, add bool, shown string, ok bool) {
	switch {
	case assocType >= 2000 && assocType <= 2006:
		kind, add = int(assocType-2000), true
	case assocType >= 3000 && assocType <= 3006:
		kind = int(assocType - 3000)
	default:
		return 0, false, "", false
	}
	if kind < len(nativeTapbackEmoji) {
		return kind, add, nativeTapbackEmoji[kind], true
	}
	shown = strings.TrimSpace(emoji)
	if shown == "" && add {
		return 0, false, "", false
	}
	return kind, add, shown, true
}

// IsNativeTapbackType reports whether a chat.db associated_message_type is a
// reaction (2000–2006 or 3000–3006). Such a row is never stored as a message.
func IsNativeTapbackType(assocType int64) bool {
	return (assocType >= 2000 && assocType <= 2006) || (assocType >= 3000 && assocType <= 3006)
}

// NativeReactionsJSON folds every reaction aimed at one message into the
// stored reactions JSON ([{emoji,count,actors}], see tapbackReaction), or ""
// when none is active.
//
// It is a pure function of the set of events: they are replayed in
// (Date, RowID) order whatever order they arrive in, so a reaction synced
// late with an older date leaves the same state as a full import. Messages
// lets each person add one Tapback per message and replaces it when they
// choose another (Apple's Messages User Guide, "React with Tapbacks"), so
// each actor holds at most one reaction: an add replaces the actor's current
// one, and a removal clears it only when it is of the removed kind (for kind
// 6, also the same emoji, unless the removal names none). Emojis are listed
// by their earliest active add, actors by their add.
func NativeReactionsJSON(events []NativeReaction) string {
	if len(events) == 0 {
		return ""
	}
	ordered := append([]NativeReaction(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Date != ordered[j].Date {
			return ordered[i].Date < ordered[j].Date
		}
		return ordered[i].RowID < ordered[j].RowID
	})
	active := map[string]NativeReaction{}
	for _, e := range ordered {
		if e.Add {
			active[e.Actor] = e
			continue
		}
		cur, ok := active[e.Actor]
		if !ok || cur.Kind != e.Kind {
			continue
		}
		if e.Kind == len(nativeTapbackEmoji) && e.Emoji != "" && e.Emoji != cur.Emoji {
			continue
		}
		delete(active, e.Actor)
	}
	if len(active) == 0 {
		return ""
	}
	adds := make([]NativeReaction, 0, len(active))
	for _, e := range active {
		adds = append(adds, e)
	}
	sort.Slice(adds, func(i, j int) bool {
		if adds[i].Date != adds[j].Date {
			return adds[i].Date < adds[j].Date
		}
		if adds[i].RowID != adds[j].RowID {
			return adds[i].RowID < adds[j].RowID
		}
		return adds[i].Actor < adds[j].Actor
	})
	var out []tapbackReaction
	index := map[string]int{}
	for _, e := range adds {
		emoji := e.Emoji
		if e.Kind < len(nativeTapbackEmoji) {
			emoji = nativeTapbackEmoji[e.Kind]
		}
		i, ok := index[emoji]
		if !ok {
			i = len(out)
			index[emoji] = i
			out = append(out, tapbackReaction{Emoji: emoji})
		}
		out[i].Actors = append(out[i].Actors, e.Actor)
		out[i].Count++
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}
