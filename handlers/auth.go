package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/ZacxDev/auditloop/internal/auth"
)

// handleAuthSync persists the Supabase access token in an HttpOnly cookie so
// full-page navigations (which can't send an Authorization header) stay
// authenticated. Mirrors a sibling Go service's sync — the load-bearing fix for the
// post-login redirect loop.
func (a *App) handleAuthSync(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFrom(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if tok := auth.BearerToken(r); tok != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     auth.SessionCookie,
			Value:    tok,
			Path:     "/",
			HttpOnly: true,
			Secure:   !a.Cfg.DevMode,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   2592000, // 30d — keep in lockstep with GOTRUE_JWT_EXP
		})
	}
	writeJSON(w, map[string]string{"status": "ok", "user_id": claims.UserID})
}

// handleSignout clears the session cookie.
func (a *App) handleSignout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   !a.Cfg.DevMode,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
