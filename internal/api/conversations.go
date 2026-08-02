package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"vertex-chat/internal/chat"
	"vertex-chat/internal/store"
)

type createConversationRequest struct {
	Title             string `json:"title"`
	SystemInstruction string `json:"system_instruction"`
}

type updateConversationRequest struct {
	Title             *string `json:"title"`
	SystemInstruction *string `json:"system_instruction"`
}

type conversationSummaryResponse struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	MessageCount  int       `json:"message_count"`
	DocumentCount int       `json:"document_count"`
}

type conversationDetailResponse struct {
	ID                string             `json:"id"`
	Title             string             `json:"title"`
	SystemInstruction string             `json:"system_instruction"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
	Messages          []chat.Message     `json:"messages"`
	Documents         []documentResponse `json:"documents"`
}

func (h *Handler) listConversations(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())
	list, err := h.store.ListConversations(r.Context(), sessionID, h.cfg.MaxConversationsPerSession)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not list conversations", "server_error", nil, "list_failed")
		return
	}

	out := make([]conversationSummaryResponse, 0, len(list))
	for _, c := range list {
		out = append(out, conversationSummaryResponse{
			ID: c.ID, Title: c.Title, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
			MessageCount: c.MessageCount, DocumentCount: c.DocumentCount,
		})
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"conversations": out})
}

func (h *Handler) createConversation(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())

	n, err := h.store.CountConversations(r.Context(), sessionID)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not create conversation", "server_error", nil, "create_failed")
		return
	}
	if n >= h.cfg.MaxConversationsPerSession {
		h.writeError(w, http.StatusTooManyRequests, "conversation limit reached for this session", "invalid_request_error", nil, "conversation_limit")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var request createConversationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		h.writeError(w, http.StatusBadRequest, "request body must be valid JSON", "invalid_request_error", nil, "invalid_json")
		return
	}

	conv, err := h.store.CreateConversation(r.Context(), sessionID, request.Title, request.SystemInstruction)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not create conversation", "server_error", nil, "create_failed")
		return
	}
	h.writeJSON(w, http.StatusCreated, conversationDetailResponse{
		ID: conv.ID, Title: conv.Title, SystemInstruction: conv.SystemInstruction,
		CreatedAt: conv.CreatedAt, UpdatedAt: conv.UpdatedAt,
		Messages: []chat.Message{}, Documents: []documentResponse{},
	})
}

func (h *Handler) getConversation(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())
	id := r.PathValue("id")

	conv, err := h.store.GetConversation(r.Context(), id, sessionID)
	if err != nil {
		h.writeStoreError(w, err, "conversation not found")
		return
	}
	messages, err := h.store.RecentMessages(r.Context(), conv.ID, h.cfg.MaxContextMessages)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not load conversation", "server_error", nil, "load_failed")
		return
	}
	docs, err := h.store.ListDocuments(r.Context(), conv.ID)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not load conversation", "server_error", nil, "load_failed")
		return
	}

	docResp := make([]documentResponse, 0, len(docs))
	for _, d := range docs {
		docResp = append(docResp, toDocumentResponse(d))
	}

	h.writeJSON(w, http.StatusOK, conversationDetailResponse{
		ID: conv.ID, Title: conv.Title, SystemInstruction: conv.SystemInstruction,
		CreatedAt: conv.CreatedAt, UpdatedAt: conv.UpdatedAt,
		Messages: messages, Documents: docResp,
	})
}

func (h *Handler) updateConversation(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())
	id := r.PathValue("id")

	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var request updateConversationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		h.writeError(w, http.StatusBadRequest, "request body must be valid JSON", "invalid_request_error", nil, "invalid_json")
		return
	}

	if request.Title != nil {
		if err := h.store.RenameConversation(r.Context(), id, sessionID, *request.Title); err != nil {
			h.writeStoreError(w, err, "conversation not found")
			return
		}
	}
	if request.SystemInstruction != nil {
		if err := h.store.UpdateSystemInstruction(r.Context(), id, sessionID, *request.SystemInstruction); err != nil {
			h.writeStoreError(w, err, "conversation not found")
			return
		}
	}
	h.writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) deleteConversation(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())
	id := r.PathValue("id")
	if err := h.store.DeleteConversation(r.Context(), id, sessionID); err != nil {
		h.writeStoreError(w, err, "conversation not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) writeStoreError(w http.ResponseWriter, err error, notFoundMessage string) {
	if errors.Is(err, store.ErrNotFound) {
		h.writeError(w, http.StatusNotFound, notFoundMessage, "invalid_request_error", nil, "not_found")
		return
	}
	h.writeError(w, http.StatusInternalServerError, "operation failed", "server_error", nil, "operation_failed")
}
