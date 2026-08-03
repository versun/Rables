package httpd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/service/activity"
	"rables/internal/service/media"
	"rables/internal/templates"
)

// RegisterStaticFilesRoutes mounts the admin static-file manager behind
// RequireAuth plus the public serving route, mirroring Rails'
// resources :static_files (index/create/destroy) and
// get "/static/*filename" => static_files#show. DELETE maps to
// POST /admin/static_files/{id}/destroy.
func RegisterStaticFilesRoutes(r chi.Router, s *Server) {
	r.Route("/admin/static_files", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.adminStaticFilesIndex)
		r.Post("/", s.adminStaticFilesCreate)
		r.Post("/{id}/destroy", s.adminStaticFilesDestroy)
	})
	r.Get("/static/*", s.serveStaticFile)
}

// adminStaticFileRow is one list row with its derived display values.
type adminStaticFileRow struct {
	ID          int64
	Filename    string
	Description string // "-" when blank, like the Rails view
	SizeHuman   string
	UploadedAt  string // formatted in settings.time_zone
	PublicPath  string
}

// adminStaticFilesIndexData feeds admin_static_files_index.html.
type adminStaticFilesIndexData struct {
	Flash templates.Flash
	Files []adminStaticFileRow
}

// adminStaticFilesIndex renders GET /admin/static_files (newest first).
func (s *Server) adminStaticFilesIndex(w http.ResponseWriter, r *http.Request) {
	s.renderStaticFilesIndex(w, r, http.StatusOK, PopFlash(r, w))
}

