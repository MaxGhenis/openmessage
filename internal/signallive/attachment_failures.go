package signallive

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
)

// ErrAttachmentNotStored reports that signal-cli's local attachment store has
// no file for the requested attachment id. getAttachment only reads files
// that `receive` already downloaded. signal-cli downloads each attachment
// once, while it handles the envelope, and when that download fails it logs
// "Failed to download attachment (<id>), ignoring", still prints and
// acknowledges the envelope, and keeps no pointer (CDN key, key, digest) it
// could fetch the attachment with later (AsamK/signal-cli#2116). Retrying
// getAttachment cannot succeed until the file is put back by hand, for
// example from a backup taken before a re-link cleared the store.
var ErrAttachmentNotStored = errors.New("signal-cli has not stored this attachment")

// maxAttachmentFailureText bounds the cause kept for one failed download: an
// exception line plus its "Caused by:" chain, never its stack frames.
const maxAttachmentFailureText = 600

// signalAttachmentDownloadFailure matches the WARN signal-cli's
// AttachmentHelper logs when a receive-time download fails. Up to 0.14.8 the
// exception message follows on the same line ("ignoring: <message>"); from
// 0.14.9 the line ends at "ignoring" and logback prints the exception on the
// lines after it.
var signalAttachmentDownloadFailure = regexp.MustCompile(`Failed to download attachment \(([^)]*)\), ignoring(?::\s*(.*))?$`)

// signalLogLevelPrefix matches the start of a signal-cli log line in its
// default "%-5level %logger{0} - %msg" layout and its verbose layout.
var signalLogLevelPrefix = regexp.MustCompile(`^(?:\S+ \[[^\]]*\] )?(?:TRACE|DEBUG|INFO|WARN|ERROR)\s`)

// attachmentDownloadFailures maps a signal-cli attachment remote id (as its
// AttachmentStore sanitizes it) to the failure signal-cli logged for it.
type attachmentDownloadFailures map[string]string

// parseAttachmentDownloadFailures collects the attachment downloads that
// failed during one signal-cli receive call from the call's combined output.
// The receive loop otherwise drops every line that is not an envelope, and
// those lines are the only record of why an attachment never reached
// signal-cli's store.
func parseAttachmentDownloadFailures(output []byte) attachmentDownloadFailures {
	if !bytes.Contains(output, []byte("Failed to download attachment (")) {
		return nil
	}
	lines := strings.Split(string(output), "\n")
	failures := attachmentDownloadFailures{}
	for i := 0; i < len(lines); i++ {
		line := sanitizeSignalOutput(lines[i])
		match := signalAttachmentDownloadFailure.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		remoteID := sanitizeSignalAttachmentID(strings.TrimSpace(match[1]))
		if remoteID == "" {
			continue
		}
		causes := []string{}
		if inline := strings.TrimSpace(match[2]); inline != "" {
			causes = append(causes, inline)
		}
		// Collect the exception logback appends: its first line and every
		// "Caused by:" line, skipping stack frames, until the next log line or
		// envelope.
		for i+1 < len(lines) {
			next := sanitizeSignalOutput(lines[i+1])
			if next == "" {
				i++
				continue
			}
			if strings.HasPrefix(next, "{") || signalLogLevelPrefix.MatchString(next) {
				break
			}
			i++
			switch {
			case strings.HasPrefix(next, "at "), strings.HasPrefix(next, "..."):
				continue
			case strings.HasPrefix(next, "Caused by: "):
				causes = append(causes, strings.TrimSpace(strings.TrimPrefix(next, "Caused by: ")))
			case len(causes) == 0:
				causes = append(causes, next)
			}
		}
		cause := strings.Join(uniqueStrings(causes), " <- ")
		if cause == "" {
			cause = "signal-cli reported no cause"
		}
		if len(cause) > maxAttachmentFailureText {
			cause = cause[:maxAttachmentFailureText] + "…"
		}
		failures[remoteID] = cause
	}
	if len(failures) == 0 {
		return nil
	}
	return failures
}

// forEnvelope returns the failures that belong to env's attachments, keyed by
// the attachment id exactly as the envelope line carries it. signal-cli names
// a stored attachment, and reports its JSON id, as the sanitized remote id
// plus at most one extension guessed from the file name or content type, so
// a failure logged for remote id R belongs to the attachment whose id is R or
// R.<ext>.
func (f attachmentDownloadFailures) forEnvelope(env *signalEnvelope) map[string]string {
	if len(f) == 0 || env == nil {
		return nil
	}
	var matched map[string]string
	for _, id := range envelopeAttachmentIDs(env) {
		cause, ok := f[id]
		if !ok {
			if dot := strings.LastIndexByte(id, '.'); dot > 0 {
				cause, ok = f[id[:dot]]
			}
		}
		if !ok {
			continue
		}
		if matched == nil {
			matched = map[string]string{}
		}
		matched[id] = cause
	}
	return matched
}

// envelopeAttachmentIDs lists the attachment ids an envelope carries in any
// of the places the decoders read attachments from.
func envelopeAttachmentIDs(env *signalEnvelope) []string {
	var ids []string
	add := func(attachments []signalAttachment) {
		for _, attachment := range attachments {
			if id := strings.TrimSpace(attachment.ID); id != "" {
				ids = append(ids, id)
			}
		}
	}
	if env.DataMessage != nil {
		add(env.DataMessage.Attachments)
	}
	if env.EditMessage != nil && env.EditMessage.DataMessage != nil {
		add(env.EditMessage.DataMessage.Attachments)
	}
	if env.SyncMessage != nil && env.SyncMessage.SentMessage != nil {
		sent := env.SyncMessage.SentMessage
		add(sent.Attachments)
		if sent.EditMessage != nil && sent.EditMessage.DataMessage != nil {
			add(sent.EditMessage.DataMessage.Attachments)
		}
	}
	return ids
}

// sanitizeSignalAttachmentID mirrors signal-cli's AttachmentStore.sanitizeId,
// which turns every character outside [A-Za-z0-9_.-] into '_' before using a
// remote id as a file name.
func sanitizeSignalAttachmentID(id string) string {
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// isSignalAttachmentNotStored reports whether a getAttachment failure is
// signal-cli saying the file is absent from its store (GetAttachmentCommand
// turns FileNotFoundException into "Could not find attachment with ID: <id>").
func isSignalAttachmentNotStored(output []byte) bool {
	return bytes.Contains(bytes.ToLower(output), []byte("could not find attachment with id"))
}

// signalAttachmentPayload returns the base64 text getAttachment printed. The
// command's output is combined with stderr, where signal-cli can log an
// account-state line (for example "Messages have been last received N days
// ago") before the payload, so log lines are skipped rather than decoded.
func signalAttachmentPayload(output []byte) string {
	lines := strings.Split(string(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(strings.ReplaceAll(lines[i], "\r", ""))
		if line == "" || signalLogLevelPrefix.MatchString(line) {
			continue
		}
		return line
	}
	return ""
}
