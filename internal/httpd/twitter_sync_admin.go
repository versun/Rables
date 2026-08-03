package httpd

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/service/activity"
	"rables/internal/service/twittersync"
	"rables/internal/templates"
)

// twitterSyncExtKey is the Server.Ext key holding the shared *twittersync.Syncer
// (the same instance the scheduler hook runs, so the mutex is shared).
const twitterSyncExtKey = "twittersync"

// TwitterSyncer returns the shared syncer, creating it on first use. main.go
// stores the scheduler-wired instance under the same key.
func (s *Server) TwitterSyncer() *twittersync.Syncer {
	v, _ := s.Ext.LoadOrStore(twitterSyncExtKey, twittersync.NewSyncer(s.DB, s.Cfg.DataDir))
	return v.(*twittersync.Syncer)
}

// RegisterTwitterSyncRoutes mounts the Twitter sync admin page (Rails:
// resource :twitter_sync, only: [:show, :update] + member post :sync_now;
// PATCH becomes POST because HTML forms cannot send it).
func RegisterTwitterSyncRoutes(r chi.Router, s *Server) {
	r.Route("/admin/twitter_sync", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Get("/", s.twitterSyncShow)
		r.Post("/", s.twitterSyncUpdate)
		r.Post("/sync_now", s.twitterSyncNow)
	})
}

// twitterSyncSchedules mirrors TwitterSync::SCHEDULES keys in view order.
var twitterSyncSchedules = []struct {
	Label string
	Value string
}{
	{"Every 15 minutes", "every_15_minutes"},
	{"Hourly", "hourly"},
	{"Every 6 hours", "every_6_hours"},
	{"Daily", "daily"},
	{"Weekly", "weekly"},
}

// twitterSyncPageData feeds admin_twitter_sync.html.
type twitterSyncPageData struct {
	Flash     templates.Flash
	Sync      query.TwitterSync
	Schedules []struct {
		Label string
		Value string
	}
	LastSyncedLong string // formatted like Rails l(..., format: :long); "" = Never
	TimeZone       string
}

// loadTwitterSync mirrors TwitterSync.instance (first_or_create).
func (s *Server) loadTwitterSync(ctx context.Context) (query.TwitterSync, error) {
	now := time.Now().Unix()
	if err := s.Q.EnsureTwitterSync(ctx, query.EnsureTwitterSyncParams{CreatedAt: now, UpdatedAt: now}); err != nil {
		return query.TwitterSync{}, err
	}
	return s.Q.GetTwitterSync(ctx)
}

