package httpd

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
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

// Upload limits for the migrate imports. The database import covers a full
// export bundle (database + media); the Rails import additionally allows a
// storage zip, which can be large.
const (
	maxImportDBUploadSize    = 2 << 30 // 2 GB
	maxImportRailsUploadSize = 4 << 30 // 4 GB
)

// Flash texts of the import submissions.
const (
	migratesImportDBNotice       = "Database Import in progress, please check the logs for details"
	migratesImportRailsNotice    = "Rails Import in progress, please check the logs for details"
	migratesImportRSSNotice      = "RSS Import in progress, please check the logs for details"
	migratesImportMissingAlert   = "Please provide a file for import"
	migratesImportDBTypeAlert    = "Import failed: only Rables export ZIPs or SQLite database files (.zip, .db, .sqlite, .sqlite3) are allowed"
	migratesImportRailsTypeAlert = "Import failed: the Rails database must be a SQLite file (.sqlite3, .db, .sqlite) and the storage upload a ZIP"
	migratesImportTooBigAlert    = "Import failed: file exceeds the upload size limit"
)

// RegisterMigratesImportRoutes mounts the import endpoints: the Rables
// database upload goes to /admin/migrates/import, the Rails database upload
// to /admin/migrates/import_rails, the RSS URL form to
// /admin/migrates/import_rss.
func RegisterMigratesImportRoutes(r chi.Router, s *Server) {
	r.With(s.RequireAuth).Post("/admin/migrates/import", s.adminMigratesImportDB)
	r.With(s.RequireAuth).Post("/admin/migrates/import_rails", s.adminMigratesImportRails)
	r.With(s.RequireAuth).Post("/admin/migrates/import_rss", s.adminMigratesImportRSS)
}

// migratesImportFail redirects back to the import tab with an alert flash.
func migratesImportFail(w http.ResponseWriter, r *http.Request, alert string) {
	SetFlash(w, templates.Flash{Alert: alert})
	http.Redirect(w, r, "/admin/migrates?tab=import", http.StatusFound)
}

// adminMigratesImportDB handles POST /admin/migrates/import: the uploaded
// Rables export zip (or bare sqlite database) is stored under data/imports
// and the import_db job is enqueued; the job deletes the file.
func (s *Server) adminMigratesImportDB(w http.ResponseWriter, r *http.Request) {
	fail := func(alert string) { migratesImportFail(w, r, alert) }
	r.Body = http.MaxBytesReader(w, r.Body, maxImportDBUploadSize)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			fail(migratesImportTooBigAlert)
		} else {
			fail(migratesImportMissingAlert)
		}
		return
	}
	src, header, err := r.FormFile("backup_file")
	if err != nil {
		fail(migratesImportMissingAlert)
		return
	}
	defer src.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	switch ext {
	case ".zip", ".db", ".sqlite", ".sqlite3":
	default:
		fail(migratesImportDBTypeAlert)
		return
	}

	tempPath, err := s.saveImportUpload(src, ext)
	if err != nil {
		s.Log.Error("store import upload", "error", err)
		fail("Import failed: " + err.Error())
		return
	}
	payload := transfer.ImportDBPayload{Path: tempPath}
	if _, err := s.Enqueuer().Enqueue(r.Context(), jobs.KindImportDB, payload, time.Now()); err != nil {
		// The job never took ownership of the file: remove it.
		os.Remove(tempPath)
		s.Log.Error("enqueue db import", "error", err)
		fail("Import failed: " + err.Error())
		return
	}
	SetFlash(w, templates.Flash{Notice: migratesImportDBNotice})
	http.Redirect(w, r, "/admin/migrates?tab=import", http.StatusFound)
}

// adminMigratesImportRails handles POST /admin/migrates/import_rails: the
// uploaded Rails sqlite database (plus an optional zip of the Rails storage/
// directory) is stored under data/imports and the import_rails job is
// enqueued; the job deletes the files.
func (s *Server) adminMigratesImportRails(w http.ResponseWriter, r *http.Request) {
	fail := func(alert string) { migratesImportFail(w, r, alert) }
	r.Body = http.MaxBytesReader(w, r.Body, maxImportRailsUploadSize)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			fail(migratesImportTooBigAlert)
		} else {
			fail(migratesImportMissingAlert)
		}
		return
	}

	dbSrc, dbHeader, err := r.FormFile("db_file")
	if err != nil {
		fail(migratesImportMissingAlert)
		return
	}
	defer dbSrc.Close()
	dbExt := strings.ToLower(filepath.Ext(dbHeader.Filename))
	switch dbExt {
	case ".db", ".sqlite", ".sqlite3":
	default:
		fail(migratesImportRailsTypeAlert)
		return
	}
	dbPath, err := s.saveImportUpload(dbSrc, dbExt)
	if err != nil {
		s.Log.Error("store rails db upload", "error", err)
		fail("Rails import failed: " + err.Error())
		return
	}

	payload := transfer.ImportRailsPayload{DBPath: dbPath}
	if storageSrc, storageHeader, err := r.FormFile("storage_file"); err == nil {
		defer storageSrc.Close()
		if strings.ToLower(filepath.Ext(storageHeader.Filename)) != ".zip" {
			os.Remove(dbPath)
			fail(migratesImportRailsTypeAlert)
			return
		}
		storagePath, err := s.saveImportUpload(storageSrc, ".zip")
		if err != nil {
			os.Remove(dbPath)
			s.Log.Error("store rails storage upload", "error", err)
			fail("Rails import failed: " + err.Error())
			return
		}
		payload.StoragePath = storagePath
	}

	if _, err := s.Enqueuer().Enqueue(r.Context(), jobs.KindImportRails, payload, time.Now()); err != nil {
		os.Remove(dbPath)
		os.Remove(payload.StoragePath)
		s.Log.Error("enqueue rails import", "error", err)
		fail("Rails import failed: " + err.Error())
		return
	}
	SetFlash(w, templates.Flash{Notice: migratesImportRailsNotice})
	http.Redirect(w, r, "/admin/migrates?tab=import", http.StatusFound)
}

// saveImportUpload streams an upload into data/imports with a generated name.
func (s *Server) saveImportUpload(src multipart.File, ext string) (string, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	importsDir := filepath.Join(s.Cfg.DataDir, "imports")
	if err := os.MkdirAll(importsDir, 0o755); err != nil {
		return "", err
	}
	tempPath := filepath.Join(importsDir, fmt.Sprintf("import_%d_%s%s", time.Now().Unix(), hex.EncodeToString(rnd[:]), ext))
	out, err := os.Create(tempPath)
	if err == nil {
		_, err = io.Copy(out, src)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tempPath)
		return "", err
	}
	return tempPath, nil
}

// adminMigratesImportRSS handles POST /admin/migrates/import_rss, mirroring
// the url branch of handle_import.
func (s *Server) adminMigratesImportRSS(w http.ResponseWriter, r *http.Request) {
	fail := func(alert string) { migratesImportFail(w, r, alert) }
	// FormValue covers urlencoded and multipart forms alike.
	feedURL := strings.TrimSpace(r.FormValue("url"))
	if feedURL == "" {
		fail("Please provide an RSS URL for import")
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
