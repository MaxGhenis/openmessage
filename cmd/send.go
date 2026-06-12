package cmd

import (
	"fmt"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
)

func RunSend(logger zerolog.Logger, conversationID, message string) error {
	a, err := app.New(logger)
	if err != nil {
		return fmt.Errorf("init app: %w", err)
	}
	defer a.Close()

	if err := a.LoadAndConnect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	conv, _, err := a.SendTextToConversation(conversationID, message)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}

	logger.Info().
		Str("conversation", conversationID).
		Str("platform", conv.SourcePlatform).
		Msg("Message sent")
	return nil
}
