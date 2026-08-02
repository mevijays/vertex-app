package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vertex-chat/internal/chat"
	"vertex-chat/internal/config"
	"vertex-chat/internal/store"
)

const testAuthToken = "test-auth-token"

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

func testHandler(t *testing.T, generator chat.Generator) http.Handler {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Config{
		ModelID:                     "gemini-3.5-flash",
		MaxRequestBytes:             1 << 20,
		MaxMessages:                 10,
		MaxConcurrentChats:          2,
		RequestTimeout:              time.Minute,
		AuthToken:                   testAuthToken,
		CookieSecure:                false,
		MaxUploadBytes:              1 << 20,
		MaxDocumentContextChars:     4000,
		MaxContextMessages:          20,
		MaxConversationsPerSession:  10,
		MaxDocumentsPerConversation: 5,
		MaxSavedPromptsPerSession:   10,
	}
	mux := http.NewServeMux()
	NewHandler(generator, chat.MockDocumentAnalyzer{}, st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	return mux
}

func authedRequest(method, path string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+testAuthToken)
	return req
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "vc_session" {
			return c
		}
	}
	t.Fatal("no vc_session cookie set on response")
	return nil
}

func TestChatCompletions(t *testing.T) {
	generator := &fakeGenerator{
		chunks: []string{"Hello", " there"},
		usage:  chat.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
	}
	request := authedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "model":"gemini-3.5-flash",
        "messages":[
          {"role":"system","content":"Be concise."},
          {"role":"user","content":"Say hello"}
        ],
        "temperature":0.4
      }`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	testHandler(t, generator).ServeHTTP(recorder, request)

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
	if response.ConversationID != "" {
		t.Fatalf("conversation_id = %q, want empty when not requested", response.ConversationID)
	}
}

func TestChatCompletionsStream(t *testing.T) {
	generator := &fakeGenerator{chunks: []string{"one ", "two"}}
	request := authedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "messages":[{"role":"user","content":"count"}],
        "stream":true
      }`))
	recorder := httptest.NewRecorder()

	testHandler(t, generator).ServeHTTP(recorder, request)

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
	request := authedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "model":"another-model",
        "messages":[{"role":"user","content":"hello"}]
      }`))
	recorder := httptest.NewRecorder()

	testHandler(t, &fakeGenerator{}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"param":"model"`) {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestChatCompletionsAcceptsStringStop(t *testing.T) {
	generator := &fakeGenerator{chunks: []string{"ok"}}
	request := authedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "messages":[{"role":"user","content":"hello"}],
        "stop":"END"
      }`))
	recorder := httptest.NewRecorder()

	testHandler(t, generator).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(generator.seen.Stop) != 1 || generator.seen.Stop[0] != "END" {
		t.Fatalf("stop = %#v", generator.seen.Stop)
	}
}

func TestDocumentationRoutes(t *testing.T) {
	handler := testHandler(t, &fakeGenerator{})

	for _, test := range []struct {
		path        string
		contentType string
		contains    string
	}{
		{path: "/openapi.json", contentType: "application/json", contains: `"openapi": "3.1.0"`},
		{path: "/docs/", contentType: "text/html", contains: "Vertex Chat API"},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := authedRequest(http.MethodGet, test.path, nil)
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

func TestRequireAuthRejectsUnauthenticated(t *testing.T) {
	handler := testHandler(t, &fakeGenerator{})

	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/api/config"},
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodGet, "/openapi.json"},
		{http.MethodGet, "/docs/"},
	} {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(`{}`))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestLoginFlow(t *testing.T) {
	handler := testHandler(t, &fakeGenerator{})

	wrong := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"token":"nope"}`))
	wrongRec := httptest.NewRecorder()
	handler.ServeHTTP(wrongRec, wrong)
	if wrongRec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", wrongRec.Code)
	}

	right := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"token":"`+testAuthToken+`"}`))
	rightRec := httptest.NewRecorder()
	handler.ServeHTTP(rightRec, right)
	if rightRec.Code != http.StatusOK {
		t.Fatalf("right token status = %d, body = %s", rightRec.Code, rightRec.Body.String())
	}

	var authCookie *http.Cookie
	for _, c := range rightRec.Result().Cookies() {
		if c.Name == "vc_auth" {
			authCookie = c
		}
	}
	if authCookie == nil {
		t.Fatal("no vc_auth cookie set after successful login")
	}

	authed := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	authed.AddCookie(authCookie)
	authedRec := httptest.NewRecorder()
	handler.ServeHTTP(authedRec, authed)
	if authedRec.Code != http.StatusOK {
		t.Fatalf("cookie-authed status = %d, body = %s", authedRec.Code, authedRec.Body.String())
	}
}

func TestConversationCRUDAndSessionIsolation(t *testing.T) {
	handler := testHandler(t, &fakeGenerator{})

	createReq := authedRequest(http.MethodPost, "/api/conversations", strings.NewReader(`{"title":"My chat"}`))
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createRec.Code, createRec.Body.String())
	}
	ownerCookie := sessionCookie(t, createRec)

	var created conversationDetailResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	getReq := authedRequest(http.MethodGet, "/api/conversations/"+created.ID, nil)
	getReq.AddCookie(ownerCookie)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("owner get status = %d, body = %s", getRec.Code, getRec.Body.String())
	}

	otherReq := authedRequest(http.MethodGet, "/api/conversations/"+created.ID, nil)
	otherRec := httptest.NewRecorder()
	handler.ServeHTTP(otherRec, otherReq)
	if otherRec.Code != http.StatusNotFound {
		t.Fatalf("cross-session get status = %d, want 404, body = %s", otherRec.Code, otherRec.Body.String())
	}

	delReq := authedRequest(http.MethodDelete, "/api/conversations/"+created.ID, nil)
	delReq.AddCookie(ownerCookie)
	delRec := httptest.NewRecorder()
	handler.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", delRec.Code, delRec.Body.String())
	}
}

// TestChatCompletionsConversationPersistence exercises the browser path:
// a session cookie (minted via an /api/* call, as the real UI would) is
// present on every /v1/chat/completions call, so the conversation is
// session-scoped and later browsable via GET /api/conversations/{id}.
func TestChatCompletionsConversationPersistence(t *testing.T) {
	generator := &fakeGenerator{chunks: []string{"first reply"}}
	handler := testHandler(t, generator)

	bootstrap := authedRequest(http.MethodGet, "/api/config", nil)
	bootstrapRec := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapRec, bootstrap)
	cookie := sessionCookie(t, bootstrapRec)

	first := authedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "messages":[{"role":"user","content":"hello there"}],
        "conversation_id":"browser-thread-1"
      }`))
	first.AddCookie(cookie)
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first call status = %d, body = %s", firstRec.Code, firstRec.Body.String())
	}
	var firstResp chatCompletionResponse
	if err := json.Unmarshal(firstRec.Body.Bytes(), &firstResp); err != nil {
		t.Fatal(err)
	}
	if firstResp.ConversationID != "browser-thread-1" {
		t.Fatalf("conversation_id = %q, want the client-supplied id echoed back", firstResp.ConversationID)
	}

	generator.chunks = []string{"second reply"}
	second := authedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "messages":[{"role":"user","content":"follow up"}],
        "conversation_id":"browser-thread-1"
      }`))
	second.AddCookie(cookie)
	secondRec := httptest.NewRecorder()
	handler.ServeHTTP(secondRec, second)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second call status = %d, body = %s", secondRec.Code, secondRec.Body.String())
	}

	if len(generator.seen.Messages) != 3 {
		t.Fatalf("second call saw %d messages, want 3 (2 stored + 1 new): %#v", len(generator.seen.Messages), generator.seen.Messages)
	}
	if generator.seen.Messages[0].Content != "hello there" || generator.seen.Messages[2].Content != "follow up" {
		t.Fatalf("unexpected message order: %#v", generator.seen.Messages)
	}

	fetchReq := authedRequest(http.MethodGet, "/api/conversations/browser-thread-1", nil)
	fetchReq.AddCookie(cookie)
	fetchRec := httptest.NewRecorder()
	handler.ServeHTTP(fetchRec, fetchReq)
	if fetchRec.Code != http.StatusOK {
		t.Fatalf("fetch status = %d, body = %s", fetchRec.Code, fetchRec.Body.String())
	}
	var conv conversationDetailResponse
	if err := json.Unmarshal(fetchRec.Body.Bytes(), &conv); err != nil {
		t.Fatal(err)
	}
	if len(conv.Messages) != 4 {
		t.Fatalf("stored message count = %d, want 4 (2 user + 2 assistant)", len(conv.Messages))
	}
	if conv.Title == "" {
		t.Fatal("expected an auto-derived title from the first user message")
	}
}

// TestChatCompletionsHeadlessConversationPersistence exercises a cookie-less
// caller (e.g. the OpenAI SDK): already authenticated via the shared
// bearer token, it never receives or sends a session cookie, and
// conversation_id alone is enough to keep using the same thread.
func TestChatCompletionsHeadlessConversationPersistence(t *testing.T) {
	generator := &fakeGenerator{chunks: []string{"first reply"}}
	handler := testHandler(t, generator)

	first := authedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "messages":[{"role":"user","content":"hello there"}],
        "conversation_id":"headless-thread-1"
      }`))
	firstRec := httptest.NewRecorder()
	handler.ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first call status = %d, body = %s", firstRec.Code, firstRec.Body.String())
	}
	for _, c := range firstRec.Result().Cookies() {
		if c.Name == "vc_session" {
			t.Fatal("headless caller should never receive a session cookie")
		}
	}

	generator.chunks = []string{"second reply"}
	second := authedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "messages":[{"role":"user","content":"follow up"}],
        "conversation_id":"headless-thread-1"
      }`))
	secondRec := httptest.NewRecorder()
	handler.ServeHTTP(secondRec, second)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second call status = %d, body = %s", secondRec.Code, secondRec.Body.String())
	}
	if len(generator.seen.Messages) != 3 {
		t.Fatalf("second call saw %d messages, want 3 (2 stored + 1 new): %#v", len(generator.seen.Messages), generator.seen.Messages)
	}
}

func TestDocumentUploadAndChatContextInjection(t *testing.T) {
	generator := &fakeGenerator{chunks: []string{"ok"}}
	handler := testHandler(t, generator)

	createReq := authedRequest(http.MethodPost, "/api/conversations", nil)
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createRec.Code, createRec.Body.String())
	}
	cookie := sessionCookie(t, createRec)
	var conv conversationDetailResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &conv); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("the quarterly revenue was $5M")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	uploadReq := authedRequest(http.MethodPost, "/api/conversations/"+conv.ID+"/documents", &body)
	uploadReq.Header.Set("Content-Type", writer.FormDataContentType())
	uploadReq.AddCookie(cookie)
	uploadRec := httptest.NewRecorder()
	handler.ServeHTTP(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", uploadRec.Code, uploadRec.Body.String())
	}
	var doc documentResponse
	if err := json.Unmarshal(uploadRec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Status != "ready" {
		t.Fatalf("document status = %q, want ready (body=%s)", doc.Status, uploadRec.Body.String())
	}

	chatReq := authedRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
        "messages":[{"role":"user","content":"what was the revenue?"}],
        "conversation_id":"`+conv.ID+`"
      }`))
	chatReq.AddCookie(cookie)
	chatRec := httptest.NewRecorder()
	handler.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", chatRec.Code, chatRec.Body.String())
	}
	if !strings.Contains(generator.seen.System, "[MOCK ANALYSIS]") {
		t.Fatalf("system prompt missing document context: %q", generator.seen.System)
	}
}

