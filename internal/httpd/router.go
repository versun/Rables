// Package httpd assembles the chi router and HTTP middleware.
package httpd

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter builds the root HTTP handler: middleware chain plus routes.
func NewRouter(s *Server) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(accessLog(s.Log))
	r.Use(s.redirectMiddleware)
	r.Use(s.setupRedirect)
	r.Use(originCheck)

	r.Get("/up", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Setup (unauthenticated; self-guards when setup is already done).
	r.Get("/setup", s.setupForm)
	r.Post("/setup", s.setupCreate)

	// Authentication. HTML forms cannot send DELETE, so logout mirrors Rails
	// DELETE /session as POST /session/destroy.
	r.Get("/session/new", s.loginForm)
	r.Post("/session", s.login)
	r.With(s.RequireAuth).Post("/session/destroy", s.logout)

	// Account (change own password). HTML forms cannot send PATCH, so the
	// update mirrors Rails PATCH /users/:id as POST /users/{id}.
	r.With(s.RequireAuth).Get("/users/{id}/edit", s.userEditForm)
	r.With(s.RequireAuth).Post("/users/{id}", s.userUpdate)

	// later features mount here (wired by integrator):
	// each feature exposes RegisterXxxRoutes(r chi.Router, s *Server)
	// (see server.go) and gets called from this spot.
	RegisterSettingsRoutes(r, s)
	RegisterMediaRoutes(r, s)
	RegisterTagsRoutes(r, s)
	RegisterCommentRoutes(r, s)
	RegisterCommentAdminRoutes(r, s)
	RegisterArticlesAdminRoutes(r, s)
	RegisterPageAdminRoutes(r, s)
	RegisterRedirectsRoutes(r, s)
	RegisterStaticFilesRoutes(r, s)
	RegisterActivitiesRoutes(r, s)
	RegisterNewsletterRoutes(r, s)
	RegisterSubscriberAdminRoutes(r, s)
	RegisterTwitterArchiveAdminRoutes(r, s)
	RegisterCrosspostRoutes(r, s)
	RegisterMigratesAdminRoutes(r, s)
	RegisterMigratesImportRoutes(r, s)
	RegisterDownloadsRoutes(r, s)
	RegisterAssetsRoutes(r, s)
	RegisterTwitterSyncRoutes(r, s)
	RegisterJobsAdminRoutes(r, s)
	RegisterSourcesRoutes(r, s)
	RegisterPublicRoutes(r, s)
	RegisterSubscriptionRoutes(r, s)
	RegisterTwitterArchivePublicRoutes(r, s)
	// Article catch-all /{slug} must be registered last.
	RegisterArticleRoutes(r, s)

	return r
}

// statusRecorder captures the response status code for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// accessLog is a minimal request logger: method, path, status, duration.
func accessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sr, r)
			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sr.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", middleware.GetReqID(r.Context()),
			)
		})
	}
}
