package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func goneTransportError() error {
	return fmt.Errorf("%w: Signal never delivered attachment remote-gone.html: signal-cli could not download it when the message arrived and cannot fetch it again; the sender can send it again: %w",
		bridge.ErrMediaGone, errors.New("download Signal attachment: exit status 1: Could not find attachment with ID: remote-gone.html"))
}

func TestResolveGoneTransportMarksTheAttachmentUnavailableForGood(t *testing.T) {
	env := newMediaTestEnvironment(t)
	seedMediaMessage(t, env, "message-gone", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-gone.html", Filename: "gone.html", MIME: "text/html",
	}})
	downloader := &scriptedDownloader{steps: []downloadStep{{err: goneTransportError()}}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	command := DownloadCommand{AccountID: "account-1", MessageID: "message-gone"}

	_, err := service.Resolve(context.Background(), command)
	if !errors.Is(err, ErrGone) || !errors.Is(err, bridge.ErrMediaGone) {
		t.Fatalf("Resolve() error = %v, want ErrGone from the transport", err)
	}
	assertPendingWithoutBlob(t, env, "message-gone", 0)
	lastError := attachmentLastError(t, env.dbPath, "message-gone", 0)
	if !lastError.Valid || !strings.HasPrefix(lastError.String, sqlite.UnavailableAttachmentErrorPrefix) ||
		!strings.Contains(lastError.String, "Could not find attachment with ID") {
		t.Fatalf("last_error = %+v, want the unavailable marker and the transport's reason", lastError)
	}
	reason, unavailable := getMediaAttachment(t, env, "message-gone", 0).UnavailableReason()
	if !unavailable || !strings.Contains(reason, "Could not find attachment with ID") {
		t.Fatalf("UnavailableReason() = %q, %v", reason, unavailable)
	}

	// Terminal: later requests answer from the row; the transport is not asked.
	for attempt := 2; attempt <= 3; attempt++ {
		_, err = service.Resolve(context.Background(), command)
		if !errors.Is(err, ErrGone) || !strings.Contains(err.Error(), "Could not find attachment with ID") {
			t.Fatalf("Resolve() attempt %d error = %v, want ErrGone with the stored reason", attempt, err)
		}
	}
	if downloader.requestCount() != 1 {
		t.Fatalf("transport calls = %d, want 1: an unavailable attachment is not re-fetched", downloader.requestCount())
	}

	// An import still fills it.
	content := []byte("<html>resent</html>")
	if _, imported, err := service.Import(context.Background(), importCommand("message-gone", content, "")); err != nil || !imported {
		t.Fatalf("Import(unavailable attachment) = imported=%v, %v", imported, err)
	}
	if descriptor, err := service.Resolve(context.Background(), command); err != nil || descriptor.Ref.Hash != sha256Hex(content) {
		t.Fatalf("Resolve() after import = %+v, %v", descriptor, err)
	}
	if _, unavailable := getMediaAttachment(t, env, "message-gone", 0).UnavailableReason(); unavailable {
		t.Fatal("an imported attachment still reads as unavailable")
	}
}

func TestResolveGoneTransportKeepsReceiveTimeCause(t *testing.T) {
	env := newMediaTestEnvironment(t)
	cause := sqlite.ReceiveAttachmentErrorPrefix + "java.io.IOException: Connection reset"
	seedMediaMessage(t, env, "message-gone-cause", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-gone.html", Filename: "gone.html", MIME: "text/html", LastError: &cause,
	}})
	// Already unavailable as projected: the transport reported the failure
	// with the message.
	if reason, unavailable := getMediaAttachment(t, env, "message-gone-cause", 0).UnavailableReason(); !unavailable ||
		reason != "java.io.IOException: Connection reset" {
		t.Fatalf("UnavailableReason() before any download = %q, %v", reason, unavailable)
	}
	downloader := &scriptedDownloader{steps: []downloadStep{{err: goneTransportError()}}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	command := DownloadCommand{AccountID: "account-1", MessageID: "message-gone-cause"}

	for attempt := 1; attempt <= 2; attempt++ {
		_, err := service.Resolve(context.Background(), command)
		if !errors.Is(err, ErrGone) {
			t.Fatalf("Resolve() attempt %d error = %v, want ErrGone", attempt, err)
		}
		if !strings.HasSuffix(err.Error(), "(when the message arrived: java.io.IOException: Connection reset)") ||
			strings.Count(err.Error(), "(when the message arrived:") != 1 {
			t.Fatalf("Resolve() attempt %d error = %q, want the receive-time cause appended once", attempt, err)
		}
		got := attachmentLastError(t, env.dbPath, "message-gone-cause", 0)
		if !got.Valid || !strings.HasPrefix(got.String, sqlite.UnavailableAttachmentErrorPrefix) ||
			!strings.HasSuffix(got.String, "(when the message arrived: java.io.IOException: Connection reset)") {
			t.Fatalf("last_error after attempt %d = %+v, want unavailable with the receive-time cause", attempt, got)
		}
	}
	if downloader.requestCount() != 1 {
		t.Fatalf("transport calls = %d, want only the first request to ask the transport", downloader.requestCount())
	}
}

