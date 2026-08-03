package httpd

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/go-chi/chi/v5"

	"rables/internal/assets"
)

// RegisterAssetsRoutes mounts GET /assets/* for the embedded frontend files
// (plan T28). URLs are fixed (no content hash) so the embedded layout
// template can reference them statically; every response carries a strong
// ETag (content sha256) plus a short max-age, so repeat visits revalidate
// cheaply with 304s and a deploy is picked up within an hour.
func RegisterAssetsRoutes(r chi.Router, s *Server) {
	r.Get("/assets/app.js", serveEmbeddedAsset("app.js", "text/javascript; charset=utf-8"))
	r.Get("/assets/app.css", serveEmbeddedAsset("app.css", "text/css; charset=utf-8"))
	r.Get("/assets/lexxy.min.js", serveEmbeddedAsset("lexxy.min.js", "text/javascript; charset=utf-8"))
	r.Get("/assets/lexxy.css", serveEmbeddedAsset("lexxy.css", "text/css; charset=utf-8"))
}

// serveEmbeddedAsset loads name from the embedded FS once and returns a
// handler serving it with ETag/Cache-Control. A missing file panics at
// registration time: //go:embed makes that a build-time asset, so a typo
// must fail fast at startup, not per request.
func serveEmbeddedAsset(name, contentType string) http.HandlerFunc {
	body, err := assets.FS.ReadFile(name)
	if err != nil {
		panic("httpd: embedded asset " + name + ": " + err.Error())
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
