package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	signed := Sign("s3cret")
	if !Verify("s3cret", signed) {
		t.Fatal("Verify() = false, want true for matching secret")
	}
	if Verify("wrong", signed) {
		t.Fatal("Verify() = true, want false for mismatched secret")
	}
}

func TestVerifyTokenConstantTime(t *testing.T) {
	if !VerifyToken("s3cret", "s3cret") {
		t.Fatal("VerifyToken() = false for exact match")
	}
	if VerifyToken("s3cret", "") {
		t.Fatal("VerifyToken() = true for empty candidate")
	}
	if VerifyToken("s3cret", "wrong") {
		t.Fatal("VerifyToken() = true for mismatch")
	}
}

func TestFromRequestCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	SetCookie(rec, "s3cret", true, time.Hour)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}

	if !FromRequest(req, "s3cret") {
		t.Fatal("FromRequest() = false, want true after SetCookie with matching secret")
	}
	if FromRequest(req, "different") {
		t.Fatal("FromRequest() = true, want false for a different secret")
	}
}

func TestFromRequestBearer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer s3cret")

	if !FromRequest(req, "s3cret") {
		t.Fatal("FromRequest() = false, want true for matching bearer token")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer wrong")
	if FromRequest(req2, "s3cret") {
		t.Fatal("FromRequest() = true, want false for mismatched bearer token")
	}
}

func TestFromRequestNoCredentials(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if FromRequest(req, "s3cret") {
		t.Fatal("FromRequest() = true, want false with no cookie or bearer header")
	}
}
