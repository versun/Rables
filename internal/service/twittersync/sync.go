// Package twittersync ports TwitterSyncService: archiving original tweets
// (and quote tweets) from the configured X account as published Articles.
// Replies and pure retweets are excluded. API credentials come from the
// crossposts "twitter" row. The scheduler hook decides when a run is due
// (T13); Run itself always performs a sync when enabled, so the admin
// "Sync Now" button enqueues a job that calls the same entry point (force
// semantics).
package twittersync

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dghubble/oauth1"

	"rables/internal/db/query"
	"rables/internal/service/activity"
	"rables/internal/service/contentmigrate"
	"rables/internal/service/media"
	"rables/internal/service/tags"
	"rables/internal/ssrf"
)

const (
	// FirstRunLimit mirrors FIRST_RUN_LIMIT: on the first run (no cursor, no
	// start date) only the latest tweets are archived.
	FirstRunLimit = 10
	// MaxPagesPerSync mirrors MAX_PAGES_PER_SYNC.
	MaxPagesPerSync = 10
	// QuotedContentLimit mirrors QUOTED_CONTENT_LIMIT (runes).
	QuotedContentLimit = 250
	// redirectLimit mirrors the follow_redirect limit of 5 hops.
	redirectLimit = 5
	// tcoResolveBudget caps how many distinct short links one tweet resolves
	// via HEAD redirects; links beyond the budget keep their t.co text, so a
	// link-stuffed (quoted) tweet cannot stall the whole sync.
	tcoResolveBudget = 10
	// redirectTimeout caps one redirect HEAD request — far tighter than the
	// default 30s client timeout, since an unresponsive shortener must not
	// stall the sync either.
	redirectTimeout = 5 * time.Second
	// maxMediaBytes caps one media download (100MB). A larger response is
	// refused outright: truncating it silently would store a corrupt file
	// that the slug dedup never retries.
	maxMediaBytes = 100 << 20

	// articleTagName is the tag attached to every article archived from a
	// tweet, so synced posts stay recognizable as twitter content.
	articleTagName = "twitter"

	defaultBaseURL = "https://api.twitter.com/2"
)

// errSyncConfigChanged marks a run aborted because the admin updated the sync
// config mid-run and the run's stale write was discarded by the CAS guard.
// It is logged, not recorded as a sync failure: the next run reads the new
// config.
var errSyncConfigChanged = errors.New("twitter sync: config changed during run")

// Syncer archives tweets. The zero-injection fields exist for tests.
type Syncer struct {
	db      *sql.DB
	q       *query.Queries
	dataDir string

	mu sync.Mutex // SyncTwitterJob limits_concurrency 1 equivalent

	baseURL    string       // "" → defaultBaseURL
	httpClient *http.Client // nil → a default 30s-timeout client
	now        func() time.Time
	log        *slog.Logger
	// lookupIP resolves redirect target hosts for the SSRF guard (tests stub
	// it); nil uses the system resolver.
	lookupIP func(ctx context.Context, host string) ([]netip.Addr, error)
}

// NewSyncer builds a Syncer rooted at dataDir (media is stored under
// dataDir/files via the media service).
func NewSyncer(db *sql.DB, dataDir string) *Syncer {
	return &Syncer{db: db, q: query.New(db), dataDir: dataDir}
}

// SetBaseURL overrides the X API base URL (tests point it at httptest).
func (s *Syncer) SetBaseURL(u string) { s.baseURL = strings.TrimSuffix(u, "/") }

// SetHTTPClient overrides the client used for API calls, media downloads,
// and t.co redirect resolution (tests inject the httptest client).
func (s *Syncer) SetHTTPClient(c *http.Client) { s.httpClient = c }

// SetClock overrides the clock (tests).
func (s *Syncer) SetClock(now func() time.Time) { s.now = now }

// SetLookupIP overrides host resolution for the redirect SSRF guard (tests
// stub it; the HTTP client still dials the original host).
func (s *Syncer) SetLookupIP(f func(ctx context.Context, host string) ([]netip.Addr, error)) {
	s.lookupIP = f
}

func (s *Syncer) base() string {
	if s.baseURL != "" {
		return s.baseURL
	}
	return defaultBaseURL
}

func (s *Syncer) client() *http.Client {
	if s.httpClient != nil {
		return s.httpClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *Syncer) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Syncer) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return slog.Default()
}

