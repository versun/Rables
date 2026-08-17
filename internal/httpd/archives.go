package httpd

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/domain"
	articlesvc "rables/internal/service/articles"
	"rables/internal/service/htmlarchive"
)

// archiveSandboxCSP isolates every response of the archive route in an opaque
// origin, even when the URL is opened directly (outside the iframe): scripts
// run, but they cannot read cookies, storage or the admin session, and their
// fetches carry no credentials. allow-same-origin must never be added.
const archiveSandboxCSP = "sandbox allow-scripts allow-popups"

// RegisterArchiveRoutes mounts the public serving route for extracted
// html_archive trees. The bare record URL redirects to its trailing-slash
// form so relative asset links inside the static site resolve against the
// record directory.
func RegisterArchiveRoutes(r chi.Router, s *Server) {
	r.Get("/archives/{kind}/{id}", s.redirectArchiveRoot)
	r.Get("/archives/{kind}/{id}/*", s.serveArchive)
}

// redirectArchiveRoot answers /archives/{kind}/{id} with a redirect to the
// trailing-slash URL (plain concatenation: both params are validated as
// alphanumeric/numeric by the serve handler after the redirect).
func (s *Server) redirectArchiveRoot(w http.ResponseWriter, r *http.Request) {
	if _, ok := htmlarchive.ParseKind(chi.URLParam(r, "kind")); !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
}

