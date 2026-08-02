package store

import (
	"crypto/rand"
	"encoding/hex"
)

// newID generates an opaque, unguessable identifier for conversations,
// documents, and saved prompts (128 bits of randomness — conversations in
// particular double as a bearer capability for cookie-less API callers,
// see internal/api/context.go, so this needs to be hard to guess, not just
// unique).
func newID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
