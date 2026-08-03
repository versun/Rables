package crosspost

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	"rables/internal/service/activity"
	"rables/internal/service/comments"
)

// commentData is the platform-neutral comment hash the Rails services return
// (Comment.upsert_from_external's comment_data plus parent_external_id).
type commentData struct {
	ExternalID       string
	AuthorName       string
	AuthorUsername   string
	AuthorAvatarURL  string
	Content          string
	URL              string
	PublishedAt      int64 // unix seconds; 0 stores NULL
	ParentExternalID string
}

// fetchCommentsResult mirrors the Rails services' { comments:, rate_limit: }.
type fetchCommentsResult struct {
	comments  []commentData
	rateLimit *rateLimitInfo
}

// rateLimitInfo mirrors parse_rate_limit_headers; limit/remaining are -1 when
// the header was absent.
type rateLimitInfo struct {
	limit     int
	remaining int
	resetAt   time.Time
	hasReset  bool
}

// parseRateLimitHeaders reads the rate limit triple; prefix is "X-RateLimit-"
// for Mastodon and "RateLimit-" for the Bluesky public API.
func parseRateLimitHeaders(h http.Header, prefix string) rateLimitInfo {
	rl := rateLimitInfo{limit: -1, remaining: -1}
	if v := h.Get(prefix + "Limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.limit = n
		}
	}
	if v := h.Get(prefix + "Remaining"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			rl.remaining = n
		}
	}
	if v := h.Get(prefix + "Reset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			rl.resetAt = time.Unix(n, 0).UTC()
			rl.hasReset = true
		}
	}
	return rl
}

// rateLimitAction is the FetchSocialCommentsJob decision for one response.
type rateLimitAction int

const (
	rateLimitOK rateLimitAction = iota
	rateLimitSlow
	rateLimitStop
)

// rateLimitSlowDelay mirrors sleep_time = 2.
const rateLimitSlowDelay = 2 * time.Second

// rateLimitActionFor mirrors the job's thresholds: remaining below stop halts
// the platform scan (before the current batch is stored), below delay inserts
// a pause between article fetches. remaining < 0 means the header was absent
// (the Rails `rate_limit[:remaining] &&` guards).
func rateLimitActionFor(remaining, stop, delay int) rateLimitAction {
	switch {
	case remaining < 0:
		return rateLimitOK
	case remaining < stop:
		return rateLimitStop
	case remaining < delay:
		return rateLimitSlow
	}
	return rateLimitOK
}

// fetchThresholds pairs a platform with its stop/delay thresholds.
type fetchThresholds struct{ stop, delay int }

var (
	mastodonFetchThresholds = fetchThresholds{stop: 5, delay: 20}
	blueskyFetchThresholds  = fetchThresholds{stop: 50, delay: 200}
)

// cronFetchPlatforms are the platforms FetchSocialCommentsJob can scan;
// twitter has no job branch in Rails (manual fetch only).
var cronFetchPlatforms = []string{"mastodon", "bluesky"}

