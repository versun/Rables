package httpd

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"sync"
	"time"

	"rables/internal/config"
	"rables/internal/db/query"
	"rables/internal/service/transfer"
	"rables/internal/templates"
)

// Server is the shared application context handed to every handler and
// middleware in this package.
type Server struct {
	DB       *sql.DB
	Q        *query.Queries
	Cfg      config.Config
	Log      *slog.Logger
	Renderer *templates.Renderer

	// Ext is the shared registry for optional cross-feature services
	// (e.g. a job scheduler or crosspost client built by a later feature
	// module). Features store values under their own string key with
	// LoadOrStore:
	//
	//	v, _ := s.Ext.LoadOrStore("jobs", buildScheduler(s))
	//	sched := v.(*jobs.Scheduler)
	//
	// so the first caller wins and later callers reuse the same instance.
	Ext sync.Map

	// RSSPreview fetches a feed and lists its importable entries for the RSS
	// import preview; nil fetches over the network with the default
	// RSSImporter. Tests stub it to avoid real fetches.
	RSSPreview func(ctx context.Context, feedURL string) ([]transfer.RSSPreviewItem, error)
}

// NewServer builds the application context. The renderer may be nil in tests
// that never render a page.
func NewServer(db *sql.DB, cfg config.Config, logger *slog.Logger, renderer *templates.Renderer) *Server {
	return &Server{
		DB:       db,
		Q:        query.New(db),
		Cfg:      cfg,
		Log:      logger,
		Renderer: renderer,
	}
}

// Route-registration convention: later features do NOT edit router.go.
// Each feature package exposes RegisterXxxRoutes(r chi.Router, s *Server)
// and the integrator wires those calls into NewRouter where marked.

// parseCappedForm parses a POST form body capped at 1 MB (these forms carry
// only text fields; without the cap a crafted multipart body spills file
// parts into os.TempDir() — maxMemory alone bounds only RAM, the rest goes
// to disk). It dispatches on Content-Type: ParseMultipartForm for multipart
// bodies, ParseForm otherwise. Calling ParseMultipartForm unconditionally
// would return ErrNotMultipart for a urlencoded body while discarding the
// *http.MaxBytesError its internal ParseForm produced, so an oversized
// urlencoded form would never reach the 413 branch. On failure it writes
// the response (413 oversized, 400 malformed) and returns false.
func parseCappedForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	// MIME types are case-insensitive (RFC 2045), so normalize before
	// dispatching; ParseMediaType lowercases the type and matches what
	// ParseMultipartForm's own check accepts.
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var err error
	if ct == "multipart/form-data" {
		err = r.ParseMultipartForm(10 << 20)
	} else {
		err = r.ParseForm()
	}
	if err == nil {
		return true
	}
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
	} else {
		http.Error(w, "bad request", http.StatusBadRequest)
	}
	return false
}

// clearRequestDeadlines lifts the server-wide 30s ReadTimeout and 60s
// WriteTimeout (cmd/server/main.go) for handlers that stream large request
// bodies (multi-GB import archives, media uploads): both are absolute
// per-request budgets, so a healthy multi-minute upload is cut off mid-body
// by the read deadline, and once it outlives the write deadline the final
// response (redirect after the file is stored and the job enqueued) fails
// with i/o timeout — the admin retries and the upload/import runs twice.
// The calling routes are admin-only and the body stays capped by
// http.MaxBytesReader, so clearing the deadlines does not widen the
// slow-client attack surface beyond authenticated sessions.
func (s *Server) clearRequestDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		s.Log.Warn("clear upload read deadline", "error", err)
	}
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		s.Log.Warn("clear upload write deadline", "error", err)
	}
}
