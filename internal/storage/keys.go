package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug normalizes a string into a filesystem/URL-safe token. Empty input yields
// "root".
func Slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "root"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// PageSlug derives a stable, unique-ish slug for a page URL: a readable path
// portion plus a short content hash so distinct URLs never collide.
func PageSlug(pageURL string) string {
	h := sha256.Sum256([]byte(pageURL))
	short := hex.EncodeToString(h[:])[:8]
	// Strip scheme+host for readability.
	readable := pageURL
	if i := strings.Index(readable, "://"); i >= 0 {
		readable = readable[i+3:]
	}
	if i := strings.Index(readable, "/"); i >= 0 {
		readable = readable[i:]
	} else {
		readable = "/"
	}
	return Slug(readable) + "-" + short
}

// ScreenshotKey builds the key for a page screenshot at a viewport.
func ScreenshotKey(targetSlug, runID, pageSlug, viewport string) string {
	return targetSlug + "/" + runID + "/" + pageSlug + "/" + Slug(viewport) + ".png"
}

// AxeKey builds the key for a page's axe results.
func AxeKey(targetSlug, runID, pageSlug string) string {
	return targetSlug + "/" + runID + "/" + pageSlug + "/axe.json"
}

// NetworkKey builds the key for a page's network log.
func NetworkKey(targetSlug, runID, pageSlug string) string {
	return targetSlug + "/" + runID + "/" + pageSlug + "/network.json"
}

// A11yDigestKey builds the key for a page's DOM/accessibility digest (Phase 1
// persona-evaluator grounding). Mirrors AxeKey — per page_slug (viewport-independent
// DOM), stored beside axe.json.
func A11yDigestKey(targetSlug, runID, pageSlug string) string {
	return targetSlug + "/" + runID + "/" + pageSlug + "/a11y.json"
}

// ReportKey builds the run-level report.json key.
func ReportKey(targetSlug, runID string) string {
	return targetSlug + "/" + runID + "/report.json"
}

// FaviconKey builds the run-scoped key for a captured site favicon. It is
// RUN-SCOPED ({target_slug}/{run_id}/favicon.{ext}) so the artifact proxy's
// per-object ownership check (resolve run_id → owner-scoped GetRun) authorizes it
// exactly like a screenshot. ext is the raster extension (png|jpg|gif|webp|ico).
func FaviconKey(targetSlug, runID, ext string) string {
	if ext == "" {
		ext = "png"
	}
	return targetSlug + "/" + runID + "/favicon." + ext
}

// FaviconContentType maps a favicon storage extension to the MIME type the
// artifact proxy serves it under. Raster types only (SVG is never stored). An
// unknown ext falls back to a generic octet-stream so it renders (never HTML).
func FaviconContentType(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

// LoginTestKey builds the key for a login-test end-state screenshot (P4). The
// first segment is the target's GLOBALLY-UNIQUE id (NOT the name-slug) so per-object
// ownership can bind authorization to the exact owning target: the browser artifact
// route resolves it via owner-scoped GetTarget. (Name-slugs are derived from the
// target name and are NOT unique per user, so keying by slug would let a same-name
// target of another user satisfy a slug check.) testID is a random token so
// successive tests don't overwrite each other.
func LoginTestKey(targetID, testID string) string {
	return targetID + "/login-tests/" + testID + ".png"
}

// WalkthroughStepKey builds the key for a goal-directed walkthrough step's
// screenshot (Phase 3). Like LoginTestKey it is TARGET-SCOPED (the first segment
// is the GLOBALLY-UNIQUE target id, NOT the name-slug) so per-object ownership can
// bind authorization to the exact owning target: the browser artifact route
// resolves it via owner-scoped GetTarget. idx is the step index within the run.
func WalkthroughStepKey(targetID, walkthroughID string, idx int) string {
	return targetID + "/walkthroughs/" + walkthroughID + "/" + strconv.Itoa(idx) + ".png"
}

// DiffKey builds the key for a page's visual-diff image at a viewport. It lives
// beside the current run's screenshot so it is discoverable from the screenshot
// key (…/mobile.png → …/mobile.diff.png).
func DiffKey(targetSlug, runID, pageSlug, viewport string) string {
	return targetSlug + "/" + runID + "/" + pageSlug + "/" + Slug(viewport) + ".diff.png"
}
