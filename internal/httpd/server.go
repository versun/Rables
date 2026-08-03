package httpd

import (
	"database/sql"
	"log/slog"
	"sync"

	"rables/internal/config"
	"rables/internal/db/query"
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
