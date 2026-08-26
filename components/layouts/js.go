package layouts

import (
	"encoding/json"

	"github.com/ZacxDev/auditloop/components"

	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

func jsonConfig(ctx components.PageContext) string {
	cfg := map[string]any{
		"supabaseUrl":     ctx.SupabaseURL,
		"supabaseAnonKey": ctx.SupabaseAnonKey,
		"devMode":         ctx.DevMode,
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

// supabaseBridge injects the client-visible config and (in non-dev mode) the
// supabase-js bearer bridge that puts the access token on every htmx request +
// syncs the session cookie. Under DevMode auth is bypassed, so the bridge is a
// no-op stub (the /login page still works).
func supabaseBridge(ctx components.PageContext) g.Node {
	return g.Group([]g.Node{
		h.Script(g.Raw("window.AUDITLOOP = " + jsonConfig(ctx) + ";")),
		h.Script(g.Attr("type", "module"), g.Raw(supabaseClientJS)),
	})
}

// supabaseClientJS mirrors a sibling Go service's bridge: one GoTrueClient per context,
// Authorization header on htmx requests, cookie re-sync on token refresh.
const supabaseClientJS = `
const cfg = window.AUDITLOOP || {};
if (cfg.devMode) {
  // Auth bypassed server-side; nothing to wire.
} else if (cfg.supabaseUrl && cfg.supabaseAnonKey && !window.auditloopSupabase) {
  const { createClient } = await import('https://esm.sh/@supabase/supabase-js@2');
  const sb = createClient(cfg.supabaseUrl, cfg.supabaseAnonKey);
  window.auditloopSupabase = sb;
  let accessToken = null;
  async function refresh() {
    const { data } = await sb.auth.getSession();
    accessToken = data?.session?.access_token || null;
  }
  await refresh();
  const syncCookie = (t) => { if (t) fetch('/api/auth/sync', { method: 'POST', headers: { 'Authorization': 'Bearer ' + t } }).catch(() => {}); };
  syncCookie(accessToken);
  sb.auth.onAuthStateChange((evt, session) => {
    accessToken = session?.access_token || null;
    if (evt === 'TOKEN_REFRESHED' || evt === 'SIGNED_IN') syncCookie(accessToken);
  });
  document.body.addEventListener('htmx:configRequest', (evt) => {
    if (accessToken) evt.detail.headers['Authorization'] = 'Bearer ' + accessToken;
  });
  window.auditloopToken = () => accessToken;
  window.auditloopLogout = async () => {
    try { await fetch('/api/auth/signout', { method: 'POST' }); } catch (e) {}
    try { await sb.auth.signOut(); } catch (e) {}
    window.location.href = '/login';
  };
}
`
