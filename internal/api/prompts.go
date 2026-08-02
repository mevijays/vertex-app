package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type savedPromptRequest struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

type savedPromptResponse struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (h *Handler) listSavedPrompts(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())
	prompts, err := h.store.ListSavedPrompts(r.Context(), sessionID)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not list saved prompts", "server_error", nil, "list_failed")
		return
	}

	out := make([]savedPromptResponse, 0, len(prompts))
	for _, p := range prompts {
		out = append(out, savedPromptResponse{ID: p.ID, Title: p.Title, Content: p.Content, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt})
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"prompts": out})
}

func (h *Handler) createSavedPrompt(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())

	n, err := h.store.CountSavedPrompts(r.Context(), sessionID)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not save prompt", "server_error", nil, "save_failed")
		return
	}
	if n >= h.cfg.MaxSavedPromptsPerSession {
		h.writeError(w, http.StatusTooManyRequests, "saved prompt limit reached for this session", "invalid_request_error", nil, "prompt_limit")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var request savedPromptRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		h.writeError(w, http.StatusBadRequest, "request body must be valid JSON", "invalid_request_error", nil, "invalid_json")
		return
	}
	if strings.TrimSpace(request.Content) == "" {
		h.writeError(w, http.StatusBadRequest, "content must not be empty", "invalid_request_error", ptr("content"), "invalid_value")
		return
	}
	if strings.TrimSpace(request.Title) == "" {
		request.Title = deriveTitle(request.Content)
	}

	p, err := h.store.CreateSavedPrompt(r.Context(), sessionID, request.Title, request.Content)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not save prompt", "server_error", nil, "save_failed")
		return
	}
	h.writeJSON(w, http.StatusCreated, savedPromptResponse{ID: p.ID, Title: p.Title, Content: p.Content, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt})
}

func (h *Handler) updateSavedPrompt(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())
	id := r.PathValue("id")

	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var request savedPromptRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		h.writeError(w, http.StatusBadRequest, "request body must be valid JSON", "invalid_request_error", nil, "invalid_json")
		return
	}
	if strings.TrimSpace(request.Content) == "" {
		h.writeError(w, http.StatusBadRequest, "content must not be empty", "invalid_request_error", ptr("content"), "invalid_value")
		return
	}
	if strings.TrimSpace(request.Title) == "" {
		request.Title = deriveTitle(request.Content)
	}

	if err := h.store.UpdateSavedPrompt(r.Context(), id, sessionID, request.Title, request.Content); err != nil {
		h.writeStoreError(w, err, "saved prompt not found")
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) deleteSavedPrompt(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())
	id := r.PathValue("id")
	if err := h.store.DeleteSavedPrompt(r.Context(), id, sessionID); err != nil {
		h.writeStoreError(w, err, "saved prompt not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
