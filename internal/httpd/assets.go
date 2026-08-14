package httpd

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"rables/internal/assets"
)

// RegisterAssetsRoutes mounts GET /assets/* for the embedded frontend files
// (plan T28). Templates reference content-fingerprinted URLs
// (/assets/app.c694bfa3.js, see assets.Name), served immutable for a year: a
// deploy changes the URL, so no cache layer can pin a stale copy. The plain
// logical URLs keep serving with ETag + a one-hour TTL for pages rendered
// before a deploy.
func RegisterAssetsRoutes(r chi.Router, s *Server) {
	r.Get("/assets/{file}", serveAsset)
}

// serveAsset serves one embedded file resolved by assets.Resolve. Only .js
// and .css are ever embedded; the content type derives from the extension.
func serveAsset(w http.ResponseWriter, r *http.Request) {
	name, fingerprinted, ok := assets.Resolve(chi.URLParam(r, "file"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Resolve only returns names from the embedded FS, so this cannot fail.
	body, _ := assets.FS.ReadFile(name)

	contentType := "text/javascript; charset=utf-8"
	if strings.HasSuffix(name, ".css") {
		contentType = "text/css; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)

	if fingerprinted {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		sum := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