// CommentFetcher executes kind=fetch_social_comments jobs
// (FetchSocialCommentsJob plus the enqueued form of the admin per-article
// fetch). Payload shapes in use: {"platform": p} from the hourly scheduler
// (scans every published article with a post URL on p, new comments stay
// pending) and {"article_id", "platform"} from the admin fetch (narrows to
// one article, new comments are approved like the Rails controller).
type CommentFetcher struct {
	DB  *sql.DB
	Log *slog.Logger

	// Platform fetchers; zero values hit the real APIs, tests inject
	// clients/servers through the platform structs.
	mastodon mastodonPlatform
	bluesky  blueskyPlatform
	twitter  twitterPlatform

	q     *query.Queries
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

// NewCommentFetcher returns a CommentFetcher with production defaults.
func NewCommentFetcher(db *sql.DB) *CommentFetcher {
	return &CommentFetcher{
		DB:    db,
		Log:   slog.Default(),
		q:     query.New(db),
		sleep: ctxSleep,
		now:   time.Now,
	}
}

// RegisterFetchCommentsHandlers installs the kind=fetch_social_comments job
// handler. The integrator wires it in cmd/server/main.go next to the other
// Register*Handlers calls (dataDir is part of the shared signature; the
// fetcher stores no files).
func RegisterFetchCommentsHandlers(w *jobs.Worker, db *sql.DB, dataDir string) {
	w.Register(jobs.KindFetchSocialComments, NewCommentFetcher(db).Handle)
}

// fetchCommentsPayload accepts both enqueue shapes (article.FetchCommentsPayload
// tags; article_id omitted means the platform-wide cron scan).
type fetchCommentsPayload struct {
	ArticleID int64  `json:"article_id"`
	Platform  string `json:"platform"`
}

// Handle runs one fetch job. Per-article failures are logged and counted
// without aborting the run (the Rails per-article rescue), so Handle only
// returns an error for infrastructure failures.
func (f *CommentFetcher) Handle(ctx context.Context, raw json.RawMessage) error {
	var p fetchCommentsPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode fetch_social_comments payload: %w", err)
	}
	if p.ArticleID > 0 {
		return f.fetchForArticle(ctx, p.ArticleID, p.Platform)
	}
	platforms := cronFetchPlatforms
	if p.Platform != "" {
		platforms = []string{p.Platform}
	}
	for _, platform := range platforms {
		if platform != "mastodon" && platform != "bluesky" {
			continue // the Rails job has no twitter branch (manual fetch only)
		}
		if err := f.fetchForPlatform(ctx, platform); err != nil {
			return err
		}
	}
	return nil
}

// fetchForArticle ports Admin::ArticlesController#fetch_comments: every
// recorded post of the article (optionally platform-filtered) is fetched and
// upserted with status approved for new comments.
func (f *CommentFetcher) fetchForArticle(ctx context.Context, articleID int64, platform string) error {
	article, err := f.q.GetAdminArticleByID(ctx, articleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load article %d: %w", articleID, err)
	}

	var posts []query.SocialMediaPost
	if platform != "" {
		posts, err = f.q.ListFetchableSocialPostsByPlatform(ctx, query.ListFetchableSocialPostsByPlatformParams{
			ArticleID: articleID, Platform: platform,
		})
	} else {
		posts, err = f.q.ListFetchableSocialPosts(ctx, articleID)
	}
	if err != nil {
		return fmt.Errorf("load social posts for article %d: %w", articleID, err)
	}

	for _, post := range posts {
		fetch, cfg, ok := f.platformFetcher(ctx, post.Platform)
		if !ok {
			continue // no fetcher for this platform, like the Rails case/else
		}
		result := fetch(ctx, cfg, post.Url)
		approved := domain.CommentApproved
		created, err := f.upsertBatch(ctx, articleID, post.Platform, result.comments, &approved)
		desc := fmt.Sprintf("platform=%s title=%s slug=%s", post.Platform,
			activity.Quote(article.Title.String), activity.Quote(article.Slug.String))
		if err != nil {
			f.Log.Warn("fetch comments: article failed", "platform", post.Platform, "article_id", articleID, "error", err)
			activity.Log(ctx, f.DB, "error", "failed", "fetch_comments", desc+" error="+activity.Quote(err.Error()))
			continue
		}
		activity.Log(ctx, f.DB, "info", "fetched", "fetch_comments", fmt.Sprintf("%s count=%d", desc, created))
	}
	return nil
}

