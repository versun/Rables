package httpd

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/jobs"
	"rables/internal/service/activity"
	"rables/internal/templates"
)

// Flash text of the export submission (Admin::MigratesController#handle_export).
const migratesExportNotice = "Export Initiated"

// RegisterMigratesAdminRoutes mounts the export/import page, mirroring
// Rails' namespace :admin resources :migrates (index/create). Import
// submission lives in migrates_import.go (RegisterMigratesImportRoutes).
func RegisterMigratesAdminRoutes(r chi.Router, s *Server) {
	r.Route("/admin/migrates", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.adminMigratesIndex)
		r.Post("/export", s.adminMigratesExport)
	})
}

// migratesExportFile is one downloadable zip in data/exports.
type migratesExportFile struct {
	Name    string
	Size    int64
	ModTime int64
}

// adminMigratesData feeds admin_migrates.html.
type adminMigratesData struct {
	Flash     templates.Flash
	ActiveTab string
	TimeZone  string
	Exports   []migratesExportFile
	Imports   []migratesExportFile
}

// adminMigratesIndex renders GET /admin/migrates, mirroring
// Admin::MigratesController#index. The export tab additionally lists the
// zips in data/exports with download links.
func (s *Server) adminMigratesIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.Settings().Get(ctx)
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	s.render(w, http.StatusOK, "admin_migrates", adminMigratesData{
		Flash:     s.PopFlash(r, w),
		ActiveTab: migrateTab(r.URL.Query().Get("tab")),
		TimeZone:  st.TimeZone,
		Exports:   s.listExportFiles(),
		Imports:   s.listImportFiles(),
	})
}

// adminMigratesExport handles POST /admin/migrates/export: it enqueues the
// export job, which packs a copy of the SQLite database plus all media blobs
// into a downloadable zip under data/exports.
func (s *Server) adminMigratesExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fail := func(alert string) {
		s.SetFlash(w, templates.Flash{Alert: alert})
		http.Redirect(w, r, "/admin/migrates?tab=export", http.StatusFound)
	}
	if _, err := s.Enqueuer().Enqueue(ctx, jobs.KindExport, nil, time.Now()); err != nil {
		s.Log.Error("enqueue export", "error", err)
		fail("Export failed: " + err.Error())
		return
	}
	activity.Log(ctx, s.DB, "info", "queued", "export", "")

	s.SetFlash(w, templates.Flash{Notice: migratesExportNotice})
	http.Redirect(w, r, "/admin/migrates?tab=export", http.StatusFound)
}

// migrateTab mirrors Admin::MigratesController#migrate_tab.
func migrateTab(value string) string {
	if value == "import" {
		return "import"
	}
	return "export"
}

// listImportFiles returns the importable files in data/imports (Rables
// backups copied onto the server, e.g. via scp), newest first. Staging
// directories created by running imports, import_* / twitter_archive_*
// uploads owned by an already-enqueued job, and *.queued files already
// enqueued as server imports are skipped.
func (s *Server) listImportFiles() []migratesExportFile {
	entries, err := os.ReadDir(filepath.Join(s.Cfg.DataDir, "imports"))
	if err != nil {
		return nil
	}
	var files []migratesExportFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// import_* files are web uploads and twitter_archive_* files are
		// twitter archive uploads, both already owned by an enqueued job;
		// listing them would invite a duplicate import. The .part temp names
		// of in-progress uploads carry the same prefixes, so they are skipped
		// here as well. name.queued files are server imports already enqueued
		// (adminMigratesImportServerFile).
		if strings.HasPrefix(entry.Name(), "import_") || strings.HasPrefix(entry.Name(), "twitter_archive_") {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".queued") {
			continue
		}
		if !importableDBExt(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, migratesExportFile{Name: entry.Name(), Size: info.Size(), ModTime: info.ModTime().Unix()})
	}
	// Import names are arbitrary (scp/rsync copies), so sort by mtime.
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime > files[j].ModTime })
	return files
}

// listExportFiles returns the *.zip files in data/exports, newest first.
func (s *Server) listExportFiles() []migratesExportFile {
	entries, err := os.ReadDir(filepath.Join(s.Cfg.DataDir, "exports"))
	if err != nil {
		return nil
	}
	var files []migratesExportFile
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".zip" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, migratesExportFile{Name: entry.Name(), Size: info.Size(), ModTime: info.ModTime().Unix()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
	return files
}
