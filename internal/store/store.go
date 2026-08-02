// Package store provides SQLite-backed persistence for conversations,
// messages, uploaded-document context, and saved prompts, scoped by
// anonymous session (see internal/session). Uses modernc.org/sqlite — a
// pure-Go, cgo-free driver — so it works unmodified with the app's
// CGO_ENABLED=0 static-binary Docker build.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Store wraps the SQLite connection pool. All exported query methods live
// in per-concern files (sessions.go, conversations.go, ...).
type Store struct {
	db *sql.DB
}

// Open creates the database file's parent directory if needed, opens a
// connection pool with WAL journaling and foreign keys enabled, and applies
// the schema (idempotent — CREATE TABLE/INDEX IF NOT EXISTS throughout).
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	// temp_store=MEMORY keeps SQLite from ever needing a writable /tmp for
	// sort/join spills, which matters under the container's
	// readOnlyRootFilesystem — the only writable paths are the DB file's own
	// directory and /tmp (mounted defensively regardless, see k8s/deployment.yaml).
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_journal_mode=WAL&_foreign_keys=on&_pragma=temp_store(MEMORY)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// WAL allows concurrent readers with one writer; busy_timeout queues
	// writers instead of erroring immediately. If "database is locked"
	// errors ever show up under real load, the documented fallback is
	// db.SetMaxOpenConns(1) to fully serialize access.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	return &Store{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}