// Run ports TwitterSyncService#perform. A concurrent run is skipped (the
// in-process equivalent of limits_concurrency to: 1). Failures are recorded
// on the twitter_syncs row (last_error) and in the activity log, never
// returned — the Rails job swallows them the same way.
func (s *Syncer) Run(ctx context.Context) error {
	if !s.mu.TryLock() {
		return nil
	}
	defer s.mu.Unlock()
	if err := s.perform(ctx); err != nil {
		s.recordFailure(ctx, err.Error())
	}
	return nil
}

func (s *Syncer) perform(ctx context.Context) error {
	syncRow, err := s.q.GetTwitterSync(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	cfg, err := s.q.GetCrosspostByPlatform(ctx, "twitter")
	if errors.Is(err, sql.ErrNoRows) {
		cfg = query.Crosspost{}
	} else if err != nil {
		return err
	}
	username := syncRow.Username.String
	if syncRow.Enabled != 1 || username == "" || cfg.Enabled != 1 {
		return nil
	}

	client := s.signingClient(cfg)
	userID := syncRow.UserID.String
	if userID == "" {
		userID, err = s.resolveUserID(ctx, client, username)
		if errors.Is(err, errSyncConfigChanged) {
			s.logger().Info("twitter sync: run aborted, config changed mid-run")
			return nil
		}
		if err != nil {
			return err
		}
		if userID == "" {
			s.recordFailure(ctx, "user not found: "+username)
			return nil
		}
	}

	tweets, includes, err := s.fetchNewTweets(ctx, client, syncRow, userID)
	if err != nil {
		return err
	}
	tweets = dedupeSortTweets(tweets)
	if !syncRow.SinceID.Valid && !syncRow.StartDate.Valid && len(tweets) > FirstRunLimit {
		tweets = tweets[len(tweets)-FirstRunLimit:]
	}

	// Archive tweet by tweet: a poison tweet is logged and skipped instead of
	// aborting the run; since_id below still advances past it.
	for _, tweet := range tweets {
		if err := s.archiveTweet(ctx, syncRow, tweet, includes); err != nil {
			s.logger().Warn("twitter sync: tweet archive failed", "tweet_id", tweet.ID, "error", err)
			activity.Log(ctx, s.db, "error", "failed", "twitter_sync",
				"error="+activity.Quote(fmt.Sprintf("tweet %s: %s", tweet.ID, err)))
		}
	}

	latest := syncRow.SinceID
	if max := maxTweetID(tweets); max != "" {
		latest = sql.NullString{String: max, Valid: true}
	}
	now := s.clock().Unix()
	n, err := s.q.SetTwitterSyncSuccess(ctx, query.SetTwitterSyncSuccessParams{
		SinceID:           latest,
		LastSyncedAt:      sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:         now,
		ExpectedSinceID:   syncRow.SinceID,
		ExpectedUsername:  syncRow.Username,
		ExpectedStartDate: syncRow.StartDate,
	})
	if err != nil {
		return err
	}
	if n == 0 {
		// The admin changed the config mid-run and reset the cursor; writing
		// this run's cursor back would undo the reset and skip the backfill
		// it was meant to trigger. Drop the write instead: the archived
		// tweets stay (slug dedup prevents re-archiving) and the next run
		// syncs from the new config.
		s.logger().Info("twitter sync: cursor write discarded, config changed mid-run")
	}
	return nil
}

// recordFailure mirrors record_failure: last_error via update_columns plus an
// error activity row.
func (s *Syncer) recordFailure(ctx context.Context, message string) {
	s.logger().Error("twitter sync failed", "error", message)
	if err := s.q.SetTwitterSyncFailure(ctx, query.SetTwitterSyncFailureParams{
		LastError: sql.NullString{String: message, Valid: true},
		UpdatedAt: s.clock().Unix(),
	}); err != nil {
		s.logger().Error("twitter sync: write last_error failed", "error", err)
	}
	activity.Log(ctx, s.db, "error", "failed", "twitter_sync", "error="+activity.Quote(message))
}

// signingClient builds an OAuth1.0a HMAC-SHA1 client for the four credential
// parts (X::Client.new with api_key/api_key_secret/access_token/
// access_token_secret), self-contained in this package like crosspost's.
func (s *Syncer) signingClient(cfg query.Crosspost) *http.Client {
	config := oauth1.NewConfig(cfg.ApiKey.String, cfg.ApiKeySecret.String)
	token := oauth1.NewToken(cfg.AccessToken.String, cfg.AccessTokenSecret.String)
	base := s.client()
	ctx := context.WithValue(oauth1.NoContext, oauth1.HTTPClient, base)
	client := config.Client(ctx, token)
	client.Timeout = base.Timeout
	return client
}

// --- X API payloads ---

type apiUser struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

type apiMedia struct {
	MediaKey string `json:"media_key"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Variants []struct {
		ContentType string `json:"content_type"`
		Bitrate     int    `json:"bitrate"`
		URL         string `json:"url"`
	} `json:"variants"`
}

type apiURLEntity struct {
	URL         string `json:"url"`
	ExpandedURL string `json:"expanded_url"`
}

type apiEntities struct {
	URLs []apiURLEntity `json:"urls"`
}

type apiTweet struct {
	ID        string      `json:"id"`
	Text      string      `json:"text"`
	Created   string      `json:"created_at"`
	AuthorID  string      `json:"author_id"`
	Entities  apiEntities `json:"entities"`
	NoteTweet *struct {
		Text     string      `json:"text"`
		Entities apiEntities `json:"entities"`
	} `json:"note_tweet"`
	Attachments *struct {
		MediaKeys []string `json:"media_keys"`
	} `json:"attachments"`
	ReferencedTweets []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"referenced_tweets"`
}

// includes holds the expansion lookups (media/quoted tweets/authors) merged
// across timeline pages, mirroring merge_includes!.
type includes struct {
	media  map[string]apiMedia
	tweets map[string]apiTweet
	users  map[string]apiUser
}

type timelineResponse struct {
	Data     *[]apiTweet `json:"data"`
	Includes *struct {
		Media  []apiMedia `json:"media"`
		Tweets []apiTweet `json:"tweets"`
		Users  []apiUser  `json:"users"`
	} `json:"includes"`
	Meta *struct {
		NextToken   string `json:"next_token"`
		ResultCount *int   `json:"result_count"`
	} `json:"meta"`
	Errors []struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
		Title   string `json:"title"`
	} `json:"errors"`
	Title string `json:"title"`
}