// twitterSyncShow renders GET /admin/twitter_sync (Admin::TwitterSyncController#show).
func (s *Server) twitterSyncShow(w http.ResponseWriter, r *http.Request) {
	syncRow, err := s.loadTwitterSync(r.Context())
	if err != nil {
		s.Log.Error("load twitter sync", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tz := s.siteTimeZone(r)
	lastSynced := ""
	if syncRow.LastSyncedAt.Valid {
		lastSynced = time.Unix(syncRow.LastSyncedAt.Int64, 0).In(tzLocation(tz)).Format("January 2, 2006 15:04")
	}
	s.render(w, http.StatusOK, "admin_twitter_sync", twitterSyncPageData{
		Flash:          PopFlash(r, w),
		Sync:           syncRow,
		Schedules:      twitterSyncSchedules,
		LastSyncedLong: lastSynced,
		TimeZone:       tz,
	})
}

// twitterSyncUpdate handles POST /admin/twitter_sync, mirroring #update: the
// permitted twitter_sync params overlay the row; changing username or
// start_date resets the cursor (since_id/last_synced_at/last_error), and a
// username change also clears the resolved user_id.
func (s *Server) twitterSyncUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	syncRow, err := s.loadTwitterSync(ctx)
	if err != nil {
		s.Log.Error("load twitter sync", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	enabled := int64(0)
	if v, _ := formCheckbox(r, "twitter_sync[enabled]"); v {
		enabled = 1
	}
	username := normalizeTwitterSyncUsername(r.PostFormValue("twitter_sync[username]"))
	syncSchedule := r.PostFormValue("twitter_sync[sync_schedule]")
	startDate := sql.NullString{}
	if v := strings.TrimSpace(r.PostFormValue("twitter_sync[start_date]")); v != "" {
		// ActiveModel date cast: an unparseable value becomes nil.
		if _, err := time.Parse(time.DateOnly, v); err == nil {
			startDate = sql.NullString{String: v, Valid: true}
		}
	}

	var errs []string
	if enabled == 1 && username == "" {
		errs = append(errs, "Username can't be blank")
	}
	if !validTwitterSyncSchedule(syncSchedule) {
		errs = append(errs, "Sync schedule is not included in the list")
	}
	if len(errs) > 0 {
		msg := strings.Join(errs, ", ")
		activity.Log(ctx, s.DB, "error", "failed", "twitter_sync", "error="+activity.Quote(msg))
		SetFlash(w, templates.Flash{Alert: msg})
		http.Redirect(w, r, "/admin/twitter_sync", http.StatusFound)
		return
	}

	newUsername := sql.NullString{String: username, Valid: username != ""}
	usernameChanged := newUsername.String != syncRow.Username.String
	cursorReset := usernameChanged || startDate.String != syncRow.StartDate.String
	if usernameChanged {
		syncRow.UserID = sql.NullString{}
	}
	if cursorReset {
		syncRow.SinceID = sql.NullString{}
		syncRow.LastSyncedAt = sql.NullInt64{}
		syncRow.LastError = sql.NullString{}
	}
	syncRow.Enabled = enabled
	syncRow.Username = newUsername
	syncRow.StartDate = startDate
	syncRow.SyncSchedule = syncSchedule

	if err := s.Q.UpdateTwitterSyncConfig(ctx, query.UpdateTwitterSyncConfigParams{
		Enabled:      syncRow.Enabled,
		Username:     syncRow.Username,
		UserID:       syncRow.UserID,
		StartDate:    syncRow.StartDate,
		SyncSchedule: syncRow.SyncSchedule,
		SinceID:      syncRow.SinceID,
		LastSyncedAt: syncRow.LastSyncedAt,
		LastError:    syncRow.LastError,
		UpdatedAt:    time.Now().Unix(),
	}); err != nil {
		s.Log.Error("update twitter sync", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	activity.Log(ctx, s.DB, "info", "updated", "twitter_sync", "")
	SetFlash(w, templates.Flash{Notice: "Twitter sync settings updated successfully."})
	http.Redirect(w, r, "/admin/twitter_sync", http.StatusFound)
}

// twitterSyncNow handles POST /admin/twitter_sync/sync_now, mirroring
// #sync_now: the guard message matches the Rails alert; a valid configuration
// runs the sync asynchronously (SyncTwitterJob.perform_later(force: true)).
func (s *Server) twitterSyncNow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	syncRow, err := s.loadTwitterSync(ctx)
	if err != nil {
		s.Log.Error("load twitter sync", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	cfg, err := s.Q.GetCrosspostByPlatform(ctx, "twitter")
	crosspostEnabled := err == nil && cfg.Enabled == 1
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.Log.Error("load twitter crosspost", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if syncRow.Enabled != 1 || syncRow.Username.String == "" || !crosspostEnabled {
		SetFlash(w, templates.Flash{Alert: "Twitter sync is not enabled or the X/Twitter credentials are missing. Enable it in the settings before syncing."})
		http.Redirect(w, r, "/admin/twitter_sync", http.StatusFound)
		return
	}
	syncer := s.TwitterSyncer()
	go func() { _ = syncer.Run(context.Background()) }()
	SetFlash(w, templates.Flash{Notice: "Twitter sync has been queued."})
	http.Redirect(w, r, "/admin/twitter_sync", http.StatusFound)
}

// normalizeTwitterSyncUsername mirrors the TwitterSync username normalizer:
// trimmed, a leading @ stripped, blank becomes nil.
func normalizeTwitterSyncUsername(raw string) string {
	return strings.TrimPrefix(strings.TrimSpace(raw), "@")
}

// validTwitterSyncSchedule mirrors the sync_schedule inclusion validation.
func validTwitterSyncSchedule(value string) bool {
	for _, sch := range twitterSyncSchedules {
		if sch.Value == value {
			return true
		}
	}
	return false
}
