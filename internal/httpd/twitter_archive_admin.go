package httpd

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/jobs"
	"rables/internal/service/activity"
	"rables/internal/service/twitterarchive"
	"rables/internal/templates"
)

// Flash texts of TwitterArchiveImportSubmission.
const (
	twitterArchiveInvalidUploadAlert = "Please upload a valid Twitter archive ZIP file"
	twitterArchiveActiveImportAlert  = "A Twitter archive import is already in progress. Wait for it to finish before uploading another archive."
	twitterArchiveSuccessNotice      = "Twitter archive import queued. Check the history below for progress."
)

// RegisterTwitterArchiveAdminRoutes mounts the admin archive page, mirroring
// Rails' namespace :admin resources :twitter_archives (index/create only).
// The Rails direct_uploads endpoint (ActiveStorage pre-signed uploads) is
// intentionally not ported: the Go version stores uploads on local disk, so
// the form posts a plain multipart body straight to create.
func RegisterTwitterArchiveAdminRoutes(r chi.Router, s *Server) {
	r.Route("/admin/twitter_archives", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.adminTwitterArchivesIndex)
		r.Post("/", s.adminTwitterArchivesCreate)
	})
}

// twitterArchivePublicPath mirrors the twitter_archive_path helper: the
// public archive lives under the article route prefix when one is set.
func twitterArchivePublicPath(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return "/twitter/archive"
	}
	return "/" + prefix + "/twitter/archive"
}

// twitterArchiveImportRow adds the humanized status label
// (import.status.humanize) to an import history row.
type twitterArchiveImportRow struct {
	query.TwitterArchiveImport
	StatusLabel string
}

// adminTwitterArchivesData feeds admin_twitter_archives.html.
type adminTwitterArchivesData struct {
	Flash           templates.Flash
	TimeZone        string
	Counts          twitterarchive.Counts
	LastImportedAt  int64
	HasLastImported bool
	PublicPath      string
	Imports         []twitterArchiveImportRow
}

// adminTwitterArchivesIndex renders GET /admin/twitter_archives, mirroring
// Admin::TwitterArchivesController#index.
func (s *Server) adminTwitterArchivesIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	counts, err := twitterarchive.LoadCounts(ctx, s.Q)
	if err != nil {
		s.listError(w, "count twitter archive", err)
		return
	}
	lastImported, hasLastImported := twitterarchive.LastImportedAt(ctx, s.Q)
	imports, err := s.Q.ListTwitterArchiveImportsRecent(ctx, 10)
	if err != nil {
		s.listError(w, "list twitter archive imports", err)
		return
	}
	rows := make([]twitterArchiveImportRow, 0, len(imports))
	for _, imp := range imports {
		rows = append(rows, twitterArchiveImportRow{TwitterArchiveImport: imp, StatusLabel: titleize(imp.Status)})
	}
	st, err := s.Settings().Get(ctx)
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}
	s.render(w, http.StatusOK, "admin_twitter_archives", adminTwitterArchivesData{
		Flash:           PopFlash(r, w),
		TimeZone:        st.TimeZone,
		Counts:          counts,
		LastImportedAt:  lastImported,
		HasLastImported: hasLastImported,
		PublicPath:      twitterArchivePublicPath(s.Cfg.ArticleRoutePrefix),
		Imports:         rows,
	})
}

// adminTwitterArchivesCreate handles POST /admin/twitter_archives, mirroring
// TwitterArchiveImportSubmission#submit for the plain multipart source: the
// uploaded zip is streamed to data/imports/, an import row is queued and the
// twitter_archive_import job is enqueued.
func (s *Server) adminTwitterArchivesCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fail := func(alert string) {
		SetFlash(w, templates.Flash{Alert: alert})
		http.Redirect(w, r, "/admin/twitter_archives", http.StatusFound)
	}

	// Malformed requests (twitter_archive posted as a plain string) surface
	// as a missing file, like the Rails controller's nil fallback.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		fail(twitterArchiveInvalidUploadAlert)
		return
	}
	src, header, err := r.FormFile("twitter_archive[file]")
	if err != nil {
		fail(twitterArchiveInvalidUploadAlert)
		return
	}
	defer src.Close()

	filename := header.Filename
	if header.Header.Get("Content-Type") != "application/zip" && !strings.HasSuffix(strings.ToLower(filename), ".zip") {
		fail(twitterArchiveInvalidUploadAlert)
		return
	}

	active, err := s.Q.HasActiveTwitterArchiveImport(ctx)
	if err != nil {
		s.listError(w, "check active twitter archive import", err)
		return
	}
	if active > 0 {
		fail(twitterArchiveActiveImportAlert)
		return
	}

	sourcePath, err := s.storeTwitterArchiveUpload(src)
	if err != nil {
		s.Log.Error("store twitter archive upload", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	now := time.Now().Unix()
	imp, err := s.Q.CreateTwitterArchiveImport(ctx, query.CreateTwitterArchiveImportParams{
		SourceFilename: filename,
		SourcePath:     sql.NullString{String: sourcePath, Valid: true},
		QueuedAt:       now,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		os.Remove(sourcePath)
		if isUniqueViolation(err) {
			// Lost the active_slot race: another import queued first.
			fail(twitterArchiveActiveImportAlert)
			return
		}
		s.Log.Error("create twitter archive import", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logTwitterArchiveActivity(ctx, "info", "queued", fmt.Sprintf("filename=%s import_id=%d", activity.Quote(imp.SourceFilename), imp.ID))

	if _, err := s.Enqueuer().Enqueue(ctx, jobs.KindTwitterArchiveImport, map[string]any{"import_id": imp.ID}, time.Now()); err != nil {
		// The submission rescue path: mark the import failed, clean up, alert.
		s.Log.Error("enqueue twitter archive import", "error", err)
		failNow := time.Now().Unix()
		_ = s.Q.FailTwitterArchiveImport(ctx, query.FailTwitterArchiveImportParams{
			ErrorMessage: sql.NullString{String: err.Error(), Valid: true},
			FinishedAt:   sql.NullInt64{Int64: failNow, Valid: true},
			UpdatedAt:    failNow,
			ID:           imp.ID,
		})
		os.Remove(sourcePath)
		fail("Twitter archive import failed: " + err.Error())
		return
	}

	SetFlash(w, templates.Flash{Notice: twitterArchiveSuccessNotice})
	http.Redirect(w, r, "/admin/twitter_archives", http.StatusFound)
}

// storeTwitterArchiveUpload streams the multipart upload to
// data/imports/twitter_archive_<unix>_<rand>.zip (write_uploaded_zip of the
// Rails submission, relocated from tmp/ to the data dir).
func (s *Server) storeTwitterArchiveUpload(src io.Reader) (string, error) {
	dir := filepath.Join(s.Cfg.DataDir, "imports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("twitter_archive_%d_%s.zip", time.Now().Unix(), hex.EncodeToString(rnd[:])))
	dst, err := os.Create(path)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(path)
		return "", err
	}
	if err := dst.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// logTwitterArchiveActivity mirrors the ActivityLog.log! calls of
// TwitterArchiveImportSubmission via the shared activity helper. It never
// breaks the main flow.
func (s *Server) logTwitterArchiveActivity(ctx context.Context, level, action, description string) {
	activity.Log(ctx, s.DB, level, action, "twitter_archive", description)
}
