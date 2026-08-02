package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type Document struct {
	ID             string
	ConversationID string
	SessionID      string
	Filename       string
	MimeType       string
	SizeBytes      int64
	Status         string
	ExtractedText  string
	Error          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (s *Store) CreateDocument(ctx context.Context, conversationID, sessionID, filename, mimeType string, size int64) (Document, error) {
	id, err := newID()
	if err != nil {
		return Document{}, err
	}
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO documents (id, conversation_id, session_id, filename, mime_type, size_bytes, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'processing', ?, ?)`,
		id, conversationID, sessionID, filename, mimeType, size, now, now,
	)
	if err != nil {
		return Document{}, err
	}
	return Document{
		ID: id, ConversationID: conversationID, SessionID: sessionID,
		Filename: filename, MimeType: mimeType, SizeBytes: size,
		Status: "processing", CreatedAt: now, UpdatedAt: now,
	}, nil
}

func (s *Store) MarkDocumentReady(ctx context.Context, id, extractedText string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE documents SET status = 'ready', extracted_text = ?, updated_at = ? WHERE id = ?`,
		extractedText, time.Now().UTC(), id,
	)
	return err
}

func (s *Store) MarkDocumentFailed(ctx context.Context, id, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE documents SET status = 'failed', error = ?, updated_at = ? WHERE id = ?`,
		errMsg, time.Now().UTC(), id,
	)
	return err
}

func (s *Store) ListDocuments(ctx context.Context, conversationID string) ([]Document, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, conversation_id, session_id, filename, mime_type, size_bytes, status, extracted_text, error, created_at, updated_at
		FROM documents WHERE conversation_id = ? ORDER BY created_at ASC`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.ConversationID, &d.SessionID, &d.Filename, &d.MimeType, &d.SizeBytes, &d.Status, &d.ExtractedText, &d.Error, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetDocument(ctx context.Context, id, conversationID string) (Document, error) {
	var d Document
	err := s.db.QueryRowContext(ctx, `
		SELECT id, conversation_id, session_id, filename, mime_type, size_bytes, status, extracted_text, error, created_at, updated_at
		FROM documents WHERE id = ? AND conversation_id = ?`, id, conversationID,
	).Scan(&d.ID, &d.ConversationID, &d.SessionID, &d.Filename, &d.MimeType, &d.SizeBytes, &d.Status, &d.ExtractedText, &d.Error, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, err
	}
	return d, nil
}

func (s *Store) DeleteDocument(ctx context.Context, id, conversationID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM documents WHERE id = ? AND conversation_id = ?`, id, conversationID)
	if err != nil {
		return err
	}
	return checkAffected(res)
}

func (s *Store) CountDocuments(ctx context.Context, conversationID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents WHERE conversation_id = ?`, conversationID).Scan(&n)
	return n, err
}

// DocumentsContext concatenates ready documents' extracted text (each
// prefixed with its filename as a heading), truncated to maxChars total —
// this is what gets appended to the system instruction sent to Gemini.
func (s *Store) DocumentsContext(ctx context.Context, conversationID string, maxChars int) (string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT filename, extracted_text FROM documents WHERE conversation_id = ? AND status = 'ready' ORDER BY created_at ASC`,
		conversationID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var filename, text string
		if err := rows.Scan(&filename, &text); err != nil {
			return "", err
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("### ")
		b.WriteString(filename)
		b.WriteString("\n")
		b.WriteString(text)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	out := b.String()
	if len(out) > maxChars {
		out = out[:maxChars]
	}
	return out, nil
}
