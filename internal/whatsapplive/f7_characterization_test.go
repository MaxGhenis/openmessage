package whatsapplive

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	wastore "go.mau.fi/whatsmeow/store"
	watypes "go.mau.fi/whatsmeow/types"
	waevents "go.mau.fi/whatsmeow/types/events"

	"github.com/maxghenis/openmessage/internal/db"
)

func TestPairPhoneProtocolFacts(t *testing.T) {
	if pairPhoneDisplayName != "Chrome (macOS)" {
		t.Fatalf("pairPhoneDisplayName = %q, want Chrome (macOS)", pairPhoneDisplayName)
	}

	// PairPhone is a concrete whatsmeow method with no test seam, so inspect its
	// call structurally. These literals guard the Wave 1 (C5) pairing refactor.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "client.go", nil, 0)
	if err != nil {
		t.Fatalf("parse client.go: %v", err)
	}

	var calls []*ast.CallExpr
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "PairPhone" || fn.Body == nil {
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		receiverPointer, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		receiverType, ok := receiverPointer.X.(*ast.Ident)
		if !ok || receiverType.Name != "Bridge" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "PairPhone" {
				return true
			}
			receiver, ok := selector.X.(*ast.Ident)
			if ok && receiver.Name == "cli" {
				calls = append(calls, call)
			}
			return true
		})
	}
	if len(calls) != 1 {
		t.Fatalf("found %d cli.PairPhone calls, want 1", len(calls))
	}
	args := calls[0].Args
	if len(args) != 5 {
		t.Fatalf("cli.PairPhone argument count = %d, want 5", len(args))
	}
	showPush, ok := args[2].(*ast.Ident)
	if !ok || showPush.Name != "true" {
		t.Fatalf("cli.PairPhone show-push-notification argument = %#v, want true", args[2])
	}
	clientType, ok := args[3].(*ast.SelectorExpr)
	if !ok || clientType.Sel.Name != "PairClientChrome" {
		t.Fatalf("cli.PairPhone client type = %#v, want whatsmeow.PairClientChrome", args[3])
	}
	clientPackage, ok := clientType.X.(*ast.Ident)
	if !ok || clientPackage.Name != "whatsmeow" {
		t.Fatalf("cli.PairPhone client type package = %#v, want whatsmeow", clientType.X)
	}
	displayName, ok := args[4].(*ast.Ident)
	if !ok || displayName.Name != "pairPhoneDisplayName" {
		t.Fatalf("cli.PairPhone display-name argument = %#v, want pairPhoneDisplayName", args[4])
	}
}

func TestHandleConnectedKeepsUnpairedTransportInPairingState(t *testing.T) {
	statusChanges := 0
	bridge := &Bridge{
		client:     &whatsmeow.Client{Store: &wastore.Device{}},
		connecting: true,
		pairing:    true,
		qr: QRSnapshot{
			Event: "code",
			Code:  "pairing-code",
		},
		callbacks: Callbacks{
			OnStatusChange: func() { statusChanges++ },
		},
	}

	// An unpaired Connected event is only pairing transport; C5 must not
	// promote it to an online account or hide the active pairing affordance.
	bridge.handleEvent(&waevents.Connected{})

	bridge.mu.RLock()
	internallyConnected := bridge.connected
	bridge.mu.RUnlock()
	if internallyConnected {
		t.Fatal("unpaired Connected event set the bridge's internal connected flag")
	}
	status := bridge.Status()
	if status.Paired {
		t.Fatal("unpaired Connected event unexpectedly reported a paired account")
	}
	if status.Connected {
		t.Fatal("unpaired Connected event unexpectedly reported an online account")
	}
	if status.Connecting {
		t.Fatal("pairing transport should no longer report connecting")
	}
	if !status.Pairing {
		t.Fatal("expected pairing to remain active until PairSuccess")
	}
	if !status.QRAvailable || status.QREvent != "code" {
		t.Fatalf("pairing QR was not retained: %+v", status)
	}
	if statusChanges != 1 {
		t.Fatalf("status changes = %d, want 1", statusChanges)
	}
}

