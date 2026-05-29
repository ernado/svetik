package lilith

import "context"

// ResponseRequest is the domain payload for generating a chat reply. It carries
// only domain types so the AI layer (and its LLM SDK) stays out of the root
// package, mirroring how DB depends on nothing but domain types.
type ResponseRequest struct {
	// CurrentTime is a preformatted, human-readable timestamp injected into the
	// system prompt (e.g. "29 May 26 14:00 +0300, пятница.").
	CurrentTime string
	// Notes are the chat notes to ground the reply.
	Notes []ChatNote
	// Members is the chat member roster.
	Members []ChatMember
	// Self is the bot's own identity in the chat.
	Self Self
	// History is the prior conversation, oldest first, excluding Current.
	History []Context
	// Current is the message being responded to.
	Current Context
	// ImageURL, when non-empty, is attached to the current message as image input.
	ImageURL string
	// Typing, when non-nil, is invoked periodically to keep the chat "typing"
	// indicator alive during long completions. The caller owns the side effect.
	Typing func(context.Context) error
}

// ResponseResult is the outcome of a Respond call.
type ResponseResult struct {
	// Text is the reply text. It may be empty when the model produced only tool
	// calls (e.g. a reaction) and no message.
	Text string
	// Reactions are canonical emoji the model chose to react with. The caller
	// applies them to the current message.
	Reactions []string
}

// AI is the language-model gateway. Implementations are stateless with respect
// to chat storage: all required context is passed in the request, and any
// persistence is the caller's responsibility.
type AI interface {
	// Respond runs the completion loop (including tool calls) and returns the
	// reply text and any reactions chosen by the model.
	Respond(ctx context.Context, req ResponseRequest) (*ResponseResult, error)
	// GenerateNotes summarizes messages into a fresh notes snapshot, given any
	// existing notes. It returns the generated text, which may be empty.
	GenerateNotes(ctx context.Context, existing []ChatNote, messages []Message) (string, error)
	// GenerateNote decides whether a single message is worth noting and returns
	// the note text, given any existing notes. The returned text may be empty.
	GenerateNote(ctx context.Context, existing []ChatNote, msg Message) (string, error)
}
