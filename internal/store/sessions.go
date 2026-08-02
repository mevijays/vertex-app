package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Session struct {
	ID         string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// CreateSession persists a session row for an already-generated id — token
// generation is internal/session's job (it must match the cookie value),
// this just stores it.
func (s *Store) CreateSession(ctx context.Context, id string) (Session, error) {
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, last_seen_at) VALUES (?, ?, ?)`,
		id, now, now,
	); err != nil {
		return Session{}, err
	}
	return Session{ID: id, CreatedAt: now, LastSeenAt: now}, nil
}

func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx,
		`SELECT id, created_at, last_seen_at FROM sessions WHERE id = ?`, id,
	).Scan(&sess.ID, &sess.CreatedAt, &sess.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	return sess, nil
}