// serveArchive serves one file out of a record's extracted archive tree.
// Visibility mirrors the public post/page rules: publish/shared are public,
// every other status needs an authenticated admin (draft preview). Anything
// off these rules — unknown record, non-archive content type, unsafe path —
// is a plain 404: this is an asset endpoint, not a page.
func (s *Server) serveArchive(w http.ResponseWriter, r *http.Request) {
	kind, ok := htmlarchive.ParseKind(chi.URLParam(r, "kind"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	contentType, status, err := s.archiveRecord(r, kind, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.listError(w, "load archive record", err)
		return
	}
	if contentType != string(domain.ContentTypeHTMLArchive) {
		http.NotFound(w, r)
		return
	}
	public := status == int64(domain.StatusPublish) || status == int64(domain.StatusShared)
	if !public && !s.authenticated(r) {
		http.NotFound(w, r)
		return
	}

	rel, ok := htmlarchive.SafeRelPath(slugParam(r, "*"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	base := htmlarchive.Dir(s.Cfg.DataDir, kind, id)
	target := base
	if rel != "" {
		target = filepath.Join(base, filepath.FromSlash(rel))
		if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
			http.NotFound(w, r)
			return
		}
	}
	info, err := os.Stat(target)
	if err == nil && info.IsDir() {
		// Directory requests resolve to their index.html, like a static host.
		target = filepath.Join(target, "index.html")
		info, err = os.Stat(target)
	}
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(target)
	if err != nil {
		s.Log.Error("open archive file", "path", target, "error", err)
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", archiveContentType(info.Name()))
	w.Header().Set("Content-Security-Policy", archiveSandboxCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Re-uploads replace files at the same URLs, so caches must revalidate;
	// ServeContent answers the conditional request with a 304. Draft previews
	// are admin-only responses, so they are marked private.
	cacheControl := "public, no-cache"
	if !public {
		cacheControl = "private, no-cache"
	}
	w.Header().Set("Cache-Control", cacheControl)
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// archiveRecord loads the owning record's content_type and status.
func (s *Server) archiveRecord(r *http.Request, kind htmlarchive.Kind, id int64) (string, int64, error) {
	switch kind {
	case htmlarchive.Article:
		article, err := s.Q.GetArticleByID(r.Context(), id)
		if err != nil {
			return "", 0, err
		}
		return article.ContentType, article.Status, nil
	default:
		page, err := s.Q.GetAdminPageByID(r.Context(), id)
		if err != nil {
			return "", 0, err
		}
		return page.ContentType, page.Status, nil
	}
}

// archiveURL builds the iframe src for a record's archive (the tree root;
// the route serves its index.html).
func archiveURL(kind htmlarchive.Kind, id int64) string {
	return fmt.Sprintf("/archives/%s/%d/", kind, id)
}

// archiveContentTypes maps file extensions to served content types. Types not
// listed (or extensionless files) are application/octet-stream. Active types
// (HTML, SVG, JS) are safe to serve inline here because every response of the
// route carries the sandbox CSP; the map trusts extensions over the zip's own
// declarations, which are attacker-controlled.
var archiveContentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".xml":         "application/xml",
	".txt":         "text/plain; charset=utf-8",
	".md":          "text/plain; charset=utf-8",
	".csv":         "text/csv; charset=utf-8",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".svg":         "image/svg+xml",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".eot":         "application/vnd.ms-fontobject",
	".pdf":         "application/pdf",
	".mp4":         "video/mp4",
	".webm":        "video/webm",
	".mp3":         "audio/mpeg",
	".wav":         "audio/wav",
	".ogg":         "audio/ogg",
	".wasm":        "application/wasm",
}

// archiveContentType resolves the served content type from the file
// extension, case-insensitively.
func archiveContentType(name string) string {
	if ct, ok := archiveContentTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return ct
	}
	return "application/octet-stream"
}

// parseContentForm parses the article/page form, which may be multipart when
// it carries an archive_file part. Large uploads cannot fit the server-wide
// read/write deadlines, like the media upload.
func (s *Server) parseContentForm(w http.ResponseWriter, r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		s.clearRequestDeadlines(w)
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
		return r.ParseMultipartForm(32 << 20)
	}
	return r.ParseForm()
}

// discardContentForm releases the temp files of a parsed multipart form.
func discardContentForm(r *http.Request) {
	if r.MultipartForm != nil {
		r.MultipartForm.RemoveAll()
	}
}

// extractArchiveUpload validates and extracts the archive_file form part into
// a staging directory (see htmlarchive.Extract). provided is false when the
// part was not submitted. The caller removes the returned staging directory
// unless it commits it.
func (s *Server) extractArchiveUpload(r *http.Request) (staging string, provided bool, err error) {
	src, header, err := r.FormFile("archive_file")
	if errors.Is(err, http.ErrMissingFile) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("Archive upload failed: %v", err)
	}
	defer src.Close()
	staging, err = htmlarchive.Extract(s.Cfg.DataDir, src, header.Size)
	if err != nil {
		// The part was submitted, so the record counts as provided: callers
		// surface only this validation error, not an extra "can't be blank".
		return "", true, err
	}
	return staging, true, nil
}

// stageArticleArchive runs the archive half of an article form submission:
// when the submitted content type is html_archive it fills the SaveParams
// validation booleans (an update may keep the stored tree) and extracts an
// uploaded ZIP into staging. The returned path is the caller's to Commit or
// remove; it is "" for other content types and missing uploads.
func (s *Server) stageArticleArchive(r *http.Request, params *articlesvc.SaveParams, existing *query.Article) (string, error) {
	if params.ContentType != string(domain.ContentTypeHTMLArchive) {
		return "", nil
	}
	if existing != nil && existing.ContentType == string(domain.ContentTypeHTMLArchive) {
		params.HasExistingArchive = htmlarchive.Has(s.Cfg.DataDir, htmlarchive.Article, existing.ID)
	}
	if r.MultipartForm == nil {
		return "", nil
	}
	staging, provided, err := s.extractArchiveUpload(r)
	if err != nil {
		return "", err
	}
	params.ArchiveProvided = provided
	return staging, nil
}

// stagePageArchive is the page-side stageArticleArchive: the booleans feed
// parsePageForm's presence check (provided stays true when the submitted ZIP
// failed validation, so only the extraction error is shown, never a doubled
// "can't be blank").
func (s *Server) stagePageArchive(r *http.Request, existing *query.Page) (staging string, provided, hasExisting bool, err error) {
	if r.PostFormValue("content_type") != string(domain.ContentTypeHTMLArchive) {
		return "", false, false, nil
	}
	if existing != nil && existing.ContentType == string(domain.ContentTypeHTMLArchive) {
		hasExisting = htmlarchive.Has(s.Cfg.DataDir, htmlarchive.Page, existing.ID)
	}
	if r.MultipartForm == nil {
		return "", false, hasExisting, nil
	}
	staging, provided, err = s.extractArchiveUpload(r)
	return staging, provided, hasExisting, err
}

// commitFormArchive installs the staged archive for a saved record, or drops
// the tree left behind when the record switched away from html_archive
// (oldContentType carries the pre-save value, "" on create).
func (s *Server) commitFormArchive(staging string, kind htmlarchive.Kind, id int64, oldContentType, newContentType string) error {
	if newContentType == string(domain.ContentTypeHTMLArchive) {
		if staging == "" {
			return nil
		}
		return htmlarchive.Commit(staging, s.Cfg.DataDir, kind, id)
	}
	if oldContentType == string(domain.ContentTypeHTMLArchive) {
		if err := htmlarchive.Remove(s.Cfg.DataDir, kind, id); err != nil {
			s.Log.Error("remove archive tree", "kind", string(kind), "id", id, "error", err)
		}
	}
	return nil
}

// formParseError answers a failed content-form parse: 413 when the upload
// exceeded maxUploadSize, 400 otherwise (mirrors the media upload handler).
func (s *Server) formParseError(w http.ResponseWriter, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "bad request", http.StatusBadRequest)
}
