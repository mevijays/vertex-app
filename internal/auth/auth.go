// Package auth implements the static shared-token login gate: the cookie
// never carries the raw secret, only an HMAC derived from it, so rotating
// AUTH_TOKEN (a new Deployment rollout) invalidates every existing login
// with no session store or cleanup needed.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"time"
)

const CookieName = "vc_auth"

const signedMessage = "vertex-chat-auth-v1"

// Sign derives the cookie value for a given shared secret.
func Sign(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signedMessage))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether cookieValue matches the signature for secret.
func Verify(secret, cookieValue string) bool {
	expected := Sign(secret)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(cookieValue)) == 1
}

// VerifyToken reports whether the raw candidate token equals secret
// (used for the Authorization: Bearer path, which carries the raw token,
// not a signed cookie value).
func VerifyToken(secret, candidate string) bool {
	if candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(candidate)) == 1
}

// SetCookie issues the signed auth cookie after a successful login.
func SetCookie(w http.ResponseWriter, secret string, secure bool, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    Sign(secret),
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
	})
}

// FromRequest checks whether the request already carries a valid auth
// cookie or bearer token for the given secret.
func FromRequest(r *http.Request, secret string) bool {
	if bearer, ok := bearerToken(r); ok && VerifyToken(secret, bearer) {
		return true
	}
	if c, err := r.Cookie(CookieName); err == nil && Verify(secret, c.Value) {
		return true
	}
	return false
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return "", false
	}
	return h[len(prefix):], true
}
