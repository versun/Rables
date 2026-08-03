package httpd

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/service/media"
	"rables/internal/templates"
)

// maxUploadSize caps multipart upload bodies (100MB).
const maxUploadSize = 100 << 20

// Media returns the shared media service, creating it on first use.
func (s *Server) Media() *media.Service {
	v, _ := s.Ext.LoadOrStore("media", media.New(s.DB, s.Cfg.DataDir))
	m := v.(*media.Service)
	m.Log = s.Log
	return m
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
	s.render(w, http.StatusOK, "admin_uploads", uploadPageData{Flash: PopFlash(r, w)})
}

// upload handles POST /admin/uploads: multipart field "file", plus optional
// record_type/record_id/name to link an attachment. Responds with JSON
// {"key": ..., "url": "/files/<key>"}.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	src, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer src.Close()

	key, err := s.Media().Store(r.Context(), src, header.Filename, header.Header.Get("Content-Type"))
	if err != nil {
		s.Log.Error("store upload", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	recordType, recordID, name := r.FormValue("record_type"), r.FormValue("record_id"), r.FormValue("name")
	if recordType != "" && recordID != "" && name != "" {
		rid, err := strconv.ParseInt(recordID, 10, 64)
		if err != nil {
			http.Error(w, "invalid record_id", http.StatusBadRequest)
			return
		}
		file, err := s.Media().FileByKey(r.Context(), key)
		if err != nil {
			s.Log.Error("lookup stored file", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := s.Media().Attach(r.Context(), file.ID, recordType, rid, name); err != nil {
			s.Log.Error("attach file", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"key": key, "url": "/files/" + key})
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
	if ct := file.ContentType.String; ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, file.Filename, time.Unix(file.CreatedAt, 0), f)
}
