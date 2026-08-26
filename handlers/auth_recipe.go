package handlers

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/auditloop/components/pages"
	"github.com/ZacxDev/auditloop/internal/auth"
	"github.com/ZacxDev/auditloop/internal/crawler"
	"github.com/ZacxDev/auditloop/internal/db"
	"github.com/ZacxDev/auditloop/internal/metrics"
	"github.com/ZacxDev/auditloop/internal/recipe"
	"github.com/ZacxDev/auditloop/internal/storage"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// maxAuthBodyBytes caps the auth/login-test request body. A login recipe is a
// small JSON step list + short credentials; 64KB is generous. Oversized posts
// are rejected (413) before parsing.
const maxAuthBodyBytes = 64 << 10 // 64 KiB

// authVM builds the P4 Authentication view model for a target. Credential VALUES
// are never included — only whether creds are set (write-only UI).
func (a *App) authVM(t *db.Target) *pages.AuthVM {
	vm := &pages.AuthVM{
		TargetID: t.ID,
		Enabled:  a.LoginRecipesEnabled(),
		Mode:     t.AuthMode,
	}
	if !vm.Enabled {
		return vm
	}
	lr, err := a.DB.GetLoginRecipe(t.ID)
	if err != nil {
		return vm // no recipe yet
	}
	vm.HasRecipe = true
	vm.HasCreds = strings.TrimSpace(lr.CredsEncrypted) != ""
	vm.StepsJSON = lr.StepsJSON
	vm.LoginURL = lr.LoginURL
	vm.SuccessSelector = lr.SuccessSelector
	vm.SuccessURLContains = lr.SuccessURLContains
	vm.SuccessTimeoutMs = lr.SuccessTimeoutMs
	if steps, perr := recipe.ParseSteps(lr.StepsJSON); perr == nil {
		if gf, ok := recipe.DeriveGuided(steps); ok {
			vm.IsGuided = true
			vm.UsernameSelector = gf.UsernameSelector
			vm.PasswordSelector = gf.PasswordSelector
			vm.SubmitSelector = gf.SubmitSelector
		}
		if pretty, merr := recipe.MarshalStepsPretty(steps); merr == nil {
			vm.StepsJSON = pretty
		}
	}
	return vm
}

