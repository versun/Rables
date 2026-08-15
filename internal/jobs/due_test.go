package jobs

import (
	"testing"
	"time"
)

var dueNow = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) *time.Time {
	t := dueNow.Add(-d)
	return &t
}

func at(t time.Time) *time.Time { return &t }

func TestCommentFetchDue(t *testing.T) {
	tests := []struct {
		name      string
		schedule  string
		lastFetch *time.Time
		want      bool
	}{
		{"never fetched", "daily", nil, true},
		{"daily, 25h ago", "daily", ago(25 * time.Hour), true},
		{"daily, 23h ago", "daily", ago(23 * time.Hour), false},
		{"daily, exactly 24h ago", "daily", ago(24 * time.Hour), false},
		{"daily, 1s past", "daily", ago(24*time.Hour + time.Second), true},
		{"weekly, 8d ago", "weekly", ago(8 * 24 * time.Hour), true},
		{"weekly, 6d ago", "weekly", ago(6 * 24 * time.Hour), false},
		{"weekly, exactly 7d ago", "weekly", ago(7 * 24 * time.Hour), false},
		{"monthly, 32d ago", "monthly", ago(32 * 24 * time.Hour), true},
		{"monthly, 2d ago", "monthly", ago(2 * 24 * time.Hour), false},
		{"monthly, exactly one calendar month ago", "monthly", at(dueNow.AddDate(0, -1, 0)), false},
		{"monthly, 1s past one calendar month", "monthly", at(dueNow.AddDate(0, -1, 0).Add(-time.Second)), true},
		{"unknown schedule", "hourly", ago(100 * 24 * time.Hour), false},
		{"empty schedule", "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CommentFetchDue(tt.schedule, tt.lastFetch, dueNow); got != tt.want {
				t.Errorf("CommentFetchDue(%q, %v, now) = %v, want %v", tt.schedule, tt.lastFetch, got, tt.want)
			}
		})
	}
}

func TestTwitterSyncDue(t *testing.T) {
	shanghai := time.FixedZone("UTC+8", 8*3600)
	kolkata := time.FixedZone("UTC+5:30", 5*3600+1800)
	tests := []struct {
		name       string
		schedule   string
		lastSynced *time.Time
		now        time.Time
		loc        *time.Location
		want       bool
	}{
		{"never synced", "hourly", nil, dueNow, time.UTC, true},
		{"every_15_minutes, 16m ago", "every_15_minutes", ago(16 * time.Minute), dueNow, time.UTC, true},
		{"every_15_minutes, 14m ago", "every_15_minutes", ago(14 * time.Minute), dueNow, time.UTC, false},
		{"every_15_minutes, exactly 15m ago", "every_15_minutes", ago(15 * time.Minute), dueNow, time.UTC, false},
		{"hourly, 61m ago", "hourly", ago(61 * time.Minute), dueNow, time.UTC, true},
		{"hourly, 59m ago", "hourly", ago(59 * time.Minute), dueNow, time.UTC, false},
		{"every_6_hours, 6h1m ago", "every_6_hours", ago(6*time.Hour + time.Minute), dueNow, time.UTC, true},
		{"every_6_hours, 5h ago", "every_6_hours", ago(5 * time.Hour), dueNow, time.UTC, false},
		{"weekly, 8d ago", "weekly", ago(8 * 24 * time.Hour), dueNow, time.UTC, true},
		{"weekly, 6d ago", "weekly", ago(6 * 24 * time.Hour), dueNow, time.UTC, false},
		{"unknown schedule falls back to 15m", "fortnightly", ago(16 * time.Minute), dueNow, time.UTC, true},
		{"unknown schedule, 14m ago", "fortnightly", ago(14 * time.Minute), dueNow, time.UTC, false},
		// daily: fixed 08:00 slot (dueNow is 2026-08-03 12:00 UTC).
		{"daily, synced after today's slot", "daily", at(time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)), dueNow, time.UTC, false},
		{"daily, synced before today's slot", "daily", at(time.Date(2026, 8, 3, 7, 0, 0, 0, time.UTC)), dueNow, time.UTC, true},
		{"daily, synced yesterday evening", "daily", at(time.Date(2026, 8, 2, 20, 0, 0, 0, time.UTC)), dueNow, time.UTC, true},
		{"daily, before 8am, synced after yesterday's slot", "daily", at(time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)), time.Date(2026, 8, 3, 6, 0, 0, 0, time.UTC), time.UTC, false},
		{"daily, before 8am, synced before yesterday's slot", "daily", at(time.Date(2026, 8, 2, 7, 0, 0, 0, time.UTC)), time.Date(2026, 8, 3, 6, 0, 0, 0, time.UTC), time.UTC, true},
		// daily in UTC+8: the slot is 08:00 local = 00:00 UTC.
		{"daily UTC+8, before slot", "daily", at(time.Date(2026, 8, 2, 23, 30, 0, 0, time.UTC)), time.Date(2026, 8, 3, 1, 0, 0, 0, time.UTC), shanghai, true},
		{"daily UTC+8, after slot", "daily", at(time.Date(2026, 8, 3, 0, 30, 0, 0, time.UTC)), time.Date(2026, 8, 3, 1, 0, 0, 0, time.UTC), shanghai, false},
		// daily in UTC+5:30: the slot is 08:00 local = 02:30 UTC.
		{"daily UTC+5:30, before slot", "daily", at(time.Date(2026, 8, 3, 2, 0, 0, 0, time.UTC)), time.Date(2026, 8, 3, 3, 0, 0, 0, time.UTC), kolkata, true},
		{"daily UTC+5:30, after slot", "daily", at(time.Date(2026, 8, 3, 2, 45, 0, 0, time.UTC)), time.Date(2026, 8, 3, 3, 0, 0, 0, time.UTC), kolkata, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TwitterSyncDue(tt.schedule, tt.lastSynced, tt.now, tt.loc); got != tt.want {
				t.Errorf("TwitterSyncDue(%q, %v, %v, %v) = %v, want %v", tt.schedule, tt.lastSynced, tt.now, tt.loc, got, tt.want)
			}
		})
	}
}

