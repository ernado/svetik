package db

import (
	"context"

	"github.com/go-faster/errors"

	"github.com/ernado/svetik"
)

// SaveMessage inserts a chat message record, doing nothing on conflict.
func (db *DB) SaveMessage(ctx context.Context, msg lilith.Message) error {
	q := psql.Insert("chat_messages").
		Columns(
			"chat_id",
			"message_id",
			"user_id",
			"date",
			"text",
			"is_myself",
			"reply_to_id",
			"reply_to_text",
			"reply_to_myself",
		).
		Values(
			msg.ChatID,
			msg.MessageID,
			msg.UserID,
			msg.Date,
			msg.Text,
			msg.IsMyself,
			msg.ReplyToID,
			msg.ReplyToText,
			msg.ReplyToMyself,
		).
		Suffix("ON CONFLICT (chat_id, message_id) DO NOTHING")

	sql, args, err := q.ToSql()
	if err != nil {
		return errors.Wrap(err, "build query")
	}

	if _, err := db.pgx.Exec(ctx, sql, args...); err != nil {
		return errors.Wrap(err, "exec")
	}

	return nil
}

// GetLastMessages returns the last n messages for a given chat ID up to and including
// lastMessageID, ordered by message_id ascending.
func (db *DB) GetLastMessages(ctx context.Context, chatID int64, n uint64, lastMessageID int64) ([]lilith.Message, error) {
	q := psql.Select(
		"chat_id",
		"message_id",
		"user_id",
		"date",
		"text",
		"is_myself",
		"reply_to_id",
		"reply_to_text",
		"reply_to_myself",
	).
		From("chat_messages").
		Where("chat_id = ? AND message_id <= ?", chatID, lastMessageID).
		OrderBy("message_id DESC").
		Limit(n)

	sql, args, err := q.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "build query")
	}

	rows, err := db.pgx.Query(ctx, sql, args...)
	if err != nil {
		return nil, errors.Wrap(err, "query")
	}
	defer rows.Close()

	var msgs []lilith.Message

	for rows.Next() {
		var msg lilith.Message

		if err := rows.Scan(
			&msg.ChatID,
			&msg.MessageID,
			&msg.UserID,
			&msg.Date,
			&msg.Text,
			&msg.IsMyself,
			&msg.ReplyToID,
			&msg.ReplyToText,
			&msg.ReplyToMyself,
		); err != nil {
			return nil, errors.Wrap(err, "scan")
		}

		msgs = append(msgs, msg)
	}

	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "rows")
	}

	// Reverse to return messages in ascending order.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}

	return msgs, nil
}

// GetMessage returns a message by chat ID and message ID.
func (db *DB) GetMessage(ctx context.Context, chatID, messageID int64) (*lilith.Message, error) {
	q := psql.Select(
		"chat_id",
		"message_id",
		"user_id",
		"date",
		"text",
		"is_myself",
		"reply_to_id",
		"reply_to_text",
		"reply_to_myself",
	).
		From("chat_messages").
		Where("chat_id = ? AND message_id = ?", chatID, messageID)

	sql, args, err := q.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "build query")
	}

	var msg lilith.Message

	err = db.pgx.QueryRow(ctx, sql, args...).Scan(
		&msg.ChatID,
		&msg.MessageID,
		&msg.UserID,
		&msg.Date,
		&msg.Text,
		&msg.IsMyself,
		&msg.ReplyToID,
		&msg.ReplyToText,
		&msg.ReplyToMyself,
	)
	if err != nil {
		return nil, errors.Wrap(err, "scan")
	}

	return &msg, nil
}
