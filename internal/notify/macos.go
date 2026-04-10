package notify

import (
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
)

const notificationHistoryCap = 256

type commandRunner func(name string, args ...string) error

type MacOSNotifier struct {
	logger               zerolog.Logger
	enabled              bool
	store                *db.Store
	mentionNames         []string
	run                  commandRunner
	baseURL              string
	terminalNotifierPath string

	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
}

func NewMacOSNotifier(logger zerolog.Logger, enabled bool, baseURL string, store *db.Store, identityName string) *MacOSNotifier {
	notifier := &MacOSNotifier{
		logger:       logger,
		enabled:      enabled,
		store:        store,
		mentionNames: mentionNamesForIdentity(identityName),
		run: func(name string, args ...string) error {
			return exec.Command(name, args...).Run()
		},
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		seen:    make(map[string]struct{}),
	}
	if enabled {
		if path, err := exec.LookPath("terminal-notifier"); err == nil {
			notifier.terminalNotifierPath = path
		}
	}
	return notifier
}

func (n *MacOSNotifier) Enabled() bool {
	return n != nil && n.enabled
}

func (n *MacOSNotifier) NotifyIncomingMessage(message *db.Message) {
	if n == nil || !n.enabled || message == nil || message.IsFromMe {
		return
	}
	if !n.notificationAllowed(message) {
		return
	}
	messageID := strings.TrimSpace(message.MessageID)
	if messageID == "" || !n.remember(messageID) {
		return
	}

	title := strings.TrimSpace(message.SenderName)
	if title == "" {
		title = strings.TrimSpace(message.SenderNumber)
	}
	if title == "" {
		title = "OpenMessage"
	}

	body := strings.TrimSpace(message.Body)
	if body == "" {
		if message.MediaID != "" || message.MimeType != "" {
			body = "Sent an attachment"
		} else {
			body = "New message"
		}
	}

	go func() {
		if err := n.notify(title, body, messageID, message.ConversationID); err != nil {
			n.logger.Debug().Err(err).Str("msg_id", messageID).Msg("macOS notification failed")
		}
	}()
}

func (n *MacOSNotifier) notificationAllowed(message *db.Message) bool {
	if n == nil || message == nil || n.store == nil {
		return true
	}
	conversation, err := n.store.GetConversation(message.ConversationID)
	if err != nil || conversation == nil {
		return true
	}
	switch conversation.NotificationMode {
	case db.NotificationModeMuted:
		return false
	case db.NotificationModeMentions:
		return message.MentionsMe || bodyMentionsAnyName(message.Body, n.mentionNames)
	default:
		return true
	}
}

func (n *MacOSNotifier) notify(title, body, messageID, conversationID string) error {
	if n.terminalNotifierPath != "" {
		args := []string{
			"-title", title,
			"-subtitle", "OpenMessage",
			"-message", body,
			"-group", "openmessage:" + messageID,
		}
		if openURL := n.openURL(conversationID); openURL != "" {
			args = append(args, "-open", openURL)
		}
		return n.run(n.terminalNotifierPath, args...)
	}
	return n.run("osascript", "-e", appleScriptNotification(title, body))
}

func (n *MacOSNotifier) openURL(conversationID string) string {
	if n == nil || n.baseURL == "" {
		return ""
	}
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return n.baseURL + "/"
	}
	return n.baseURL + "/?conversation=" + url.QueryEscape(conversationID)
}

func (n *MacOSNotifier) remember(messageID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	if _, exists := n.seen[messageID]; exists {
		return false
	}
	n.seen[messageID] = struct{}{}
	n.order = append(n.order, messageID)
	if len(n.order) > notificationHistoryCap {
		evicted := n.order[0]
		n.order = n.order[1:]
		delete(n.seen, evicted)
	}
	return true
}

func appleScriptNotification(title, body string) string {
	return fmt.Sprintf(
		"display notification %s with title %s",
		appleScriptString(body),
		appleScriptString(title),
	)
}

func appleScriptString(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(value) + `"`
}

func mentionNamesForIdentity(identity string) []string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return nil
	}
	seen := map[string]struct{}{}
	var names []string
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if len(candidate) < 3 {
			return
		}
		key := strings.ToLower(candidate)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		names = append(names, candidate)
	}

	add(identity)
	fields := strings.Fields(identity)
	if len(fields) > 0 {
		add(fields[0])
	}
	return names
}

func bodyMentionsAnyName(body string, names []string) bool {
	body = strings.TrimSpace(body)
	if body == "" || len(names) == 0 {
		return false
	}
	for _, name := range names {
		pattern := `(?i)(^|[^a-z0-9])` + regexp.QuoteMeta(name) + `([^a-z0-9]|$)`
		if matched, _ := regexp.MatchString(pattern, body); matched {
			return true
		}
	}
	return false
}
