package httpd

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"rables/internal/service/activity"
	"rables/internal/templates"
)

// RegisterActivitiesRoutes mounts GET /admin/activities behind RequireAuth,
// mirroring Admin::ActivitiesController#index (latest 100 activity_logs).
func RegisterActivitiesRoutes(r chi.Router, s *Server) {
	r.Route("/admin/activities", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.adminActivitiesIndex)
	})
}

// adminActivityRow is one activity_logs row with its display values resolved.
type adminActivityRow struct {
	Time        string // created_at in settings.time_zone
	Level       string // info/warn/error token
	Action      string
	Target      string
	Description string
}

// adminActivitiesData feeds admin_activities.html.
type adminActivitiesData struct {
	Flash templates.Flash
	Logs  []adminActivityRow
}

// adminActivitiesIndex renders GET /admin/activities (newest first).
func (s *Server) adminActivitiesIndex(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Q.ListRecentActivityLogs(r.Context())
	if err != nil {
		s.Log.Error("list activity logs", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tz := s.siteTimeZone(r)
	logs := make([]adminActivityRow, 0, len(rows))
	for _, row := range rows {
		logs = append(logs, adminActivityRow{
			Time:        templates.FormatTime(row.CreatedAt, tz, "2006-01-02 15:04:05"),
			Level:       activity.LevelName(row.Level),
			Action:      row.Action.String,
			Target:      row.Target.String,
			Description: row.Description.String,
		})
	}
	s.render(w, http.StatusOK, "admin_activities", adminActivitiesData{
		Flash: PopFlash(r, w),
		Logs:  logs,
	})
}
