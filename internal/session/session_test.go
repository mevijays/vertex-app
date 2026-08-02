package session

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewTokenUnique(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("NewToken() produced identical tokens twice")
	}
	if len(a) < 32 {
		t.Fatalf("token too short: %d chars", len(a))
	}
}

func TestSetCookieAndFromRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	SetCookie(rec, "abc123", true, time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}

	token, ok := FromRequest(req)
	if !ok {
		t.Fatal("FromRequest() ok = false, want true")
	}
	if token != "abc123" {
		t.Fatalf("token = %q, want %q", token, "abc123")
	}
}

func TestFromRequestNoCookie(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := FromRequest(req); ok {
		t.Fatal("FromRequest() ok = true, want false with no cookie set")
	}
}
