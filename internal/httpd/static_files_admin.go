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
	s.renderStaticFilesIndex(w, r, http.StatusOK, s.PopFlash(r, w))
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
	// Large uploads cannot fit the server-wide 30s ReadTimeout / 60s
	// WriteTimeout.
	s.clearRequestDeadlines(w)
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "bad request", http.StatusBadRequest)
		}
		return
	}
	// File parts above maxMemory spill to os.TempDir(); net/http never
	// cleans them up. Store below reads the part synchronously, so the
	// deferred removal runs after the copy finished.
	defer r.MultipartForm.RemoveAll()
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

	file, err := s.Media().Store(r.Context(), src, filename, header.Header.Get("Content-Type"))
	if err != nil {
		s.Log.Error("store static file", "error", err)
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
			// The DB step failed after Store succeeded: purge the freshly
			// stored blob so it does not stay publicly reachable at
			// /files/{key} with nothing referencing it.
			s.purgeStoredFile(r.Context(), file)
			if isUniqueViolation(err) {
				// A concurrent upload of the same new filename inserted the
				// row first (both passed the existence check above): the
				// same lost-race outcome as the compare-and-swap mismatch
				// on the overwrite path below.
				activity.Log(r.Context(), s.DB, "error", "failed", "static_file",
					fmt.Sprintf("filename=%s errors=%s", activity.Quote(filename), activity.Quote("concurrent upload")))
				s.renderStaticFilesIndex(w, r, http.StatusOK, templates.Flash{Alert: "文件上传失败: 与另一个上传冲突，请重试"})
				return
			}
			s.Log.Error("create static file", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		activity.Log(r.Context(), s.DB, "info", "created", "static_file",
			fmt.Sprintf("filename=%s", activity.Quote(filename)))
		s.SetFlash(w, templates.Flash{Notice: "文件上传成功"})
	case err != nil:
		s.purgeStoredFile(r.Context(), file)
		s.Log.Error("find static file", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	default:
		// Overwrite in place, keeping the original filename like the Rails
		// controller; the previously stored blob is purged afterwards. The
		// update compare-and-swaps on the file_id read above, so a concurrent
		// overwrite (or destroy) of the same filename loses the race instead
		// of leaking its stored blob.
		oldFile, oldErr := s.Q.GetFileForStaticFilename(r.Context(), filename)
		updated, err := s.Q.UpdateStaticFile(r.Context(), query.UpdateStaticFileParams{
			Description:    sql.NullString{String: description, Valid: description != ""},
			FileID:         file.ID,
			UpdatedAt:      now,
			ID:             existing.ID,
			ExpectedFileID: existing.FileID,
		})
		if err != nil {
			// Purge only the new blob: the static_files row still points at
			// the previous file, whose blob must stay.
			s.purgeStoredFile(r.Context(), file)
			s.Log.Error("update static file", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if updated == 0 {
			// Lost the race: the row now points at the winner's blob (or is
			// gone), so purge only the blob this request stored.
			s.purgeStoredFile(r.Context(), file)
			activity.Log(r.Context(), s.DB, "error", "failed", "static_file",
				fmt.Sprintf("filename=%s errors=%s", activity.Quote(filename), activity.Quote("concurrent overwrite")))
			s.renderStaticFilesIndex(w, r, http.StatusOK, templates.Flash{Alert: "文件上传失败: 与另一个上传冲突，请重试"})
			return
		}
		if oldErr == nil {
			s.purgeStoredFile(r.Context(), oldFile)
		} else if !errors.Is(oldErr, sql.ErrNoRows) {
			// The old blob could not be resolved for purging (transient DB
			// error); log it instead of silently leaking it.
			s.Log.Error("get replaced static file blob", "filename", filename, "error", oldErr)
		}
		activity.Log(r.Context(), s.DB, "info", "updated", "static_file",
			fmt.Sprintf("filename=%s", activity.Quote(filename)))
		s.SetFlash(w, templates.Flash{Notice: "文件上传成功（已覆盖同名文件）"})
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
	// Delete returns the file_id the row pointed at when it was actually
	// removed: a concurrent overwrite may have swapped the row to a new blob
	// since the read above, and purging the stale read would orphan the
	// current blob (files row plus disk blob left reachable at /files/{key}).
	fileID, err := s.Q.DeleteStaticFile(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		// A concurrent destroy removed the row first and owns the purge.
		s.SetFlash(w, templates.Flash{Notice: fmt.Sprintf("文件 %s 已删除", staticFile.Filename)})
		http.Redirect(w, r, "/admin/static_files", http.StatusFound)
		return
	}
	if err != nil {
		activity.Log(r.Context(), s.DB, "error", "failed", "static_file",
			fmt.Sprintf("filename=%s", activity.Quote(staticFile.Filename)))
		s.SetFlash(w, templates.Flash{Alert: "删除失败"})
		http.Redirect(w, r, "/admin/static_files", http.StatusFound)
		return
	}
	file, fileErr := s.Q.GetFileByID(r.Context(), fileID)
	if fileErr == nil {
		s.purgeStoredFile(r.Context(), file)
	} else if !errors.Is(fileErr, sql.ErrNoRows) {
		s.Log.Error("get static file blob", "file_id", fileID, "error", fileErr)
	}
	activity.Log(r.Context(), s.DB, "info", "deleted", "static_file",
		fmt.Sprintf("filename=%s", activity.Quote(staticFile.Filename)))
	s.SetFlash(w, templates.Flash{Notice: fmt.Sprintf("文件 %s 已删除", staticFile.Filename)})
	http.Redirect(w, r, "/admin/static_files", http.StatusFound)
}

// serveStaticFile handles GET /static/*filename, mirroring
// StaticFilesController#show: the filename resolves to its stored blob and
// the response redirects to the blob's service URL (/files/{key}, which
// serves inert types inline with the stored Content-Type but forces an
// attachment download for active or missing ones). Unknown filenames are
// 404.
func (s *Server) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	// Filenames may contain characters clients percent-encode (non-ASCII or
	// sub-delims like "&"), so decode the wildcard param like slugParam does:
	// chi leaves it escaped whenever it routed on URL.RawPath.
	filename := slugParam(r, "*")
	file, err := s.Q.GetFileForStaticFilename(r.Context(), filename)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/files/"+file.Key, http.StatusFound)
}

// purgeStoredFile removes a files row (plus any image variant rows) and its
// on-disk blobs, mirroring ActiveStorage's dependent purge of an attachment's
// blob. A database import can merge a static file onto a files row that is
// also referenced by an attachment (or another static_files entry), so a row
// that is still referenced keeps its row, variants and blob; the static_files
// row itself is deleted or re-pointed by the caller before purging, so it no
// longer counts. Errors are logged, never fatal.
func (s *Server) purgeStoredFile(ctx context.Context, file query.File) {
	referenced, err := s.fileReferenced(ctx, file.ID)
	if err != nil {
		s.Log.Error("count file references", "file_id", file.ID, "error", err)
		return
	}
	if referenced {
		return // still referenced elsewhere: keep row, variants and blob
	}
	variants, err := s.Q.ListFileVariants(ctx, sql.NullInt64{Int64: file.ID, Valid: true})
	if err != nil {
		// The variant family is unknown, so purging the original could orphan
		// a referenced variant: abort the whole purge like a reference-count
		// error below does.
		s.Log.Error("list file variants", "file_id", file.ID, "error", err)
		return
	}
	for _, variant := range variants {
		vReferenced, err := s.fileReferenced(ctx, variant.ID)
		if err != nil {
			s.Log.Error("count file references", "file_id", variant.ID, "error", err)
			return
		}
		if vReferenced {
			// A variant that is itself referenced pins its original via
			// files.variant_of: keep the whole family.
			return
		}
	}
	for _, variant := range variants {
		s.removeStoredFile(ctx, variant)
	}
	s.removeStoredFile(ctx, file)
}

// fileReferenced reports whether a files row is still referenced by an
// attachment or a static_files entry. static_files.file_id references
// files(id) under foreign_keys enforcement, and a database import can merge a
// static file onto a row that is also attached to a record, so both count.
func (s *Server) fileReferenced(ctx context.Context, fileID int64) (bool, error) {
	attached, err := s.Q.CountAttachmentsForFile(ctx, fileID)
	if err != nil {
		return false, err
	}
	static, err := s.Q.CountStaticFilesForFile(ctx, fileID)
	if err != nil {
		return false, err
	}
	return attached+static > 0, nil
}

// removeStoredFile deletes one files row and its blob on disk. When the row
// delete fails (e.g. an import merged a fresh reference between the
// fileReferenced check and now), the blob must stay: the row still points at
// it, so removing it would leave a DB row behind a missing file.
func (s *Server) removeStoredFile(ctx context.Context, file query.File) {
	if err := s.Q.DeleteFileByID(ctx, file.ID); err != nil {
		s.Log.Error("delete file row", "file_id", file.ID, "error", err)
		return
	}
	if media.ValidKey(file.Key) {
		if err := os.Remove(s.Media().PathFor(file.Key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.Log.Error("remove blob", "key", file.Key, "error", err)
		}
	}
}

// staticFilenameError mirrors the StaticFile filename validations, returning
// the Rails full-message wording or "" when valid. It is stricter than Rails
// about '#', '?', '%' and spaces: the public path is a bare
// "/static/"+filename concatenation, and html/template's URL normalization
// keeps '#' and '?' as fragment/query delimiters (a#b.png would link to
// /static/a and 404), preserves valid percent-escapes (a%20b.png stays
// escaped in the href, so the server decodes the request to /static/a b.png,
// which no longer matches the stored filename and 404s), while a raw space
// breaks the URL anywhere it is not percent-encoded. '\' is rejected too:
// Part.FileName only applies filepath.Base to '/', so a\b.txt passes through
// on Linux, but WHATWG URL parsing treats '\' as '/' in special schemes, so
// the browser requests /static/a/b.txt, which never matches and 404s.
// Dot-only names (".", "..", ...) are rejected for the same reason: the
// browser normalizes /static/.. to / and /static/. to /static/, so the
// public link never reaches the stored file. Validation runs only at
// upload, so existing rows with such names keep serving.
func staticFilenameError(filename string) string {
	if strings.TrimSpace(filename) == "" {
		return "Filename can't be blank"
	}
	if strings.Trim(filename, ".") == "" {
		return "Filename must not be only dots"
	}
	for _, c := range filename {
		if c == '/' || c == '\\' || c == ' ' || c == '#' || c == '?' || c == '%' || c < 0x20 || c == 0x7f {
			return "Filename must not contain slashes, spaces, ?, #, % or control characters"
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