func TestHandleEventSurfacesPairingFailures(t *testing.T) {
	tests := []struct {
		name  string
		event any
		want  string
	}{
		{
			name:  "passkey request directs user to QR",
			event: &waevents.PairPasskeyRequest{},
			want:  "this WhatsApp account is protected by a passkey, which phone-number linking doesn't support yet — scan the QR code instead",
		},
		{
			name: "passkey error includes cause",
			event: &waevents.PairPasskeyError{
				Error: errors.New("assertion rejected"),
			},
			want: "passkey pairing failed: assertion rejected",
		},
		{
			name:  "QR scan without multidevice gives recovery instruction",
			event: &waevents.QRScannedWithoutMultidevice{},
			want:  "scan the QR code again after enabling multi-device support in WhatsApp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			statusChanges := 0
			bridge := &Bridge{
				connected:  true,
				connecting: true,
				pairing:    true,
				qr: QRSnapshot{
					Event: "code",
					Code:  "stale-code",
				},
				callbacks: Callbacks{
					OnStatusChange: func() { statusChanges++ },
				},
			}

			// These branches used to disappear silently. Guard the actionable C5
			// terminal state as well as the exact user-facing recovery text.
			bridge.handleEvent(tt.event)

			bridge.mu.RLock()
			internallyConnected := bridge.connected
			bridge.mu.RUnlock()
			if internallyConnected {
				t.Fatal("pairing failure left the internal connected flag set")
			}
			status := bridge.Status()
			if status.Connected || status.Connecting || status.Pairing {
				t.Fatalf("pairing failure left an active state: %+v", status)
			}
			if status.QRAvailable || status.QREvent != "" {
				t.Fatalf("pairing failure retained a stale QR: %+v", status)
			}
			if status.LastError != tt.want {
				t.Fatalf("last error = %q, want %q", status.LastError, tt.want)
			}
			if statusChanges != 1 {
				t.Fatalf("status changes = %d, want 1", statusChanges)
			}
		})
	}
}

func TestHandleTemporaryBanSurfacesExpiryOnlyInLastError(t *testing.T) {
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	statusChanges := 0
	bridge := &Bridge{
		connected:  true,
		connecting: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{ID: &ownJID},
		},
		callbacks: Callbacks{
			OnStatusChange: func() { statusChanges++ },
		},
	}
	event := &waevents.TemporaryBan{
		Code:   waevents.TempBanSentTooManySameMessage,
		Expire: 17 * time.Minute,
	}

	bridge.handleEvent(event)

	bridge.mu.RLock()
	internallyConnected := bridge.connected
	bridge.mu.RUnlock()
	if internallyConnected {
		t.Fatal("temporary ban left the internal connected flag set")
	}
	status := bridge.Status()
	if status.Connected || status.Connecting {
		t.Fatalf("temporary ban left an active connection state: %+v", status)
	}
	if !status.Paired {
		t.Fatal("temporary ban should retain the paired device")
	}
	if status.LastError != event.PermanentDisconnectDescription() {
		t.Fatalf("last error = %q, want %q", status.LastError, event.PermanentDisconnectDescription())
	}
	if !strings.Contains(status.LastError, "17m0s") {
		t.Fatalf("temporary-ban expiry was not surfaced in error text: %q", status.LastError)
	}
	// BUG(F7): expiry discarded, C5 must persist it
	// The current bridge keeps only this formatted duration, not a typed
	// RetryAt/blocked-until value that can prevent reconnects before expiry.
	if statusChanges != 1 {
		t.Fatalf("status changes = %d, want 1", statusChanges)
	}
}

