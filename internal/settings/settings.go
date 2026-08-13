// Package settings caches the singleton settings row (id = 1) in-process,
// replacing the Rails CacheableSettings / Setting.setup_incomplete? caching.
// Reads are served from memory for a TTL (5 minutes, spec §1); writes go
// through Update, which invalidates the cache so the next read reloads.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"rables/internal/db/query"
)

// ttl mirrors the 5-minute site-settings cache TTL of the Rails app.
const ttl = 5 * time.Minute

// Cache wraps the generated queries with a process-wide read cache for the
// settings singleton. The zero-value row is created on first access, matching
// Rails' Setting.first_or_create. Safe for concurrent use.
type Cache struct {
	q *query.Queries

	// Overridable in tests.
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	cached   query.Setting
	loadedAt time.Time
	valid    bool
}

// NewCache builds the cache around db. The first Get creates the settings
// row if it is missing. The logger is accepted for symmetry with other
// feature constructors; cache errors are returned to callers instead.
func NewCache(db *sql.DB, _ *slog.Logger) *Cache {
	return &Cache{q: query.New(db), ttl: ttl, now: time.Now}
}

// Get returns the settings row, creating it (first_or_create) when absent.
// Results are cached for the TTL; Update and Invalidate drop the cached row.
func (c *Cache) Get(ctx context.Context) (query.Setting, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.now().Sub(c.loadedAt) < c.ttl {
		return c.cached, nil
	}
	now := c.now().Unix()
	if err := c.q.EnsureSettings(ctx, query.EnsureSettingsParams{CreatedAt: now, UpdatedAt: now}); err != nil {
		return query.Setting{}, fmt.Errorf("settings: ensure row: %w", err)
	}
	row, err := c.q.GetSettings(ctx)
	if err != nil {
		return query.Setting{}, fmt.Errorf("settings: load row: %w", err)
	}
	c.cached, c.loadedAt, c.valid = row, c.now(), true
	return row, nil
}

// Update writes the editable columns and invalidates the cache, like the
// Rails after_commit that clears the settings caches. The row is created
// first when missing (the admin controller's first_or_create), so an UPDATE
// against an empty table never silently no-ops.
func (c *Cache) Update(ctx context.Context, p query.UpdateSettingsParams) error {
	now := c.now().Unix()
	if err := c.q.EnsureSettings(ctx, query.EnsureSettingsParams{CreatedAt: now, UpdatedAt: now}); err != nil {
		return fmt.Errorf("settings: ensure row: %w", err)
	}
	if err := c.q.UpdateSettings(ctx, p); err != nil {
		return fmt.Errorf("settings: update row: %w", err)
	}
	c.Invalidate()
	return nil
}

// Invalidate drops the cached row so the next Get reloads. Writers that
// bypass Update (setup completion, imports) must call this.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}

// RoutePrefix resolves the effective article route prefix for background
// services that run outside the httpd settings cache: the
// settings.article_route_prefix column when set, otherwise envFallback (the
// ARTICLE_ROUTE_PREFIX environment value, like config.Load).
func RoutePrefix(ctx context.Context, q *query.Queries, envFallback string) string {
	if row, err := q.GetSettings(ctx); err == nil {
		if p := strings.Trim(row.ArticleRoutePrefix.String, "/"); p != "" {
			return p
		}
	}
	return strings.Trim(envFallback, "/")
}

// SocialLink is one platform entry of the settings.social_links JSON object.
type SocialLink struct {
	URL  string `json:"url"`
	Icon string `json:"icon"`
}

// SocialLinks maps platform names to their link data.
type SocialLinks map[string]SocialLink

// UnmarshalSocialLinks decodes the stored social_links JSON. An empty string
// (NULL column) yields nil. Entries without the SocialLink shape are skipped,
// like the Rails view ignoring links it cannot render, and a row that fails
// to decode at all degrades to no links, so a malformed row stored before
// validation tightened never takes down every public page.
func UnmarshalSocialLinks(text string) (SocialLinks, error) {
	if text == "" {
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, nil
	}
	links := make(SocialLinks, len(raw))
	for platform, value := range raw {
		var link SocialLink
		if err := json.Unmarshal(value, &link); err != nil {
			continue
		}
		links[platform] = link
	}
	return links, nil
}

// MarshalSocialLinks encodes links for storage; nil yields "" (NULL column).
func MarshalSocialLinks(links SocialLinks) string {
	if links == nil {
		return ""
	}
	data, err := json.Marshal(links)
	if err != nil {
		return ""
	}
	return string(data)
}

// SocialLinks returns the decoded social links of the current settings.
func (c *Cache) SocialLinks(ctx context.Context) (SocialLinks, error) {
	row, err := c.Get(ctx)
	if err != nil {
		return nil, err
	}
	return UnmarshalSocialLinks(row.SocialLinks.String)
}

// NormalizeSocialLinks validates the JSON submitted from the admin form and
// returns it compact-encoded for storage. Like Rails' parse_social_links_json
// the top level must be a JSON object; each value must also have the
// SocialLink shape ({url, icon}), since the public layout decodes into
// SocialLinks and a malformed entry would fail every page.
func NormalizeSocialLinks(raw string) (string, error) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", fmt.Errorf("settings: invalid social links JSON: %w", err)
	}
	if parsed == nil { // "null" decodes without error into a nil map
		return "", fmt.Errorf("settings: social links must be a JSON object")
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return "", fmt.Errorf("settings: invalid social links JSON: %w", err)
	}
	for platform, value := range entries {
		// A null value decodes into a zero SocialLink without error, so it
		// must be rejected explicitly instead of relying on the struct decode.
		if string(value) == "null" {
			return "", fmt.Errorf("settings: social link %q must be an object", platform)
		}
		var link SocialLink
		if err := json.Unmarshal(value, &link); err != nil {
			return "", fmt.Errorf("settings: invalid social link %q: %w", platform, err)
		}
	}
	data, err := json.Marshal(parsed)
	if err != nil {
		return "", fmt.Errorf("settings: encode social links: %w", err)
	}
	return string(data), nil
}
