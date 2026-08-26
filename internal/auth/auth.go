// Package auth verifies Supabase-issued JWTs (HS256) and threads the resulting
// identity through the request context. It mirrors a sibling Go service's verifier: the
// browser sends the access token as a Bearer header on htmx requests AND the
// app sets an HttpOnly cookie (mirroring the token) for full-page navigations,
// so the middleware accepts bearer OR cookie. Signup is invite-only (handled in
// Supabase); this layer only verifies.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

type ctxKey string

const claimsKey ctxKey = "auditloop.claims"

// SessionCookie holds the Supabase access token for full-page navigations
// (which can't send an Authorization header). Set on /api/auth/sync, cleared on
// /api/auth/signout. HttpOnly so JS can't read it.
const SessionCookie = "auditloop_at"

// DefaultDevUser is the synthetic identity injected under DevMode.
const DefaultDevUser = "00000000-0000-0000-0000-000000000001"

// Claims is the verified identity extracted from a Supabase JWT.
type Claims struct {
	UserID string // Supabase user UUID (sub)
	Email  string
}

// Verifier verifies Supabase HS256 JWTs against the shared secret.
type Verifier struct {
	secret  []byte
	devMode bool
	devUser string
}

// NewVerifier builds a verifier. When devMode is true, every request is granted
// a fixed synthetic identity (local dev / tests); the secret is ignored.
func NewVerifier(secret string, devMode bool) *Verifier {
	return &Verifier{secret: []byte(secret), devMode: devMode, devUser: DefaultDevUser}
}

// Verify parses and validates a raw bearer token, returning its claims.
func (v *Verifier) Verify(raw string) (*Claims, error) {
	if v.devMode {
		return &Claims{UserID: v.devUser, Email: "dev@auditloop.local"}, nil
	}
	if len(v.secret) == 0 {
		return nil, errors.New("auth: JWT secret not configured")
	}
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return v.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, fmt.Errorf("auth: verify: %w", err)
	}
	mc, ok := tok.Claims.(jwt.MapClaims)
	if !ok || !tok.Valid {
		return nil, errors.New("auth: invalid token claims")
	}
	sub, _ := mc["sub"].(string)
	if sub == "" {
		return nil, errors.New("auth: missing sub claim")
	}
	email, _ := mc["email"].(string)
	return &Claims{UserID: sub, Email: email}, nil
}

// Middleware verifies a bearer token (or the session cookie) if present and
// stuffs claims into the request context. It never blocks — RequireAuth gates.
// In dev mode it always injects the dev identity.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v.devMode {
			c, _ := v.Verify("")
			next.ServeHTTP(w, r.WithContext(WithClaims(r.Context(), c)))
			return
		}
		raw := bearer(r)
		if raw == "" {
			if ck, err := r.Cookie(SessionCookie); err == nil {
				raw = ck.Value
			}
		}
		if raw != "" {
			if c, err := v.Verify(raw); err == nil {
				r = r.WithContext(WithClaims(r.Context(), c))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAuth 401s (or redirects for full-page navigations) when no verified
// identity is present.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := ClaimsFrom(r.Context()); !ok {
			if r.Header.Get("HX-Request") != "" || strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// BearerToken returns the raw token from the Authorization header, or "".
func BearerToken(r *http.Request) string { return bearer(r) }

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return ""
}

// WithClaims returns a context carrying the given claims.
func WithClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, claimsKey, c)
}

// ClaimsFrom extracts claims from a context.
func ClaimsFrom(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(claimsKey).(*Claims)
	return c, ok && c != nil
}

// UserID returns the verified Supabase user id, or "". This is the data-scoping
// key: targets/runs belong to a user (WHERE user_id=?).
func UserID(ctx context.Context) string {
	if c, ok := ClaimsFrom(ctx); ok {
		return c.UserID
	}
	return ""
}
