package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"vertex-chat/internal/auth"
)

type loginRequest struct {
	Token string `json:"token"`
}

// login is deliberately unauthenticated (it's how a caller obtains auth in
// the first place) — POST /api/login.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	h.setCORS(w, r)
	r.Body = http.MaxBytesReader(w, r.Body, 4096)

	var request loginRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		message := "request body must be valid JSON"
		if errors.Is(err, io.EOF) {
			message = "request body is required"
		}
		h.writeError(w, http.StatusBadRequest, message, "invalid_request_error", nil, "invalid_json")
		return
	}

	if subtle.ConstantTimeCompare([]byte(request.Token), []byte(h.cfg.AuthToken)) != 1 {
		h.writeError(w, http.StatusUnauthorized, "invalid token", "authentication_error", nil, "invalid_token")
		return
	}

	auth.SetCookie(w, h.cfg.AuthToken, h.cfg.CookieSecure, sessionCookieMaxAge)
	h.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// session discovers or creates the anonymous per-browser session for
// headless API callers that want one explicitly — GET /api/session. Runs
// behind requireAuth + withSession, so the cookie is already set by the
// time this handler runs.
func (h *Handler) sessionInfo(w http.ResponseWriter, r *http.Request) {
	h.writeJSON(w, http.StatusOK, map[string]string{"session_id": sessionIDFromContext(r.Context())})
}