// fetchForPlatform ports FetchSocialCommentsJob#process_platform_comments for
// one platform: scan the published articles with a post URL, fetch each
// thread, honor the rate-limit thresholds, and upsert with the default
// (pending) status.
func (f *CommentFetcher) fetchForPlatform(ctx context.Context, platform string) error {
	cfg, err := f.q.GetCrosspostByPlatform(ctx, platform)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Crosspost.for creates a disabled row: not enabled
	}
	if err != nil {
		return fmt.Errorf("load crosspost config %q: %w", platform, err)
	}
	if cfg.Enabled != 1 || cfg.AutoFetchComments != 1 {
		return nil
	}

	targets, err := f.q.ListCommentFetchTargets(ctx, platform)
	if err != nil {
		return fmt.Errorf("list %q fetch targets: %w", platform, err)
	}
	if len(targets) == 0 {
		return nil
	}

	var fetch func(context.Context, query.Crosspost, string) fetchCommentsResult
	var th fetchThresholds
	switch platform {
	case "mastodon":
		fetch, th = f.mastodon.fetchComments, mastodonFetchThresholds
	case "bluesky":
		fetch, th = f.bluesky.fetchComments, blueskyFetchThresholds
	default:
		return nil
	}

	successCount, errorCount, totalComments := 0, 0, 0
	stopped := false
	for _, target := range targets {
		result := fetch(ctx, cfg, target.Url)

		if rl := result.rateLimit; rl != nil {
			switch rateLimitActionFor(rl.remaining, th.stop, th.delay) {
			case rateLimitStop:
				f.Log.Warn("fetch comments: rate limit stop", "platform", platform, "remaining", rl.remaining)
				activity.Log(ctx, f.DB, "warn", "paused", "fetch_comments",
					fmt.Sprintf("platform=%s remaining=%d limit=%d", platform, rl.remaining, rl.limit))
				stopped = true
			case rateLimitSlow:
				if err := f.sleep(ctx, rateLimitSlowDelay); err != nil {
					return err
				}
			}
		}
		if stopped {
			break // the current batch is discarded, like the Rails break
		}

		created, err := f.upsertBatch(ctx, target.ID, platform, result.comments, nil)
		if err != nil {
			errorCount++
			f.Log.Warn("fetch comments: article failed", "platform", platform, "article_id", target.ID, "error", err)
			activity.Log(ctx, f.DB, "error", "failed", "fetch_comments",
				fmt.Sprintf("platform=%s article_id=%d error=%s", platform, target.ID, activity.Quote(err.Error())))
			continue
		}
		totalComments += created
		successCount++
	}

	desc := fmt.Sprintf("platform=%s success_count=%d total_comments=%d error_count=%d",
		platform, successCount, totalComments, errorCount)
	if stopped {
		desc += " stopped=true"
	}
	activity.Log(ctx, f.DB, "info", "completed", "fetch_comments", desc)
	return nil
}

// platformFetcher resolves the fetch method and config for one platform,
// mirroring the controller's case/when. ok is false for platforms without a
// fetcher.
func (f *CommentFetcher) platformFetcher(ctx context.Context, platform string) (func(context.Context, query.Crosspost, string) fetchCommentsResult, query.Crosspost, bool) {
	var fetch func(context.Context, query.Crosspost, string) fetchCommentsResult
	switch platform {
	case "mastodon":
		fetch = f.mastodon.fetchComments
	case "bluesky":
		fetch = f.bluesky.fetchComments
	case "twitter":
		fetch = f.twitter.fetchComments
	default:
		return nil, query.Crosspost{}, false
	}
	cfg, err := f.q.GetCrosspostByPlatform(ctx, platform)
	if errors.Is(err, sql.ErrNoRows) {
		// Crosspost.for(platform) creates a disabled row; the services then
		// return the default (empty) response.
		return fetch, query.Crosspost{Platform: platform}, true
	}
	if err != nil {
		f.Log.Warn("fetch comments: load config", "platform", platform, "error", err)
		return fetch, query.Crosspost{Platform: platform}, true
	}
	return fetch, cfg, true
}

