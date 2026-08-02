package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Conversation struct {
	ID                string
	SessionID         string
	Title             string
	SystemInstruction string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ConversationSummary struct {
	ID            string
	Title         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	MessageCount  int
	DocumentCount int
}

func (s *Store) CreateConversation(ctx context.Context, sessionID, title, system string) (Conversation, error) {
	id, err := newID()
	if err != nil {
		return Conversation{}, err
	}
	return s.CreateConversationWithID(ctx, id, sessionID, title, system)
}

// CreateConversationWithID creates a conversation under a caller-supplied
// id — used when a /v1/chat/completions caller supplies its own
// conversation_id (a self-generated thread id, common idempotency-key
// style) so the id it gets back matches the one it sent, letting it
// reference the same conversation on every subsequent call.
func (s *Store) CreateConversationWithID(ctx context.Context, id, sessionID, title, system string) (Conversation, error) {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO conversations (id, session_id, title, system_instruction, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, sessionID, title, system, now, now,
	)
	if err != nil {
		return Conversation{}, err
	}
	return Conversation{ID: id, SessionID: sessionID, Title: title, SystemInstruction: system, CreatedAt: now, UpdatedAt: now}, nil
}

// GetConversation looks up a conversation scoped to sessionID — a row that
// exists but belongs to a different session returns ErrNotFound, same as a
// row that doesn't exist at all (see errors.go).
func (s *Store) GetConversation(ctx context.Context, id, sessionID string) (Conversation, error) {
	return s.getConversation(ctx, `id = ? AND session_id = ?`, id, sessionID)
}

// GetConversationUnscoped looks up a conversation by id alone, regardless
// of owning session — used only for the cookie-less API-caller path, where
// conversation_id itself is the bearer capability (see internal/api/context.go).
func (s *Store) GetConversationUnscoped(ctx context.Context, id string) (Conversation, error) {
	return s.getConversation(ctx, `id = ?`, id)
}

func (s *Store) getConversation(ctx context.Context, where string, args ...any) (Conversation, error) {
	var c Conversation
	query := `SELECT id, session_id, title, system_instruction, created_at, updated_at FROM conversations WHERE ` + where
	err := s.db.QueryRowContext(ctx, query, args...).
		Scan(&c.ID, &c.SessionID, &c.Title, &c.SystemInstruction, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, err
	}
	return c, nil
}

func (s *Store) ListConversations(ctx context.Context, sessionID string, limit int) ([]ConversationSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.title, c.created_at, c.updated_at,
		       (SELECT COUNT(*) FROM messages m WHERE m.conversation_id = c.id) AS message_count,
		       (SELECT COUNT(*) FROM documents d WHERE d.conversation_id = c.id) AS document_count
		FROM conversations c
		WHERE c.session_id = ?
		ORDER BY c.updated_at DESC
		LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConversationSummary
	for rows.Next() {
		var cs ConversationSummary
		if err := rows.Scan(&cs.ID, &cs.Title, &cs.CreatedAt, &cs.UpdatedAt, &cs.MessageCount, &cs.DocumentCount); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

func (s *Store) RenameConversation(ctx context.Context, id, sessionID, title string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET title = ?, updated_at = ? WHERE id = ? AND session_id = ?`,
		title, time.Now().UTC(), id, sessionID,
	)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func (s *Store) UpdateSystemInstruction(ctx context.Context, id, sessionID, system string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET system_instruction = ?, updated_at = ? WHERE id = ? AND session_id = ?`,
		system, time.Now().UTC(), id, sessionID,
	)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

// TouchConversation updates updated_at unconditionally (no session scoping
// — called internally after storing new messages, where the caller has
// already resolved the conversation via a scoped or unscoped lookup).
func (s *Store) TouchConversation(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = ? WHERE id = ?`, time.Now().UTC(), id)
	return err
}

func (s *Store) DeleteConversation(ctx context.Context, id, sessionID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ? AND session_id = ?`, id, sessionID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func (s *Store) CountConversations(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}

func checkAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