func TestResolveTransientFailureKeepsTheReceiveTimeCause(t *testing.T) {
	env := newMediaTestEnvironment(t)
	cause := sqlite.ReceiveAttachmentErrorPrefix + "java.io.IOException: Connection reset"
	seedMediaMessage(t, env, "message-offline", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-offline.html", MIME: "text/html", LastError: &cause,
	}})
	offline := bridge.OpError{Class: bridge.FailureTransient, Operation: "download_media", Cause: errors.New("Signal is not connected")}
	downloader := &scriptedDownloader{steps: []downloadStep{{err: offline}, {err: goneTransportError()}}}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	command := DownloadCommand{AccountID: "account-1", MessageID: "message-offline"}

	if _, err := service.Resolve(context.Background(), command); errors.Is(err, ErrGone) || err == nil {
		t.Fatalf("Resolve(offline) error = %v, want the transient failure", err)
	}
	if got := attachmentLastError(t, env.dbPath, "message-offline", 0); !got.Valid || got.String != cause {
		t.Fatalf("last_error after a transient failure = %+v, want the receive-time cause kept", got)
	}
	if _, err := service.Resolve(context.Background(), command); !errors.Is(err, ErrGone) {
		t.Fatalf("Resolve(back online) error = %v, want ErrGone", err)
	}
}

func importCommand(messageID string, content []byte, sha string) ImportCommand {
	return ImportCommand{AccountID: "account-1", MessageID: messageID, Content: bytes.NewReader(content), SHA256: sha}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestImportFillsPendingAttachmentAndServesIt(t *testing.T) {
	env := newMediaTestEnvironment(t)
	size := int64(len("<html>copy</html>"))
	seedMediaMessage(t, env, "message-import", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-import.html", Filename: "Report.html", MIME: "text/html", SizeBytes: &size,
	}})
	downloader := &scriptedDownloader{}
	service := newMediaTestService(t, env, availableDownloadRegistry(downloader))
	content := []byte("<html>copy</html>")

	descriptor, imported, err := service.Import(context.Background(), importCommand("message-import", content, sha256Hex(content)))
	if err != nil || !imported {
		t.Fatalf("Import() = %+v, %v, %v; want imported", descriptor, imported, err)
	}
	if descriptor.Ref.Hash != sha256Hex(content) || descriptor.Size != size ||
		descriptor.Filename != "Report.html" || descriptor.Disposition != DispositionForcedDownload {
		t.Fatalf("Import() descriptor = %+v", descriptor)
	}
	row := getMediaAttachment(t, env, "message-import", 0)
	if row.State != "downloaded" || row.BlobHash == nil || *row.BlobHash != sha256Hex(content) || row.MIME != "text/html" {
		t.Fatalf("row after import = %+v, want downloaded with the content hash and the transport's MIME", row)
	}
	resolved, err := service.Resolve(context.Background(), DownloadCommand{AccountID: "account-1", MessageID: "message-import"})
	if err != nil || resolved.Ref.Hash != descriptor.Ref.Hash {
		t.Fatalf("Resolve() after import = %+v, %v", resolved, err)
	}
	if downloader.requestCount() != 0 {
		t.Fatalf("transport calls = %d, want none for an imported attachment", downloader.requestCount())
	}

	again, imported, err := service.Import(context.Background(), importCommand("message-import", content, ""))
	if err != nil || imported || again.Ref.Hash != descriptor.Ref.Hash {
		t.Fatalf("Import(same bytes again) = %+v, imported=%v, %v; want idempotent success", again, imported, err)
	}
	if _, _, err := service.Import(context.Background(), importCommand("message-import", []byte("<html>othr</html>"), "")); !errors.Is(err, ErrImportConflict) {
		t.Fatalf("Import(different bytes) error = %v, want ErrImportConflict", err)
	}
}

func TestImportRejectsMismatchesWithoutStoringABlob(t *testing.T) {
	env := newMediaTestEnvironment(t)
	size := int64(10)
	seedMediaMessage(t, env, "message-sized", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-sized", MIME: "application/pdf", SizeBytes: &size,
	}})
	zero := int64(0)
	seedMediaMessage(t, env, "message-unsized", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-unsized", MIME: "application/pdf", SizeBytes: &zero,
	}})
	service := newMediaTestService(t, env, availableDownloadRegistry(&scriptedDownloader{}))
	service.maxBytes = 64

	cases := []struct {
		name    string
		command ImportCommand
		want    error
	}{
		{"declared size", importCommand("message-sized", []byte("nine byte"), ""), ErrImportMismatch},
		{"digest", importCommand("message-sized", []byte("ten bytes!"), strings.Repeat("0", 64)), ErrImportMismatch},
		{"too large", importCommand("message-unsized", bytes.Repeat([]byte("x"), 65), ""), ErrTooLarge},
		{"unknown message", importCommand("message-missing", []byte("x"), ""), ErrNotFound},
		{"unknown ordinal", ImportCommand{AccountID: "account-1", MessageID: "message-sized", Ordinal: 1, Content: strings.NewReader("x")}, ErrNotFound},
		{"other account", ImportCommand{AccountID: "account-2", MessageID: "message-sized", Content: strings.NewReader("ten bytes!")}, ErrNotFound},
	}
	for _, tc := range cases {
		if _, _, err := service.Import(context.Background(), tc.command); !errors.Is(err, tc.want) {
			t.Errorf("Import(%s) error = %v, want %v", tc.name, err, tc.want)
		}
	}
	if files := blobFiles(t, env.blobRoot); len(files) != 0 {
		t.Fatalf("rejected imports left blobs %v", files)
	}
	assertPendingWithoutBlob(t, env, "message-sized", 0)

	// An attachment whose transport declared no size accepts any size up to
	// the limit.
	if _, imported, err := service.Import(context.Background(), importCommand("message-unsized", []byte("any length"), "")); err != nil || !imported {
		t.Fatalf("Import(unsized) = imported=%v, %v", imported, err)
	}
}

