package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "super-secret-jwt-signing-key"

func signToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerifyValidToken(t *testing.T) {
	v := NewVerifier(testSecret, false)
	raw := signToken(t, jwt.MapClaims{"sub": "user-123", "email": "a@b.com", "exp": time.Now().Add(time.Hour).Unix()})
	c, err := v.Verify(raw)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if c.UserID != "user-123" || c.Email != "a@b.com" {
		t.Errorf("claims = %+v", c)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	v := NewVerifier(testSecret, false)
	raw := signToken(t, jwt.MapClaims{"sub": "u", "exp": time.Now().Add(-time.Hour).Unix()})
	if _, err := v.Verify(raw); err == nil {
		t.Error("expected expired token to fail")
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	v := NewVerifier("different-secret", false)
	raw := signToken(t, jwt.MapClaims{"sub": "u", "exp": time.Now().Add(time.Hour).Unix()})
	if _, err := v.Verify(raw); err == nil {
		t.Error("expected wrong-secret token to fail")
	}
}

func TestVerifyRejectsNoneAlg(t *testing.T) {
	v := NewVerifier(testSecret, false)
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "u"})
	raw, _ := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := v.Verify(raw); err == nil {
		t.Error("expected alg=none token to be rejected")
	}
}

func TestVerifyMissingSub(t *testing.T) {
	v := NewVerifier(testSecret, false)
	raw := signToken(t, jwt.MapClaims{"email": "a@b.com", "exp": time.Now().Add(time.Hour).Unix()})
	if _, err := v.Verify(raw); err == nil {
		t.Error("expected missing-sub token to fail")
	}
}

func TestDevModeBypass(t *testing.T) {
	v := NewVerifier("", true)
	c, err := v.Verify("")
	if err != nil {
		t.Fatalf("dev verify: %v", err)
	}
	if c.UserID != DefaultDevUser {
		t.Errorf("dev user = %q", c.UserID)
	}
}

// probe is a handler that reports whether claims made it into the context.
func probe() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := UserID(r.Context()); id != "" {
			w.WriteHeader(200)
			_, _ = w.Write([]byte(id))
			return
		}
		w.WriteHeader(499)
	})
}

func TestMiddlewareBearer(t *testing.T) {
	v := NewVerifier(testSecret, false)
	raw := signToken(t, jwt.MapClaims{"sub": "bearer-user", "exp": time.Now().Add(time.Hour).Unix()})
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rw := httptest.NewRecorder()
	v.Middleware(probe()).ServeHTTP(rw, req)
	if rw.Code != 200 || rw.Body.String() != "bearer-user" {
		t.Errorf("bearer path: code=%d body=%q", rw.Code, rw.Body.String())
	}
}

func TestMiddlewareCookie(t *testing.T) {
	// Load-bearing: full-page navigations carry no Authorization header, only
	// the session cookie. The middleware must accept it.
	v := NewVerifier(testSecret, false)
	raw := signToken(t, jwt.MapClaims{"sub": "cookie-user", "exp": time.Now().Add(time.Hour).Unix()})
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: raw})
	rw := httptest.NewRecorder()
	v.Middleware(probe()).ServeHTTP(rw, req)
	if rw.Code != 200 || rw.Body.String() != "cookie-user" {
		t.Errorf("cookie path: code=%d body=%q", rw.Code, rw.Body.String())
	}
}

func TestMiddlewareDevInjects(t *testing.T) {
	v := NewVerifier("", true)
	req := httptest.NewRequest("GET", "/", nil)
	rw := httptest.NewRecorder()
	v.Middleware(probe()).ServeHTTP(rw, req)
	if rw.Code != 200 || rw.Body.String() != DefaultDevUser {
		t.Errorf("dev inject: code=%d body=%q", rw.Code, rw.Body.String())
	}
}

func TestRequireAuthBlocksAPI(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/targets", nil)
	rw := httptest.NewRecorder()
	RequireAuth(probe()).ServeHTTP(rw, req)
	if rw.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rw.Code)
	}
}

func TestRequireAuthRedirectsPage(t *testing.T) {
	req := httptest.NewRequest("GET", "/dashboard", nil)
	rw := httptest.NewRecorder()
	RequireAuth(probe()).ServeHTTP(rw, req)
	if rw.Code != http.StatusFound {
		t.Errorf("expected 302 redirect, got %d", rw.Code)
	}
	if loc := rw.Header().Get("Location"); loc != "/login" {
		t.Errorf("redirect to %q, want /login", loc)
	}
}

func TestRequireAuthPassesWhenAuthed(t *testing.T) {
	v := NewVerifier("", true)
	req := httptest.NewRequest("GET", "/dashboard", nil)
	rw := httptest.NewRecorder()
	// Chain middleware(dev inject) → RequireAuth → probe.
	v.Middleware(RequireAuth(probe())).ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Errorf("authed request blocked: %d", rw.Code)
	}
}