// upsertBatch ports the controller's two passes: upsert every comment with
// Comment.upsert_from_external, then attach parents via parent_external_id
// (in-batch map first, then the DB so replies attach to parents imported in
// earlier batches). It returns the number of created comments. Blank content
// is skipped like the controller's `next if comment_data[:content].blank?`
// (the cron job lets the model validation fail the whole article instead —
// skipping is the union of both behaviors and never loses valid comments).
func (f *CommentFetcher) upsertBatch(ctx context.Context, articleID int64, platform string, batch []commentData, status *domain.CommentStatus) (int, error) {
	byExternalID := map[string]query.Comment{}
	parentOf := map[string]string{}
	created := 0
	for _, d := range batch {
		if strings.TrimSpace(d.Content) == "" {
			continue
		}
		c, result, err := comments.UpsertExternal(ctx, f.q, "Article", articleID, platform, comments.ExternalData{
			ExternalID:      d.ExternalID,
			AuthorName:      d.AuthorName,
			AuthorUsername:  d.AuthorUsername,
			AuthorAvatarURL: d.AuthorAvatarURL,
			Content:         d.Content,
			URL:             d.URL,
			PublishedAt:     d.PublishedAt,
		}, status)
		if err != nil {
			return created, err
		}
		if result == comments.UpsertCreated {
			created++
		}
		byExternalID[d.ExternalID] = c
		if d.ParentExternalID != "" {
			parentOf[d.ExternalID] = d.ParentExternalID
		}
	}

	for externalID, parentExternalID := range parentOf {
		c := byExternalID[externalID]
		parent, ok := byExternalID[parentExternalID]
		if !ok {
			var err error
			parent, err = f.q.GetExternalComment(ctx, query.GetExternalCommentParams{
				CommentableType: sql.NullString{String: "Article", Valid: true},
				CommentableID:   sql.NullInt64{Int64: articleID, Valid: true},
				Platform:        sql.NullString{String: platform, Valid: true},
				ExternalID:      sql.NullString{String: parentExternalID, Valid: true},
			})
			if err != nil {
				continue // parent not imported (yet): leave the reply top-level
			}
		}
		// The lookups above are already platform-scoped, so the Rails
		// same-platform guard is implied; the self-reference guard is not.
		if parent.ID == c.ID {
			continue
		}
		if c.ParentID.Valid && c.ParentID.Int64 == parent.ID {
			continue
		}
		if err := f.q.SetCommentParent(ctx, query.SetCommentParentParams{
			ParentID:  sql.NullInt64{Int64: parent.ID, Valid: true},
			UpdatedAt: f.now().UTC().Unix(),
			ID:        c.ID,
		}); err != nil {
			return created, fmt.Errorf("set comment parent: %w", err)
		}
	}
	return created, nil
}

// --- mastodon (MastodonService#fetch_comments) ---

// mastodonStatusIDRe ports extract_status_id_from_url.
var mastodonStatusIDRe = regexp.MustCompile(`/(?:@\w+|users/\w+/statuses)/(\d+)`)