func TestDocumentUploadTooLarge(t *testing.T) {
	handler := testHandler(t, &fakeGenerator{})

	createReq := authedRequest(http.MethodPost, "/api/conversations", nil)
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	cookie := sessionCookie(t, createRec)
	var conv conversationDetailResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &conv); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("a"), 2<<20)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	uploadReq := authedRequest(http.MethodPost, "/api/conversations/"+conv.ID+"/documents", &body)
	uploadReq.Header.Set("Content-Type", writer.FormDataContentType())
	uploadReq.AddCookie(cookie)
	uploadRec := httptest.NewRecorder()
	handler.ServeHTTP(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body = %s", uploadRec.Code, uploadRec.Body.String())
	}
}

func TestDocumentUploadUnsupportedType(t *testing.T) {
	handler := testHandler(t, &fakeGenerator{})

	createReq := authedRequest(http.MethodPost, "/api/conversations", nil)
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	cookie := sessionCookie(t, createRec)
	var conv conversationDetailResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &conv); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "binary.dat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte{0xff, 0xfe, 0x00, 0xff, 0x00, 0xd8, 0x00, 0xff}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	uploadReq := authedRequest(http.MethodPost, "/api/conversations/"+conv.ID+"/documents", &body)
	uploadReq.Header.Set("Content-Type", writer.FormDataContentType())
	uploadReq.AddCookie(cookie)
	uploadRec := httptest.NewRecorder()
	handler.ServeHTTP(uploadRec, uploadReq)
	if uploadRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415, body = %s", uploadRec.Code, uploadRec.Body.String())
	}
}

