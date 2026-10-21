package signallive_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/bridge"
	signaladapter "github.com/maxghenis/openmessage/internal/bridgeadapters/signal"
	"github.com/maxghenis/openmessage/internal/ingest"
	"github.com/maxghenis/openmessage/internal/media"
	"github.com/maxghenis/openmessage/internal/messaging"
	"github.com/maxghenis/openmessage/internal/signallive"
	"github.com/maxghenis/openmessage/internal/storage/blob"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

// The live failure, 2026-10-21, with synthetic ids, names and content: a
// sender's linked device sends an HTML file. signal-cli 0.14.9's receive
// starts the download, the download fails, and signal-cli logs the failure,
// prints the envelope anyway and acknowledges it. A second envelope in the
// same call carries an attachment that downloaded fine.
const (
	notStoredSender       = "0d6c1f9e-3b8a-4c2e-9f71-5a4e2b8c7d10"
	notStoredConversation = "signal:" + notStoredSender
	notStoredAttachmentID = "Q3x9KpLmN2vB7cRtYw4z.html"
	storedAttachmentID    = "Zp0aB1cD2eF3gH4iJ5kL.png"
	notStoredSentAt       = int64(1_792_622_101_057)
	notStoredDeclaredSize = 85841
)

func notStoredReceiveOutput() []byte {
	return []byte(strings.Join([]string{
		"WARN  AttachmentHelper - Failed to download attachment (Q3x9KpLmN2vB7cRtYw4z), ignoring",
		"java.io.IOException: Connection reset",
		"\tat org.whispersystems.signalservice.internal.push.PushServiceSocket.retrieveAttachment(PushServiceSocket.java:1301)",
		"\tat org.asamk.signal.manager.helper.AttachmentHelper.retrieveAttachmentAsStream(AttachmentHelper.java:318)",
		"Caused by: java.net.SocketException: Connection reset",
		"\tat java.base/sun.nio.ch.NioSocketImpl.implRead(NioSocketImpl.java:318)",
		"\t... 14 common frames omitted",
		fmt.Sprintf(
			`{"envelope":{"source":%q,"sourceNumber":null,"sourceUuid":%q,"sourceName":"Sam Example","sourceDevice":2,"timestamp":%d,"serverReceivedTimestamp":%d,"serverDeliveredTimestamp":%d,"dataMessage":{"timestamp":%d,"message":"the spec is attached","expiresInSeconds":0,"isExpirationUpdate":false,"viewOnce":false,"attachments":[{"contentType":"text/html","filename":"Quarterly Report.html","id":%q,"size":%d,"width":null,"height":null,"caption":null,"uploadTimestamp":%d,"isVoiceNote":false}]}},"account":%q}`,
			notStoredSender, notStoredSender, notStoredSentAt, notStoredSentAt+108126, notStoredSentAt+108140,
			notStoredSentAt, notStoredAttachmentID, notStoredDeclaredSize, notStoredSentAt+57, quoteDiffAccount,
		),
		fmt.Sprintf(
			`{"envelope":{"source":%q,"sourceNumber":null,"sourceUuid":%q,"sourceName":"Sam Example","sourceDevice":2,"timestamp":%d,"dataMessage":{"timestamp":%d,"message":"and a picture","attachments":[{"contentType":"image/png","filename":null,"id":%q,"size":34061}]}},"account":%q}`,
			notStoredSender, notStoredSender, notStoredSentAt+5000, notStoredSentAt+5000, storedAttachmentID, quoteDiffAccount,
		),
		"",
	}, "\n"))
}

// notStoredSignalCLI stands in for signal-cli after that receive: its store
// has the PNG but not the HTML file, so getAttachment for the HTML fails the
// way signal-cli 0.14.9 does.
type notStoredSignalCLI struct {
	mu             sync.Mutex
	getAttachments [][]string
}

func installNotStoredSignalCLI(t *testing.T, stored map[string][]byte) *notStoredSignalCLI {
	t.Helper()
	cli := &notStoredSignalCLI{}
	restore := signallive.SetRunSignalCLIForTest(func(_ context.Context, _ string, args ...string) ([]byte, error) {
		cli.mu.Lock()
		defer cli.mu.Unlock()
		switch {
		case slices.Contains(args, "getAttachment"):
			cli.getAttachments = append(cli.getAttachments, slices.Clone(args))
			id := args[slices.Index(args, "--id")+1]
			if data, ok := stored[id]; ok {
				return []byte(base64.StdEncoding.EncodeToString(data)), nil
			}
			return []byte("Could not find attachment with ID: " + id + "\n"), errors.New("exit status 1")
		case slices.Contains(args, "listContacts"), slices.Contains(args, "listGroups"):
			return []byte("[]"), nil
		}
		return nil, fmt.Errorf("unexpected signal-cli call %q", args)
	})
	t.Cleanup(restore)
	return cli
}

