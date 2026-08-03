package httpd

import (
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
)

// RegisterDownloadsRoutes mounts GET /admin/downloads/{filename}, mirroring
// Admin::DownloadsController#show. It serves export/import zips from
// data/exports and data/imports only; traversal is blocked by filepath.Base
// plus a filepath.EvalSymlinks prefix check (plan §4.11). Unlike the Rails
// redirect-with-flash on error, invalid names get 400 and missing files 404.
func RegisterDownloadsRoutes(r chi.Router, s *Server) {
	r.Route("/admin/downloads", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/{filename:[^/]+}", s.adminDownloadShow)
	})
}

func (s *Server) adminDownloadShow(w http.ResponseWriter, r *http.Request) {
	// chi hands over the raw (still escaped) segment, so decode first: an
	// encoded slash (%2f) must be caught by the basename check below.
	name, err := url.PathUnescape(chi.URLParam(r, "filename"))
	if err != nil {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}
	if name != filepath.Base(name) || name == "" || strings.HasPrefix(name, ".") {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

	for _, dir := range []string{
		filepath.Join(s.Cfg.DataDir, "exports"),
		filepath.Join(s.Cfg.DataDir, "imports"),
	} {
		if serveDownload(w, r, dir, name) {
			return
		}
	}
	http.NotFound(w, r)
}

// serveDownload serves dir/name as an attachment when the file exists and
// its symlink-resolved path stays inside dir. It reports whether the file
// was found (and served or rejected).
func serveDownload(w http.ResponseWriter, r *http.Request, dir, name string) bool {
	path := filepath.Join(dir, name)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}

	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	if realPath != realDir && !strings.HasPrefix(realPath, realDir+string(os.PathSeparator)) {
		// Symlink escapes the download directory.
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return true
	}

	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(w, r, name, info.ModTime(), f)
	return true
}
