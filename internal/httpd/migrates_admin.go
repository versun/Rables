package httpd

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/jobs"
	"rables/internal/service/activity"
	"rables/internal/service/transfer"
	"rables/internal/templates"
)

// Flash texts of Admin::MigratesController#handle_export.
const (
	migratesExportNotice         = "Export Initiated"
	migratesMarkdownExportNotice = "Markdown Export Initiated"
	migratesBadExportTypeAlert   = "Unsupported export type"
)

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
		Flash:     PopFlash(r, w),
		ActiveTab: migrateTab(r.URL.Query().Get("tab")),
		TimeZone:  st.TimeZone,
		Exports:   s.listExportFiles(),
	})
}

// adminMigratesExport handles POST /admin/migrates/export, mirroring
// handle_export: it enqueues the export job and flashes the same notices.
func (s *Server) adminMigratesExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fail := func(alert string) {
		SetFlash(w, templates.Flash{Alert: alert})
		http.Redirect(w, r, "/admin/migrates?tab=export", http.StatusFound)
	}
	if err := r.ParseForm(); err != nil {
		fail(migratesBadExportTypeAlert)
		return
	}

	exportType := r.PostFormValue("export_type")
	if exportType == "" {
		exportType = "default"
	}
	notice := migratesExportNotice
	switch exportType {
	case "default":
	case "markdown":
		notice = migratesMarkdownExportNotice
	default:
		fail(migratesBadExportTypeAlert)
		return
	}

	payload := transfer.ExportPayload{
		Format:          exportType,
		KeepCredentials: r.PostFormValue("keep_credentials") == "1",
	}
	if _, err := s.Enqueuer().Enqueue(ctx, jobs.KindExport, payload, time.Now()); err != nil {
		s.Log.Error("enqueue export", "error", err)
		fail("Export failed: " + err.Error())
		return
	}
	activity.Log(ctx, s.DB, "info", "queued", "export", "format="+exportType)

	SetFlash(w, templates.Flash{Notice: notice})
	http.Redirect(w, r, "/admin/migrates?tab=export", http.StatusFound)
}

// migrateTab mirrors Admin::MigratesController#migrate_tab.
func migrateTab(value string) string {
	if value == "import" {
		return "import"
	}
	return "export"
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
