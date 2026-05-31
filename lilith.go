package lilith

import "time"

// ChatType identifies whether a chat is a Telegram channel/supergroup or a
// regular group chat.
type ChatType string

const (
	// ChatTypeChannel represents a Telegram channel or supergroup.
	ChatTypeChannel ChatType = "channel"
	// ChatTypeChat represents a regular Telegram group chat.
	ChatTypeChat ChatType = "chat"
	// ChatTypePrivate represents a private (one-on-one) chat with a user.
	ChatTypePrivate ChatType = "private"
)

// Chat represents a Telegram chat.
type Chat struct {
	ID             int64
	Info           string
	LastNotesMsgID int64
	// Model is the per-chat model override. Empty means use the default model.
	Model string
	// AccessHash is the Telegram access hash required to address channels.
	// Zero for regular group chats.
	AccessHash int64
	// Type distinguishes channels/supergroups from regular group chats.
	// Defaults to ChatTypeChannel.
	Type ChatType
}

// ChatNote represents a note attached to a chat.
type ChatNote struct {
	ID     int64  `json:"id"`
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

// ChatMember represents a member of a Telegram chat.
type ChatMember struct {
	ChatID    int64  `json:"chat_id"`
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	IsAdmin   bool   `json:"is_admin"`
	IsCreator bool   `json:"is_creator"`
	Rank      string `json:"rank"`
}

// Self represents the bot's identity in a chat.
type Self struct {
	Nickname string `json:"nickname"`
	Name     string `json:"name"`
	Rank     string `json:"rank"`
}

type UserMetadata struct {
	IsBot bool `json:"is_bot"`
}

// Context is the per-message payload sent to the LLM as a user message.
type Context struct {
	Message      *Message      `json:"message,omitempty"`
	Self         *Self         `json:"self,omitempty"`
	User         *ChatMember   `json:"user,omitempty"`
	UserMetadata *UserMetadata `json:"user_metadata,omitempty"`
}

// Message represents a Telegram chat message.
type Message struct {
	ChatID        int64     `json:"chat_id"`
	MessageID     int64     `json:"message_id"`
	UserID        int64     `json:"user_id"`
	Date          time.Time `json:"date"`
	Text          string    `json:"text"`
	IsMyself      bool      `json:"is_myself"`
	ReplyToID     *int64    `json:"reply_to_id,omitempty"`
	ReplyToText   *string   `json:"reply_to_text,omitempty"`
	ReplyToMyself *bool     `json:"reply_to_myself,omitempty"`

	// MessageThreadID is Telegram's native forum topic id. It is distinct from
	// the logical ThreadID and is used only to scope logical threads so they
	// never cross forum-topic boundaries.
	MessageThreadID *int64 `json:"message_thread_id,omitempty"`
	// ThreadID is the stable identifier of the logical conversation thread. It
	// is the root message id of the thread; all messages in the same
	// conversation share it.
	ThreadID *int64 `json:"thread_id,omitempty"`
	// ThreadRootMessageID is the message_id of the message that started the thread.
	ThreadRootMessageID *int64 `json:"thread_root_message_id,omitempty"`
	// ThreadParentMessageID is the message this one is a direct continuation of.
	ThreadParentMessageID *int64 `json:"thread_parent_message_id,omitempty"`
	// ThreadSource labels how the thread was derived (for debugging/analytics).
	ThreadSource *string `json:"thread_source,omitempty"`
}
