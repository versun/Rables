// Package twittersync ports TwitterSyncService: archiving original tweets
// (and quote tweets) from the configured X account as published Articles.
// Replies and pure retweets are excluded. API credentials come from the
// crossposts "twitter" row. The scheduler hook decides when a run is due
// (T13); Run itself always performs a sync when enabled, so the admin
// "Sync Now" button calls the same entry point (force semantics).
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
	"net/url"
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
	"rables/internal/service/media"
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

	defaultBaseURL = "https://api.twitter.com/2"
)

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
	return s.q.SetTwitterSyncSuccess(ctx, query.SetTwitterSyncSuccessParams{
		SinceID:      latest,
		LastSyncedAt: sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:    now,
	})
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
	if err := s.q.SetTwitterSyncUserID(ctx, query.SetTwitterSyncUserIDParams{
		UserID:    sql.NullString{String: resp.Data.ID, Valid: true},
		UpdatedAt: s.clock().Unix(),
	}); err != nil {
		return "", err
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
// social_media_posts rows and the activity entry.
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

	// Download media (own attachments first, then the quoted tweet's).
	var stored []storedMedia
	for _, t := range collectMediaTweets(tweet, quotedID, inc) {
		stored = append(stored, s.downloadTweetMedia(ctx, t, inc.media)...)
	}

	now := s.clock().Unix()
	article, err := s.q.CreateArticle(ctx, query.CreateArticleParams{
		Slug:                        sql.NullString{String: slug, Valid: true},
		ContentHtml:                 sql.NullString{String: buildTweetContent(fullText, stored), Valid: true},
		ContentType:                 "rich_text",
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
	mediaSvc := media.New(s.db, s.dataDir)
	for _, m := range stored {
		if err := mediaSvc.Attach(ctx, m.fileID, "Article", article.ID, "embeds"); err != nil {
			s.logger().Warn("twitter sync: attach media failed", "tweet_id", tweet.ID, "error", err)
		}
	}
	if err := s.q.UpsertSocialMediaPost(ctx, query.UpsertSocialMediaPostParams{
		ArticleID: article.ID,
		Platform:  "twitter",
		Url:       "https://x.com/" + syncRow.Username.String + "/status/" + tweet.ID,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		return err
	}
	activity.Log(ctx, s.db, "info", "posted", "twitter_sync",
		"slug="+activity.Quote(slug)+" url="+activity.Quote(sourceURL))
	return nil
}

// collectMediaTweets yields the tweet and then its quoted tweet (when
// expanded), mirroring the blob collection order.
func collectMediaTweets(tweet apiTweet, quotedID string, inc includes) []apiTweet {
	out := []apiTweet{tweet}
	if quotedID != "" {
		if quoted, ok := inc.tweets[quotedID]; ok {
			out = append(out, quoted)
		}
	}
	return out
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
// tweet are removed instead.
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

	out := tcoShortRe.ReplaceAllStringFunc(text, func(short string) string {
		if removable[short] {
			return ""
		}
		resolved, ok := replacements[short]
		if !ok {
			resolved = s.followRedirect(ctx, short, redirectLimit)
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
func (s *Syncer) followRedirect(ctx context.Context, rawURL string, limit int) string {
	location := s.redirectLocation(ctx, rawURL)
	if location == "" {
		return rawURL
	}
	if limit <= 1 {
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
// Location header of a 3xx response, "" otherwise.
func (s *Syncer) redirectLocation(ctx context.Context, rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	client := *s.client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if client.Timeout == 0 {
		client.Timeout = 5 * time.Second
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		s.logger().Warn("twitter sync: link resolution failed", "url", rawURL, "error", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 3 {
		return resp.Header.Get("Location")
	}
	return ""
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		return nil, err
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	ext := path.Ext(rawURL)
	filename := fmt.Sprintf("tweet-%s-%s%s", tweetID, hex.EncodeToString(rnd[:]), ext)
	mediaSvc := media.New(s.db, s.dataDir)
	key, err := mediaSvc.Store(ctx, bytes.NewReader(body), filename, contentType)
	if err != nil {
		return nil, err
	}
	file, err := mediaSvc.FileByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &storedMedia{key: key, fileID: file.ID, filename: filename, contentType: contentType}, nil
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
