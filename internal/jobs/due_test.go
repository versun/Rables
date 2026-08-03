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
	tests := []struct {
		name       string
		schedule   string
		lastSynced *time.Time
		want       bool
	}{
		{"never synced", "hourly", nil, true},
		{"every_15_minutes, 16m ago", "every_15_minutes", ago(16 * time.Minute), true},
		{"every_15_minutes, 14m ago", "every_15_minutes", ago(14 * time.Minute), false},
		{"every_15_minutes, exactly 15m ago", "every_15_minutes", ago(15 * time.Minute), false},
		{"hourly, 61m ago", "hourly", ago(61 * time.Minute), true},
		{"hourly, 59m ago", "hourly", ago(59 * time.Minute), false},
		{"every_6_hours, 6h1m ago", "every_6_hours", ago(6*time.Hour + time.Minute), true},
		{"every_6_hours, 5h ago", "every_6_hours", ago(5 * time.Hour), false},
		{"daily, 25h ago", "daily", ago(25 * time.Hour), true},
		{"daily, 23h ago", "daily", ago(23 * time.Hour), false},
		{"weekly, 8d ago", "weekly", ago(8 * 24 * time.Hour), true},
		{"weekly, 6d ago", "weekly", ago(6 * 24 * time.Hour), false},
		{"unknown schedule falls back to 15m", "fortnightly", ago(16 * time.Minute), true},
		{"unknown schedule, 14m ago", "fortnightly", ago(14 * time.Minute), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TwitterSyncDue(tt.schedule, tt.lastSynced, dueNow); got != tt.want {
				t.Errorf("TwitterSyncDue(%q, %v, now) = %v, want %v", tt.schedule, tt.lastSynced, got, tt.want)
			}
		})
	}
}