func TestHandleReceiptServerErrorPreservesMonotonicStatus(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const messageID = "whatsapp:receipt-failure-sequence"
	if err := store.UpsertMessage(&db.Message{
		MessageID:      messageID,
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Body:           "status sequence",
		TimestampMS:    1700000000000,
		Status:         "sent",
		IsFromMe:       true,
		SourcePlatform: "whatsapp",
		SourceID:       "receipt-failure-sequence",
	}); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	bridge := &Bridge{store: store}
	receipt := func(receiptType watypes.ReceiptType) {
		bridge.handleReceipt(&waevents.Receipt{
			MessageIDs: []watypes.MessageID{"receipt-failure-sequence"},
			Type:       receiptType,
		})
	}
	assertStatus := func(want string) {
		t.Helper()
		msg, err := store.GetMessageByID(messageID)
		if err != nil {
			t.Fatalf("GetMessageByID(): %v", err)
		}
		if msg == nil || msg.Status != want {
			t.Fatalf("message status = %#v, want %q", msg, want)
		}
	}

	// Server errors can fail an unconfirmed send, but later receipts still
	// advance it and a late error must not downgrade delivered/read state. This
	// failure sequence guards monotonic receipt handling through C5.
	receipt(watypes.ReceiptTypeServerError)
	assertStatus("failed")
	receipt(watypes.ReceiptTypeDelivered)
	assertStatus("delivered")
	receipt(watypes.ReceiptTypeServerError)
	assertStatus("delivered")
}

func TestHandleMessageCanonicalizesLIDMention(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	groupJID := watypes.NewJID("120363019999999999", watypes.GroupServer)
	ownPN := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	ownLID := watypes.NewJID("134149377675278", watypes.HiddenUserServer)
	if err := store.UpsertConversation(&db.Conversation{
		ConversationID: waConversationID(groupJID),
		Name:           "Alias Group",
		IsGroup:        true,
		Participants:   `[{"name":"Max Ghenis","number":"+15551230000"}]`,
		SourcePlatform: "whatsapp",
	}); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	bridge := &Bridge{
		store: store,
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID: &ownPN,
				LIDs: &testLIDStore{
					NoopStore: wastore.NoopStore{},
					lidToPN: map[string]watypes.JID{
						ownLID.String(): ownPN,
					},
				},
			},
		},
	}
	// A LID mention must resolve to the account PN for both display replacement
	// and MentionsMe detection; this guards identity handling through C5.
	bridge.handleMessage(&waevents.Message{
		Info: watypes.MessageInfo{
			MessageSource: watypes.MessageSource{
				Chat:     groupJID,
				Sender:   watypes.NewJID("15551234567", watypes.DefaultUserServer),
				IsGroup:  true,
				IsFromMe: false,
			},
			ID:        "lid-mention",
			PushName:  "Jenn",
			Timestamp: time.UnixMilli(1775002000000),
		},
		Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: strPtr("hello @134149377675278"),
				ContextInfo: &waE2E.ContextInfo{
					MentionedJID: []string{ownLID.String()},
				},
			},
		},
	})

	msg, err := store.GetMessageByID("whatsapp:lid-mention")
	if err != nil {
		t.Fatalf("GetMessageByID(): %v", err)
	}
	if msg == nil {
		t.Fatal("expected stored LID-mention message")
	}
	if !msg.MentionsMe {
		t.Fatal("expected LID alias of own PN to set MentionsMe")
	}
	if !strings.Contains(msg.Body, "@~Max") {
		t.Fatalf("body = %q, want canonical @~Max label", msg.Body)
	}
	if strings.Contains(msg.Body, ownLID.User) {
		t.Fatalf("body = %q, raw LID mention was not replaced", msg.Body)
	}
}

