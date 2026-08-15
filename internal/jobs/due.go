package jobs

import "time"

// TwitterSyncIntervalSchedules mirrors TwitterSync::SCHEDULES, minus "daily",
// which is wall-clock based (see TwitterSyncDailyHour) instead of interval
// based.
var TwitterSyncIntervalSchedules = map[string]time.Duration{
	"every_15_minutes": 15 * time.Minute,
	"hourly":           time.Hour,
	"every_6_hours":    6 * time.Hour,
	"weekly":           7 * 24 * time.Hour,
}

// TwitterSyncDailyHour is the local hour the "daily" sync fires: 8:00 AM in
// the site time zone (settings.time_zone). 08:00 lands exactly on a 15-minute
// scheduler tick in every real time zone (all UTC offsets are whole quarters
// of an hour), so the */15 cron wake-up fires it right on time.
const TwitterSyncDailyHour = 8

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

// TwitterSyncDue ports TwitterSync#due_to_sync?, with "daily" changed to a
// fixed wall-clock slot: it is due when the latest 08:00 (loc) slot has
// passed and lastSynced is before it, so a downtime that covers 08:00 is
// caught up at the next tick instead of skipped. Interval schedules are due
// when lastSynced is before now minus the interval. Never synced → due.
// Unknown schedules fall back to every_15_minutes, like the Rails fetch.
func TwitterSyncDue(schedule string, lastSynced *time.Time, now time.Time, loc *time.Location) bool {
	if lastSynced == nil {
		return true
	}
	if schedule == "daily" {
		return lastSynced.Before(latestDailySlot(now, loc))
	}
	interval, ok := TwitterSyncIntervalSchedules[schedule]
	if !ok {
		interval = TwitterSyncIntervalSchedules["every_15_minutes"]
	}
	return lastSynced.Before(now.Add(-interval))
}

// NextTwitterSyncRun returns the next 15-minute scheduler tick at which
// TwitterSyncDue is true, for display in the admin Sync Status section.
func NextTwitterSyncRun(schedule string, lastSynced *time.Time, now time.Time, loc *time.Location) time.Time {
	if TwitterSyncDue(schedule, lastSynced, now, loc) {
		return tickAfter(now)
	}
	if schedule == "daily" {
		return latestDailySlot(now, loc).AddDate(0, 0, 1)
	}
	// Interval schedule, not yet due: the first tick strictly after
	// lastSynced+interval (the due check is strict Before).
	interval, ok := TwitterSyncIntervalSchedules[schedule]
	if !ok {
		interval = TwitterSyncIntervalSchedules["every_15_minutes"]
	}
	return tickAfter(lastSynced.Add(interval))
}

// latestDailySlot returns the most recent 08:00 (loc) slot at or before now.
func latestDailySlot(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	slot := time.Date(n.Year(), n.Month(), n.Day(), TwitterSyncDailyHour, 0, 0, 0, loc)
	if slot.After(now) {
		slot = slot.AddDate(0, 0, -1)
	}
	return slot
}

// tickAfter returns the next 15-minute cron tick strictly after t.
func tickAfter(t time.Time) time.Time {
	return t.Truncate(15 * time.Minute).Add(15 * time.Minute)
}