func TestSavedPromptsCRUD(t *testing.T) {
	handler := testHandler(t, &fakeGenerator{})

	createReq := authedRequest(http.MethodPost, "/api/prompts", strings.NewReader(`{"title":"Greeting","content":"Say hello politely"}`))
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createRec.Code, createRec.Body.String())
	}
	cookie := sessionCookie(t, createRec)
	var prompt savedPromptResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &prompt); err != nil {
		t.Fatal(err)
	}

	listReq := authedRequest(http.MethodGet, "/api/prompts", nil)
	listReq.AddCookie(cookie)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), "Greeting") {
		t.Fatalf("list status = %d, body = %s", listRec.Code, listRec.Body.String())
	}

	updateReq := authedRequest(http.MethodPut, "/api/prompts/"+prompt.ID, strings.NewReader(`{"title":"Greeting v2","content":"Say hi"}`))
	updateReq.AddCookie(cookie)
	updateRec := httptest.NewRecorder()
	handler.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", updateRec.Code, updateRec.Body.String())
	}

	deleteReq := authedRequest(http.MethodDelete, "/api/prompts/"+prompt.ID, nil)
	deleteReq.AddCookie(cookie)
	deleteRec := httptest.NewRecorder()
	handler.ServeHTTP(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", deleteRec.Code, deleteRec.Body.String())
	}
}
