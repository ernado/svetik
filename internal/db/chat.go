package db

import (
	"context"

	"github.com/go-faster/errors"

	"github.com/ernado/lilith"
)

// UpsertChat inserts or updates a chat record.
func (db *DB) UpsertChat(ctx context.Context, chat lilith.Chat) error {
	q := psql.Insert("chat").
		Columns("id", "info").
		Values(chat.ID, chat.Info).
		Suffix("ON CONFLICT (id) DO UPDATE SET info = EXCLUDED.info")

	sql, args, err := q.ToSql()
	if err != nil {
		return errors.Wrap(err, "build query")
	}

	if _, err := db.pgx.Exec(ctx, sql, args...); err != nil {
		return errors.Wrap(err, "exec")
	}

	return nil
}

// GetChat returns a chat by ID.
func (db *DB) GetChat(ctx context.Context, id int64) (*lilith.Chat, error) {
	q := psql.Select("id", "info", "last_notes_msg_id").
		From("chat").
		Where("id = ?", id)

	sql, args, err := q.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "build query")
	}

	var chat lilith.Chat

	err = db.pgx.QueryRow(ctx, sql, args...).Scan(&chat.ID, &chat.Info, &chat.LastNotesMsgID)
	if err != nil {
		return nil, errors.Wrap(err, "scan")
	}

	return &chat, nil
}

// SetLastNotesMsgID updates the last_notes_msg_id for a chat atomically,
// only if the new value is greater than the stored one.
// It returns the value that was stored before the update.
func (db *DB) SetLastNotesMsgID(ctx context.Context, chatID int64, msgID int64) (prev int64, err error) {
	sql := `UPDATE chat
	        SET last_notes_msg_id = $1
	        WHERE id = $2 AND last_notes_msg_id < $1
	        RETURNING (SELECT last_notes_msg_id FROM chat WHERE id = $2)`

	// Use a raw query: fetch prev value first, then conditionally update.
	// Simpler approach: SELECT then UPDATE in one round-trip using a CTE.
	const q = `
WITH prev AS (SELECT last_notes_msg_id FROM chat WHERE id = $2),
     upd  AS (
         UPDATE chat
         SET last_notes_msg_id = $1
         WHERE id = $2 AND last_notes_msg_id < $1
     )
SELECT last_notes_msg_id FROM prev`
	_ = sql

	row := db.pgx.QueryRow(ctx, q, msgID, chatID)
	if err = row.Scan(&prev); err != nil {
		return 0, errors.Wrap(err, "scan")
	}

	return prev, nil
}
