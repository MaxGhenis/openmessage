package db

import (
	"encoding/json"
	"sort"
	"strings"
)

// Person aggregates a single human you've messaged, merged across their 1:1
// conversations (and platforms) by normalized name.
type Person struct {
	Key             string   `json:"key"`
	Name            string   `json:"name"`
	Numbers         []string `json:"numbers"`
	Platforms       []string `json:"platforms"`
	LastContactedTS int64    `json:"last_contacted_ts"`
	MessageCount    int      `json:"message_count"`
	ConversationIDs []string `json:"-"`
}

// realMessageCountsByConversation counts real (non-system, content-bearing)
// messages per conversation in a single query, mirroring story.FilterRealMessages
// closely enough for list ordering.
func (s *Store) realMessageCountsByConversation() (map[string]int, error) {
	rows, err := s.db.Query(`
		SELECT conversation_id, COUNT(*) FROM messages
		WHERE (TRIM(body) != '' OR TRIM(COALESCE(media_id,'')) != '')
		  AND lower(body) NOT LIKE 'rcs chat with%'
		  AND lower(body) NOT LIKE 'this rcs chat is now%'
		  AND lower(body) NOT LIKE 'you created this rcs%'
		  AND lower(body) NOT LIKE 'you started an rcs%'
		  AND lower(body) NOT LIKE 'this chat is now%'
		  AND lower(body) NOT LIKE 'sms/mms with%'
		GROUP BY conversation_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ListMessagedPeople returns everyone with 1:1 message history, grouped by
// normalized name and ordered by most recently contacted.
func (s *Store) ListMessagedPeople() ([]*Person, error) {
	convs, err := s.ListConversations(5000)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*Person{}
	var order []string
	for _, c := range convs {
		if c.IsGroup {
			continue
		}
		name := strings.TrimSpace(c.Name)
		nums := participantNumbers(c.Participants)
		if name == "" {
			if len(nums) > 0 {
				name = nums[0]
			} else {
				continue
			}
		}
		key := PersonKey(name)
		if key == "" {
			continue
		}
		p := byKey[key]
		if p == nil {
			p = &Person{Key: key, Name: name}
			byKey[key] = p
			order = append(order, key)
		}
		for _, n := range nums {
			p.Numbers = appendUniqueStr(p.Numbers, n)
		}
		p.Platforms = appendUniqueStr(p.Platforms, platformOrSMS(c.SourcePlatform))
		if c.LastMessageTS > p.LastContactedTS {
			p.LastContactedTS = c.LastMessageTS
		}
		p.ConversationIDs = append(p.ConversationIDs, c.ConversationID)
	}
	people := make([]*Person, 0, len(order))
	for _, k := range order {
		people = append(people, byKey[k])
	}
	if counts, err := s.realMessageCountsByConversation(); err == nil {
		for _, p := range people {
			total := 0
			for _, cid := range p.ConversationIDs {
				total += counts[cid]
			}
			p.MessageCount = total
		}
	}
	sort.SliceStable(people, func(i, j int) bool {
		return people[i].LastContactedTS > people[j].LastContactedTS
	})
	return people, nil
}

// PersonByKey resolves a normalized person key back to the aggregated person.
func (s *Store) PersonByKey(key string) (*Person, error) {
	people, err := s.ListMessagedPeople()
	if err != nil {
		return nil, err
	}
	for _, p := range people {
		if p.Key == key {
			return p, nil
		}
	}
	return nil, nil
}

// PersonMessages returns the deduplicated messages across a person's
// conversations, ordered chronologically.
func (s *Store) PersonMessages(conversationIDs []string) ([]*Message, error) {
	if len(conversationIDs) == 0 {
		return nil, nil
	}
	msgs, err := s.GetMessagesByConversations(conversationIDs, 500000)
	if err != nil {
		return nil, err
	}
	return dedupePersonMessages(msgs), nil
}

func participantNumbers(participantsJSON string) []string {
	participantsJSON = strings.TrimSpace(participantsJSON)
	if participantsJSON == "" || participantsJSON == "[]" {
		return nil
	}
	var ps []struct {
		Number string `json:"number"`
		IsMe   bool   `json:"is_me"`
	}
	if err := json.Unmarshal([]byte(participantsJSON), &ps); err != nil {
		return nil
	}
	var out []string
	for _, p := range ps {
		if p.IsMe {
			continue
		}
		if n := strings.TrimSpace(p.Number); n != "" {
			out = appendUniqueStr(out, n)
		}
	}
	return out
}

func platformOrSMS(p string) string {
	if strings.TrimSpace(p) == "" {
		return "sms"
	}
	return p
}

func appendUniqueStr(list []string, v string) []string {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return list
		}
	}
	return append(list, v)
}

// dedupePersonMessages removes near-duplicate cross-platform messages (same body
// + sender within 2s), keeping the first occurrence.
func dedupePersonMessages(msgs []*Message) []*Message {
	if len(msgs) <= 1 {
		return msgs
	}
	sort.SliceStable(msgs, func(i, j int) bool {
		return msgs[i].TimestampMS < msgs[j].TimestampMS
	})
	var result []*Message
	for _, m := range msgs {
		dup := false
		start := len(result) - 20
		if start < 0 {
			start = 0
		}
		for i := len(result) - 1; i >= start; i-- {
			prev := result[i]
			diff := m.TimestampMS - prev.TimestampMS
			if diff > 2000 {
				break
			}
			if diff >= 0 && m.Body == prev.Body && m.IsFromMe == prev.IsFromMe {
				dup = true
				break
			}
		}
		if !dup {
			result = append(result, m)
		}
	}
	return result
}
