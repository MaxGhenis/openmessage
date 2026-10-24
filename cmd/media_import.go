package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/localapi"
	"github.com/maxghenis/openmessage/internal/messaging"
)

const mediaImportUsage = "Usage: openmessage media import <message-id|v2msg:<message-id>:<n>> <file> [--ordinal N] [--sha256 HEX] [--replace] [--json]"

// mediaImportTimeout bounds the whole import, upload included.
const mediaImportTimeout = 3 * time.Minute

// RunMedia dispatches `openmessage media <subcommand>`.
func RunMedia(args []string) error {
	if len(args) == 0 || args[0] != "import" {
		return errors.New(mediaImportUsage)
	}
	client := localapi.NewClient("", localapi.LoadControlToken(app.DefaultDataDir()))
	ctx, cancel := context.WithTimeout(context.Background(), mediaImportTimeout)
	defer cancel()
	// The upload can't be replayed, so settle on a token the daemon accepts
	// first; a status failure surfaces through the import itself.
	_, _, _ = client.Status(ctx)
	return runMediaImport(ctx, os.Stdout, client, args[1:])
}

type mediaImportArgs struct {
	messageID string
	ordinal   int64
	path      string
	sha256    string
	replace   bool
	json      bool
}

func parseMediaImportArgs(args []string) (mediaImportArgs, error) {
	var parsed mediaImportArgs
	ordinalSet := false
	positional := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--ordinal":
			if i+1 >= len(args) {
				return parsed, errors.New(mediaImportUsage)
			}
			i++
			value, err := strconv.ParseInt(args[i], 10, 64)
			if err != nil || value < 0 {
				return parsed, fmt.Errorf("--ordinal must be a non-negative integer")
			}
			parsed.ordinal = value
			ordinalSet = true
		case "--sha256":
			if i+1 >= len(args) {
				return parsed, errors.New(mediaImportUsage)
			}
			i++
			parsed.sha256 = strings.ToLower(strings.TrimSpace(args[i]))
			if decoded, err := hex.DecodeString(parsed.sha256); err != nil || len(decoded) != 32 {
				return parsed, fmt.Errorf("--sha256 must be a hex SHA-256 digest")
			}
		case "--json":
			parsed.json = true
		case "--replace":
			parsed.replace = true
		default:
			if strings.HasPrefix(args[i], "--") {
				return parsed, fmt.Errorf("unknown flag %s\n%s", args[i], mediaImportUsage)
			}
			positional = append(positional, args[i])
		}
	}
	if len(positional) != 2 {
		return parsed, errors.New(mediaImportUsage)
	}
	parsed.messageID = strings.TrimSpace(positional[0])
	parsed.path = positional[1]
	// Accept the media_id the read API reports for one attachment.
	if rest, ok := strings.CutPrefix(parsed.messageID, "v2msg:"); ok {
		cut := strings.LastIndexByte(rest, ':')
		value, err := strconv.ParseInt(rest[cut+1:], 10, 64)
		if cut <= 0 || err != nil || value < 0 {
			return parsed, fmt.Errorf("media id %q is not v2msg:<message-id>:<n>", parsed.messageID)
		}
		if ordinalSet && value != parsed.ordinal {
			return parsed, fmt.Errorf("--ordinal %d contradicts media id %q", parsed.ordinal, parsed.messageID)
		}
		parsed.messageID, parsed.ordinal = rest[:cut], value
	}
	if parsed.messageID == "" {
		return parsed, errors.New(mediaImportUsage)
	}
	return parsed, nil
}

// runMediaImport hands the running app a local copy of one pending inbound
// attachment that its platform can no longer deliver, such as a Signal
// attachment signal-cli never stored that the sender sent again another way.
// The app owns the store: it checks the bytes against the size the platform
// declared and the digest sent here, stores them, and marks the attachment
// downloaded. --replace corrects an earlier import of the wrong file.
func runMediaImport(ctx context.Context, out io.Writer, client *localapi.Client, args []string) error {
	parsed, err := parseMediaImportArgs(args)
	if err != nil {
		return err
	}
	file, err := os.Open(parsed.path)
	if err != nil {
		return fmt.Errorf("open %s: %w", parsed.path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, messaging.DefaultMaxMediaBytes+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", parsed.path, err)
	}
	if int64(len(data)) > messaging.DefaultMaxMediaBytes {
		return fmt.Errorf("%s is larger than the %d-byte attachment limit", parsed.path, messaging.DefaultMaxMediaBytes)
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if parsed.sha256 != "" && parsed.sha256 != digest {
		return fmt.Errorf("%s has SHA-256 %s, not the expected %s", parsed.path, digest, parsed.sha256)
	}

	result, err := client.ImportMessageAttachment(ctx, parsed.messageID, parsed.ordinal, bytes.NewReader(data), digest, parsed.replace)
	if err != nil {
		if localapi.IsConnectionRefused(err) {
			return fmt.Errorf("the OpenMessage app isn't running; start it and retry: %w", err)
		}
		return fmt.Errorf("import attachment: %w", err)
	}
	if parsed.json {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	verb := "imported"
	if !result.Imported {
		verb = "already held these bytes"
	}
	fmt.Fprintf(out, "%s:%d %s: %s (%s, %d bytes, sha256 %s)\n",
		result.MessageID, result.Ordinal, verb, result.Filename, result.MIME, result.SizeBytes, result.BlobHash)
	return nil
}
