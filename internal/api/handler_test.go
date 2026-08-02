package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vertex-chat/internal/chat"
	"vertex-chat/internal/config"
)

type fakeGenerator struct {
	chunks []string
	usage  chat.Usage
	err    error
	seen   chat.Request
}

func (f *fakeGenerator) Stream(_ context.Context, request chat.Request, onText func(string) error) (chat.Result, error) {
	f.seen = request
	for _, chunk := range f.chunks {
		if err := onText(chunk); err != nil {
			return chat.Result{}, err
		}
	}
	return chat.Result{Usage: f.usage}, f.err
}

func testHandler(generator chat.Generator) http.Handler {
	cfg := config.Config{
		ModelID:            "gemini-3.5-flash",
		MaxRequestBytes:    1 << 20,
		MaxMessages:        10,
		MaxConcurrentChats: 2,
		RequestTimeout:     time.Minute,
	}
	mux := http.NewServeMux()
	NewHandler(generator, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	return mux
}

func TestChatCompletions(t *testing.T) {
	generator := &fakeGenerator{
		chunks: []string{"Hello", " there"},
		usage:  chat.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "model":"gemini-3.5-flash",
        "messages":[
          {"role":"system","content":"Be concise."},
          {"role":"user","content":"Say hello"}
        ],
        "temperature":0.4
      }`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	testHandler(generator).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response chatCompletionResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := response.Choices[0].Message.Content; got != "Hello there" {
		t.Fatalf("content = %q", got)
	}
	if generator.seen.System != "Be concise." {
		t.Fatalf("system = %q", generator.seen.System)
	}
	if len(generator.seen.Messages) != 1 || generator.seen.Messages[0].Role != "user" {
		t.Fatalf("messages = %#v", generator.seen.Messages)
	}
	if response.Usage == nil || response.Usage.TotalTokens != 6 {
		t.Fatalf("usage = %#v", response.Usage)
	}
}

func TestChatCompletionsStream(t *testing.T) {
	generator := &fakeGenerator{chunks: []string{"one ", "two"}}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "messages":[{"role":"user","content":"count"}],
        "stream":true
      }`))
	recorder := httptest.NewRecorder()

	testHandler(generator).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q", got)
	}
	body := recorder.Body.String()
	for _, expected := range []string{`"role":"assistant"`, `"content":"one "`, `"content":"two"`, `"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(body, expected) {
			t.Errorf("stream does not contain %q:\n%s", expected, body)
		}
	}
}

func TestChatCompletionsRejectsUnknownModel(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "model":"another-model",
        "messages":[{"role":"user","content":"hello"}]
      }`))
	recorder := httptest.NewRecorder()

	testHandler(&fakeGenerator{}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"param":"model"`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestChatCompletionsAcceptsStringStop(t *testing.T) {
	generator := &fakeGenerator{chunks: []string{"ok"}}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "messages":[{"role":"user","content":"hello"}],
        "stop":"END"
      }`))
	recorder := httptest.NewRecorder()

	testHandler(generator).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(generator.seen.Stop) != 1 || generator.seen.Stop[0] != "END" {
		t.Fatalf("stop = %#v", generator.seen.Stop)
	}
}

func TestDocumentationRoutes(t *testing.T) {
	handler := testHandler(&fakeGenerator{})

	for _, test := range []struct {
		path        string
		contentType string
		contains    string
	}{
		{path: "/openapi.json", contentType: "application/json", contains: `"openapi": "3.1.0"`},
		{path: "/docs/", contentType: "text/html", contains: "Vertex Chat API"},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, test.contentType) {
				t.Fatalf("content type = %q", got)
			}
			if !strings.Contains(recorder.Body.String(), test.contains) {
				t.Fatalf("body does not contain %q", test.contains)
			}
		})
	}
}
