package httpd

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/service/media"
	"rables/internal/templates"
)

// maxUploadSize caps multipart upload bodies (100MB).
const maxUploadSize = 100 << 20

// Media returns the shared media service, creating it on first use.
func (s *Server) Media() *media.Service {
	// Log is set on the fresh instance before it is published: assigning it
	// on the already-stored shared value would race concurrent requests.
	m := media.New(s.DB, s.Cfg.DataDir)
	m.Log = s.Log
	v, _ := s.Ext.LoadOrStore("media", m)
	return v.(*media.Service)
}

// RegisterMediaRoutes mounts the upload endpoint (authenticated) and the
// public file serving route.
func RegisterMediaRoutes(r chi.Router, s *Server) {
	r.With(s.RequireAuth).Get("/admin/uploads/new", s.uploadForm)
	r.With(s.RequireAuth).Post("/admin/uploads", s.upload)
	r.Get("/files/{key}", s.serveFile)
}

// uploadPageData feeds admin_uploads.html.
type uploadPageData struct {
	Flash templates.Flash
}

// uploadForm renders GET /admin/uploads/new.
func (s *Server) uploadForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "admin_uploads", uploadPageData{Flash: s.PopFlash(r, w)})
}

// upload handles POST /admin/uploads: multipart field "file", plus optional
// record_type/record_id/name to link an attachment. Responds with JSON
// {"key": ..., "url": "/files/<key>"}.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
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
	// File parts larger than maxMemory spill to os.TempDir(); net/http does
	// not clean them up, so the handler must. Store below reads the part
	// synchronously, so the deferred removal runs after the copy finished.
	defer r.MultipartForm.RemoveAll()
	src, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer src.Close()

	// Validate the attachment fields before storing so a bad record_id does
	// not leave an orphaned file row and blob behind.
	recordType, recordID, name := r.FormValue("record_type"), r.FormValue("record_id"), r.FormValue("name")
	var rid int64
	if recordType != "" && recordID != "" && name != "" {
		rid, err = strconv.ParseInt(recordID, 10, 64)
		if err != nil {
			http.Error(w, "invalid record_id", http.StatusBadRequest)
			return
		}
	}

	file, err := s.Media().Store(r.Context(), src, header.Filename, header.Header.Get("Content-Type"))
	if err != nil {
		s.Log.Error("store upload", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if recordType != "" && recordID != "" && name != "" {
		if err := s.Media().Attach(r.Context(), file.ID, recordType, rid, name); err != nil {
			s.Log.Error("attach file", "error", err)
			// The client never learns the key, so the stored file and its
			// variant would be orphans no one can reach: reclaim them.
			s.Media().Purge(r.Context(), file)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"key": file.Key, "url": "/files/" + file.Key})
}

// serveFile handles GET /files/{key}: public, immutable, Content-Type from the
// files row. Unknown or unsafe keys are 404.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if !media.ValidKey(key) {
		http.NotFound(w, r)
		return
	}
	file, err := s.Media().FileByKey(r.Context(), key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.Media().PathFor(key))
	if err != nil {
		s.Log.Error("open file", "key", key, "error", err)
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ct := file.ContentType.String
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || !strings.Contains(mediaType, "/") || !validHeaderValue(ct) || isActiveContentType(ct) {
		// Active content (HTML/SVG/XML/JS) rendered inline would execute
		// script in the admin's same-origin session (stored XSS via the
		// import paths, which persist attacker-controlled content types). An
		// empty or malformed content type ("text/html x", "tex t/html") is
		// just as dangerous: it evades the active-type check, but browsers
		// drop the invalid Content-Type of a top-level navigation and sniff
		// the body as HTML (nosniff only constrains script/style), or
		// ServeContent would guess from the filename extension (.html →
		// text/html). Force a binary download like Rails ActiveStorage does.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment")
	} else {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, file.Filename, time.Unix(file.CreatedAt, 0), f)
}

// activeContentTypes are never served inline; see serveFile.
var activeContentTypes = map[string]bool{
	"text/html":              true,
	"image/svg+xml":          true,
	"application/xhtml+xml":  true,
	"text/xml":               true,
	"application/xml":        true,
	"text/javascript":        true,
	"application/javascript": true,
}

// isActiveContentType reports whether ct (parameters and case ignored) is an
// active content type that must be downloaded rather than rendered.
func isActiveContentType(ct string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	// Any +xml subtype (application/rss+xml, application/atom+xml, ...) is
	// active too: Firefox applies <?xml-stylesheet?> XSLT to arbitrary XML
	// types and renders the result as same-origin HTML.
	return activeContentTypes[base] || strings.HasSuffix(base, "+xml")
}

// validHeaderValue reports whether s contains only bytes legal in an HTTP
// header field value (tab, printable ASCII, obs-text). Raw control bytes are
// possible even when mime.ParseMediaType succeeds — it tolerates them inside
// quoted parameter values — and can make browsers drop the Content-Type
// header entirely, falling back to sniffing the body.
func validHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if b := s[i]; b != '\t' && (b < 0x20 || b == 0x7f) {
			return false
		}
	}
	return true
}
