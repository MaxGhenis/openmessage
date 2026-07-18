// Package v2read adapts the clean-slate SQLite store to the canonical legacy
// read DTOs. Search is deliberately LIKE-based in R5; FTS/ranking is deferred.
package v2read

import (
	"fmt"
	"time"

	"github.com/maxghenis/openmessage/internal/readsource"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

const sourceMessagePageSize = 200

// Source reads canonical conversation/message DTOs from a v2 store.
type Source struct {
	store       *sqlite.Store
	messages    *sqlite.MessageRepository
	attachments *sqlite.MessageAttachmentRepository
	outbox      *sqlite.OutboxRepository
	reactions   *sqlite.ReactionRepository
}

// New constructs a v2 canonical read source. A nil store is retained as an
// invalid source so callers receive method errors rather than a constructor
// panic; production wiring always passes an opened store.
func New(store *sqlite.Store) *Source {
	source := &Source{store: store}
	if store == nil {
		return source
	}
	source.messages, _ = sqlite.NewMessageRepository(store, time.Now)
	source.attachments, _ = sqlite.NewMessageAttachmentRepository(store, time.Now)
	source.outbox, _ = sqlite.NewOutboxRepository(store, time.Now)
	source.reactions, _ = sqlite.NewReactionRepository(store, time.Now)
	return source
}

func (s *Source) ready() error {
	if s == nil || s.store == nil || s.messages == nil || s.attachments == nil || s.outbox == nil || s.reactions == nil {
		return fmt.Errorf("v2 read source: store is nil")
	}
	return nil
}

var _ readsource.ReadSource = (*Source)(nil)
