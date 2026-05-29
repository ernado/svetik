package lilith

import "context"

// DB is the database interface.
type DB interface {
	UpsertChat(ctx context.Context, chat Chat) error
	GetChat(ctx context.Context, id int64) (*Chat, error)
	SetLastNotesMsgID(ctx context.Context, chatID int64, msgID int64) (prev int64, err error)
	SaveMessage(ctx context.Context, msg Message) error
	GetMessage(ctx context.Context, chatID, messageID int64) (*Message, error)
	GetLastMessages(ctx context.Context, chatID int64, n uint64, lastMessageID int64) ([]Message, error)
	CountMessagesSince(ctx context.Context, chatID, sinceMessageID, upToMessageID int64) (int64, error)
	UpsertChatMember(ctx context.Context, m ChatMember) error
	GetChatMember(ctx context.Context, chatID, userID int64) (*ChatMember, error)
	GetChatMembers(ctx context.Context, chatID int64) ([]ChatMember, error)
	AddChatNote(ctx context.Context, chatID int64, text string) (*ChatNote, error)
	GetChatNotes(ctx context.Context, chatID int64) ([]ChatNote, error)
	DeleteChatNote(ctx context.Context, chatID, noteID int64) error
	TrimChatNotes(ctx context.Context, chatID int64, maxNotes int) error
	SetChatModel(ctx context.Context, chatID int64, model string) error
	Lobotomy(ctx context.Context, chatID int64) error
}