func TestNextTwitterSyncRun(t *testing.T) {
	tests := []struct {
		name       string
		schedule   string
		lastSynced *time.Time
		want       time.Time
	}{
		// dueNow is 2026-08-03 12:00 UTC; due runs fire at the next tick, 12:15.
		{"never synced", "hourly", nil, time.Date(2026, 8, 3, 12, 15, 0, 0, time.UTC)},
		{"daily, due", "daily", at(time.Date(2026, 8, 3, 7, 0, 0, 0, time.UTC)), time.Date(2026, 8, 3, 12, 15, 0, 0, time.UTC)},
		{"daily, not due → tomorrow 08:00", "daily", at(time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)), time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC)},
		{"hourly, due at an exact tick → following tick", "hourly", at(time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC)), time.Date(2026, 8, 3, 12, 15, 0, 0, time.UTC)},
		{"hourly, due at 12:30 → 12:45 tick", "hourly", at(time.Date(2026, 8, 3, 11, 30, 0, 0, time.UTC)), time.Date(2026, 8, 3, 12, 45, 0, 0, time.UTC)},
		{"every_15_minutes, due at 12:05 → 12:15 tick", "every_15_minutes", at(time.Date(2026, 8, 3, 11, 50, 0, 0, time.UTC)), time.Date(2026, 8, 3, 12, 15, 0, 0, time.UTC)},
		{"weekly, not due", "weekly", at(time.Date(2026, 8, 2, 13, 7, 0, 0, time.UTC)), time.Date(2026, 8, 9, 13, 15, 0, 0, time.UTC)},
		{"unknown schedule falls back to 15m", "fortnightly", at(time.Date(2026, 8, 3, 11, 50, 0, 0, time.UTC)), time.Date(2026, 8, 3, 12, 15, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NextTwitterSyncRun(tt.schedule, tt.lastSynced, dueNow, time.UTC); !got.Equal(tt.want) {
				t.Errorf("NextTwitterSyncRun(%q, %v, now) = %v, want %v", tt.schedule, tt.lastSynced, got, tt.want)
			}
		})
	}
}