func TestReactionActorIDUsesCanonicalGroupSenderAndDirectChat(t *testing.T) {
	lid := watypes.NewJID("134149377675278", watypes.HiddenUserServer)
	pn := watypes.NewJID("15551234567", watypes.DefaultUserServer)
	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		client: &whatsmeow.Client{
			Store: &wastore.Device{
				ID: &ownJID,
				LIDs: &testLIDStore{
					NoopStore: wastore.NoopStore{},
					lidToPN: map[string]watypes.JID{
						lid.String(): pn,
					},
				},
			},
		},
	}

	tests := []struct {
		name string
		info watypes.MessageInfo
		want string
	}{
		{
			name: "group reaction uses sender",
			info: watypes.MessageInfo{MessageSource: watypes.MessageSource{
				Chat:    watypes.NewJID("120363019999999999", watypes.GroupServer),
				Sender:  lid,
				IsGroup: true,
			}},
			want: pn.String(),
		},
		{
			name: "direct reaction uses chat",
			info: watypes.MessageInfo{MessageSource: watypes.MessageSource{
				Chat:   lid,
				Sender: watypes.NewJID("15550000000", watypes.DefaultUserServer),
			}},
			want: pn.String(),
		},
		{
			name: "own-device reaction uses account",
			info: watypes.MessageInfo{MessageSource: watypes.MessageSource{
				Chat:     watypes.NewJID("120363019999999999", watypes.GroupServer),
				Sender:   watypes.NewJID("15550000000", watypes.DefaultUserServer),
				IsGroup:  true,
				IsFromMe: true,
			}},
			want: ownJID.String(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The actor is sender-scoped in groups, chat-scoped in DMs, and
			// account-scoped for own events; preserve that split through C5.
			got := bridge.reactionActorID(&waevents.Message{Info: tt.info})
			if got != tt.want {
				t.Fatalf("reaction actor = %q, want canonical %q", got, tt.want)
			}
		})
	}
}

func TestBridgeSendReactionMarksOwnTargetFromMe(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	const conversationID = "whatsapp:120363019999999999@g.us"
	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:own-target",
		ConversationID: conversationID,
		SenderName:     "Me",
		// Deliberately differs from the own JID so the test fails if the
		// IsFromMe target rule is accidentally bypassed during C5.
		SenderNumber:   "+15559999999",
		Body:           "my group message",
		TimestampMS:    1700000000000,
		IsFromMe:       true,
		SourcePlatform: "whatsapp",
		SourceID:       "own-target",
	}); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		store:     store,
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{ID: &ownJID},
		},
	}

	originalSend := sendTextMessage
	originalIsConnected := clientIsConnected
	defer func() {
		sendTextMessage = originalSend
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }
	target, err := store.GetMessageByID("whatsapp:own-target")
	if err != nil || target == nil {
		t.Fatalf("load target: %v / %#v", err, target)
	}
	groupJID := watypes.NewJID("120363019999999999", watypes.GroupServer)
	if sender := bridge.reactionTargetSenderJID(target, groupJID); !sender.IsEmpty() {
		t.Fatalf("own reaction target sender = %q, want empty", sender.String())
	}

	var captured *waE2E.Message
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, _ watypes.JID, message *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		captured = message
		return whatsmeow.SendResponse{}, nil
	}

	if err := bridge.SendReaction(conversationID, "whatsapp:own-target", "✅", "add"); err != nil {
		t.Fatalf("SendReaction(): %v", err)
	}
	reaction := extractReactionMessage(captured)
	if reaction == nil {
		t.Fatal("expected outgoing reaction message")
	}
	if !reaction.GetKey().GetFromMe() {
		t.Fatal("expected reaction target key to mark own message FromMe")
	}
	if reaction.GetKey().GetParticipant() != "" {
		t.Fatalf("participant = %q, want empty for own target", reaction.GetKey().GetParticipant())
	}
}

func TestStoredMediaRefCodecCompatibility(t *testing.T) {
	// Persisted refs must remain readable across the C5 extraction. This is a
	// fixed compatibility vector, not an encode/decode round trip.
	const encoded = "wa:eyJ1cmwiOiJodHRwczovL2Nkbi5leGFtcGxlLnRlc3QvaW1hZ2UiLCJkaXJlY3RfcGF0aCI6Ii9tbXMvaW1hZ2UiLCJmaWxlX3NoYTI1NiI6IjA1MDYiLCJmaWxlX2VuY19zaGEyNTYiOiIwMzA0IiwiZmlsZV9sZW5ndGgiOjl9"
	want := storedMediaRef{
		URL:           "https://cdn.example.test/image",
		DirectPath:    "/mms/image",
		FileSHA256:    "0506",
		FileEncSHA256: "0304",
		FileLength:    9,
	}

	decoded, err := decodeStoredMediaRef(encoded)
	if err != nil {
		t.Fatalf("decodeStoredMediaRef(): %v", err)
	}
	if decoded != want {
		t.Fatalf("decoded ref = %#v, want %#v", decoded, want)
	}
	if got := encodeStoredMediaRef(want); got != encoded {
		t.Fatalf("encoded ref = %q, want compatibility vector %q", got, encoded)
	}
}

