package store

import "errors"

// ErrNotFound is returned both when a row genuinely doesn't exist and when
// it exists but belongs to a different session — deliberately the same
// code path, so callers can never distinguish "doesn't exist" from "not
// yours" and accidentally leak cross-session existence information.
var ErrNotFound = errors.New("not found")