// handleSaveAuth saves or clears a target's login recipe (P4). It validates the
// canonical steps, enforces same-domain + SSRF on every goto URL via the crawl
// guard, and stores credentials ENCRYPTED (write-only in the UI). 503 when the
// feature is disabled (no encryption key).
func (a *App) handleSaveAuth(w http.ResponseWriter, r *http.Request) {
	if !a.LoginRecipesEnabled() {
		http.Error(w, "login recipes are not enabled (no AUDITLOOP_ENCRYPTION_KEY configured)", http.StatusServiceUnavailable)
		return
	}
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	t, err := a.DB.GetTarget(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Bound the request body (steps_json / credentials) — reject oversized posts
	// before parsing so a huge steps_json can't exhaust memory.
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "request too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	// auth_mode=none clears the recipe.
	if r.FormValue("auth_mode") != db.AuthLogin {
		if err := a.DB.DeleteLoginRecipe(t.ID); err != nil {
			http.Error(w, "failed to clear login recipe", http.StatusInternalServerError)
			return
		}
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Build canonical steps from the chosen authoring mode.
	var steps []recipe.Step
	var successSel, successURL string
	var successTO int
	switch r.FormValue("recipe_mode") {
	case "advanced":
		steps, err = recipe.ParseSteps(r.FormValue("steps_json"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if gf, ok := recipe.DeriveGuided(steps); ok {
			successSel, successURL, successTO = gf.SuccessSelector, gf.SuccessURLContains, gf.SuccessTimeoutMs
		} else {
			// Best-effort: pull success fields from the last waitFor step.
			for _, s := range steps {
				if s.Type == recipe.StepWaitFor {
					successSel, successURL, successTO = s.Selector, s.URLContains, s.TimeoutMs
				}
			}
		}
	default: // guided
		successTO, _ = strconv.Atoi(strings.TrimSpace(r.FormValue("success_timeout_ms")))
		gf := recipe.GuidedForm{
			LoginURL:           r.FormValue("login_url"),
			UsernameSelector:   r.FormValue("username_selector"),
			PasswordSelector:   r.FormValue("password_selector"),
			SubmitSelector:     r.FormValue("submit_selector"),
			SuccessSelector:    r.FormValue("success_selector"),
			SuccessURLContains: r.FormValue("success_url_contains"),
			SuccessTimeoutMs:   successTO,
		}
		steps = gf.Compile()
		successSel, successURL, successTO = gf.SuccessSelector, gf.SuccessURLContains, gf.SuccessTimeoutMs
	}

	if err := recipe.Validate(steps); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Same-domain + SSRF: every goto URL must be within the target's verified
	// domains and pass the IP guard (rejects a foreign registrable domain / SSO
	// host / private-IP / metadata pivot).
	guard := a.loginGuard(t)
	for _, raw := range recipe.GotoURLs(steps) {
		if err := guard.CheckURL(raw); err != nil {
			http.Error(w, "login URL rejected (same-domain only): "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Credentials: write-only. A blank field on update keeps the stored value.
	creds, err := a.resolveCreds(t.ID, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	credMap := creds.Map()
	for _, ref := range recipe.RequiredRefs(steps) {
		if strings.TrimSpace(credMap[ref]) == "" {
			http.Error(w, "the recipe fills "+ref+" but no "+ref+" value is set", http.StatusBadRequest)
			return
		}
	}

	plain, err := creds.Marshal()
	if err != nil {
		http.Error(w, "failed to encode credentials", http.StatusInternalServerError)
		return
	}
	encB64, err := a.Cipher.EncryptToBase64(plain)
	if err != nil {
		http.Error(w, "failed to encrypt credentials", http.StatusInternalServerError)
		return
	}

	stepsJSON, err := recipe.MarshalSteps(steps)
	if err != nil {
		http.Error(w, "failed to encode recipe", http.StatusInternalServerError)
		return
	}
	lr := &db.LoginRecipe{
		TargetID:           t.ID,
		LoginURL:           firstOr(recipe.GotoURLs(steps)),
		StepsJSON:          stepsJSON,
		SuccessSelector:    successSel,
		SuccessURLContains: successURL,
		SuccessTimeoutMs:   successTO,
		CredsEncrypted:     encB64,
	}
	if err := a.DB.SetLoginRecipe(lr); err != nil {
		http.Error(w, "failed to save login recipe", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

// resolveCreds merges submitted credential values over any stored ones (blank =
// keep existing), so the UI can stay write-only. If no recipe exists yet, only
// the submitted values are used.
func (a *App) resolveCreds(targetID string, r *http.Request) (recipe.Credentials, error) {
	var creds recipe.Credentials
	if lr, err := a.DB.GetLoginRecipe(targetID); err == nil && lr.CredsEncrypted != "" {
		if plain, derr := a.Cipher.DecryptFromBase64(lr.CredsEncrypted); derr == nil {
			creds, _ = recipe.ParseCredentials(plain)
		}
	}
	if u := strings.TrimSpace(r.FormValue("username")); u != "" {
		creds.Username = u
	}
	// Password may legitimately contain leading/trailing spaces; only treat an
	// entirely-empty submission as "unchanged".
	if p := r.FormValue("password"); p != "" {
		creds.Password = p
	}
	return creds, nil
}

// handleLoginTest runs ONLY the login steps in a headless browser and returns a
// pass/fail fragment with an end-state screenshot. Credentials are decrypted
// server-side and never returned.
func (a *App) handleLoginTest(w http.ResponseWriter, r *http.Request) {
	if !a.LoginRecipesEnabled() {
		http.Error(w, "login recipes are not enabled", http.StatusServiceUnavailable)
		return
	}
	uid := auth.UserID(r.Context())
	id := mux.Vars(r)["id"]
	// Rate-limit: each login-test spawns a headless Chromium. Throttle per
	// (user,target) to blunt a DoS / SSRF-brute loop on one target while still
	// letting a user test different targets back-to-back.
	if a.loginTestLimiter != nil && !a.loginTestLimiter.Allow(uid+":"+id) {
		http.Error(w, "login test is rate-limited; wait a few seconds and retry", http.StatusTooManyRequests)
		return
	}
	// Bound the (form) request body — this route takes no large payload.
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	t, err := a.DB.GetTarget(uid, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	lr, err := a.DB.GetLoginRecipe(t.ID)
	if err != nil {
		render(w, pages.LoginTestResult(&pages.LoginTestVM{Err: "Save a login recipe first, then test it."}))
		return
	}
	steps, err := recipe.ParseSteps(lr.StepsJSON)
	if err != nil {
		render(w, pages.LoginTestResult(&pages.LoginTestVM{Err: "The stored recipe is invalid."}))
		return
	}
	plain, err := a.Cipher.DecryptFromBase64(lr.CredsEncrypted)
	if err != nil {
		render(w, pages.LoginTestResult(&pages.LoginTestVM{Err: "Could not decrypt stored credentials."}))
		return
	}
	creds, _ := recipe.ParseCredentials(plain)

	res, err := crawler.RunLoginProbe(r.Context(), crawler.LoginProbeOptions{
		Login:              &crawler.LoginConfig{Steps: steps, Credentials: creds.Map()},
		AllowedHosts:       a.allowedHosts(t),
		ChromiumPath:       a.Cfg.ChromiumPath,
		AllowLoopback:      a.Cfg.CrawlAllowLoopback,
		InternalAllowHosts: a.Cfg.InternalAllowHosts,
		Timeout:            90 * time.Second,
	})
	if err != nil {
		render(w, pages.LoginTestResult(&pages.LoginTestVM{Err: "Login test could not run: " + err.Error()}))
		return
	}

	vm := &pages.LoginTestVM{OK: res.OK, EndURL: res.EndURL, FailReason: res.FailReason}
	if res.OK {
		metrics.LoginAttempts.WithLabelValues("probe_ok").Inc()
	} else {
		metrics.LoginAttempts.WithLabelValues("probe_failed").Inc()
	}

	// Store + presign the end-state screenshot.
	if len(res.Screenshot) > 0 {
		key := storage.LoginTestKey(t.ID, uuid.NewString())
		if perr := a.Store.Put(r.Context(), key, "image/png", strings.NewReader(string(res.Screenshot)), int64(len(res.Screenshot))); perr == nil {
			vm.ScreenshotURL = artifactURL(key)
			vm.ScreenshotKey = key
		}
	}
	render(w, pages.LoginTestResult(vm))
}

// loginGuard builds the SSRF/same-domain guard for a target's login URLs. It
// honors the dev/test loopback escape hatch so a hermetic fixture can be tested.
func (a *App) loginGuard(t *db.Target) crawler.GuardConfig {
	return crawler.GuardConfig{
		AllowedHosts:       a.allowedHosts(t),
		AllowLoopback:      a.Cfg.CrawlAllowLoopback,
		InternalAllowHosts: crawler.InternalAllowSet(a.Cfg.InternalAllowHosts),
	}
}

func (a *App) allowedHosts(t *db.Target) []string {
	if len(t.VerifiedDomains) > 0 {
		return t.VerifiedDomains
	}
	if h := hostOf(t.BaseURL); h != "" {
		return []string{h}
	}
	return nil
}

func (a *App) targetSlug(t *db.Target) string {
	s := storage.Slug(t.Name)
	if s == "root" {
		s = storage.Slug(hostOf(t.BaseURL))
	}
	return s
}

func firstOr(ss []string) string {
	if len(ss) > 0 {
		return ss[0]
	}
	return ""
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
