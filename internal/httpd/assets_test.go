package httpd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestAssetsRoutes covers the embedded frontend assets: 200 with the right
// Content-Type, an ETag + Cache-Control on every file, 304 on revalidation,
// and 404 for anything else under /assets/.
func TestAssetsRoutes(t *testing.T) {
	r := chi.NewRouter()
	RegisterAssetsRoutes(r, nil)

	cases := []struct {
		path        string
		contentType string
	}{
		{"/assets/app.js", "text/javascript; charset=utf-8"},
		{"/assets/app.css", "text/css; charset=utf-8"},
		{"/assets/admin.css", "text/css; charset=utf-8"},
		{"/assets/lexxy.min.js", "text/javascript; charset=utf-8"},
		{"/assets/lexxy.css", "text/css; charset=utf-8"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != tc.contentType {
				t.Errorf("Content-Type = %q, want %q", ct, tc.contentType)
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
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("If-None-Match", etag)
			r.ServeHTTP(re, req)
			if re.Code != http.StatusNotModified {
				t.Errorf("revalidate status = %d, want 304", re.Code)
			}
			if re.Body.Len() != 0 {
				t.Errorf("304 body = %d bytes, want 0", re.Body.Len())
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
}
