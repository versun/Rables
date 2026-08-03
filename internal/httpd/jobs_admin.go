package httpd

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/templates"
)

// adminJobsPerPage mirrors the plan §4.12 admin-list pagination.
const adminJobsPerPage = 100

// RegisterJobsAdminRoutes mounts GET /admin/jobs behind RequireAuth: a
// read-only job_runs list (plan section 5) — no Mission Control-style
// management actions.
func RegisterJobsAdminRoutes(r chi.Router, s *Server) {
	r.Route("/admin/jobs", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.adminJobsIndex)
	})
}

// adminJobRow is one job_runs row with its display values resolved.
type adminJobRow struct {
	ID        int64
	Kind      string
	Status    string
	RunAt     string // run_at in settings.time_zone
	Attempts  int64
	LastError string
	CreatedAt string // created_at in settings.time_zone
}

// adminJobsData feeds admin_jobs.html.
type adminJobsData struct {
	Flash  templates.Flash
	Jobs   []adminJobRow
	Status string
	Page   int
	Pages  int
}

// adminJobsIndex renders GET /admin/jobs: job_runs id DESC, 100 per page,
// optional ?status= filter (queued|running|done|failed; anything else lists
// everything). Invalid page params 404 like WillPaginate::InvalidPage.
func (s *Server) adminJobsIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	status := r.URL.Query().Get("status")
	filtered := isJobRunStatus(status)

	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			http.NotFound(w, r)
			return
		}
		page = n
	}
	offset := int64(page-1) * adminJobsPerPage

	var rows []query.JobRun
	var total int64
	var err error
	if filtered {
		total, err = s.Q.CountAdminJobRunsByStatus(ctx, status)
		if err == nil {
			rows, err = s.Q.ListAdminJobRunsByStatus(ctx, query.ListAdminJobRunsByStatusParams{
				Status: status, Limit: adminJobsPerPage, Offset: offset,
			})
		}
	} else {
		total, err = s.Q.CountAdminJobRuns(ctx)
		if err == nil {
			rows, err = s.Q.ListAdminJobRuns(ctx, query.ListAdminJobRunsParams{
				Limit: adminJobsPerPage, Offset: offset,
			})
		}
	}
	if err != nil {
		s.Log.Error("list job runs", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	tz := s.siteTimeZone(r)
	jobs := make([]adminJobRow, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, adminJobRow{
			ID:        row.ID,
			Kind:      row.Kind,
			Status:    row.Status,
			RunAt:     templates.FormatTime(row.RunAt, tz, "2006-01-02 15:04:05"),
			Attempts:  row.Attempts,
			LastError: row.LastError.String,
			CreatedAt: templates.FormatTime(row.CreatedAt, tz, "2006-01-02 15:04:05"),
		})
	}

	if status == "" {
		status = "all"
	}
	s.render(w, http.StatusOK, "admin_jobs", adminJobsData{
		Flash:  PopFlash(r, w),
		Jobs:   jobs,
		Status: status,
		Page:   page,
		Pages:  int((total + adminJobsPerPage - 1) / adminJobsPerPage),
	})
}

// isJobRunStatus reports whether name is one of the job_runs statuses.
func isJobRunStatus(name string) bool {
	switch name {
	case "queued", "running", "done", "failed":
		return true
	}
	return false
}