// Property: for any content and declared size, Import succeeds exactly when
// the size is undeclared or matches, the served bytes then equal the
// imported ones, and a second import of the same bytes is a no-op success.
func TestImportRoundTripProperty(t *testing.T) {
	property := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		env := newMediaTestEnvironment(t)
		content := make([]byte, rng.Intn(2048))
		rng.Read(content)
		declared := int64(len(content))
		switch rng.Intn(3) {
		case 0:
			declared = 0
		case 1:
			declared += int64(1 + rng.Intn(5))
		}
		seedMediaMessage(t, env, "message-property", []sqlite.MessageAttachment{{
			Ordinal: 0, RemoteID: "remote-property", MIME: "application/octet-stream", SizeBytes: &declared,
		}})
		service := newMediaTestService(t, env, availableDownloadRegistry(&scriptedDownloader{}))
		_, imported, err := service.Import(context.Background(), importCommand("message-property", content, ""))
		accept := declared == 0 || declared == int64(len(content))
		if !accept {
			return errors.Is(err, ErrImportMismatch) && len(blobFiles(t, env.blobRoot)) == 0
		}
		if err != nil || !imported {
			return false
		}
		descriptor, err := service.Resolve(context.Background(), DownloadCommand{AccountID: "account-1", MessageID: "message-property"})
		if err != nil {
			return false
		}
		reader, err := service.Open(descriptor.Ref)
		if err != nil {
			return false
		}
		served, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil || !bytes.Equal(served, content) {
			return false
		}
		_, again, err := service.Import(context.Background(), importCommand("message-property", content, sha256Hex(content)))
		return err == nil && !again
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 60, Rand: rand.New(rand.NewSource(20261021))}); err != nil {
		t.Fatal(err)
	}
}

func TestImportRejectsEmptyContentAndReplacesOnlyWhenAsked(t *testing.T) {
	env := newMediaTestEnvironment(t)
	zero := int64(0)
	seedMediaMessage(t, env, "message-undeclared", []sqlite.MessageAttachment{{
		Ordinal: 0, RemoteID: "remote-undeclared", MIME: "text/html", SizeBytes: &zero,
	}})
	service := newMediaTestService(t, env, availableDownloadRegistry(&scriptedDownloader{}))

	if _, _, err := service.Import(context.Background(), importCommand("message-undeclared", nil, "")); !errors.Is(err, ErrImportMismatch) {
		t.Fatalf("Import(empty) error = %v, want ErrImportMismatch", err)
	}
	assertPendingWithoutBlob(t, env, "message-undeclared", 0)

	wrong := []byte("<html>the wrong file</html>")
	if _, imported, err := service.Import(context.Background(), importCommand("message-undeclared", wrong, "")); err != nil || !imported {
		t.Fatalf("Import(wrong file) = imported=%v, %v", imported, err)
	}
	right := []byte("<html>the right file, a little longer</html>")
	if _, _, err := service.Import(context.Background(), importCommand("message-undeclared", right, "")); !errors.Is(err, ErrImportConflict) {
		t.Fatalf("Import(different bytes, no replace) error = %v, want ErrImportConflict", err)
	}
	replace := importCommand("message-undeclared", right, sha256Hex(right))
	replace.Replace = true
	descriptor, imported, err := service.Import(context.Background(), replace)
	if err != nil || !imported || descriptor.Ref.Hash != sha256Hex(right) || descriptor.Size != int64(len(right)) {
		t.Fatalf("Import(replace) = %+v, imported=%v, %v; want the right file", descriptor, imported, err)
	}
	if row := getMediaAttachment(t, env, "message-undeclared", 0); row.State != "downloaded" || *row.BlobHash != sha256Hex(right) {
		t.Fatalf("row after replace = %+v", row)
	}
	// Replacing with the bytes it already holds is still a no-op success.
	replace.Content = bytes.NewReader(right)
	if _, imported, err := service.Import(context.Background(), replace); err != nil || imported {
		t.Fatalf("Import(replace with same bytes) = imported=%v, %v; want a no-op", imported, err)
	}
}
