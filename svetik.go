package svetik

import "time"

// Chat represents a Telegram chat.
type Chat struct {
	ID   int64
	Info string
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

// Context is the per-message payload sent to the LLM as a user message.
type Context struct {
	Message *Message    `json:"message"`
	Self    *Self       `json:"self"`
	User    *ChatMember `json:"user"`
}

// Message represents a Telegram chat message.
type Message struct {
	ChatID        int64     `json:"chat_id"`
	MessageID     int64     `json:"message_id"`
	UserID        int64     `json:"user_id"`
	Date          time.Time `json:"date"`
	Text          string    `json:"text"`
	IsMyself      bool      `json:"is_myself"`
	ReplyToID     *int64    `json:"reply_to_id"`
	ReplyToText   *string   `json:"reply_to_text"`
	ReplyToMyself *bool     `json:"reply_to_myself"`
}
