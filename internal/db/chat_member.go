package db

import (
	"context"

	"github.com/go-faster/errors"

	"github.com/ernado/svetik"
)

// UpsertChatMember inserts or updates a chat member record.
func (db *DB) UpsertChatMember(ctx context.Context, m svetik.ChatMember) error {
	q := psql.Insert("chat_members").
		Columns(
			"chat_id",
			"user_id",
			"username",
			"first_name",
			"last_name",
			"is_admin",
			"is_creator",
			"rank",
		).
		Values(
			m.ChatID,
			m.UserID,
			m.Username,
			m.FirstName,
			m.LastName,
			m.IsAdmin,
			m.IsCreator,
			m.Rank,
		).
		Suffix(`ON CONFLICT (chat_id, user_id) DO UPDATE SET
			username   = EXCLUDED.username,
			first_name = EXCLUDED.first_name,
			last_name  = EXCLUDED.last_name,
			is_admin   = EXCLUDED.is_admin,
			is_creator = EXCLUDED.is_creator,
			rank       = EXCLUDED.rank`)

	sql, args, err := q.ToSql()
	if err != nil {
		return errors.Wrap(err, "build query")
	}

	if _, err := db.pgx.Exec(ctx, sql, args...); err != nil {
		return errors.Wrap(err, "exec")
	}

	return nil
}

// GetChatMember returns a chat member by chat ID and user ID.
func (db *DB) GetChatMember(ctx context.Context, chatID, userID int64) (*svetik.ChatMember, error) {
	q := psql.Select(
		"chat_id",
		"user_id",
		"username",
		"first_name",
		"last_name",
		"is_admin",
		"is_creator",
		"rank",
	).
		From("chat_members").
		Where("chat_id = ? AND user_id = ?", chatID, userID)

	sql, args, err := q.ToSql()
	if err != nil {
		return nil, errors.Wrap(err, "build query")
	}

	var m svetik.ChatMember

	err = db.pgx.QueryRow(ctx, sql, args...).Scan(
		&m.ChatID,
		&m.UserID,
		&m.Username,
		&m.FirstName,
		&m.LastName,
		&m.IsAdmin,
		&m.IsCreator,
		&m.Rank,
	)
	if err != nil {
		return nil, errors.Wrap(err, "scan")
	}

	return &m, nil
}
