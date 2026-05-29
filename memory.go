package lilith

import "context"

// Memory is the chat-notes layer. It owns the policy for when and how notes are
// generated and persisted; the orchestrator delegates note maintenance to it.
type Memory interface {
	// Maintain runs the per-message notes policy for a chat: it decides whether
	// to generate a full snapshot or a single-message note and persists the
	// result. It is safe to call concurrently for the same chat.
	Maintain(ctx context.Context, chatID, currentMsgID int64, msg Message) error
	// Notes returns the current notes for a chat, for use when building reply
	// context.
	Notes(ctx context.Context, chatID int64) ([]ChatNote, error)
}
