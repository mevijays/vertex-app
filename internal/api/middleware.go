package api

import (
	"context"
	"net/http"
	"time"

	"vertex-chat/internal/auth"
	"vertex-chat/internal/session"
)

type contextKey int

const sessionIDKey contextKey = iota

const sessionCookieMaxAge = 365 * 24 * time.Hour

// requireAuth gates a handler behind the shared static token, checked via
// either the Authorization: Bearer header (API/OpenAI-SDK callers) or the
// vc_auth cookie (browser UI) — both compared against the same secret.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if auth.FromRequest(r, h.cfg.AuthToken) {
			next(w, r)
			return
		}
		h.writeError(w, http.StatusUnauthorized, "authentication required", "authentication_error", nil, "unauthorized")
	}
}

// withSession resolves the anonymous per-browser session, minting and
// setting a cookie on first visit. Used for UI-facing /api/* endpoints,
// where the browser is expected to persist the cookie across requests.
func (h *Handler) withSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if token, ok := session.FromRequest(r); ok {
			if _, err := h.store.GetSession(ctx, token); err == nil {
				next(w, r.WithContext(context.WithValue(ctx, sessionIDKey, token)))
				return
			}
			// stale or tampered cookie — fall through and mint a fresh session
		}

		token, err := session.NewToken()
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, "could not create session", "server_error", nil, "session_failed")
			return
		}
		if _, err := h.store.CreateSession(ctx, token); err != nil {
			h.writeError(w, http.StatusInternalServerError, "could not create session", "server_error", nil, "session_failed")
			return
		}
		session.SetCookie(w, token, h.cfg.CookieSecure, sessionCookieMaxAge)
		next(w, r.WithContext(context.WithValue(ctx, sessionIDKey, token)))
	}
}

// withOptionalSession attaches an existing session to the context if a
// valid cookie is present, but never mints one or sets a cookie. Used for
// /v1/chat/completions: a cookie-less caller is already authenticated via
// the shared token (requireAuth ran first), so conversation_id itself
// becomes the operating capability — see resolveConversation.
func (h *Handler) withOptionalSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if token, ok := session.FromRequest(r); ok {
			if _, err := h.store.GetSession(ctx, token); err == nil {
				ctx = context.WithValue(ctx, sessionIDKey, token)
			}
		}
		next(w, r.WithContext(ctx))
	}
}

func sessionIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionIDKey).(string)
	return v
}
