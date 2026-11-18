package signaldesktop

import "github.com/maxghenis/openmessage/internal/ingest"

// HistorySnapshot is the codec input for one chunk. account is the Signal
// account's own E.164 number from v2.
func (c Chunk) HistorySnapshot(account string) ingest.SignalDesktopHistorySnapshot {
	snapshot := ingest.SignalDesktopHistorySnapshot{
		AccountAddress:       account,
		RemoteConversationID: c.Chat.RemoteID,
		Title:                c.Chat.Title,
	}
	for _, participant := range c.Chat.Participants {
		snapshot.Participants = append(snapshot.Participants, ingest.SignalDesktopHistoryParticipant{
			Address: participant.Address, Name: participant.Name, IsSelf: participant.IsSelf,
		})
	}
	for _, message := range c.Messages {
		captured := ingest.SignalDesktopHistoryMessage{
			AuthorAddress: message.AuthorAddress, AuthorName: message.AuthorName,
			Outgoing: message.Outgoing, SentAtMS: message.SentAtMS, Body: message.Body,
			Sticker: message.Sticker, Contact: message.Contact, Payment: message.Payment,
			Poll: message.Poll, Preview: message.Preview, ViewOnce: message.ViewOnce,
			StoryReply: message.StoryReply,
		}
		for _, attachment := range message.Attachments {
			captured.Attachments = append(captured.Attachments, ingest.SignalDesktopHistoryAttachment{
				ContentType: attachment.ContentType, FileName: attachment.FileName,
			})
		}
		for _, reaction := range message.Reactions {
			captured.Reactions = append(captured.Reactions, ingest.SignalDesktopHistoryReaction{
				ActorAddress: reaction.ActorAddress, ActorName: reaction.ActorName,
				IsSelf: reaction.IsSelf, Emoji: reaction.Emoji, TimestampMS: reaction.TimestampMS,
			})
		}
		snapshot.Messages = append(snapshot.Messages, captured)
	}
	return snapshot
}
