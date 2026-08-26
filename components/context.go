// Package components holds shared gomponents view helpers and the per-request
// page context threaded into every view.
package components

// PageContext carries per-request rendering config into the layouts/pages.
type PageContext struct {
	Title           string
	UserEmail       string
	DevMode         bool
	SupabaseURL     string
	SupabaseAnonKey string
	JSVersion       string
	CSSVersion      string
}
