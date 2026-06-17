package cmd

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
)

func RunSendGroup(logger zerolog.Logger, phones []string, message string) error {
	a, err := app.New(logger)
	if err != nil {
		return fmt.Errorf("init app: %w", err)
	}
	defer a.Close()

	if err := a.LoadAndConnect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	cli := a.GetClient()
	if cli == nil {
		return fmt.Errorf("client not connected")
	}

	convResp, err := cli.GM.GetOrCreateConversation(&gmproto.GetOrCreateConversationRequest{
		Numbers: app.NewContactNumbers(phones),
	})
	if err != nil {
		a.HandleGoogleAuthExpiredError(err)
		return fmt.Errorf("get/create group conversation: %w", err)
	}

	conv := convResp.GetConversation()
	if conv == nil {
		return fmt.Errorf("no conversation returned")
	}

	payload := app.BuildSendPayload(conv.GetConversationID(), message, "", "", nil)
	_, err = cli.GM.SendMessage(payload)
	if err != nil {
		a.HandleGoogleAuthExpiredError(err)
		return fmt.Errorf("send: %w", err)
	}

	logger.Info().
		Str("conversation", conv.GetConversationID()).
		Str("recipients", strings.Join(phones, ", ")).
		Msg("Group message sent")
	return nil
}
