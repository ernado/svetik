package lilith

import "time"

// Chat represents a Telegram chat.
type Chat struct {
	ID             int64
	Info           string
	LastNotesMsgID int64
	// Model is the per-chat model override. Empty means use the default model.
	Model string
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
}