// fetchComments ports MastodonService#fetch_comments: GET
// /api/v1/statuses/:id/context and parse the descendants. Any failure
// (network, non-2xx, decode) yields the default empty result, like the Rails
// rescue; the rate-limit triple always rides along when the headers exist.
func (p mastodonPlatform) fetchComments(ctx context.Context, cfg query.Crosspost, statusURL string) fetchCommentsResult {
	if cfg.Enabled != 1 || statusURL == "" {
		return fetchCommentsResult{}
	}
	m := mastodonStatusIDRe.FindStringSubmatch(statusURL)
	if m == nil {
		return fetchCommentsResult{}
	}
	statusID := m[1]
	u, err := mastodonAPIURL(cfg.ServerUrl.String, "/api/v1/statuses/"+statusID+"/context")
	if err != nil {
		return fetchCommentsResult{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fetchCommentsResult{}
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AccessToken.String)
	resp, err := p.http().Do(req)
	if err != nil {
		slog.Warn("mastodon: fetch comments failed", "url", statusURL, "error", err)
		return fetchCommentsResult{}
	}
	defer resp.Body.Close()
	rl := parseRateLimitHeaders(resp.Header, "X-RateLimit-")
	logMastodonRateLimit(resp.Header)
	if resp.StatusCode != http.StatusOK {
		// 429 and other non-2xx both return the empty batch plus the parsed
		// rate limit (handle_rate_limit_exceeded only logs).
		return fetchCommentsResult{rateLimit: &rl}
	}
	var contextData struct {
		Descendants []mastodonReply `json:"descendants"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&contextData); err != nil {
		slog.Warn("mastodon: decode context failed", "error", err)
		return fetchCommentsResult{rateLimit: &rl}
	}
	var out []commentData
	for _, reply := range contextData.Descendants {
		if c, ok := reply.comment(statusID); ok {
			out = append(out, c)
		}
	}
	return fetchCommentsResult{comments: out, rateLimit: &rl}
}

// mastodonReply is one statuses/:id/context descendant.
type mastodonReply struct {
	ID          string  `json:"id"`
	URL         string  `json:"url"`
	Content     string  `json:"content"`
	CreatedAt   string  `json:"created_at"`
	InReplyToID *string `json:"in_reply_to_id"`
	Account     struct {
		DisplayName string `json:"display_name"`
		Username    string `json:"username"`
		Acct        string `json:"acct"`
		Avatar      string `json:"avatar"`
	} `json:"account"`
}

// comment ports build_comment_data: a reply with a non-http(s) URL or
// malformed timestamps is skipped so one bad entry never discards the batch.
func (r mastodonReply) comment(originalStatusID string) (commentData, bool) {
	if !isHTTPURL(r.URL) {
		return commentData{}, false
	}
	publishedAt, err := parseAPITime(r.CreatedAt)
	if err != nil {
		return commentData{}, false
	}
	authorName := r.Account.DisplayName
	if strings.TrimSpace(authorName) == "" { // display_name.presence || username
		authorName = r.Account.Username
	}
	parent := ""
	if r.InReplyToID != nil && *r.InReplyToID != "" && *r.InReplyToID != originalStatusID {
		parent = *r.InReplyToID
	}
	return commentData{
		ExternalID:       r.ID,
		AuthorName:       authorName,
		AuthorUsername:   r.Account.Acct,
		AuthorAvatarURL:  r.Account.Avatar,
		Content:          r.Content,
		URL:              r.URL,
		PublishedAt:      publishedAt,
		ParentExternalID: parent,
	}, true
}

// --- bluesky (BlueskyService#fetch_comments) ---

// blueskyPostURLRe ports the extract_post_uri_from_url match.
var blueskyPostURLRe = regexp.MustCompile(`bsky\.app/profile/([^/]+)/post/(\w+)`)

// fetchComments ports BlueskyService#fetch_comments: resolve the post AT-URI,
// then read the thread from the unauthenticated public API (depth 10) and
// flatten the nested replies. verify_tokens runs first, like Rails — a broken
// session means a broken config and the fetch is skipped.
func (p blueskyPlatform) fetchComments(ctx context.Context, cfg query.Crosspost, postURL string) fetchCommentsResult {
	if cfg.Enabled != 1 || postURL == "" {
		return fetchCommentsResult{}
	}
	m := blueskyPostURLRe.FindStringSubmatch(postURL)
	if m == nil {
		return fetchCommentsResult{}
	}
	did := p.ResolveHandle(ctx, m[1])
	if did == "" {
		return fetchCommentsResult{}
	}
	atURI := "at://" + did + "/app.bsky.feed.post/" + m[2]

	server := cfg.ServerUrl.String
	if server == "" {
		server = blueskyDefaultServer
	}
	if _, err := p.ensureSession(ctx, server, cfg); err != nil {
		slog.Warn("bluesky: session check failed, skipping comment fetch", "error", err)
		return fetchCommentsResult{}
	}

	endpoint := p.publicAPI() + "/xrpc/app.bsky.feed.getPostThread?uri=" + url.QueryEscape(atURI) + "&depth=10"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fetchCommentsResult{}
	}
	resp, err := p.http().Do(req)
	if err != nil {
		slog.Warn("bluesky: fetch comments failed", "url", postURL, "error", err)
		return fetchCommentsResult{}
	}
	defer resp.Body.Close()
	rl := parseRateLimitHeaders(resp.Header, "RateLimit-")
	if resp.StatusCode != http.StatusOK {
		return fetchCommentsResult{rateLimit: &rl}
	}
	var thread struct {
		Thread struct {
			Replies []blueskyThreadItem `json:"replies"`
		} `json:"thread"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&thread); err != nil {
		slog.Warn("bluesky: decode thread failed", "error", err)
		return fetchCommentsResult{rateLimit: &rl}
	}
	return fetchCommentsResult{comments: flattenBlueskyReplies(thread.Thread.Replies, ""), rateLimit: &rl}
}

// blueskyThreadItem is one app.bsky.feed.getPostThread tree node; non-post
// entries (blocked/deleted) carry no post and are skipped.
type blueskyThreadItem struct {
	Post *struct {
		URI    string `json:"uri"`
		Author struct {
			DisplayName string `json:"displayName"`
			Handle      string `json:"handle"`
			Avatar      string `json:"avatar"`
		} `json:"author"`
		Record struct {
			Text      string `json:"text"`
			CreatedAt string `json:"createdAt"`
		} `json:"record"`
	} `json:"post"`
	Replies []blueskyThreadItem `json:"replies"`
}

// flattenBlueskyReplies ports flatten_thread_replies: the external id is the
// post rkey (last AT-URI segment) and the parent is the enclosing reply's
// rkey ("" at the top level, not the original post).
func flattenBlueskyReplies(replies []blueskyThreadItem, parentExternalID string) []commentData {
	var out []commentData
	for _, item := range replies {
		if item.Post == nil {
			continue
		}
		post := item.Post
		rkey := post.URI[strings.LastIndex(post.URI, "/")+1:]
		publishedAt, err := parseAPITime(post.Record.CreatedAt)
		if err != nil {
			continue
		}
		authorName := post.Author.DisplayName
		if strings.TrimSpace(authorName) == "" { // displayName.presence || handle
			authorName = post.Author.Handle
		}
		out = append(out, commentData{
			ExternalID:       rkey,
			AuthorName:       authorName,
			AuthorUsername:   post.Author.Handle,
			AuthorAvatarURL:  post.Author.Avatar,
			Content:          post.Record.Text,
			URL:              "https://bsky.app/profile/" + post.Author.Handle + "/post/" + rkey,
			PublishedAt:      publishedAt,
			ParentExternalID: parentExternalID,
		})
		out = append(out, flattenBlueskyReplies(item.Replies, rkey)...)
	}
	return out
}

// --- twitter (TwitterService#fetch_comments) ---

// fetchComments ports TwitterService#fetch_comments: look up the tweet for
// its conversation_id, then collect direct replies, quote tweets, and the
// replies to each quote. Failures yield the empty result (the Rails rescue);
// the manual caller ignores rate-limit info, so it is not parsed.
func (p twitterPlatform) fetchComments(ctx context.Context, cfg query.Crosspost, postURL string) fetchCommentsResult {
	if cfg.Enabled != 1 || postURL == "" {
		return fetchCommentsResult{}
	}
	u := normalizeTweetURL(postURL)
	if u == nil {
		return fetchCommentsResult{}
	}
	m := twitterStatusPathRe.FindStringSubmatch(u.Path)
	if m == nil {
		return fetchCommentsResult{}
	}
	tweetID := m[1]
	client := p.signingClient(cfg)

	body, err := p.doJSON(ctx, client, http.MethodGet, p.base()+"/tweets/"+tweetID+
		"?expansions=author_id,referenced_tweets.id&tweet.fields=conversation_id,created_at,author_id&user.fields=username,name,profile_image_url", nil, "")
	if err != nil {
		slog.Warn("twitter: fetch post failed", "url", postURL, "error", err)
		return fetchCommentsResult{}
	}
	data, _ := body["data"].(map[string]any)
	if data == nil {
		return fetchCommentsResult{}
	}
	conversationID, _ := data["conversation_id"].(string)
	if conversationID == "" {
		return fetchCommentsResult{}
	}

	var out []commentData
	// 1. Direct replies (fetch_conversation_comments step 1).
	replies, err := p.searchRecent(ctx, client, "conversation_id:"+conversationID+" is:reply")
	if err == nil {
		out = append(out, replies.comments(tweetID)...)
	}
	// 2. Quote tweets, then 3. the replies to each quote's conversation.
	quotes, err := p.searchRecent(ctx, client, "url:"+postURL+" is:quote")
	if err != nil {
		return fetchCommentsResult{comments: out}
	}
	quoteComments := quotes.comments(tweetID)
	out = append(out, quoteComments...)
	for i, tweet := range quotes.Data {
		if tweet.ConversationID == "" {
			continue // fetch_quote_tweet_replies returns early
		}
		quoteReplies, err := p.searchRecent(ctx, client, "conversation_id:"+tweet.ConversationID+" is:reply")
		if err != nil {
			continue
		}
		out = append(out, quoteReplies.comments(quoteComments[i].ExternalID)...)
	}
	return fetchCommentsResult{comments: out}
}

// xSearchResponse is the tweets/search/recent shape process_tweets consumes.
type xSearchResponse struct {
	Data []struct {
		ID               string `json:"id"`
		Text             string `json:"text"`
		AuthorID         string `json:"author_id"`
		ConversationID   string `json:"conversation_id"`
		CreatedAt        string `json:"created_at"`
		ReferencedTweets []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"referenced_tweets"`
	} `json:"data"`
	Includes struct {
		Users []struct {
			ID              string `json:"id"`
			Username        string `json:"username"`
			Name            string `json:"name"`
			ProfileImageURL string `json:"profile_image_url"`
		} `json:"users"`
	} `json:"includes"`
}

// searchRecent runs one tweets/search/recent query with the expansions
// fetch_conversation_comments uses.
func (p twitterPlatform) searchRecent(ctx context.Context, client *http.Client, query string) (*xSearchResponse, error) {
	endpoint := p.base() + "/tweets/search/recent?query=" + url.QueryEscape(query) +
		"&expansions=author_id,referenced_tweets.id&tweet.fields=created_at,referenced_tweets,conversation_id&user.fields=username,name,profile_image_url&max_results=100"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, transientNetError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("twitter: search recent: %s", resp.Status)
	}
	var out xSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("twitter: decode search response: %w", err)
	}
	return &out, nil
}

// comments ports process_tweets + build_comment_data: tweets whose author is
// missing from includes are skipped; the parent is the replied_to reference,
// then the quoted one, then the default parent id.
func (r *xSearchResponse) comments(defaultParentID string) []commentData {
	users := map[string]int{}
	for i, user := range r.Includes.Users {
		users[user.ID] = i
	}
	var out []commentData
	for _, tweet := range r.Data {
		idx, ok := users[tweet.AuthorID]
		if !ok {
			continue
		}
		publishedAt, err := parseAPITime(tweet.CreatedAt)
		if err != nil {
			continue
		}
		parent := defaultParentID
		for _, ref := range tweet.ReferencedTweets {
			if ref.Type == "replied_to" {
				parent = ref.ID
				break
			}
			if ref.Type == "quoted" {
				parent = ref.ID
			}
		}
		author := r.Includes.Users[idx]
		out = append(out, commentData{
			ExternalID:       tweet.ID,
			AuthorName:       author.Name,
			AuthorUsername:   author.Username,
			AuthorAvatarURL:  author.ProfileImageURL,
			Content:          tweet.Text,
			URL:              "https://x.com/" + author.Username + "/status/" + tweet.ID,
			PublishedAt:      publishedAt,
			ParentExternalID: parent,
		})
	}
	return out
}

// parseAPITime parses the ISO 8601 timestamps all three APIs return
// (Time.parse in the Rails services).
func parseAPITime(s string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}

// isHTTPURL ports MastodonService#http_url?: http(s) scheme with a host.
func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
