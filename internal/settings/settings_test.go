package settings

import (
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"rables/internal/db"
	"rables/internal/db/query"
)

// newTestCache opens a real SQLite DB in a temp dir and returns a cache with
// a controllable clock.
func newTestCache(t *testing.T) (*Cache, *sql.DB, *time.Time) {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	fakeNow := time.Unix(1_700_000_000, 0)
	c := NewCache(database, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	c.now = func() time.Time { return fakeNow }
	return c, database, &fakeNow
}

func updateParams(title string) query.UpdateSettingsParams {
	return query.UpdateSettingsParams{
		Title:     sql.NullString{String: title, Valid: title != ""},
		TimeZone:  "UTC",
		UpdatedAt: 1_700_000_000,
	}
}

// TestGetCreatesRowOnFirstAccess mirrors Rails' Setting.first_or_create.
func TestGetCreatesRowOnFirstAccess(t *testing.T) {
	c, database, _ := newTestCache(t)
	ctx := t.Context()

	row, err := c.Get(ctx)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if row.ID != 1 {
		t.Errorf("id = %d, want 1", row.ID)
	}
	if row.SetupCompleted != 0 {
		t.Errorf("setup_completed = %d, want 0", row.SetupCompleted)
	}
	if row.TimeZone != "UTC" {
		t.Errorf("time_zone = %q, want UTC (column default)", row.TimeZone)
	}
	if row.CreatedAt == 0 || row.UpdatedAt == 0 {
		t.Errorf("timestamps not set: created_at=%d updated_at=%d", row.CreatedAt, row.UpdatedAt)
	}

	// The row really exists in the database and a second Get reads it.
	var count int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM settings").Scan(&count); err != nil {
		t.Fatalf("count settings: %v", err)
	}
	if count != 1 {
		t.Errorf("settings rows = %d, want 1", count)
	}
	if _, err := c.Get(ctx); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if count := queryCount(t, database); count != 1 {
		t.Errorf("settings rows after second Get = %d, want 1 (idempotent)", count)
	}
}

func queryCount(t *testing.T, database *sql.DB) int {
	t.Helper()
	var count int
	if err := database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM settings").Scan(&count); err != nil {
		t.Fatalf("count settings: %v", err)
	}
	return count
}

// TestGetCachesWithinTTL: a read inside the TTL is served from memory, so a
// direct (bypassing) DB write is invisible until the TTL elapses.
func TestGetCachesWithinTTL(t *testing.T) {
	c, database, fakeNow := newTestCache(t)
	ctx := t.Context()

	if _, err := c.Get(ctx); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := database.ExecContext(ctx, "UPDATE settings SET title = 'direct' WHERE id = 1"); err != nil {
		t.Fatalf("direct update: %v", err)
	}

	row, err := c.Get(ctx)
	if err != nil {
		t.Fatalf("cached Get: %v", err)
	}
	if row.Title.String == "direct" {
		t.Error("Get inside TTL saw a bypassing write; cache not used")
	}

	*fakeNow = fakeNow.Add(ttl + time.Second)
	row, err = c.Get(ctx)
	if err != nil {
		t.Fatalf("Get after TTL: %v", err)
	}
	if row.Title.String != "direct" {
		t.Errorf("title after TTL = %q, want %q (cache should have expired)", row.Title.String, "direct")
	}
}

// TestUpdateInvalidatesCache: a write through the cache is visible on the
// very next read, without waiting for the TTL.
func TestUpdateInvalidatesCache(t *testing.T) {
	c, _, _ := newTestCache(t)
	ctx := t.Context()

	if _, err := c.Get(ctx); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := c.Update(ctx, updateParams("New Title")); err != nil {
		t.Fatalf("Update: %v", err)
	}
	row, err := c.Get(ctx)
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if row.Title.String != "New Title" {
		t.Errorf("title = %q, want %q right after Update", row.Title.String, "New Title")
	}
}