// renderStaticFilesIndex lists the files and renders the index page with the
// given status/flash (validation failures re-render it like Rails'
// flash.now + render :index).
func (s *Server) renderStaticFilesIndex(w http.ResponseWriter, r *http.Request, status int, flash templates.Flash) {
	rows, err := s.Q.ListStaticFiles(r.Context())
	if err != nil {
		s.Log.Error("list static files", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tz := s.siteTimeZone(r)
	files := make([]adminStaticFileRow, 0, len(rows))
	for _, row := range rows {
		desc := row.Description.String
		if desc == "" {
			desc = "-"
		}
		files = append(files, adminStaticFileRow{
			ID:          row.ID,
			Filename:    row.Filename,
			Description: desc,
			SizeHuman:   humanFileSize(row.ByteSize),
			UploadedAt:  templates.FormatTime(row.CreatedAt, tz, "2006-01-02 15:04"),
			PublicPath:  "/static/" + row.Filename,
		})
	}
	s.render(w, status, "admin_static_files_index", adminStaticFilesIndexData{Flash: flash, Files: files})
}

// adminStaticFilesCreate handles POST /admin/static_files, mirroring
// Admin::StaticFilesController#create: the upload keeps its original
// filename, and uploading over an existing filename replaces the stored file
// in place.
func (s *Server) adminStaticFilesCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	src, header, err := r.FormFile("file")
	if err != nil {
		s.renderStaticFilesIndex(w, r, http.StatusOK, templates.Flash{Alert: "请选择要上传的文件"})
		return
	}
	defer src.Close()

	filename := header.Filename
	description := r.FormValue("description")
	if msg := staticFilenameError(filename); msg != "" {
		activity.Log(r.Context(), s.DB, "error", "failed", "static_file",
			fmt.Sprintf("filename=%s errors=%s", activity.Quote(filename), activity.Quote(msg)))
		s.renderStaticFilesIndex(w, r, http.StatusOK, templates.Flash{Alert: "文件上传失败: " + msg})
		return
	}

	key, err := s.Media().Store(r.Context(), src, filename, header.Header.Get("Content-Type"))
	if err != nil {
		s.Log.Error("store static file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	file, err := s.Media().FileByKey(r.Context(), key)
	if err != nil {
		s.Log.Error("lookup stored static file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	now := time.Now().Unix()
	existing, err := s.Q.GetStaticFileByFilename(r.Context(), filename)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := s.Q.CreateStaticFile(r.Context(), query.CreateStaticFileParams{
			Filename:    filename,
			Description: sql.NullString{String: description, Valid: description != ""},
			FileID:      file.ID,
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			s.Log.Error("create static file", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		activity.Log(r.Context(), s.DB, "info", "created", "static_file",
			fmt.Sprintf("filename=%s", activity.Quote(filename)))
		SetFlash(w, templates.Flash{Notice: "文件上传成功"})
	case err != nil:
		s.Log.Error("find static file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	default:
		// Overwrite in place, keeping the original filename like the Rails
		// controller; the previously stored blob is purged afterwards.
		oldFile, oldErr := s.Q.GetFileForStaticFilename(r.Context(), filename)
		if err := s.Q.UpdateStaticFile(r.Context(), query.UpdateStaticFileParams{
			Description: sql.NullString{String: description, Valid: description != ""},
			FileID:      file.ID,
			UpdatedAt:   now,
			ID:          existing.ID,
		}); err != nil {
			s.Log.Error("update static file", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if oldErr == nil {
			s.purgeStoredFile(r.Context(), oldFile)
		}
		activity.Log(r.Context(), s.DB, "info", "updated", "static_file",
			fmt.Sprintf("filename=%s", activity.Quote(filename)))
		SetFlash(w, templates.Flash{Notice: "文件上传成功（已覆盖同名文件）"})
	}
	http.Redirect(w, r, "/admin/static_files", http.StatusFound)
}

// adminStaticFilesDestroy handles POST /admin/static_files/{id}/destroy
// (Rails DELETE /admin/static_files/:id), mirroring
// Admin::StaticFilesController#destroy.
func (s *Server) adminStaticFilesDestroy(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	staticFile, err := s.Q.GetStaticFileByID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.Log.Error("get static file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	file, fileErr := s.Q.GetFileForStaticFilename(r.Context(), staticFile.Filename)
	if err := s.Q.DeleteStaticFile(r.Context(), id); err != nil {
		activity.Log(r.Context(), s.DB, "error", "failed", "static_file",
			fmt.Sprintf("filename=%s", activity.Quote(staticFile.Filename)))
		SetFlash(w, templates.Flash{Alert: "删除失败"})
		http.Redirect(w, r, "/admin/static_files", http.StatusFound)
		return
	}
	if fileErr == nil {
		s.purgeStoredFile(r.Context(), file)
	}
	activity.Log(r.Context(), s.DB, "info", "deleted", "static_file",
		fmt.Sprintf("filename=%s", activity.Quote(staticFile.Filename)))
	SetFlash(w, templates.Flash{Notice: fmt.Sprintf("文件 %s 已删除", staticFile.Filename)})
	http.Redirect(w, r, "/admin/static_files", http.StatusFound)
}

// serveStaticFile handles GET /static/*filename, mirroring
// StaticFilesController#show: the filename resolves to its stored blob and
// the response redirects to the blob's service URL (/files/{key}, which sets
// the stored Content-Type). Unknown filenames are 404.
func (s *Server) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	filename := chi.URLParam(r, "*")
	file, err := s.Q.GetFileForStaticFilename(r.Context(), filename)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/files/"+file.Key, http.StatusFound)
}

// purgeStoredFile removes a files row (plus any image variant rows) and its
// on-disk blobs, mirroring ActiveStorage's dependent purge of an attachment's
// blob. Errors are logged, never fatal.
func (s *Server) purgeStoredFile(ctx context.Context, file query.File) {
	variants, err := s.Q.ListFileVariants(ctx, sql.NullInt64{Int64: file.ID, Valid: true})
	if err != nil {
		s.Log.Error("list file variants", "file_id", file.ID, "error", err)
	}
	for _, variant := range variants {
		s.removeStoredFile(ctx, variant)
	}
	s.removeStoredFile(ctx, file)
}

// removeStoredFile deletes one files row and its blob on disk.
func (s *Server) removeStoredFile(ctx context.Context, file query.File) {
	if err := s.Q.DeleteFileByID(ctx, file.ID); err != nil {
		s.Log.Error("delete file row", "file_id", file.ID, "error", err)
	}
	if media.ValidKey(file.Key) {
		if err := os.Remove(s.Media().PathFor(file.Key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.Log.Error("remove blob", "key", file.Key, "error", err)
		}
	}
}

// staticFilenameError mirrors the StaticFile filename validations, returning
// the Rails full-message wording or "" when valid.
func staticFilenameError(filename string) string {
	if strings.TrimSpace(filename) == "" {
		return "Filename can't be blank"
	}
	for _, c := range filename {
		if c == '/' || c < 0x20 || c == 0x7f {
			return "Filename must not contain slashes or control characters"
		}
	}
	return ""
}

// humanFileSize mirrors StaticFile#file_size_human: bytes below 1KB, KB below
// 1MB, then MB, rounded to two decimals.
func humanFileSize(size int64) string {
	const kb = 1024
	const mb = 1024 * kb
	switch {
	case size < kb:
		return fmt.Sprintf("%d B", size)
	case size < mb:
		return fmt.Sprintf("%s KB", strconv.FormatFloat(round2(float64(size)/kb), 'f', -1, 64))
	default:
		return fmt.Sprintf("%s MB", strconv.FormatFloat(round2(float64(size)/mb), 'f', -1, 64))
	}
}

// round2 mirrors Rails Float#round(2).
func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
