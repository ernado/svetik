package svetik

import "context"

// DB is the database interface.
type DB interface {
	UpsertChat(ctx context.Context, chat Chat) error
	SaveMessage(ctx context.Context, msg Message) error
	GetChat(ctx context.Context, id int64) (*Chat, error)
	GetMessage(ctx context.Context, chatID, messageID int64) (*Message, error)
	GetLastMessages(ctx context.Context, chatID int64, n uint64) ([]Message, error)
	UpsertChatMember(ctx context.Context, m ChatMember) error
	GetChatMember(ctx context.Context, chatID, userID int64) (*ChatMember, error)
}
