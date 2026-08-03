package httpd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/jobs"
	"rables/internal/service/transfer"
	"rables/internal/templates"
)

// Flash texts of Admin::MigratesController#handle_import / #import_from_zip.
const (
	migratesImportZipNotice    = "ZIP Import in progress, please check the logs for details"
	migratesImportRSSNotice    = "RSS Import in progress, please check the logs for details"
	migratesImportMissingAlert = "Please provide either RSS URL or ZIP file for import"
	migratesImportZipTypeAlert = "ZIP import failed: Only ZIP files are allowed for import"
)

// RegisterMigratesImportRoutes mounts the import endpoints of
// Admin::MigratesController#handle_import: the ZIP upload goes to
// /admin/migrates/import, the RSS URL form to /admin/migrates/import_rss.
func RegisterMigratesImportRoutes(r chi.Router, s *Server) {
	r.With(s.RequireAuth).Post("/admin/migrates/import", s.adminMigratesImportZip)
	r.With(s.RequireAuth).Post("/admin/migrates/import_rss", s.adminMigratesImportRSS)
}

// adminMigratesImportZip handles POST /admin/migrates/import, mirroring
// import_from_zip: the upload is stored under data/imports with a generated
// name and the import_zip job is enqueued; the job deletes the file.
func (s *Server) adminMigratesImportZip(w http.ResponseWriter, r *http.Request) {
	fail := func(alert string) {
		SetFlash(w, templates.Flash{Alert: alert})
		http.Redirect(w, r, "/admin/migrates?tab=import", http.StatusFound)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		fail(migratesImportMissingAlert)
		return
	}
	src, header, err := r.FormFile("zip_file")
	if err != nil {
		fail(migratesImportMissingAlert)
		return
	}
	defer src.Close()

	// Same acceptance rule as Rails: zip content type or a .zip filename.
	if header.Header.Get("Content-Type") != "application/zip" && !strings.HasSuffix(header.Filename, ".zip") {
		fail(migratesImportZipTypeAlert)
		return
	}

	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		s.Log.Error("generate import filename", "error", err)
		fail("ZIP import failed: " + err.Error())
		return
	}
	importsDir := filepath.Join(s.Cfg.DataDir, "imports")
	if err := os.MkdirAll(importsDir, 0o755); err != nil {
		s.Log.Error("create imports dir", "error", err)
		fail("ZIP import failed: " + err.Error())
		return
	}
	tempPath := filepath.Join(importsDir, fmt.Sprintf("import_%d_%s.zip", time.Now().Unix(), hex.EncodeToString(rnd[:])))
	out, err := os.Create(tempPath)
	if err == nil {
		_, err = io.Copy(out, src)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tempPath)
		s.Log.Error("store import upload", "error", err)
		fail("ZIP import failed: " + err.Error())
		return
	}

	payload := transfer.ImportZipPayload{Path: tempPath}
	if _, err := s.Enqueuer().Enqueue(r.Context(), jobs.KindImportZip, payload, time.Now()); err != nil {
		// The job never took ownership of the file: remove it like Rails.
		os.Remove(tempPath)
		s.Log.Error("enqueue zip import", "error", err)
		fail("ZIP import failed: " + err.Error())
		return
	}
	SetFlash(w, templates.Flash{Notice: migratesImportZipNotice})
	http.Redirect(w, r, "/admin/migrates?tab=import", http.StatusFound)
}

// adminMigratesImportRSS handles POST /admin/migrates/import_rss, mirroring
// the url branch of handle_import.
func (s *Server) adminMigratesImportRSS(w http.ResponseWriter, r *http.Request) {
	fail := func(alert string) {
		SetFlash(w, templates.Flash{Alert: alert})
		http.Redirect(w, r, "/admin/migrates?tab=import", http.StatusFound)
	}
	// FormValue covers urlencoded and multipart forms alike.
	feedURL := strings.TrimSpace(r.FormValue("url"))
	if feedURL == "" {
		fail(migratesImportMissingAlert)
		return
	}
	payload := transfer.ImportRSSPayload{
		URL:          feedURL,
		ImportImages: r.FormValue("import_images") != "",
	}
	if _, err := s.Enqueuer().Enqueue(r.Context(), jobs.KindImportRSS, payload, time.Now()); err != nil {
		s.Log.Error("enqueue rss import", "error", err)
		fail("An unexpected error occurred: " + err.Error())
		return
	}
	SetFlash(w, templates.Flash{Notice: migratesImportRSSNotice})
	http.Redirect(w, r, "/admin/migrates?tab=import", http.StatusFound)
}