// TestInvalidate: an external writer (setup, imports) can force a reload.
func TestInvalidate(t *testing.T) {
	c, database, _ := newTestCache(t)
	ctx := t.Context()

	if _, err := c.Get(ctx); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := database.ExecContext(ctx, "UPDATE settings SET title = 'external' WHERE id = 1"); err != nil {
		t.Fatalf("direct update: %v", err)
	}
	c.Invalidate()
	row, err := c.Get(ctx)
	if err != nil {
		t.Fatalf("Get after Invalidate: %v", err)
	}
	if row.Title.String != "external" {
		t.Errorf("title = %q, want %q after Invalidate", row.Title.String, "external")
	}
}

func TestSocialLinksRoundTrip(t *testing.T) {
	t.Run("typed marshal/unmarshal", func(t *testing.T) {
		tests := []struct {
			name  string
			links SocialLinks
			want  string
		}{
			{name: "nil is empty", links: nil, want: ""},
			{name: "empty map", links: SocialLinks{}, want: "{}"},
			{
				name: "platforms",
				links: SocialLinks{
					"github": {URL: "https://github.com/versun", Icon: "fa-brands fa-github"},
					"rss":    {URL: "/feed.rss", Icon: "fa-solid fa-square-rss"},
				},
				want: `{"github":{"url":"https://github.com/versun","icon":"fa-brands fa-github"},"rss":{"url":"/feed.rss","icon":"fa-solid fa-square-rss"}}`,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got := MarshalSocialLinks(tt.links)
				if got != tt.want {
					t.Errorf("MarshalSocialLinks = %q, want %q", got, tt.want)
				}
				back, err := UnmarshalSocialLinks(got)
				if err != nil {
					t.Fatalf("UnmarshalSocialLinks(%q): %v", got, err)
				}
				if len(back) != len(tt.links) {
					t.Fatalf("round trip lost entries: got %v, want %v", back, tt.links)
				}
				for k, v := range tt.links {
					if back[k] != v {
						t.Errorf("round trip entry %q = %+v, want %+v", k, back[k], v)
					}
				}
			})
		}
	})

	t.Run("unmarshal empty yields nil", func(t *testing.T) {
		links, err := UnmarshalSocialLinks("")
		if err != nil || links != nil {
			t.Errorf("UnmarshalSocialLinks(\"\") = %v, %v; want nil, nil", links, err)
		}
	})

	t.Run("unmarshal invalid JSON errors", func(t *testing.T) {
		if _, err := UnmarshalSocialLinks("{nope"); err == nil {
			t.Error("expected error for invalid JSON")
		}
	})

	t.Run("through the cache", func(t *testing.T) {
		c, _, _ := newTestCache(t)
		ctx := t.Context()
		p := updateParams("t")
		p.SocialLinks = sql.NullString{String: `{"github":{"url":"https://github.com/versun","icon":"fa-brands fa-github"}}`, Valid: true}
		if err := c.Update(ctx, p); err != nil {
			t.Fatalf("Update: %v", err)
		}
		links, err := c.SocialLinks(ctx)
		if err != nil {
			t.Fatalf("SocialLinks: %v", err)
		}
		if links["github"].URL != "https://github.com/versun" || links["github"].Icon != "fa-brands fa-github" {
			t.Errorf("SocialLinks = %v", links)
		}
	})
}

// TestNormalizeSocialLinks covers the admin form validation: any JSON object
// is accepted (Rails Hash check) and stored compact; anything else errors.
func TestNormalizeSocialLinks(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "object compacted", raw: "{\n  \"github\": {\"url\": \"https://github.com/versun\"}\n}", want: `{"github":{"url":"https://github.com/versun"}}`},
		{name: "empty object", raw: "{}", want: "{}"},
		{name: "unknown keys preserved", raw: `{"x":{"url":"u","icon":"i","color":"red"}}`, want: `{"x":{"color":"red","icon":"i","url":"u"}}`},
		{name: "malformed JSON", raw: "{nope", wantErr: true},
		{name: "array rejected", raw: `[1,2]`, wantErr: true},
		{name: "string rejected", raw: `"x"`, wantErr: true},
		{name: "null rejected", raw: "null", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeSocialLinks(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Errorf("NormalizeSocialLinks(%q): expected error, got %q", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeSocialLinks(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeSocialLinks(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
