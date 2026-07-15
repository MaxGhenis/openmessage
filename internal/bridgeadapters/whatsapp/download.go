package whatsapp

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"

	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/whatsapplive"
	"github.com/maxghenis/openmessage/internal/whatsappmedia"
)

// whatsappMediaOpaqueV1 is the WhatsApp-owned Wave-4 -> M4b wire contract.
// The future WhatsApp decoder must pack message_attachments.remote_ref with
// this exact versioned JSON shape. Until Wave-4 does so, this production path
// is intentionally inert even though crafted payloads make it unit-testable.
type whatsappMediaOpaqueV1 struct {
	V             int    `json:"v"`
	Kind          string `json:"kind"`
	DirectPath    string `json:"direct_path"`
	FileSHA256    string `json:"file_sha256"`
	FileEncSHA256 string `json:"file_enc_sha256"`
	MediaKey      string `json:"media_key"`
	FileLength    uint64 `json:"file_length"`
	LocalRef      string `json:"local_ref"`
}

func marshalWhatsAppMediaOpaqueV1(payload whatsappMediaOpaqueV1) ([]byte, error) {
	payload.V = 1
	return json.Marshal(payload)
}

func unmarshalWhatsAppMediaOpaque(raw []byte) (whatsappMediaOpaqueV1, error) {
	var payload whatsappMediaOpaqueV1
	if !utf8.Valid(raw) {
		return payload, whatsappOpaqueUnsupported(
			"whatsapp_opaque_malformed",
			errors.New("WhatsApp media Opaque is not valid UTF-8"),
		)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, whatsappOpaqueUnsupported(
			"whatsapp_opaque_malformed",
			fmt.Errorf("decode WhatsApp media Opaque JSON: %w", err),
		)
	}
	if payload.V != 1 {
		return payload, whatsappOpaqueUnsupported(
			"opaque_version_unsupported",
			fmt.Errorf("unsupported WhatsApp media Opaque version %d", payload.V),
		)
	}

	switch payload.Kind {
	case "remote":
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			// The typed unmarshal above already succeeded. Keep this defensive
			// branch classified as immutable structural input if that changes.
			return payload, whatsappOpaqueUnsupported(
				"whatsapp_opaque_malformed",
				fmt.Errorf("inspect WhatsApp media Opaque fields: %w", err),
			)
		}
		fileLength, hasFileLength := fields["file_length"]
		if strings.TrimSpace(payload.DirectPath) == "" ||
			strings.TrimSpace(payload.FileEncSHA256) == "" ||
			strings.TrimSpace(payload.MediaKey) == "" ||
			!hasFileLength || strings.TrimSpace(string(fileLength)) == "null" {
			return payload, whatsappOpaqueUnsupported(
				"whatsapp_opaque_remote_invalid",
				errors.New("WhatsApp remote media Opaque requires direct_path, file_enc_sha256, media_key, and file_length"),
			)
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{name: "file_enc_sha256", value: payload.FileEncSHA256},
			{name: "file_sha256", value: payload.FileSHA256},
			{name: "media_key", value: payload.MediaKey},
		} {
			name, value := field.name, field.value
			value = strings.TrimSpace(value)
			if value == "" && name == "file_sha256" {
				// Empty plaintext hash is an intentional compatibility mode;
				// the retained downloader maps it to allowNoHash=true.
				continue
			}
			if _, err := hex.DecodeString(value); err != nil {
				return payload, whatsappOpaqueUnsupported(
					"whatsapp_opaque_hex_invalid",
					fmt.Errorf("decode WhatsApp media Opaque %s: %w", name, err),
				)
			}
		}
	case "local":
		// Local refs intentionally ignore all remote-only fields; the retained
		// local route needs only the encoded desktop reference. The adapter's
		// uniform passive readiness gate still runs before decoding.
		if strings.TrimSpace(payload.LocalRef) == "" {
			return payload, whatsappOpaqueUnsupported(
				"whatsapp_opaque_local_invalid",
				errors.New("WhatsApp local media Opaque requires local_ref"),
			)
		}
		localPath, err := whatsappmedia.DecodeLocalMediaRef(payload.LocalRef)
		if err != nil || strings.TrimSpace(localPath) == "" {
			if err == nil {
				err = errors.New("WhatsApp local media reference has an empty path")
			}
			return payload, whatsappOpaqueUnsupported(
				"whatsapp_opaque_local_invalid",
				fmt.Errorf("decode WhatsApp local media Opaque local_ref: %w", err),
			)
		}
		if _, err := whatsappmedia.ResolveLocalMediaPath("whatsapp-media-validation-root", payload.LocalRef); err != nil {
			return payload, whatsappOpaqueUnsupported(
				"whatsapp_opaque_local_invalid",
				fmt.Errorf("validate WhatsApp local media Opaque local_ref: %w", err),
			)
		}
	default:
		return payload, whatsappOpaqueUnsupported(
			"whatsapp_opaque_kind_unsupported",
			fmt.Errorf("unsupported WhatsApp media Opaque kind %q", payload.Kind),
		)
	}
	return payload, nil
}

