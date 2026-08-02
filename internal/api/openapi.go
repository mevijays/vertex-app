package api

import (
	_ "embed"
	"net/http"
)

// openAPISpec is the public API contract served to Swagger UI and API clients.
//
//go:embed openapi.json
var openAPISpec []byte

func (h *Handler) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(openAPISpec); err != nil {
		h.logger.Error("write OpenAPI specification", "error", err)
	}
}