func TestMediaCodecsCoverVideoAndDocument(t *testing.T) {
	upload := whatsmeow.UploadResponse{
		URL:           "https://cdn.example.test/media",
		DirectPath:    "/mms/media",
		MediaKey:      []byte{0x01, 0x02},
		FileEncSHA256: []byte{0x03, 0x04},
		FileSHA256:    []byte{0x05, 0x06},
		FileLength:    9,
	}

	tests := []struct {
		name       string
		mime       string
		filename   string
		wantType   whatsmeow.MediaType
		assertWire func(*testing.T, *waE2E.Message)
	}{
		{
			name:     "video",
			mime:     "video/mp4",
			filename: "clip.mp4",
			wantType: whatsmeow.MediaVideo,
			assertWire: func(t *testing.T, msg *waE2E.Message) {
				t.Helper()
				video := msg.GetVideoMessage()
				if video == nil {
					t.Fatal("expected VideoMessage")
				}
				if video.GetMimetype() != "video/mp4" || video.GetCaption() != "codec caption" {
					t.Fatalf("video mime/caption = %q/%q", video.GetMimetype(), video.GetCaption())
				}
			},
		},
		{
			name:     "document",
			mime:     "application/pdf",
			filename: "reports/quarterly.pdf",
			wantType: whatsmeow.MediaDocument,
			assertWire: func(t *testing.T, msg *waE2E.Message) {
				t.Helper()
				document := msg.GetDocumentMessage()
				if document == nil {
					t.Fatal("expected DocumentMessage")
				}
				if document.GetMimetype() != "application/pdf" || document.GetCaption() != "codec caption" {
					t.Fatalf("document mime/caption = %q/%q", document.GetMimetype(), document.GetCaption())
				}
				if document.GetFileName() != "quarterly.pdf" || document.GetTitle() != "quarterly.pdf" {
					t.Fatalf("document filename/title = %q/%q, want quarterly.pdf", document.GetFileName(), document.GetTitle())
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mediaType, err := mediaTypeForMIME(tt.mime)
			if err != nil {
				t.Fatalf("mediaTypeForMIME(): %v", err)
			}
			if mediaType != tt.wantType {
				t.Fatalf("media type = %v, want %v", mediaType, tt.wantType)
			}

			msg := outgoingMediaMessage(upload, tt.mime, tt.filename, mediaType, "codec caption", "whatsapp:reply-codec")
			tt.assertWire(t, msg)
			if got := extractMessageBody(msg); got != "codec caption" {
				t.Fatalf("extracted body = %q, want codec caption", got)
			}
			if got := extractReplyToID(msg); got != "whatsapp:reply-codec" {
				t.Fatalf("extracted reply = %q, want whatsapp:reply-codec", got)
			}

			ref, key, mime, ok := extractStoredMediaRef(msg)
			if !ok {
				t.Fatal("expected outgoing media to produce a stored ref")
			}
			wantRef := storedMediaRef{
				URL:           upload.URL,
				DirectPath:    upload.DirectPath,
				FileSHA256:    "0506",
				FileEncSHA256: "0304",
				FileLength:    upload.FileLength,
			}
			if ref != wantRef {
				t.Fatalf("stored ref = %#v, want %#v", ref, wantRef)
			}
			if string(key) != string(upload.MediaKey) {
				t.Fatalf("media key = %x, want %x", key, upload.MediaKey)
			}
			if mime != tt.mime {
				t.Fatalf("stored mime = %q, want %q", mime, tt.mime)
			}
		})
	}
}

func TestExtractStoredMediaRefDefaultsStickerMIME(t *testing.T) {
	ref, key, mime, ok := extractStoredMediaRef(&waE2E.Message{
		StickerMessage: &waE2E.StickerMessage{
			URL:           strPtr("https://cdn.example.test/sticker"),
			DirectPath:    strPtr("/mms/sticker"),
			MediaKey:      []byte{0x01},
			FileSHA256:    []byte{0x02},
			FileEncSHA256: []byte{0x03},
			FileLength:    uint64Ptr(4),
		},
	})
	if !ok {
		t.Fatal("expected sticker media ref")
	}
	if mime != "image/webp" {
		t.Fatalf("sticker MIME = %q, want image/webp fallback", mime)
	}
	if ref.DirectPath != "/mms/sticker" || string(key) != string([]byte{0x01}) {
		t.Fatalf("sticker ref/key = %#v/%x", ref, key)
	}
}

func TestDownloadStoredMediaAllowsLegacyMissingPlaintextHash(t *testing.T) {
	bridge := &Bridge{
		connected: true,
		client:    &whatsmeow.Client{},
	}

	originalDownload := downloadMediaWithPath
	originalIsConnected := clientIsConnected
	defer func() {
		downloadMediaWithPath = originalDownload
		clientIsConnected = originalIsConnected
	}()
	clientIsConnected = func(*whatsmeow.Client) bool { return true }

	downloadMediaWithPath = func(_ *whatsmeow.Client, _ context.Context, directPath string, encFileHash, fileHash, mediaKey []byte, mediaType whatsmeow.MediaType, _ string, allowNoHash bool) ([]byte, error) {
		if directPath != "/mms/legacy" {
			t.Fatalf("direct path = %q, want /mms/legacy", directPath)
		}
		if string(encFileHash) != string([]byte{0x03, 0x04}) || len(fileHash) != 0 || string(mediaKey) != string([]byte{0x01, 0x02}) {
			t.Fatalf("download hashes/key = %x/%x/%x", encFileHash, fileHash, mediaKey)
		}
		if mediaType != whatsmeow.MediaImage {
			t.Fatalf("media type = %v, want image", mediaType)
		}
		if !allowNoHash {
			t.Fatal("allowNoHash = false, want legacy missing-hash compatibility")
		}
		return []byte("legacy-image"), nil
	}

	data, mime, err := bridge.DownloadStoredMedia(&db.Message{
		MediaID: encodeStoredMediaRef(storedMediaRef{
			DirectPath:    "/mms/legacy",
			FileEncSHA256: "0304",
		}),
		MimeType:      "image/jpeg",
		DecryptionKey: "0102",
	})
	if err != nil {
		t.Fatalf("DownloadStoredMedia(): %v", err)
	}
	if string(data) != "legacy-image" || mime != "image/jpeg" {
		t.Fatalf("download = %q/%q, want legacy-image/image/jpeg", data, mime)
	}
}

func TestExtractTextAndReplyThroughWhatsAppWrappers(t *testing.T) {
	inner := func() *waE2E.Message {
		return &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: strPtr(" wrapped text "),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID: strPtr("wrapped-reply"),
				},
			},
		}
	}
	tests := []struct {
		name string
		wrap func(*waE2E.Message) *waE2E.Message
	}{
		{
			name: "device sent",
			wrap: func(msg *waE2E.Message) *waE2E.Message {
				return &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: msg}}
			},
		},
		{
			name: "view once v1",
			wrap: func(msg *waE2E.Message) *waE2E.Message {
				return &waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{Message: msg}}
			},
		},
		{
			name: "view once v2 extension",
			wrap: func(msg *waE2E.Message) *waE2E.Message {
				return &waE2E.Message{ViewOnceMessageV2Extension: &waE2E.FutureProofMessage{Message: msg}}
			},
		},
		{
			name: "document with caption",
			wrap: func(msg *waE2E.Message) *waE2E.Message {
				return &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: msg}}
			},
		},
		{
			name: "edited",
			wrap: func(msg *waE2E.Message) *waE2E.Message {
				return &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: msg}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// These were the untested unwrap branches; preserve both text and
			// reply extraction when C5 moves the transport implementation.
			msg := tt.wrap(inner())
			if got := extractMessageBody(msg); got != "wrapped text" {
				t.Fatalf("body = %q, want wrapped text", got)
			}
			if got := extractReplyToID(msg); got != "whatsapp:wrapped-reply" {
				t.Fatalf("reply = %q, want whatsapp:wrapped-reply", got)
			}
		})
	}
}

