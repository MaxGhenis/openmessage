package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/readsource"
)

type resolvedRoute struct {
	Conversation conversationSummary `json:"conversation"`
	Sendable     bool                `json:"sendable"`
}

type resolvedRouteMatch struct {
	MatchID                      string          `json:"match_id"`
	Kind                         string          `json:"kind"`
	DisplayName                  string          `json:"display_name"`
	UnifiedID                    string          `json:"unified_id,omitempty"`
	ParticipantID                string          `json:"participant_id,omitempty"`
	PreferredReplyConversationID string          `json:"preferred_reply_conversation_id,omitempty"`
	RouteCount                   int             `json:"route_count"`
	Routes                       []resolvedRoute `json:"routes"`
}

type routeIdentity struct {
	ID   string
	Name string
}

type routeIdentifier struct {
	Platform string `json:"platform"`
	Value    string `json:"value"`
}

type routeParticipant struct {
	Name      string `json:"name"`
	Number    string `json:"number"`
	Phone     string `json:"phone"`
	Email     string `json:"email"`
	ID        string `json:"id"`
	IsMe      bool   `json:"is_me"`
	IsMeCamel bool   `json:"isMe"`
}

func resolveContactRoutesTool() mcp.Tool {
	return mcp.NewTool("resolve_contact_routes",
		mcp.WithDescription("Resolve a person, phone number, or conversation name to existing conversation routes and the preferred reply route"),
		mcp.WithString("query", mcp.Required(), mcp.Description("Person name, phone number, email, or existing conversation name/ID")),
		mcp.WithNumber("limit", mcp.Description("Maximum route matches to return (default 10)")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
	)
}

// resolveContactRoutesHandler finds routes in the serving store, so every
// conversation ID it returns is one the send tools accept. On a v2-primary
// install that is the v2 store. The legacy store files Google conversations
// under the phone's thread numbers, which are device-local: a phone change
// re-numbers them (issue #177), and the v2 outbox knows none of them, so a
// legacy ID either fails the send (HTTP 404) or, aliased, could name another
// person's thread. The legacy store still says who a name or number belongs
// to (the address book and unified contacts), never which thread to use.
func resolveContactRoutesHandler(a *app.App, configured ...Options) server.ToolHandlerFunc {
	options := resolvedOptions(a, configured)
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		query := strings.TrimSpace(strArg(args, "query"))
		limit := intArg(args, "limit", 10)

		if query == "" {
			return errorResult("query is required"), nil
		}
		if limit <= 0 {
			limit = 10
		}

		contacts, err := a.Store.ListContacts("", 1)
		if err == nil && len(contacts) == 0 && a.GetClient() != nil {
			if err := fetchAndCacheContacts(a); err != nil {
				a.Logger.Warn().Err(err).Msg("Failed to fetch contacts from phone")
			}
		}

		convos, err := findRouteConversations(options.Reads, a.Store, query, limit*8)
		if err != nil {
			return errorResult(fmt.Sprintf("resolve routes: %v", err)), nil
		}

		var matches []resolvedRouteMatch
		if len(convos) > 0 {
			whatsAppConnected, signalConnected := routePlatformsConnected(ctx, a, options)
			matches = buildResolvedRouteMatches(a.Store, convos, limit, whatsAppConnected, signalConnected)
		}
		if len(matches) == 0 {
			return structuredResult(map[string]any{
				"query":   query,
				"count":   0,
				"matches": []resolvedRouteMatch{},
			}, fmt.Sprintf("No conversation routes found for %q.", query)), nil
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%d route matches for %q:\n\n", len(matches), query)
		for _, match := range matches {
			fmt.Fprintf(&sb, "- %s: ", match.DisplayName)
			parts := make([]string, 0, len(match.Routes))
			for _, route := range match.Routes {
				label := route.Conversation.SourcePlatform
				if route.Conversation.ConversationID == match.PreferredReplyConversationID {
					label += " (preferred)"
				} else if !route.Sendable {
					label += " (history)"
				}
				parts = append(parts, label)
			}
			sb.WriteString(strings.Join(parts, ", "))
			sb.WriteByte('\n')
		}

		return structuredResult(map[string]any{
			"query":   query,
			"count":   len(matches),
			"matches": matches,
		}, sb.String()), nil
	}
}

// findRouteConversations returns the query's conversations from reads, the
// serving store. people supplies the numbers of matching address-book
// contacts and of matching participants in its own conversations; it then
// searches reads for those numbers, never for a people-store conversation ID.
func findRouteConversations(reads readsource.ReadSource, people *db.Store, query string, limit int) ([]*db.Conversation, error) {
	if limit <= 0 {
		limit = 20
	}

	seen := make(map[string]*db.Conversation)
	addConversation := func(conv *db.Conversation) {
		if conv == nil {
			return
		}
		seen[conv.ConversationID] = conv
	}
	addConversations := func(convos []*db.Conversation) {
		for _, conv := range convos {
			addConversation(conv)
		}
	}

	if conv, err := reads.GetConversation(query); err == nil && conv != nil {
		addConversation(conv)
	}

	convos, err := reads.SearchConversationsByMetadata(query, limit)
	if err != nil {
		return nil, err
	}
	addConversations(convos)

	contacts, err := people.ListContacts(query, limit)
	if err == nil {
		for _, contact := range contacts {
			if contact == nil {
				continue
			}
			if number := strings.TrimSpace(contact.Number); number != "" {
				numberConvos, err := reads.SearchConversationsByMetadata(number, limit)
				if err == nil {
					addConversations(numberConvos)
				}
			}
		}
	}

	if len(seen) == 0 {
		fallbackContacts, err := people.ListContactsFromConversations(query, limit)
		if err == nil {
			for _, contact := range fallbackContacts {
				if contact == nil {
					continue
				}
				if number := strings.TrimSpace(contact.Number); number != "" {
					numberConvos, err := reads.SearchConversationsByMetadata(number, limit)
					if err == nil {
						addConversations(numberConvos)
					}
				}
			}
		}
	}

	results := make([]*db.Conversation, 0, len(seen))
	for _, conv := range seen {
		results = append(results, conv)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].LastMessageTS != results[j].LastMessageTS {
			return results[i].LastMessageTS > results[j].LastMessageTS
		}
		return results[i].ConversationID < results[j].ConversationID
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func buildResolvedRouteMatches(people *db.Store, convos []*db.Conversation, limit int, whatsAppConnected, signalConnected bool) []resolvedRouteMatch {
	if len(convos) == 0 {
		return nil
	}

	identityIndex := loadRouteIdentityIndex(people)

	type routeBucket struct {
		MatchID       string
		Kind          string
		DisplayName   string
		UnifiedID     string
		ParticipantID string
		Routes        []resolvedRoute
	}

	buckets := map[string]*routeBucket{}
	for _, conv := range convos {
		if conv == nil {
			continue
		}
		matchID, kind, displayName, unifiedID, participantID := resolveRouteBucketIdentity(conv, identityIndex)
		bucket := buckets[matchID]
		if bucket == nil {
			bucket = &routeBucket{
				MatchID:       matchID,
				Kind:          kind,
				DisplayName:   displayName,
				UnifiedID:     unifiedID,
				ParticipantID: participantID,
			}
			buckets[matchID] = bucket
		}
		if bucket.DisplayName == "" {
			bucket.DisplayName = displayName
		}
		if bucket.UnifiedID == "" {
			bucket.UnifiedID = unifiedID
		}
		if bucket.ParticipantID == "" {
			bucket.ParticipantID = participantID
		}
		bucket.Routes = append(bucket.Routes, resolvedRoute{
			Conversation: summarizeConversation(conv),
			Sendable:     routeSupportsOutbound(conv, whatsAppConnected, signalConnected),
		})
	}

	matches := make([]resolvedRouteMatch, 0, len(buckets))
	for _, bucket := range buckets {
		sortResolvedRoutes(bucket.Routes)
		match := resolvedRouteMatch{
			MatchID:       bucket.MatchID,
			Kind:          bucket.Kind,
			DisplayName:   firstNonEmpty(bucket.DisplayName, bucket.ParticipantID, bucket.MatchID),
			UnifiedID:     bucket.UnifiedID,
			ParticipantID: bucket.ParticipantID,
			RouteCount:    len(bucket.Routes),
			Routes:        bucket.Routes,
		}
		match.PreferredReplyConversationID = preferredReplyConversationID(bucket.Routes)
		matches = append(matches, match)
	}

	sort.Slice(matches, func(i, j int) bool {
		iTS := newestRouteTimestamp(matches[i].Routes)
		jTS := newestRouteTimestamp(matches[j].Routes)
		if iTS != jTS {
			return iTS > jTS
		}
		iSendable := hasSendableRoute(matches[i].Routes)
		jSendable := hasSendableRoute(matches[j].Routes)
		if iSendable != jSendable {
			return iSendable
		}
		if matches[i].RouteCount != matches[j].RouteCount {
			return matches[i].RouteCount > matches[j].RouteCount
		}
		return matches[i].DisplayName < matches[j].DisplayName
	})

	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

func resolveRouteBucketIdentity(conv *db.Conversation, identityIndex map[string]routeIdentity) (matchID, kind, displayName, unifiedID, participantID string) {
	if conv == nil {
		return "", "conversation", "", "", ""
	}
	if conv.IsGroup {
		return "conversation:" + conv.ConversationID, "conversation", conversationName(conv), "", ""
	}

	displayName = conversationName(conv)
	participantID = primaryRouteParticipantID(conv)
	if participantID == "" {
		return "conversation:" + conv.ConversationID, "conversation", displayName, "", ""
	}

	if identity, ok := identityIndex[routeIdentityKey(conv.SourcePlatform, participantID)]; ok {
		displayName = firstNonEmpty(identity.Name, displayName, participantID)
		return "unified:" + identity.ID, "person", displayName, identity.ID, participantID
	}

	return "participant:" + normalizeRouteIdentifier(participantID), "person", firstNonEmpty(displayName, participantID), "", participantID
}

// routePlatformsConnected reports whether WhatsApp and Signal can send now. In
// client mode this process holds no transports, so the running app answers.
// Asking this process instead would build its own bridges, which opens the
// app's WhatsApp session store and starts Signal temp-dir sweeps, only to
// report bridges that are not connected. An app that does not answer sends
// nothing.
func routePlatformsConnected(ctx context.Context, a *app.App, options Options) (whatsApp, signal bool) {
	if options.Daemon != nil {
		raw, err := options.Daemon.RawStatus(ctx)
		if err != nil {
			return false, false
		}
		return daemonPlatformConnected(raw, "whatsapp"), daemonPlatformConnected(raw, "signal")
	}
	return whatsAppStatus(a).Connected, signalStatus(a).Connected
}

func daemonPlatformConnected(raw map[string]any, key string) bool {
	platform, _ := raw[key].(map[string]any)
	connected, _ := platform["connected"].(bool)
	return connected
}

func preferredReplyConversationID(routes []resolvedRoute) string {
	for _, route := range routes {
		if route.Sendable && route.Conversation.SourcePlatform == "sms" {
			return route.Conversation.ConversationID
		}
	}
	for _, route := range routes {
		if route.Sendable {
			return route.Conversation.ConversationID
		}
	}
	return ""
}

func sortResolvedRoutes(routes []resolvedRoute) {
	sort.Slice(routes, func(i, j int) bool {
		ai := platformOrderIndex(routes[i].Conversation.SourcePlatform)
		bi := platformOrderIndex(routes[j].Conversation.SourcePlatform)
		if ai != bi {
			return ai < bi
		}
		if routes[i].Conversation.LastMessageTS != routes[j].Conversation.LastMessageTS {
			return routes[i].Conversation.LastMessageTS > routes[j].Conversation.LastMessageTS
		}
		return routes[i].Conversation.ConversationID < routes[j].Conversation.ConversationID
	})
}

func platformOrderIndex(platform string) int {
	switch normalizedPlatform(platform) {
	case "sms":
		return 0
	case "whatsapp":
		return 1
	case "imessage":
		return 2
	case "gchat":
		return 3
	case "signal":
		return 4
	case "telegram":
		return 5
	default:
		return 100
	}
}

func routeSupportsOutbound(conv *db.Conversation, whatsAppConnected, signalConnected bool) bool {
	if conv == nil {
		return false
	}
	switch normalizedPlatform(conv.SourcePlatform) {
	case "sms":
		return true
	case "whatsapp":
		return whatsAppConnected
	case "signal":
		return signalConnected
	default:
		return false
	}
}

func hasSendableRoute(routes []resolvedRoute) bool {
	for _, route := range routes {
		if route.Sendable {
			return true
		}
	}
	return false
}

func newestRouteTimestamp(routes []resolvedRoute) int64 {
	var latest int64
	for _, route := range routes {
		if route.Conversation.LastMessageTS > latest {
			latest = route.Conversation.LastMessageTS
		}
	}
	return latest
}

func loadRouteIdentityIndex(store *db.Store) map[string]routeIdentity {
	contacts, err := store.ListUnifiedContacts("", 10000)
	if err != nil || len(contacts) == 0 {
		return nil
	}
	index := make(map[string]routeIdentity)
	for _, contact := range contacts {
		if contact == nil || strings.TrimSpace(contact.UnifiedID) == "" {
			continue
		}
		var identifiers []routeIdentifier
		if err := json.Unmarshal([]byte(contact.Identifiers), &identifiers); err != nil {
			continue
		}
		identity := routeIdentity{
			ID:   strings.TrimSpace(contact.UnifiedID),
			Name: strings.TrimSpace(contact.DisplayName),
		}
		for _, identifier := range identifiers {
			key := routeIdentityKey(identifier.Platform, identifier.Value)
			if key != "" {
				index[key] = identity
			}
		}
	}
	return index
}

func primaryRouteParticipantID(conv *db.Conversation) string {
	if conv == nil || strings.TrimSpace(conv.Participants) == "" {
		return ""
	}
	var participants []routeParticipant
	if err := json.Unmarshal([]byte(conv.Participants), &participants); err != nil {
		return ""
	}
	for _, participant := range participants {
		if participant.IsMe || participant.IsMeCamel {
			continue
		}
		if id := routeParticipantIdentifier(participant); id != "" {
			return id
		}
	}
	if len(participants) == 0 {
		return ""
	}
	return routeParticipantIdentifier(participants[0])
}

func routeParticipantIdentifier(participant routeParticipant) string {
	for _, value := range []string{participant.Number, participant.Phone, participant.Email, participant.ID} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func routeIdentityKey(platform, value string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	value = normalizeRouteIdentifier(value)
	if platform == "" || value == "" {
		return ""
	}
	return platform + "\x00" + value
}

func normalizeRouteIdentifier(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	digits := make([]byte, 0, len(value))
	phoneLike := true
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= '0' && c <= '9':
			digits = append(digits, c)
		case c == '+' || c == '(' || c == ')' || c == '-' || c == '.' || c == ' ':
		default:
			phoneLike = false
		}
	}
	if phoneLike && len(digits) >= 7 {
		return string(digits)
	}
	return value
}
