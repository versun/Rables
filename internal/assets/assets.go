// Package assets embeds the static frontend files served under
// /assets/: app.js (vanilla JS, plan §9), app.css (public/auth core styles),
// admin.css (the scoped admin design system, loaded by admin_layout.html),
// the vendored lexxy rich-text editor (lexxy.min.js + lexxy.css, lexxy gem
// 0.9.28, MIT — the same editor Rails uses via the lexxy gem), the
// @rails/activestorage shim lexxy's attachment uploads resolve through the
// admin import map (activestorage_shim.js), and the vendored EasyMDE markdown
// editor (easymde.min.js + easymde.min.css, v2.20.0, MIT).
package assets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"strings"
)

//go:embed app.js app.css admin.css lexxy.min.js lexxy.css activestorage_shim.js easymde.min.js easymde.min.css
var FS embed.FS

// Templates reference content-fingerprinted file names
// ("app.js" -> "app.c694bfa3.js") via the assetURL template func, served with
// an immutable year-long cache: a deploy changes the URL, so no cache layer
// (browser, CDN browser-TTL override — see the 2026-08-14 incident) can pin a
// stale copy. The plain logical names stay valid with a short TTL for pages
// rendered before a deploy (and for app.js's lazy imports of the vendored
// editor bundles, whose versions are pinned in the file contents).
var (
	fingerprinted = map[string]string{} // logical name -> fingerprinted name
	logical       = map[string]string{} // fingerprinted name -> logical name
)

func init() {
	entries, err := FS.ReadDir(".")
	if err != nil {
		panic("assets: " + err.Error())
	}
	for _, e := range entries {
		body, err := FS.ReadFile(e.Name())
		if err != nil {
			panic("assets: " + e.Name() + ": " + err.Error())
		}
		sum := sha256.Sum256(body)
		base, ext := e.Name(), ""
		if i := strings.LastIndex(base, "."); i >= 0 {
			base, ext = base[:i], base[i:]
		}
		fp := base + "." + hex.EncodeToString(sum[:4]) + ext
		fingerprinted[e.Name()] = fp
		logical[fp] = e.Name()
	}
}

// Name returns the content-fingerprinted file name for a logical asset name,
// panicking on an unknown name: a typo must fail fast at render time, not 404
// per page load.
func Name(name string) string {
	fp, ok := fingerprinted[name]
	if !ok {
		panic("assets: unknown asset " + name)
	}
	return fp
}

// Resolve maps a requested file name back to the embedded logical name and
// reports whether the request carried the fingerprint (cacheable forever) or
// the plain logical name (short TTL). ok is false for anything never
// embedded.
func Resolve(name string) (logicalName string, hasFingerprint bool, ok bool) {
	if l, hit := logical[name]; hit {
		return l, true, true
	}
	if _, hit := fingerprinted[name]; hit {
		return name, false, true
	}
	return "", false, false
}