func TestExtractReplyToIDFromSupportedMessageContexts(t *testing.T) {
	contextInfo := func() *waE2E.ContextInfo {
		return &waE2E.ContextInfo{StanzaID: strPtr("context-reply")}
	}
	// messageContextInfo intentionally enumerates every reply-bearing envelope;
	// keep these non-text branches intact when C5 moves message extraction.
	tests := []struct {
		name    string
		message func() *waE2E.Message
	}{
		{name: "image", message: func() *waE2E.Message {
			return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{ContextInfo: contextInfo()}}
		}},
		{name: "PTV", message: func() *waE2E.Message {
			return &waE2E.Message{PtvMessage: &waE2E.VideoMessage{ContextInfo: contextInfo()}}
		}},
		{name: "audio", message: func() *waE2E.Message {
			return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{ContextInfo: contextInfo()}}
		}},
		{name: "sticker", message: func() *waE2E.Message {
			return &waE2E.Message{StickerMessage: &waE2E.StickerMessage{ContextInfo: contextInfo()}}
		}},
		{name: "location", message: func() *waE2E.Message {
			return &waE2E.Message{LocationMessage: &waE2E.LocationMessage{ContextInfo: contextInfo()}}
		}},
		{name: "live location", message: func() *waE2E.Message {
			return &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{ContextInfo: contextInfo()}}
		}},
		{name: "event", message: func() *waE2E.Message {
			return &waE2E.Message{EventMessage: &waE2E.EventMessage{ContextInfo: contextInfo()}}
		}},
		{name: "event invite", message: func() *waE2E.Message {
			return &waE2E.Message{EventInviteMessage: &waE2E.EventInviteMessage{ContextInfo: contextInfo()}}
		}},
		{name: "group invite", message: func() *waE2E.Message {
			return &waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{ContextInfo: contextInfo()}}
		}},
		{name: "contact", message: func() *waE2E.Message {
			return &waE2E.Message{ContactMessage: &waE2E.ContactMessage{ContextInfo: contextInfo()}}
		}},
		{name: "contacts", message: func() *waE2E.Message {
			return &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{ContextInfo: contextInfo()}}
		}},
		{name: "album", message: func() *waE2E.Message {
			return &waE2E.Message{AlbumMessage: &waE2E.AlbumMessage{ContextInfo: contextInfo()}}
		}},
		{name: "poll v1", message: func() *waE2E.Message {
			return &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{ContextInfo: contextInfo()}}
		}},
		{name: "poll v2", message: func() *waE2E.Message {
			return &waE2E.Message{PollCreationMessageV2: &waE2E.PollCreationMessage{ContextInfo: contextInfo()}}
		}},
		{name: "poll v3", message: func() *waE2E.Message {
			return &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{ContextInfo: contextInfo()}}
		}},
		{name: "poll v5", message: func() *waE2E.Message {
			return &waE2E.Message{PollCreationMessageV5: &waE2E.PollCreationMessage{ContextInfo: contextInfo()}}
		}},
		{name: "poll v6", message: func() *waE2E.Message {
			return &waE2E.Message{PollCreationMessageV6: &waE2E.PollCreationMessage{ContextInfo: contextInfo()}}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractReplyToID(tt.message()); got != "whatsapp:context-reply" {
				t.Fatalf("reply = %q, want whatsapp:context-reply", got)
			}
		})
	}
}

