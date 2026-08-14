package httpd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"rables/internal/assets"
)

// TestAssetsRoutes covers the embedded frontend assets: fingerprinted URLs
// are served immutable for a year, the plain logical URLs keep ETag + a
// one-hour TTL with 304 revalidation, and unknown names 404.
func TestAssetsRoutes(t *testing.T) {
	r := chi.NewRouter()
	RegisterAssetsRoutes(r, nil)

	names := []string{
		"app.js", "app.css", "admin.css",
		"lexxy.min.js", "lexxy.css", "activestorage_shim.js",
		"easymde.min.js", "easymde.min.css",
	}
	contentType := func(name string) string {
		if name[len(name)-4:] == ".css" {
			return "text/css; charset=utf-8"
		}
		return "text/javascript; charset=utf-8"
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/"+name, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != contentType(name) {
				t.Errorf("Content-Type = %q, want %q", ct, contentType(name))
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
				t.Errorf("Cache-Control = %q", cc)
			}
			etag := rec.Header().Get("ETag")
			if etag == "" {
				t.Fatal("missing ETag")
			}
			if rec.Body.Len() == 0 {
				t.Fatal("empty body")
			}

			re := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/assets/"+name, nil)
			req.Header.Set("If-None-Match", etag)
			r.ServeHTTP(re, req)
			if re.Code != http.StatusNotModified {
				t.Errorf("revalidate status = %d, want 304", re.Code)
			}
			if re.Body.Len() != 0 {
				t.Errorf("304 body = %d bytes, want 0", re.Body.Len())
			}
		})

		t.Run(name+" fingerprinted", func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/"+assets.Name(name), nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != contentType(name) {
				t.Errorf("Content-Type = %q, want %q", ct, contentType(name))
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
				t.Errorf("Cache-Control = %q", cc)
			}
			if rec.Body.Len() == 0 {
				t.Fatal("empty body")
			}
		})
	}

	t.Run("unknown asset 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/nope.js", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("wrong fingerprint 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.00000000.js", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}
