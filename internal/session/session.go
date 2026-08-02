// Package session implements the anonymous per-browser session cookie used
// to scope stored conversations/documents/prompts between different
// browsers — independent of and orthogonal to internal/auth's login gate.
// Knowing the shared login token lets you into the app; the session cookie
// just keeps your data separate from anyone else who also knows it.
package session

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"time"
)

const CookieName = "vc_session"

// NewToken generates a fresh random session identifier.
func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// FromRequest reads the session cookie, if present, without validating it
// against any store — callers look it up and treat a miss the same as absent.
func FromRequest(r *http.Request) (string, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

// SetCookie issues the session cookie.
func SetCookie(w http.ResponseWriter, token string, secure bool, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
	})
}
