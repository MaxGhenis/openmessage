package google

import (
	"context"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// gmCompat adapts upstream libgm (v0.2611, context-first methods) to the
// context-free seams this package was written against.
type gmCompat struct{ *libgm.Client }

func (c gmCompat) Connect() error { return c.Client.Connect(context.Background()) }

func (c gmCompat) GetConversation(id string) (*gmproto.Conversation, error) {
	return c.Client.GetConversation(context.Background(), id)
}

func (c gmCompat) SendMessage(p *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
	return c.Client.SendMessage(context.Background(), p)
}

func (c gmCompat) SendReaction(p *gmproto.SendReactionRequest) (*gmproto.SendReactionResponse, error) {
	return c.Client.SendReaction(context.Background(), p)
}

func (c gmCompat) MarkRead(conversationID, messageID string) error {
	return c.Client.MarkRead(context.Background(), conversationID, messageID)
}

func (c gmCompat) UploadMedia(data []byte, filename, mime string) (*gmproto.MediaContent, error) {
	return c.Client.UploadMedia(context.Background(), data, filename, mime)
}

// NotifyDittoActivity stands in for the fork's public phone ping, which
// upstream keeps private. IsBugleDefault is a cheap request the phone itself
// answers, so a reply is the same liveness evidence. The channel receives one
// message on success and is closed without a value on failure.
func (c gmCompat) NotifyDittoActivity() (<-chan *libgm.IncomingRPCMessage, error) {
	if c.Client == nil || !c.Client.IsConnected() {
		return nil, libgm.ErrClientIsNil
	}
	ch := make(chan *libgm.IncomingRPCMessage, 1)
	go func() {
		if _, err := c.Client.IsBugleDefault(context.Background()); err != nil {
			close(ch)
			return
		}
		ch <- &libgm.IncomingRPCMessage{}
	}()
	return ch, nil
}
