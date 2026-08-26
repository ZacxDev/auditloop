package handlers

import "net/http"

// handleManifest serves the PWA web manifest (installability).
func (a *App) handleManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(manifestJSON))
}

// handleServiceWorker serves the service worker at the root scope. It MUST be
// served from the origin root (not /static/) so its scope covers the whole app.
func (a *App) handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript")
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(serviceWorkerJS))
}

const manifestJSON = `{
  "name": "auditloop",
  "short_name": "auditloop",
  "description": "Generic UX-audit crawler",
  "start_url": "/dashboard",
  "scope": "/",
  "display": "standalone",
  "background_color": "#0f172a",
  "theme_color": "#0f172a",
  "icons": [
    { "src": "/static/img/icon.svg", "sizes": "any", "type": "image/svg+xml", "purpose": "any maskable" }
  ]
}`

// serviceWorkerJS is a minimal static-asset cache. It deliberately does NOT
// intercept navigations: page routes (e.g. /dashboard) 302-redirect (auth,
// trailing-slash), and a redirected response can't be returned to a navigation
// whose redirect mode is "manual" ("a redirected response was used for a request
// whose redirect mode is not follow"). Caching the redirected /dashboard shell
// poisoned every subsequent navigation with that error, so navigations now go
// straight to the network and only truly-static /static/ assets are cached.
const serviceWorkerJS = `
const CACHE = 'auditloop-v2';
const SHELL = ['/static/output.css', '/static/img/icon.svg'];
self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});
self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))).then(() => self.clients.claim()));
});
self.addEventListener('fetch', (e) => {
  if (e.request.method !== 'GET') return;
  // Never touch navigations — let the browser handle redirects/auth itself.
  if (e.request.mode === 'navigate') return;
  const url = new URL(e.request.url);
  // Cache-first ONLY for our own static assets; everything else passes through.
  if (url.origin !== self.location.origin || !url.pathname.startsWith('/static/')) return;
  e.respondWith(
    caches.match(e.request).then((hit) => hit || fetch(e.request).then((res) => {
      const copy = res.clone();
      caches.open(CACHE).then((c) => c.put(e.request, copy));
      return res;
    }))
  );
});
`
