package jobs

import "time"

// TwitterSyncIntervals mirrors TwitterSync::SCHEDULES.
var TwitterSyncIntervals = map[string]time.Duration{
	"every_15_minutes": 15 * time.Minute,
	"hourly":           time.Hour,
	"every_6_hours":    6 * time.Hour,
	"daily":            24 * time.Hour,
	"weekly":           7 * 24 * time.Hour,
}

// CommentFetchDue ports ScheduledFetchSocialCommentsJob#should_fetch_now?:
// never fetched → due; otherwise due when lastFetch is before the schedule
// window. Unknown schedules are never due.
func CommentFetchDue(schedule string, lastFetch *time.Time, now time.Time) bool {
	if lastFetch == nil {
		return true
	}
	var cutoff time.Time
	switch schedule {
	case "daily":
		cutoff = now.Add(-24 * time.Hour)
	case "weekly":
		cutoff = now.Add(-7 * 24 * time.Hour)
	case "monthly":
		cutoff = now.AddDate(0, -1, 0)
	default:
		return false
	}
	return lastFetch.Before(cutoff)
}

// TwitterSyncDue ports TwitterSync#due_to_sync?: never synced → due;
// otherwise due when lastSynced is before now minus the schedule interval.
// Unknown schedules fall back to every_15_minutes, like the Rails fetch.
func TwitterSyncDue(schedule string, lastSynced *time.Time, now time.Time) bool {
	if lastSynced == nil {
		return true
	}
	interval, ok := TwitterSyncIntervals[schedule]
	if !ok {
		interval = TwitterSyncIntervals["every_15_minutes"]
	}
	return lastSynced.Before(now.Add(-interval))
}
