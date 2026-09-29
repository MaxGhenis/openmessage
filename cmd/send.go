package cmd

import (
	"context"
	"fmt"
	"strconv"

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
	// Force submits even when the daemon's near-duplicate guard would refuse
	// the send as a repeat of one submitted to the conversation moments ago.
	Force bool
}

// ParseSendOptions parses the flags that follow
// `openmessage send <conversation_id> <message>`.
func ParseSendOptions(args []string) (SendOptions, error) {
	var options SendOptions
	for index := 0; index < len(args); index++ {
		option := args[index]
		switch option {
		case "--force":
			options.Force = true
			continue
		case "--not-before-ms", "--idempotency-key":
		default:
			return SendOptions{}, fmt.Errorf("unknown send option %s", option)
		}
		if index+1 >= len(args) {
			return SendOptions{}, fmt.Errorf("send option %s requires a value", option)
		}
		index++
		value := args[index]
		if option == "--idempotency-key" {
			options.IdempotencyKey = value
			continue
		}
		notBeforeMS, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return SendOptions{}, fmt.Errorf("--not-before-ms must be an integer: %w", err)
		}
		options.NotBeforeMS = &notBeforeMS
	}
	return options, nil
}

func RunSendWithOptions(logger zerolog.Logger, conversationID, message string, options SendOptions) error {
	deps := defaultSendCommandDeps(func(conversationID, message string) error {
		return runLegacySend(logger, conversationID, message)
	})
	return runSendWithDeps(context.Background(), deps, conversationID, message, options)
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
