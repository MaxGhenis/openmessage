package cmd

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/app"
)

func RunSend(logger zerolog.Logger, conversationID, message string) error {
	return RunSendWithOptions(logger, conversationID, message, SendOptions{})
}

func RunSendScheduled(logger zerolog.Logger, conversationID, message string, notBeforeMS *int64) error {
	return RunSendWithOptions(logger, conversationID, message, SendOptions{NotBeforeMS: notBeforeMS})
}

type SendOptions struct {
	NotBeforeMS    *int64
	IdempotencyKey string
}

func RunSendWithOptions(logger zerolog.Logger, conversationID, message string, options SendOptions) error {
	deps := defaultSendCommandDeps(func(conversationID, message string) error {
		return runLegacySend(logger, conversationID, message)
	})
	if options.IdempotencyKey != "" {
		deps.newKey = func() (string, error) { return options.IdempotencyKey, nil }
	}
	return runSendWithDeps(context.Background(), deps, conversationID, message, options.NotBeforeMS)
}

func runLegacySend(logger zerolog.Logger, conversationID, message string) error {
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
