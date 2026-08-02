package store

import (
	"context"
	"time"

	"vertex-chat/internal/chat"
)

// AppendMessages stores one or more messages for a conversation, in order,
// inside a single transaction.
func (s *Store) AppendMessages(ctx context.Context, conversationID string, msgs []chat.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO messages (conversation_id, role, content, created_at) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC()
	for _, m := range msgs {
		if _, err := stmt.ExecContext(ctx, conversationID, m.Role, m.Content, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecentMessages returns the last `limit` messages for a conversation,
// oldest first, ready to feed directly into a chat.Request.
func (s *Store) RecentMessages(ctx context.Context, conversationID string, limit int) ([]chat.Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content FROM (
			SELECT role, content, id FROM messages
			WHERE conversation_id = ?
			ORDER BY id DESC
			LIMIT ?
		) ORDER BY id ASC`, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []chat.Message
	for rows.Next() {
		var m chat.Message
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