// resolveUserID ports resolve_user_id: users/by/username lookup persisted on
// the row; "" means the account was not found.
func (s *Syncer) resolveUserID(ctx context.Context, client *http.Client, username string) (string, error) {
	var resp struct {
		Data *struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := s.getJSON(ctx, client, s.base()+"/users/by/username/"+url.PathEscape(username), &resp); err != nil {
		return "", err
	}
	if resp.Data == nil || resp.Data.ID == "" {
		return "", nil
	}
	n, err := s.q.SetTwitterSyncUserID(ctx, query.SetTwitterSyncUserIDParams{
		UserID:           sql.NullString{String: resp.Data.ID, Valid: true},
		UpdatedAt:        s.clock().Unix(),
		ExpectedUsername: sql.NullString{String: username, Valid: true},
	})
	if err != nil {
		return "", err
	}
	if n == 0 {
		// The admin changed the username while the lookup was in flight; the
		// resolved id belongs to the old account. Leave the admin's clear in
		// place and abort the run instead of syncing the old timeline under
		// the new username.
		return "", errSyncConfigChanged
	}
	return resp.Data.ID, nil
}

// fetchNewTweets ports fetch_new_tweets: follows next_token pagination (cap
// MaxPagesPerSync) for both the initial backfill and incremental syncs.
func (s *Syncer) fetchNewTweets(ctx context.Context, client *http.Client, syncRow query.TwitterSync, userID string) ([]apiTweet, includes, error) {
	var tweets []apiTweet
	inc := includes{media: map[string]apiMedia{}, tweets: map[string]apiTweet{}, users: map[string]apiUser{}}
	paginationToken := ""
	for pages := 0; ; {
		resp, err := s.fetchTimeline(ctx, client, syncRow, userID, paginationToken)
		if err != nil {
			return nil, inc, err
		}
		if err := timelineAPIError(resp); err != nil {
			return nil, inc, err
		}
		if resp.Data != nil {
			tweets = append(tweets, *resp.Data...)
		}
		mergeIncludes(&inc, resp)

		paginationToken = ""
		if resp.Meta != nil {
			paginationToken = resp.Meta.NextToken
		}
		pages++
		if paginationToken == "" || pages >= MaxPagesPerSync {
			break
		}
	}
	return tweets, inc, nil
}

// fetchTimeline ports fetch_timeline (query parameters verbatim).
func (s *Syncer) fetchTimeline(ctx context.Context, client *http.Client, syncRow query.TwitterSync, userID, paginationToken string) (*timelineResponse, error) {
	u := s.base() + "/users/" + url.PathEscape(userID) + "/tweets" +
		"?exclude=retweets,replies" +
		"&max_results=100" +
		"&tweet.fields=created_at,attachments,referenced_tweets,note_tweet,entities,author_id" +
		"&expansions=attachments.media_keys,referenced_tweets.id,referenced_tweets.id.author_id,referenced_tweets.id.attachments.media_keys" +
		"&media.fields=url,preview_image_url,type,variants,alt_text" +
		"&user.fields=name,username"
	if syncRow.SinceID.Valid {
		u += "&since_id=" + url.QueryEscape(syncRow.SinceID.String)
	}
	if start := syncRow.StartDate.String; syncRow.StartDate.Valid && start != "" {
		// start_date.in_time_zone.beginning_of_day.iso8601; the Rails app runs
		// with the default UTC zone.
		if t, err := time.Parse(time.DateOnly, start); err == nil {
			u += "&start_time=" + url.QueryEscape(t.UTC().Format(time.RFC3339))
		}
	}
	if paginationToken != "" {
		u += "&pagination_token=" + url.QueryEscape(paginationToken)
	}
	var resp timelineResponse
	if err := s.getJSON(ctx, client, u, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// timelineAPIError ports api_error_response?/api_error_message: a page must
// carry data (possibly empty) or a meta result_count; anything else is an
// error payload and fails the sync.
func timelineAPIError(resp *timelineResponse) error {
	if resp.Data != nil {
		return nil
	}
	if resp.Meta != nil && resp.Meta.ResultCount != nil {
		return nil
	}
	return errors.New(apiErrorMessage(resp))
}

// apiErrorMessage ports api_error_message.
func apiErrorMessage(resp *timelineResponse) string {
	var messages []string
	for _, e := range resp.Errors {
		if m := firstNonEmpty(e.Message, e.Detail, e.Title); m != "" {
			messages = append(messages, m)
		}
	}
	if len(messages) > 0 {
		return strings.Join(messages, ", ")
	}
	if resp.Title != "" {
		return resp.Title
	}
	return "Twitter API returned an unexpected response"
}

func mergeIncludes(inc *includes, resp *timelineResponse) {
	if resp.Includes == nil {
		return
	}
	for _, m := range resp.Includes.Media {
		inc.media[m.MediaKey] = m
	}
	for _, t := range resp.Includes.Tweets {
		inc.tweets[t.ID] = t
	}
	for _, u := range resp.Includes.Users {
		inc.users[u.ID] = u
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// getJSON performs one GET and decodes the JSON body.
func (s *Syncer) getJSON(ctx context.Context, client *http.Client, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := ""
		var errResp timelineResponse
		if json.Unmarshal(body, &errResp) == nil {
			msg = apiErrorMessage(&errResp)
		}
		if msg == "" || msg == "Twitter API returned an unexpected response" {
			msg = fmt.Sprintf("twitter: HTTP %d", resp.StatusCode)
		}
		return errors.New(msg)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("twitter: decode response: %w", err)
	}
	return nil
}

// dedupeSortTweets mirrors tweets.uniq { id }.sort_by { id.to_i }.
func dedupeSortTweets(tweets []apiTweet) []apiTweet {
	seen := make(map[string]bool, len(tweets))
	out := tweets[:0]
	for _, t := range tweets {
		if seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return tweetIDInt(out[i].ID) < tweetIDInt(out[j].ID)
	})
	return out
}

// tweetIDInt mirrors String#to_i: garbage parses as 0.
func tweetIDInt(id string) int64 {
	n, _ := strconv.ParseInt(id, 10, 64)
	return n
}

func maxTweetID(tweets []apiTweet) string {
	best := ""
	for _, t := range tweets {
		if best == "" || tweetIDInt(t.ID) > tweetIDInt(best) {
			best = t.ID
		}
	}
	return best
}

// --- archiving ---

var (
	// redundantLink? patterns.
	ownMediaRe = func(tweetID string) *regexp.Regexp {
		return regexp.MustCompile(`(?i)/status/` + regexp.QuoteMeta(tweetID) + `/(photo|video)/\d+`)
	}
	quotedLinkRe = func(quotedID string) *regexp.Regexp {
		return regexp.MustCompile(`(?i)/(x\.com|twitter\.com)/[^/]+/status/` + regexp.QuoteMeta(quotedID) + `/?$`)
	}
	articleAnnouncementRe = regexp.MustCompile(`(?i)^https?://(www\.)?(x\.com|twitter\.com)/i/article/`)
	tcoLinkRe             = regexp.MustCompile(`(?i)^https?://t\.co/`)
	tcoShortRe            = regexp.MustCompile(`https://t\.co/\w+`)
	trailingSpaceRe       = regexp.MustCompile(`(?m)[ \t]+$`)
)

// archiveTweet ports archive_tweet: defensive retweet/reply filter, X-Article
// announcement skip, start-date filter, slug dedupe, then the Article +
// social_media_posts rows and the activity entry. Every archived article also
// carries the twitter tag (find-or-created on first use).
func (s *Syncer) archiveTweet(ctx context.Context, syncRow query.TwitterSync, tweet apiTweet, inc includes) error {
	// Defensive filter: exclude retweets/replies even if the API returned them.
	quotedID := ""
	for _, ref := range tweet.ReferencedTweets {
		if ref.Type == "retweeted" || ref.Type == "replied_to" {
			return nil
		}
		if ref.Type == "quoted" {
			quotedID = ref.ID
		}
	}
	if articleAnnouncement(tweet) {
		return nil
	}
	before, err := beforeStartDate(syncRow.StartDate.String, syncRow.StartDate.Valid, tweet.Created)
	if err != nil {
		return err
	}
	if before {
		return nil
	}

	slug := "tweet-" + tweet.ID
	if n, err := s.q.CountAdminArticlesBySlug(ctx, query.CountAdminArticlesBySlugParams{Slug: sql.NullString{String: slug, Valid: true}, ID: 0}); err != nil {
		return err
	} else if n > 0 {
		return nil
	}

	fullText := tweet.Text
	if tweet.NoteTweet != nil && tweet.NoteTweet.Text != "" {
		fullText = tweet.NoteTweet.Text
	}
	fullText = s.resolveTcoLinks(ctx, fullText, tweet, quotedID)

	sourceURL := ""
	if quotedID != "" {
		sourceURL = "https://x.com/i/web/status/" + quotedID
	}
	sourceAuthor, sourceContent := s.quotedSourceReference(ctx, quotedID, inc)

	createdAt := s.clock()
	if tweet.Created != "" {
		t, err := time.Parse(time.RFC3339, tweet.Created)
		if err != nil {
			return fmt.Errorf("parse created_at: %w", err)
		}
		createdAt = t
	}

	// Download media (own attachments first, then the quoted tweet's — the
	// Rails blob order). Own media embeds in the tweet content; the quoted
	// tweet's media embeds in the source-reference block, not the body.
	ownMedia := s.downloadTweetMedia(ctx, tweet, inc.media)
	var quotedMedia []storedMedia
	if quoted, ok := inc.tweets[quotedID]; quotedID != "" && ok {
		quotedMedia = s.downloadTweetMedia(ctx, quoted, inc.media)
	}
	if len(quotedMedia) > 0 {
		sourceContent = buildQuotedSourceContent(sourceContent, quotedMedia)
	}
	stored := append(ownMedia, quotedMedia...)

	now := s.clock().Unix()

	// Tweet content is stored as Markdown: buildTweetContent's HTML goes
	// through the same ToMarkdown write path as migrated rich_text content
	// (media embeds stay raw HTML inside the Markdown source).
	contentMarkdown, contentHTML, err := contentmigrate.ToMarkdown(buildTweetContent(fullText, ownMedia))
	if err != nil {
		s.discardStoredMedia(ctx, stored)
		return fmt.Errorf("convert tweet content to markdown: %w", err)
	}

	// The Article, its tag join row, its media attachments and the
	// social_media_posts row land in one transaction: a mid-way failure must
	// not leave a committed article without its social post row (since_id
	// advances past the tweet either way, so a partial write would never be
	// repaired). The media stored above lives outside the transaction, so a
	// failure reclaims it.
	err = func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		qtx := s.q.WithTx(tx)
		article, err := qtx.CreateArticle(ctx, query.CreateArticleParams{
			Slug:                        sql.NullString{String: slug, Valid: true},
			ContentHtml:                 sql.NullString{String: contentHTML, Valid: true},
			ContentType:                 "markdown",
			ContentMarkdown:             sql.NullString{String: contentMarkdown, Valid: contentMarkdown != ""},
			SourceAuthor:                sql.NullString{String: sourceAuthor, Valid: sourceAuthor != ""},
			SourceUrl:                   sql.NullString{String: sourceURL, Valid: sourceURL != ""},
			SourceContent:               sql.NullString{String: sourceContent, Valid: sourceContent != ""},
			Status:                      1, // publish
			Comment:                     1,
			ScheduledCrosspostPlatforms: "[]",
			CreatedAt:                   createdAt.Unix(),
			UpdatedAt:                   now,
		})
		if err != nil {
			return err
		}
		tagIDs, err := tags.FindOrCreateByNames(ctx, qtx, []string{articleTagName})
		if err != nil {
			return err
		}
		for _, tagID := range tagIDs {
			if err := qtx.InsertArticleTag(ctx, query.InsertArticleTagParams{
				ArticleID: article.ID, TagID: tagID, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				return err
			}
		}
		for _, m := range stored {
			if err := qtx.CreateAttachment(ctx, query.CreateAttachmentParams{
				FileID:     m.fileID,
				RecordType: "Article",
				RecordID:   article.ID,
				Name:       "embeds",
				CreatedAt:  now,
			}); err != nil {
				// Roll back: committing here would leave the stored media
				// without an attachment reference, orphaned forever.
				return err
			}
		}
		if err := qtx.UpsertSocialMediaPost(ctx, query.UpsertSocialMediaPostParams{
			ArticleID: article.ID,
			Platform:  "twitter",
			Url:       "https://x.com/" + syncRow.Username.String + "/status/" + tweet.ID,
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		s.discardStoredMedia(ctx, stored)
		return err
	}
	activity.Log(ctx, s.db, "info", "posted", "twitter_sync",
		"slug="+activity.Quote(slug)+" url="+activity.Quote(sourceURL))
	return nil
}

// buildQuotedSourceContent builds the source_content of a quote tweet with
// media attachments: the truncated quote text as escaped paragraphs followed
// by the media embeds — the quote's media belongs to the source-reference
// block, not the tweet body. Unlike plain-text source_content this is an
// HTML fragment; render paths emit it through the content sanitizer.
func buildQuotedSourceContent(text string, media []storedMedia) string {
	var parts []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts = append(parts, "<p>"+escapeHTML(line)+"</p>")
	}
	for _, m := range media {
		parts = append(parts, mediaAttachmentHTML(m))
	}
	return strings.Join(parts, "")
}

// articleAnnouncement ports article_announcement?: X Articles surface only as
// a t.co wrapper, so tweets linking to x.com/i/article are skipped.
func articleAnnouncement(tweet apiTweet) bool {
	for _, u := range tweet.Entities.URLs {
		if articleAnnouncementRe.MatchString(u.ExpandedURL) {
			return true
		}
	}
	return false
}

// beforeStartDate ports before_start_date? (start_date beginning of day, UTC).
func beforeStartDate(startDate string, hasStartDate bool, created string) (bool, error) {
	if !hasStartDate || startDate == "" || created == "" {
		return false, nil
	}
	createdAt, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return false, fmt.Errorf("parse created_at: %w", err)
	}
	start, err := time.Parse(time.DateOnly, startDate)
	if err != nil {
		return false, nil // an uncastable date is NULL in Rails
	}
	return createdAt.Before(start.UTC()), nil
}

// quotedSourceReference ports quoted_source_reference: author + truncated
// quote text from the expanded includes; missing data is non-fatal.
func (s *Syncer) quotedSourceReference(ctx context.Context, quotedID string, inc includes) (string, string) {
	if quotedID == "" {
		return "", ""
	}
	quoted, ok := inc.tweets[quotedID]
	if !ok {
		return "", ""
	}
	author := ""
	if u, ok := inc.users[quoted.AuthorID]; ok {
		author = u.Name
	}
	content := quoted.Text
	if quoted.NoteTweet != nil && quoted.NoteTweet.Text != "" {
		content = quoted.NoteTweet.Text
	}
	content = s.resolveTcoLinks(ctx, content, quoted, "")
	if r := []rune(content); len(r) > QuotedContentLimit {
		content = string(r[:QuotedContentLimit])
	}
	return author, content
}

// resolveTcoLinks ports resolve_tco_links: url entities first, then a HEAD
// redirect follow; links redundant with the embedded media or the quoted
// tweet are removed instead. Redirect follows are memoized per text and
// capped at tcoResolveBudget per call; links past the budget keep their
// t.co text.
func (s *Syncer) resolveTcoLinks(ctx context.Context, text string, tweet apiTweet, quotedID string) string {
	var entities []apiURLEntity
	entities = append(entities, tweet.Entities.URLs...)
	if tweet.NoteTweet != nil {
		entities = append(entities, tweet.NoteTweet.Entities.URLs...)
	}
	replacements := map[string]string{}
	removable := map[string]bool{}
	for _, e := range entities {
		if e.URL == "" {
			continue
		}
		if e.ExpandedURL == "" {
			continue
		}
		switch {
		case redundantLink(e.ExpandedURL, tweet.ID, quotedID):
			removable[e.URL] = true
		case !tcoLinkRe.MatchString(e.ExpandedURL):
			replacements[e.URL] = e.ExpandedURL
		}
	}

	memo := map[string]string{}
	budget := tcoResolveBudget
	out := tcoShortRe.ReplaceAllStringFunc(text, func(short string) string {
		if removable[short] {
			return ""
		}
		resolved, ok := replacements[short]
		if !ok {
			if resolved, ok = memo[short]; !ok {
				if budget == 0 {
					return short
				}
				budget--
				resolved = s.followRedirect(ctx, short, redirectLimit)
				memo[short] = resolved
			}
		}
		if resolved != "" && redundantLink(resolved, tweet.ID, quotedID) {
			return ""
		}
		if resolved == "" {
			return short
		}
		return resolved
	})
	return trailingSpaceRe.ReplaceAllString(out, "")
}

// redundantLink ports redundant_link?: a link is redundant when it points at
// the tweet's own media attachments or at the quoted tweet.
func redundantLink(rawURL, tweetID, quotedID string) bool {
	if tweetID != "" && ownMediaRe(tweetID).MatchString(rawURL) {
		return true
	}
	if quotedID != "" && quotedLinkRe(quotedID).MatchString(rawURL) {
		return true
	}
	return false
}

// followRedirect ports follow_redirect: HEAD requests, up to limit hops,
// http/https only; "" on failure so the caller keeps the original text.
// A target refused by the SSRF guard also yields "": the blocked URL must
// not leak into the article content. The final target reached when the hop
// budget runs out gets no HEAD, but the same screening still applies.
func (s *Syncer) followRedirect(ctx context.Context, rawURL string, limit int) string {
	if limit <= 0 {
		if !ssrf.SafeRemoteURL(ctx, rawURL, s.lookupIP) {
			s.logger().Warn("twitter sync: redirect target blocked", "url", rawURL)
			return ""
		}
		return rawURL
	}
	location, blocked := s.redirectLocation(ctx, rawURL)
	if blocked {
		return ""
	}
	if location == "" {
		return rawURL
	}
	next, err := url.Parse(location)
	if err != nil {
		return ""
	}
	base, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	resolved := base.ResolveReference(next)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return rawURL
	}
	return s.followRedirect(ctx, resolved.String(), limit-1)
}

// redirectLocation ports redirect_location: a HEAD request returning the
// Location header of a 3xx response, "" otherwise. The second return value
// reports that the SSRF guard refused the target before any request was
// made. Tweet text is attacker-influenced via quoted tweets, so targets are
// screened with ssrf.SafeRemoteURL before the request goes out; note the
// check and the dial resolve DNS independently, leaving a small rebinding
// (TOCTOU) window between the two — the trade-off the project-wide SSRF
// guard already accepts everywhere it is used.
func (s *Syncer) redirectLocation(ctx context.Context, rawURL string) (string, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	if !ssrf.SafeRemoteURL(ctx, rawURL, s.lookupIP) {
		s.logger().Warn("twitter sync: redirect target blocked", "url", rawURL)
		return "", true
	}
	client := *s.client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// Redirect resolution runs tighter than API calls and media downloads: a
	// hung shortener must not stall the sync.
	if client.Timeout == 0 || client.Timeout > redirectTimeout {
		client.Timeout = redirectTimeout
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		s.logger().Warn("twitter sync: link resolution failed", "url", rawURL, "error", err)
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 3 {
		return resp.Header.Get("Location"), false
	}
	return "", false
}

// storedMedia is one downloaded attachment ready for content embedding.
type storedMedia struct {
	key         string
	fileID      int64
	filename    string
	contentType string
}

// downloadTweetMedia ports build_media_attachments: every media key with a
// usable download URL is fetched and stored; failures are logged and skipped.
func (s *Syncer) downloadTweetMedia(ctx context.Context, tweet apiTweet, mediaByKey map[string]apiMedia) []storedMedia {
	if tweet.Attachments == nil {
		return nil
	}
	var out []storedMedia
	for _, key := range tweet.Attachments.MediaKeys {
		m, ok := mediaByKey[key]
		if !ok {
			continue
		}
		downloadURL, contentType := mediaDownloadURL(m)
		if downloadURL == "" {
			continue
		}
		stored, err := s.downloadMedia(ctx, downloadURL, contentType, tweet.ID)
		if err != nil {
			s.logger().Warn("twitter sync: media download failed", "tweet_id", tweet.ID, "url", downloadURL, "error", err)
			continue
		}
		out = append(out, *stored)
	}
	return out
}

// mediaDownloadURL ports media_download_url: photos use their url, videos and
// GIFs use the highest-bitrate mp4 variant.
func mediaDownloadURL(m apiMedia) (string, string) {
	switch m.Type {
	case "photo":
		if m.URL == "" {
			return "", ""
		}
		ext := path.Ext(m.URL)
		contentType := mime.TypeByExtension(ext)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		return m.URL, contentType
	case "video", "animated_gif":
		best := -1
		bestURL := ""
		for i, v := range m.Variants {
			if v.ContentType == "video/mp4" && (best == -1 || v.Bitrate > m.Variants[best].Bitrate) {
				best, bestURL = i, v.URL
			}
		}
		if best == -1 {
			return "", ""
		}
		return bestURL, "video/mp4"
	}
	return "", ""
}

// downloadMedia ports download_media: fetch, then store under
// "tweet-<id>-<8 hex><ext>" via the media service.
func (s *Syncer) downloadMedia(ctx context.Context, rawURL, contentType, tweetID string) (*storedMedia, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMediaBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxMediaBytes {
		return nil, fmt.Errorf("media exceeds the 100MB download limit")
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	// The extension comes from the URL path only: video variant URLs carry a
	// query string (?tag=N) that path.Ext on the raw URL would swallow into
	// the stored filename.
	var ext string
	if u, err := url.Parse(rawURL); err == nil {
		ext = path.Ext(u.Path)
	}
	filename := fmt.Sprintf("tweet-%s-%s%s", tweetID, hex.EncodeToString(rnd[:]), ext)
	mediaSvc := media.New(s.db, s.dataDir)
	file, err := mediaSvc.Store(ctx, bytes.NewReader(body), filename, contentType)
	if err != nil {
		return nil, err
	}
	return &storedMedia{key: file.Key, fileID: file.ID, filename: filename, contentType: contentType}, nil
}

// discardStoredMedia reclaims the files rows and disk blobs stored for a
// tweet whose archive failed after the downloads (content conversion error,
// or transaction failure). When the archive transaction began, it must run
// after the rollback (its attachment rows would otherwise still reference
// the files and block the deletes). Best effort: failures are logged, not
// fatal. The failure may come with an already-canceled ctx, so the cleanup
// runs on a context that cannot be canceled.
func (s *Syncer) discardStoredMedia(ctx context.Context, stored []storedMedia) {
	ctx = context.WithoutCancel(ctx)
	mediaSvc := media.New(s.db, s.dataDir)
	remove := func(id int64, key string) {
		if err := s.q.DeleteFile(ctx, id); err != nil {
			s.logger().Warn("twitter sync: delete orphan media row", "id", id, "error", err)
			return
		}
		if err := os.Remove(mediaSvc.PathFor(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logger().Warn("twitter sync: remove orphan media file", "key", key, "error", err)
		}
	}
	for _, m := range stored {
		// Variants reference their original via files.variant_of; under
		// foreign_keys enforcement they must be deleted first.
		variants, err := s.q.ListFileVariants(ctx, sql.NullInt64{Int64: m.fileID, Valid: true})
		if err != nil {
			s.logger().Warn("twitter sync: list media variants", "id", m.fileID, "error", err)
		}
		for _, v := range variants {
			remove(v.ID, v.Key)
		}
		remove(m.fileID, m.key)
	}
}

// buildTweetContent ports build_tweet_content: one <p> per non-blank stripped
// line (escaped), "<p></p>" when empty, then the media attachments.
func buildTweetContent(fullText string, blobs []storedMedia) string {
	var parts []string
	for _, line := range strings.Split(fullText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts = append(parts, "<p>"+escapeHTML(line)+"</p>")
	}
	if len(parts) == 0 {
		parts = append(parts, "<p></p>")
	}
	for _, b := range blobs {
		parts = append(parts, mediaAttachmentHTML(b))
	}
	return strings.Join(parts, "")
}

// mediaAttachmentHTML renders the stored file like the Go write-path
// conventions: <img> for images, <video controls> for playable video.
func mediaAttachmentHTML(b storedMedia) string {
	src := "/files/" + b.key
	if strings.HasPrefix(b.contentType, "video/") {
		return `<video src="` + src + `" controls></video>`
	}
	return `<img src="` + src + `" alt="` + escapeHTML(b.filename) + `" loading="lazy">`
}

// escapeHTML mirrors CGI.escapeHTML (& < > " only; ' is left alone).
func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
