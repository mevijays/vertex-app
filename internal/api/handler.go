package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/swaggest/swgui/v5emb"

	"vertex-chat/internal/chat"
	"vertex-chat/internal/config"
)

var unmarshalJSON = json.Unmarshal

type Handler struct {
	generator chat.Generator
	cfg       config.Config
	logger    *slog.Logger
	sem       chan struct{}
}

func NewHandler(generator chat.Generator, cfg config.Config, logger *slog.Logger) *Handler {
	return &Handler{
		generator: generator,
		cfg:       cfg,
		logger:    logger,
		sem:       make(chan struct{}, cfg.MaxConcurrentChats),
	}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", h.ChatCompletions)
	mux.HandleFunc("OPTIONS /v1/chat/completions", h.preflight)
	mux.HandleFunc("GET /openapi.json", h.openAPI)
	mux.Handle("GET /docs/", v5emb.New("Vertex Chat API", "/openapi.json", "/docs/"))
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("GET /api/config", h.appConfig)
	mux.HandleFunc("GET /healthz", h.health)
}

func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	h.setCORS(w, r)
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	var request ChatCompletionRequest
	if err := decoder.Decode(&request); err != nil {
		message := "request body must be valid JSON"
		if errors.Is(err, io.EOF) {
			message = "request body is required"
		}
		h.writeError(w, http.StatusBadRequest, message, "invalid_request_error", nil, "invalid_json")
		return
	}
	if err := ensureEOF(decoder); err != nil {
		h.writeError(w, http.StatusBadRequest, "request body must contain one JSON object", "invalid_request_error", nil, "invalid_json")
		return
	}

	chatRequest, param, err := h.validateAndConvert(request)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", param, "invalid_value")
		return
	}

	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		h.writeError(w, http.StatusTooManyRequests, "the service is handling too many concurrent chats; retry shortly", "rate_limit_error", nil, "concurrency_limit")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.RequestTimeout)
	defer cancel()

	id := completionID()
	created := time.Now().Unix()
	if request.Stream {
		h.stream(ctx, w, id, created, chatRequest)
		return
	}
	h.complete(ctx, w, id, created, chatRequest)
}

func (h *Handler) validateAndConvert(request ChatCompletionRequest) (chat.Request, *string, error) {
	if request.Model == "" {
		request.Model = h.cfg.ModelID
	}
	if request.Model != h.cfg.ModelID {
		return chat.Request{}, ptr("model"), fmt.Errorf("model %q is not available; use %q", request.Model, h.cfg.ModelID)
	}
	if len(request.Messages) == 0 {
		return chat.Request{}, ptr("messages"), errors.New("messages must contain at least one message")
	}
	if len(request.Messages) > h.cfg.MaxMessages {
		return chat.Request{}, ptr("messages"), fmt.Errorf("messages must contain at most %d entries", h.cfg.MaxMessages)
	}
	if request.N > 1 {
		return chat.Request{}, ptr("n"), errors.New("only n=1 is supported")
	}
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
		return chat.Request{}, ptr("temperature"), errors.New("temperature must be between 0 and 2")
	}
	if request.TopP != nil && (*request.TopP < 0 || *request.TopP > 1) {
		return chat.Request{}, ptr("top_p"), errors.New("top_p must be between 0 and 1")
	}
	if request.MaxTokens < 0 {
		return chat.Request{}, ptr("max_tokens"), errors.New("max_tokens must be greater than zero")
	}
	if len(request.Stop) > 5 {
		return chat.Request{}, ptr("stop"), errors.New("stop supports at most 5 sequences")
	}

	var system []string
	messages := make([]chat.Message, 0, len(request.Messages))
	for i, message := range request.Messages {
		message.Role = strings.ToLower(strings.TrimSpace(message.Role))
		message.Content = strings.TrimSpace(message.Content)
		if message.Content == "" {
			return chat.Request{}, ptr("messages"), fmt.Errorf("messages[%d].content must not be empty", i)
		}
		switch message.Role {
		case "system", "developer":
			system = append(system, message.Content)
		case "user", "assistant":
			messages = append(messages, message)
		default:
			return chat.Request{}, ptr("messages"), fmt.Errorf("messages[%d].role %q is unsupported", i, message.Role)
		}
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != "user" {
		return chat.Request{}, ptr("messages"), errors.New("the final non-system message must have the user role")
	}

	return chat.Request{
		Messages:    messages,
		System:      strings.Join(system, "\n\n"),
		Model:       request.Model,
		Temperature: request.Temperature,
		TopP:        request.TopP,
		MaxTokens:   request.MaxTokens,
		Stop:        request.Stop,
	}, nil, nil
}

