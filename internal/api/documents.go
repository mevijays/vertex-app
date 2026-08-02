package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"time"
	"unicode/utf8"

	"vertex-chat/internal/store"
)

type documentResponse struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	MimeType  string    `json:"mime_type"`
	SizeBytes int64     `json:"size_bytes"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type documentDetailResponse struct {
	documentResponse
	ExtractedText string `json:"extracted_text"`
}

func toDocumentResponse(d store.Document) documentResponse {
	return documentResponse{
		ID: d.ID, Filename: d.Filename, MimeType: d.MimeType, SizeBytes: d.SizeBytes,
		Status: d.Status, Error: d.Error, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

// uploadDocument handles POST /api/conversations/{id}/documents. Analysis
// runs synchronously — the "processing" status exists mainly so a row is
// visible immediately (and so a crash mid-analysis leaves an honest
// status), not to support async polling.
func (h *Handler) uploadDocument(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())
	convID := r.PathValue("id")

	conv, err := h.store.GetConversation(r.Context(), convID, sessionID)
	if err != nil {
		h.writeStoreError(w, err, "conversation not found")
		return
	}

	n, err := h.store.CountDocuments(r.Context(), conv.ID)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not upload document", "server_error", nil, "upload_failed")
		return
	}
	if n >= h.cfg.MaxDocumentsPerConversation {
		h.writeError(w, http.StatusTooManyRequests, "document limit reached for this conversation", "invalid_request_error", nil, "document_limit")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		h.writeError(w, http.StatusRequestEntityTooLarge, "upload exceeds the maximum allowed size", "invalid_request_error", nil, "upload_too_large")
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("file")
	if err != nil {
		h.writeError(w, http.StatusBadRequest, `multipart field "file" is required`, "invalid_request_error", ptr("file"), "missing_file")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, h.cfg.MaxUploadBytes+1))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "could not read uploaded file", "invalid_request_error", nil, "read_failed")
		return
	}
	if int64(len(data)) > h.cfg.MaxUploadBytes {
		h.writeError(w, http.StatusRequestEntityTooLarge, "upload exceeds the maximum allowed size", "invalid_request_error", nil, "upload_too_large")
		return
	}

	mimeType, ok := sniffDocumentType(data)
	if !ok {
		h.writeError(w, http.StatusUnsupportedMediaType, "only PDF and plain-text documents are supported", "invalid_request_error", nil, "unsupported_type")
		return
	}

	filename := filepath.Base(header.Filename)
	if filename == "." || filename == "/" || filename == "" {
		filename = "document"
	}

	doc, err := h.store.CreateDocument(r.Context(), conv.ID, sessionID, filename, mimeType, int64(len(data)))
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not upload document", "server_error", nil, "upload_failed")
		return
	}

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		_ = h.store.MarkDocumentFailed(r.Context(), doc.ID, "service busy")
		h.writeError(w, http.StatusTooManyRequests, "the service is handling too many concurrent requests; retry shortly", "rate_limit_error", nil, "concurrency_limit")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.RequestTimeout)
	defer cancel()

	text, err := h.analyzer.AnalyzeDocument(ctx, data, mimeType, filename)
	if err != nil {
		message := publicDocumentError(err)
		_ = h.store.MarkDocumentFailed(r.Context(), doc.ID, message)
		doc.Status, doc.Error = "failed", message
		h.writeJSON(w, http.StatusCreated, toDocumentResponse(doc))
		return
	}
	if err := h.store.MarkDocumentReady(r.Context(), doc.ID, text); err != nil {
		h.writeError(w, http.StatusInternalServerError, "could not save analyzed document", "server_error", nil, "upload_failed")
		return
	}
	doc.Status = "ready"
	h.writeJSON(w, http.StatusCreated, toDocumentResponse(doc))
}

func (h *Handler) getDocument(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())
	convID := r.PathValue("id")
	docID := r.PathValue("docId")

	conv, err := h.store.GetConversation(r.Context(), convID, sessionID)
	if err != nil {
		h.writeStoreError(w, err, "conversation not found")
		return
	}
	doc, err := h.store.GetDocument(r.Context(), docID, conv.ID)
	if err != nil {
		h.writeStoreError(w, err, "document not found")
		return
	}
	h.writeJSON(w, http.StatusOK, documentDetailResponse{documentResponse: toDocumentResponse(doc), ExtractedText: doc.ExtractedText})
}

func (h *Handler) deleteDocument(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())
	convID := r.PathValue("id")
	docID := r.PathValue("docId")

	conv, err := h.store.GetConversation(r.Context(), convID, sessionID)
	if err != nil {
		h.writeStoreError(w, err, "conversation not found")
		return
	}
	if err := h.store.DeleteDocument(r.Context(), docID, conv.ID); err != nil {
		h.writeStoreError(w, err, "document not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func sniffDocumentType(data []byte) (string, bool) {
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		return "application/pdf", true
	}
	if utf8.Valid(data) {
		return "text/plain", true
	}
	return "", false
}

func publicDocumentError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "document analysis timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "the request was canceled"
	}
	return "the document could not be analyzed"
}
