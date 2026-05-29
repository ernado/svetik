package db

import (
	"context"

	"github.com/go-faster/errors"

	"github.com/ernado/lilith"
)

// AddChatNote inserts a new note for the given chat and returns the created note.
func (db *DB) AddChatNote(ctx context.Context, chatID int64, text string) (*lilith.ChatNote, error) {
	q := psql.Insert("chat_notes").
		Columns("chat_id", "text").
		Values(chatID, text).
		Suffix("RETURNING id, chat_id, text")

	sql, args, err := q.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "build query")
	}

	var note lilith.ChatNote

	if err := db.pgx.QueryRow(ctx, sql, args...).Scan(
		&note.ID,
		&note.ChatID,
		&note.Text,
	); err != nil {
		return nil, errors.Wrap(err, "scan")
	}

	return &note, nil
}

// GetChatNotes returns all notes for the given chat.
func (db *DB) GetChatNotes(ctx context.Context, chatID int64) ([]lilith.ChatNote, error) {
	q := psql.Select("id", "chat_id", "text").
		From("chat_notes").
		Where("chat_id = ?", chatID).
		OrderBy("id ASC")

	sql, args, err := q.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "build query")
	}

	rows, err := db.pgx.Query(ctx, sql, args...)
	if err != nil {
		return nil, errors.Wrap(err, "query")
	}
	defer rows.Close()

	var notes []lilith.ChatNote

	for rows.Next() {
		var note lilith.ChatNote

		if err := rows.Scan(&note.ID, &note.ChatID, &note.Text); err != nil {
			return nil, errors.Wrap(err, "scan")
		}

		notes = append(notes, note)
	}

	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "rows")
	}

	return notes, nil
}

// DeleteChatNote removes a note by its ID and chat ID.
func (db *DB) DeleteChatNote(ctx context.Context, chatID, noteID int64) error {
	q := psql.Delete("chat_notes").
		Where("id = ? AND chat_id = ?", noteID, chatID)

	sql, args, err := q.ToSql()
	if err != nil {
		return errors.Wrap(err, "build query")
	}

	if _, err := db.pgx.Exec(ctx, sql, args...); err != nil {
		return errors.Wrap(err, "exec")
	}

	return nil
}

// TrimChatNotes removes the oldest notes for a chat, keeping at most maxNotes.
func (db *DB) TrimChatNotes(ctx context.Context, chatID int64, maxNotes int) error {
	q := psql.Delete("chat_notes").
		Where(
			"id IN (SELECT id FROM chat_notes WHERE chat_id = ? ORDER BY id ASC LIMIT (GREATEST(0, (SELECT COUNT(*) FROM chat_notes WHERE chat_id = ?) - ?)))",
			chatID, chatID, maxNotes,
		)

	sql, args, err := q.ToSql()
	if err != nil {
		return errors.Wrap(err, "build query")
	}

	if _, err := db.pgx.Exec(ctx, sql, args...); err != nil {
		return errors.Wrap(err, "exec")
	}

	return nil
}
