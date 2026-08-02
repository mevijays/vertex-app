package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type SavedPrompt struct {
	ID        string
	SessionID string
	Title     string
	Content   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (s *Store) CreateSavedPrompt(ctx context.Context, sessionID, title, content string) (SavedPrompt, error) {
	id, err := newID()
	if err != nil {
		return SavedPrompt{}, err
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO saved_prompts (id, session_id, title, content, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, sessionID, title, content, now, now,
	)
	if err != nil {
		return SavedPrompt{}, err
	}
	return SavedPrompt{ID: id, SessionID: sessionID, Title: title, Content: content, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Store) ListSavedPrompts(ctx context.Context, sessionID string) ([]SavedPrompt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, title, content, created_at, updated_at FROM saved_prompts WHERE session_id = ? ORDER BY updated_at DESC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SavedPrompt
	for rows.Next() {
		var p SavedPrompt
		if err := rows.Scan(&p.ID, &p.SessionID, &p.Title, &p.Content, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetSavedPrompt(ctx context.Context, id, sessionID string) (SavedPrompt, error) {
	var p SavedPrompt
	err := s.db.QueryRowContext(ctx,
		`SELECT id, session_id, title, content, created_at, updated_at FROM saved_prompts WHERE id = ? AND session_id = ?`,
		id, sessionID,
	).Scan(&p.ID, &p.SessionID, &p.Title, &p.Content, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SavedPrompt{}, ErrNotFound
	}
	if err != nil {
		return SavedPrompt{}, err
	}
	return p, nil
}

func (s *Store) UpdateSavedPrompt(ctx context.Context, id, sessionID, title, content string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE saved_prompts SET title = ?, content = ?, updated_at = ? WHERE id = ? AND session_id = ?`,
		title, content, time.Now().UTC(), id, sessionID,
	)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func (s *Store) DeleteSavedPrompt(ctx context.Context, id, sessionID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM saved_prompts WHERE id = ? AND session_id = ?`, id, sessionID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func (s *Store) CountSavedPrompts(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM saved_prompts WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}