func (c *notStoredSignalCLI) calls() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.getAttachments)
}

// TestSignalAttachmentSignalCLINeverStoredIsGoneNotTransient reproduces the
// 2026-10-21 failure end to end: the receive output above goes through the
// live capture path, the durable frame, the v2 decoder and the ingest
// worker; the media service then asks the real Signal adapter for the HTML
// attachment, and signal-cli answers "Could not find attachment with ID".
//
// Before the fix the attachment was recorded with size 0 and no cause, and
// every download attempt came back as a transient OpError (served as a 500)
// that the next request retried forever. Now the row carries the declared
// size and signal-cli's receive-time cause, the download reports
// media.ErrGone with that cause, and a copy of the file obtained elsewhere
// can be imported once.
func TestSignalAttachmentSignalCLINeverStoredIsGoneNotTransient(t *testing.T) {
	pngBytes := []byte("\x89PNG\r\n\x1a\nstored by signal-cli")
	cli := installNotStoredSignalCLI(t, map[string][]byte{storedAttachmentID: pngBytes})
	dir := t.TempDir()
	legacy := openQuoteDiffLegacy(t, filepath.Join(dir, "legacy.sqlite3"))
	v2Store, messages, sink, worker := openQuoteDiffV2(t, filepath.Join(dir, "v2.sqlite3"))

	live := signallive.NewConnectedBridgeForTest(legacy, t.TempDir(), quoteDiffAccount, nil)
	var captureErrs []error
	// The same capture -> frame step the Signal adapter's observer runs.
	unregister := live.ObserveIngressLine(func(captured signallive.IngressLine) {
		record, ephemeral, err := ingest.BuildSignalIngressWithAttachmentErrors(
			quoteDiffAccountID, 1, captured.Account, captured.Line,
			captured.ResolvedSource, captured.ResolvedDestination,
			captured.AttachmentErrors, time.Now(),
		)
		if err == nil && ephemeral == nil && record != nil {
			err = sink.AppendIngress(context.Background(), *record)
		}
		if err != nil {
			captureErrs = append(captureErrs, err)
		}
	})
	defer unregister()
	if err := signallive.HandleReceiveOutputForTest(live, quoteDiffAccount, notStoredReceiveOutput()); err != nil {
		t.Fatalf("handle receive output: %v", err)
	}
	if len(captureErrs) != 0 {
		t.Fatalf("capture errors = %v", captureErrs)
	}
	drainQuoteDiffWorker(t, worker, messages)

	conversation, err := v2Store.GetConversationByRemote(quoteDiffAccountID, notStoredConversation)
	if err != nil {
		t.Fatalf("GetConversationByRemote(%q): %v", notStoredConversation, err)
	}
	rows, err := messages.ListMessagesByConversation(context.Background(), conversation.ConversationID, 0, "", 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("v2 messages = %+v, %v; want the two ingested messages", rows, err)
	}
	var htmlMessage, pngMessage sqlite.Message
	for _, row := range rows {
		if strings.Contains(row.Body, "spec") {
			htmlMessage = row
		} else {
			pngMessage = row
		}
	}

	attachments, err := sqlite.NewMessageAttachmentRepository(v2Store, time.Now)
	if err != nil {
		t.Fatalf("NewMessageAttachmentRepository(): %v", err)
	}
	htmlRow, err := attachments.GetForDownload(context.Background(), htmlMessage.MessageID, 0)
	if err != nil {
		t.Fatalf("GetForDownload(html): %v", err)
	}
	if htmlRow.State != "pending" || htmlRow.SizeBytes == nil || *htmlRow.SizeBytes != notStoredDeclaredSize {
		t.Fatalf("html attachment = %+v, want pending with the declared %d bytes", htmlRow, notStoredDeclaredSize)
	}
	wantCause := "receive: java.io.IOException: Connection reset <- java.net.SocketException: Connection reset"
	if htmlRow.LastError == nil || *htmlRow.LastError != wantCause {
		t.Fatalf("html last_error = %v, want %q", htmlRow.LastError, wantCause)
	}
	pngRow, err := attachments.GetForDownload(context.Background(), pngMessage.MessageID, 0)
	if err != nil {
		t.Fatalf("GetForDownload(png): %v", err)
	}
	if pngRow.LastError != nil {
		t.Fatalf("png attachment last_error = %q, want none: its download did not fail", *pngRow.LastError)
	}

	registry := bridge.NewRegistry()
	if err := registry.Register(signaladapter.New(quoteDiffAccountID, live)); err != nil {
		t.Fatalf("Register(Signal adapter): %v", err)
	}
	blobs, err := blob.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob.New(): %v", err)
	}
	service, err := media.NewService(v2Store, registry, blobs, messaging.SystemClock{})
	if err != nil {
		t.Fatalf("media.NewService(): %v", err)
	}
	htmlCommand := media.DownloadCommand{AccountID: quoteDiffAccountID, MessageID: htmlMessage.MessageID}

	for attempt := 1; attempt <= 2; attempt++ {
		_, err = service.Resolve(context.Background(), htmlCommand)
		if !errors.Is(err, media.ErrGone) || !errors.Is(err, bridge.ErrMediaGone) {
			t.Fatalf("Resolve(html) attempt %d error = %v, want media.ErrGone", attempt, err)
		}
		var opErr bridge.OpError
		if errors.As(err, &opErr) {
			t.Fatalf("Resolve(html) attempt %d returned OpError %+v; a missing signal-cli file is not a transport failure to retry", attempt, opErr)
		}
		for _, want := range []string{
			"signal-cli did not store attachment " + notStoredAttachmentID,
			"Could not find attachment with ID: " + notStoredAttachmentID,
			"when the message arrived: java.io.IOException: Connection reset",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("Resolve(html) error %q does not mention %q", err, want)
			}
		}
		after, err := attachments.GetForDownload(context.Background(), htmlMessage.MessageID, 0)
		if err != nil {
			t.Fatalf("GetForDownload(html) after attempt %d: %v", attempt, err)
		}
		if after.State != "pending" || after.LastError == nil || *after.LastError != wantCause {
			t.Fatalf("html attachment after attempt %d = %+v, want pending with the receive-time cause kept", attempt, after)
		}
	}
	wantArgv := []string{"-a", quoteDiffAccount, "getAttachment", "--id", notStoredAttachmentID, "--recipient", notStoredSender}
	if calls := cli.calls(); len(calls) != 2 || !slices.Equal(calls[0], wantArgv) {
		t.Fatalf("getAttachment calls = %q, want two of %q", calls, wantArgv)
	}

	pngDescriptor, err := service.Resolve(context.Background(), media.DownloadCommand{
		AccountID: quoteDiffAccountID, MessageID: pngMessage.MessageID,
	})
	if err != nil {
		t.Fatalf("Resolve(png): %v", err)
	}
	if pngDescriptor.Size != int64(len(pngBytes)) {
		t.Fatalf("png descriptor = %+v, want the %d stored bytes", pngDescriptor, len(pngBytes))
	}

	// The recovery: a copy saved from another client of the account.
	copyBytes := bytes.Repeat([]byte("<p>x</p>"), notStoredDeclaredSize/8)
	copyBytes = append(copyBytes, bytes.Repeat([]byte(" "), notStoredDeclaredSize-len(copyBytes))...)
	sum := sha256.Sum256(copyBytes)
	digest := hex.EncodeToString(sum[:])
	if _, _, err := service.Import(context.Background(), media.ImportCommand{
		AccountID: quoteDiffAccountID, MessageID: htmlMessage.MessageID,
		Content: bytes.NewReader(copyBytes[:len(copyBytes)-1]),
	}); !errors.Is(err, media.ErrImportMismatch) {
		t.Fatalf("Import(truncated copy) error = %v, want ErrImportMismatch against the declared size", err)
	}
	imported, fresh, err := service.Import(context.Background(), media.ImportCommand{
		AccountID: quoteDiffAccountID, MessageID: htmlMessage.MessageID,
		Content: bytes.NewReader(copyBytes), SHA256: digest,
	})
	if err != nil || !fresh {
		t.Fatalf("Import(copy) = %+v, imported=%v, %v", imported, fresh, err)
	}
	served, err := service.Resolve(context.Background(), htmlCommand)
	if err != nil {
		t.Fatalf("Resolve(html) after import: %v", err)
	}
	if served.Ref.Hash != digest || served.Size != notStoredDeclaredSize ||
		served.MIME != "application/octet-stream" || served.Disposition != media.DispositionForcedDownload {
		t.Fatalf("served html = %+v, want hash %s, %d bytes, forced download", served, digest, notStoredDeclaredSize)
	}
	reader, err := service.Open(served.Ref)
	if err != nil {
		t.Fatalf("Open(html): %v", err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, copyBytes) {
		t.Fatalf("served html bytes differ from the imported copy (err %v)", err)
	}
	if calls := cli.calls(); len(calls) != 3 {
		t.Fatalf("getAttachment calls after import = %d, want 3 (none for the downloaded HTML)", len(calls))
	}
}