func (h *Handler) complete(ctx context.Context, w http.ResponseWriter, id string, created int64, request chat.Request) {
	var text strings.Builder
	result, err := h.generator.Stream(ctx, request, func(chunk string) error {
		_, writeErr := text.WriteString(chunk)
		return writeErr
	})
	if err != nil {
		h.logGenerationError(err, id)
		h.writeGenerationError(w, err)
		return
	}

	finish := "stop"
	h.writeJSON(w, http.StatusOK, chatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   request.Model,
		Choices: []completionChoice{{
			Index:        0,
			Message:      &assistantMessage{Role: "assistant", Content: text.String()},
			FinishReason: &finish,
		}},
		Usage: &result.Usage,
	})
}

func (h *Handler) stream(ctx context.Context, w http.ResponseWriter, id string, created int64, request chat.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, http.StatusInternalServerError, "streaming is not supported by this server", "server_error", nil, "streaming_unavailable")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if err := writeSSE(w, streamChunk(id, created, request.Model, streamDelta{Role: "assistant"}, nil)); err != nil {
		return
	}
	flusher.Flush()

	_, err := h.generator.Stream(ctx, request, func(text string) error {
		if err := writeSSE(w, streamChunk(id, created, request.Model, streamDelta{Content: text}, nil)); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
	if err != nil {
		h.logGenerationError(err, id)
		_ = writeSSE(w, errorEnvelope{Error: errorBody{
			Message: publicGenerationError(err),
			Type:    "server_error",
			Code:    "generation_failed",
		}})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	finish := "stop"
	_ = writeSSE(w, streamChunk(id, created, request.Model, streamDelta{}, &finish))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func streamChunk(id string, created int64, model string, delta streamDelta, finish *string) chatCompletionResponse {
	return chatCompletionResponse{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []completionChoice{{Index: 0, Delta: &delta, FinishReason: finish}},
	}
}

func writeSSE(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func (h *Handler) writeGenerationError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	code := "generation_failed"
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
		code = "generation_timeout"
	}
	h.writeError(w, status, publicGenerationError(err), "server_error", nil, code)
}

func publicGenerationError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "the model response timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "the request was canceled"
	}
	return "the model could not generate a response"
}

func (h *Handler) logGenerationError(err error, requestID string) {
	if !errors.Is(err, context.Canceled) {
		h.logger.Error("chat generation failed", "request_id", requestID, "error", err)
	}
}

func (h *Handler) appConfig(w http.ResponseWriter, _ *http.Request) {
	h.writeJSON(w, http.StatusOK, map[string]any{
		"model":        h.cfg.ModelID,
		"max_messages": h.cfg.MaxMessages,
		"mock":         h.cfg.DevMock,
	})
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	h.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) preflight(w http.ResponseWriter, r *http.Request) {
	h.setCORS(w, r)
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	for _, allowed := range h.cfg.AllowedOrigins {
		if allowed == "*" || allowed == origin {
			w.Header().Set("Access-Control-Allow-Origin", allowed)
			w.Header().Set("Vary", "Origin")
			return
		}
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, message, errorType string, param *string, code string) {
	h.writeJSON(w, status, errorEnvelope{Error: errorBody{
		Message: message,
		Type:    errorType,
		Param:   param,
		Code:    code,
	}})
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		h.logger.Error("write JSON response", "error", err)
	}
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("extra JSON value")
		}
		return err
	}
	return nil
}

func completionID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(bytes[:])
}

func ptr(value string) *string { return &value }