// DownloadMedia adapts the fully-buffered retained WhatsApp download path to
// bridge.MediaStream. This necessarily forfeits true streaming until a future
// transport-native streaming API replaces the retained []byte boundary.
func (a *Adapter) DownloadMedia(ctx context.Context, _ string, ref bridge.MediaRef) (bridge.MediaStream, error) {
	if a == nil || a.mediaReady == nil || !a.mediaReady() || a.downloadMediaRef == nil {
		return bridge.MediaStream{}, whatsappDownloadPreCallFailure(
			"whatsapp_not_connected",
			whatsmeow.ErrNotConnected,
		)
	}
	if ctx == nil {
		return bridge.MediaStream{}, whatsappDownloadPreCallFailure(
			"whatsapp_download_context_missing",
			errors.New("WhatsApp media download context is nil"),
		)
	}
	if err := ctx.Err(); err != nil {
		return bridge.MediaStream{}, whatsappDownloadPreCallFailure("whatsapp_download_context_done", err)
	}

	payload, err := unmarshalWhatsAppMediaOpaque(ref.Opaque)
	if err != nil {
		return bridge.MediaStream{}, err
	}
	data, transportMIME, err := a.downloadMediaRef(
		payload.Kind,
		whatsapplive.StoredMediaRef{
			DirectPath:    payload.DirectPath,
			FileSHA256:    payload.FileSHA256,
			FileEncSHA256: payload.FileEncSHA256,
			FileLength:    payload.FileLength,
		},
		payload.MediaKey,
		ref.MIME,
		payload.LocalRef,
	)
	if err != nil {
		// Deliberately no adapter-level lifecycle report (D1). The retained
		// WhatsApp core remains the sole owner of guarded reconnect reporting.
		failure := a.classifyError(
			err,
			"download_media",
			"whatsapp_download_media_failed",
		)
		// Downloads are idempotent reads, not dispatches. Do not let a wrapped
		// OpError from a lower layer leak send certainty into this path.
		failure.Dispatch = ""
		return bridge.MediaStream{}, failure
	}

	return bridge.MediaStream{
		ReadCloser: io.NopCloser(bytes.NewReader(data)),
		Size:       int64(len(data)),
		Filename:   ref.Filename,
		MIME:       firstNonEmpty(transportMIME, ref.MIME),
	}, nil
}

func whatsappDownloadPreCallFailure(fingerprint string, cause error) bridge.OpError {
	return bridge.OpError{
		Class:       bridge.FailureTransient,
		Operation:   "download_media",
		Fingerprint: fingerprint,
		Cause:       cause,
	}
}

func whatsappOpaqueUnsupported(fingerprint string, cause error) bridge.OpError {
	// Opaque bytes are persisted immutable input. Malformed or structurally
	// incomplete payloads cannot heal on retry, so fail closed as terminal.
	return bridge.OpError{
		Class:       bridge.FailureUnsupported,
		Operation:   "download_media",
		Fingerprint: fingerprint,
		Cause:       cause,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

var _ bridge.MediaDownloader = (*Adapter)(nil)