func TestOutgoingTextMessageNormalizesReplyID(t *testing.T) {
	t.Run("no reply uses Conversation", func(t *testing.T) {
		msg := outgoingTextMessage("plain text", "")
		if msg.GetConversation() != "plain text" {
			t.Fatalf("conversation = %q, want plain text", msg.GetConversation())
		}
		if msg.GetExtendedTextMessage() != nil {
			t.Fatal("plain outgoing text unexpectedly used ExtendedTextMessage")
		}
	})

	t.Run("bare reply is trimmed and prefixed", func(t *testing.T) {
		msg := outgoingTextMessage("reply text", " bare-reply ")
		if msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID() != "bare-reply" {
			t.Fatalf("wire stanza = %q, want bare-reply", msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID())
		}
		if got := normalizeReplyToID(" bare-reply "); got != "whatsapp:bare-reply" {
			t.Fatalf("normalized reply = %q, want whatsapp:bare-reply", got)
		}
	})
}

func TestUnavailableRepairCooldownAllowsRetryAfterExpiry(t *testing.T) {
	if unavailablePlaceholderRepairCooldown != 30*time.Minute {
		t.Fatalf("repair cooldown = %v, want 30m", unavailablePlaceholderRepairCooldown)
	}
	bridge := &Bridge{}
	const key = "15551234567@s.whatsapp.net|media-source"
	if !bridge.markUnavailableRepairRequest(key) {
		t.Fatal("first repair request was unexpectedly suppressed")
	}
	if bridge.markUnavailableRepairRequest(key) {
		t.Fatal("recent duplicate repair request was not suppressed")
	}

	bridge.mu.Lock()
	bridge.unavailableRepairRequests[key] = time.Now().Add(-29 * time.Minute)
	bridge.mu.Unlock()
	if bridge.markUnavailableRepairRequest(key) {
		t.Fatal("repair request was allowed before the 30-minute cooldown expired")
	}

	bridge.mu.Lock()
	bridge.unavailableRepairRequests[key] = time.Now().Add(-31 * time.Minute)
	bridge.mu.Unlock()
	if !bridge.markUnavailableRepairRequest(key) {
		t.Fatal("repair request did not resume after cooldown expiry")
	}
	// The allowed retry must renew its reservation so C5 cannot create a
	// peer-resend storm immediately after the cooldown boundary.
	if bridge.markUnavailableRepairRequest(key) {
		t.Fatal("post-expiry retry did not renew duplicate suppression")
	}
}

