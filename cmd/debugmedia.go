package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/client"
)

func RunDebugMedia(logger zerolog.Logger, convID string) error {
	a, err := app.New(logger)
	if err != nil {
		return err
	}
	defer a.Close()
	if err := a.LoadAndConnect(); err != nil {
		return err
	}

	cli := a.GetClient()
	if cli == nil {
		return fmt.Errorf("client not connected")
	}

	resp, err := cli.GM.FetchMessages(context.Background(), convID, 10, nil)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	for _, msg := range resp.GetMessages() {
		mi := client.ExtractMediaInfo(msg)
		if mi != nil {
			fmt.Printf("MsgID=%s MediaID=%q MimeType=%s KeyLen=%d\n", msg.GetMessageID(), mi.MediaID, mi.MimeType, len(mi.DecryptionKey))
		}
		for _, info := range msg.GetMessageInfo() {
			if mc := info.GetMediaContent(); mc != nil {
				raw, _ := json.Marshal(mc)
				fmt.Printf("  Raw MediaContent: %s\n", raw)
			}
		}
	}
	return nil
}