func TestUnavailableRepairFailureReleasesCooldown(t *testing.T) {
	store, err := db.New(":memory:")
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	defer store.Close()

	if err := store.UpsertMessage(&db.Message{
		MessageID:      "whatsapp:failed-repair",
		ConversationID: "whatsapp:15551234567@s.whatsapp.net",
		Body:           "[Photo]",
		TimestampMS:    1774986930000,
		SourcePlatform: "whatsapp",
		SourceID:       "failed-repair",
	}); err != nil {
		t.Fatalf("seed placeholder: %v", err)
	}

	ownJID := watypes.NewJID("15551230000", watypes.DefaultUserServer)
	bridge := &Bridge{
		store:     store,
		logger:    zerolog.Nop(),
		connected: true,
		client: &whatsmeow.Client{
			Store: &wastore.Device{ID: &ownJID},
		},
	}

	originalSend := sendTextMessage
	defer func() { sendTextMessage = originalSend }()
	calls := 0
	sendTextMessage = func(_ *whatsmeow.Client, _ context.Context, _ watypes.JID, _ *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		calls++
		if calls == 1 {
			return whatsmeow.SendResponse{}, errors.New("peer send failed")
		}
		return whatsmeow.SendResponse{ID: "peer-request"}, nil
	}

	// A failed peer send must release its reservation for one immediate retry;
	// a successful retry must restore the C5 cooldown.
	if err := bridge.RepairUnavailableMediaPlaceholders(10); err != nil {
		t.Fatalf("first RepairUnavailableMediaPlaceholders(): %v", err)
	}
	if err := bridge.RepairUnavailableMediaPlaceholders(10); err != nil {
		t.Fatalf("second RepairUnavailableMediaPlaceholders(): %v", err)
	}
	if err := bridge.RepairUnavailableMediaPlaceholders(10); err != nil {
		t.Fatalf("third RepairUnavailableMediaPlaceholders(): %v", err)
	}
	if calls != 2 {
		t.Fatalf("send calls = %d, want failed attempt + one immediate retry", calls)
	}
}
