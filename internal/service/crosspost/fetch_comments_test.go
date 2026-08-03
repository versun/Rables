package crosspost

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"rables/internal/db"
)

func newTestCommentFetcher(t *testing.T) (*CommentFetcher, *sql.DB) {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	f := NewCommentFetcher(database)
	f.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	f.sleep = func(context.Context, time.Duration) error { return nil }
	return f, database
}

// insertFetchCrosspost inserts a crossposts row with the credentials the
// platform clients need (the fake servers accept any of them).
func insertFetchCrosspost(t *testing.T, database *sql.DB, platform, serverURL string, enabled, autoFetch int64) {
	t.Helper()
	_, err := database.ExecContext(t.Context(),
		`INSERT INTO crossposts (platform, enabled, server_url, username, app_password,
		  access_token, access_token_secret, api_key, api_key_secret,
		  auto_fetch_comments, created_at, updated_at)
		 VALUES (?, ?, ?, 'handle.test', 'app-pw', 'tok', 'toksecret', 'key', 'secret', ?, 1000, 1000)`,
		platform, enabled, serverURL, autoFetch)
	if err != nil {
		t.Fatalf("insert crosspost %q: %v", platform, err)
	}
}

func insertArticleWithStatus(t *testing.T, database *sql.DB, slug string, status int64) int64 {
	t.Helper()
	res, err := database.ExecContext(t.Context(),
		`INSERT INTO articles (slug, status, created_at, updated_at) VALUES (?, ?, 1000, 1000)`, slug, status)
	if err != nil {
		t.Fatalf("insert article: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

type storedComment struct {
	authorName     string
	authorUsername string
	authorAvatar   string
	content        string
	status         int64
	platform       string
	externalID     string
	url            string
	publishedAt    sql.NullInt64
	parentID       sql.NullInt64
}

func loadComments(t *testing.T, database *sql.DB, articleID int64) []storedComment {
	t.Helper()
	rows, err := database.QueryContext(t.Context(),
		`SELECT author_name, author_username, author_avatar_url, content, status, platform,
		        external_id, url, published_at, parent_id
		 FROM comments WHERE commentable_type = 'Article' AND commentable_id = ? ORDER BY id`, articleID)
	if err != nil {
		t.Fatalf("load comments: %v", err)
	}
	defer rows.Close()
	var out []storedComment
	for rows.Next() {
		var c storedComment
		var username, avatar, platform, externalID, url sql.NullString
		if err := rows.Scan(&c.authorName, &username, &avatar, &c.content, &c.status, &platform,
			&externalID, &url, &c.publishedAt, &c.parentID); err != nil {
			t.Fatalf("scan comment: %v", err)
		}
		c.authorUsername, c.authorAvatar, c.platform, c.externalID, c.url =
			username.String, avatar.String, platform.String, externalID.String, url.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate comments: %v", err)
	}
	return out
}

func runFetch(t *testing.T, f *CommentFetcher, payload string) {
	t.Helper()
	if err := f.Handle(t.Context(), json.RawMessage(payload)); err != nil {
		t.Fatalf("Handle(%s): %v", payload, err)
	}
}

// --- rate limit thresholds (pure) ---

func TestRateLimitActionFor(t *testing.T) {
	cases := []struct {
		remaining, stop, delay int
		want                   rateLimitAction
	}{
		{-1, 5, 20, rateLimitOK},   // header absent: no opinion
		{4, 5, 20, rateLimitStop},  // mastodon below stop
		{5, 5, 20, rateLimitSlow},  // at stop boundary
		{19, 5, 20, rateLimitSlow}, // below delay
		{20, 5, 20, rateLimitOK},   // at delay boundary
		{49, 50, 200, rateLimitStop},
		{50, 50, 200, rateLimitSlow},
		{199, 50, 200, rateLimitSlow},
		{200, 50, 200, rateLimitOK},
	}
	for _, tc := range cases {
		if got := rateLimitActionFor(tc.remaining, tc.stop, tc.delay); got != tc.want {
			t.Errorf("rateLimitActionFor(%d, %d, %d) = %d, want %d",
				tc.remaining, tc.stop, tc.delay, got, tc.want)
		}
	}
}

// --- mastodon ---

// mastodonContextFixture mirrors a real /api/v1/statuses/:id/context
// response: one top-level reply, one nested reply, one malformed entry.
const mastodonContextFixture = `{
  "ancestors": [],
  "descendants": [
    {
      "id": "1001",
      "created_at": "2026-01-02T03:04:05.000Z",
      "in_reply_to_id": "123",
      "content": "<p>great post</p>",
      "url": "https://mastodon.social/@alice/1001",
      "account": {"display_name": "Alice A", "username": "alice", "acct": "alice", "avatar": "https://mastodon.social/avatars/alice.png"}
    },
    {
      "id": "1002",
      "created_at": "2026-01-02T04:05:06.000Z",
      "in_reply_to_id": "1001",
      "content": "<p>thanks</p>",
      "url": "https://mastodon.social/@bob/1002",
      "account": {"display_name": "", "username": "bob", "acct": "bob@mastodon.social", "avatar": "https://mastodon.social/avatars/bob.png"}
    },
    {
      "id": "1003",
      "created_at": "2026-01-02T05:06:07.000Z",
      "in_reply_to_id": "123",
      "content": "<p>no url</p>",
      "url": "notaurl",
      "account": {"display_name": "Mallory", "username": "mallory", "acct": "mallory", "avatar": ""}
    }
  ]
}`

func TestFetchMastodonCommentsCron(t *testing.T) {
	f, database := newTestCommentFetcher(t)

	var mu sync.Mutex
	requested := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requested[r.URL.Path]++
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "300")
		w.Header().Set("X-RateLimit-Remaining", "297")
		fmt.Fprint(w, mastodonContextFixture)
	}))
	t.Cleanup(srv.Close)
	f.mastodon = mastodonPlatform{client: srv.Client()}

	insertFetchCrosspost(t, database, "mastodon", srv.URL, 1, 1)
	published := insertArticleWithStatus(t, database, "pub", 1)
	draft := insertArticleWithStatus(t, database, "draft", 0)
	recordSocialURL(t, database, published, "mastodon", "https://mastodon.social/@me/123", 1000)
	recordSocialURL(t, database, draft, "mastodon", "https://mastodon.social/@me/456", 1000)

	runFetch(t, f, `{"platform":"mastodon"}`)

	// The draft article is outside Article.published and never fetched.
	if requested["/api/v1/statuses/123/context"] != 1 || requested["/api/v1/statuses/456/context"] != 0 {
		t.Errorf("requests = %v", requested)
	}

	got := loadComments(t, database, published)
	if len(got) != 2 { // the non-http url reply is skipped
		t.Fatalf("comments = %+v", got)
	}
	top, nested := got[0], got[1]

	if top.externalID != "1001" || top.platform != "mastodon" {
		t.Errorf("top = %+v", top)
	}
	if top.authorName != "Alice A" || top.authorUsername != "alice" ||
		top.authorAvatar != "https://mastodon.social/avatars/alice.png" {
		t.Errorf("top author = %+v", top)
	}
	if top.content != "<p>great post</p>" || top.url != "https://mastodon.social/@alice/1001" {
		t.Errorf("top content/url = %+v", top)
	}
	wantTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix()
	if !top.publishedAt.Valid || top.publishedAt.Int64 != wantTime {
		t.Errorf("top published_at = %v, want %d", top.publishedAt, wantTime)
	}
	if top.status != 0 { // cron fetch leaves new comments pending
		t.Errorf("top status = %d, want pending(0)", top.status)
	}
	if top.parentID.Valid { // in_reply_to the original post: top-level
		t.Errorf("top parent_id = %v, want NULL", top.parentID)
	}

	if nested.authorName != "bob" { // display_name "" falls back to username
		t.Errorf("nested author_name = %q, want username fallback", nested.authorName)
	}
	if !nested.parentID.Valid { // replies to 1001 attach to it (second pass)
		t.Errorf("nested parent_id = NULL, want the 1001 comment")
	}
}

func TestFetchMastodonRateLimitFlow(t *testing.T) {
	f, database := newTestCommentFetcher(t)

	var sleeps []time.Duration
	f.sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}

	// Per-status responses: plenty -> slow -> critically low.
	remaining := map[string]string{
		"/api/v1/statuses/1/context": "100",
		"/api/v1/statuses/2/context": "15",
		"/api/v1/statuses/3/context": "4",
	}
	var mu sync.Mutex
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		statusID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/statuses/"), "/context")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", remaining[r.URL.Path])
		fmt.Fprintf(w, `{"ancestors": [], "descendants": [{
		  "id": "c%s",
		  "created_at": "2026-01-02T03:04:05.000Z", "in_reply_to_id": null,
		  "content": "<p>hi</p>", "url": "https://mastodon.social/@a/b",
		  "account": {"display_name": "A", "username": "a", "acct": "a", "avatar": ""}}]}`, statusID)
	}))
	t.Cleanup(srv.Close)
	f.mastodon = mastodonPlatform{client: srv.Client()}

	insertFetchCrosspost(t, database, "mastodon", srv.URL, 1, 1)
	var ids []int64
	for _, slug := range []string{"a1", "a2", "a3", "a4"} {
		id := insertArticleWithStatus(t, database, slug, 1)
		ids = append(ids, id)
	}
	for i, id := range ids {
		recordSocialURL(t, database, id, "mastodon", fmt.Sprintf("https://mastodon.social/@me/%d", i+1), 1000)
	}

	runFetch(t, f, `{"platform":"mastodon"}`)

	// Articles 1-3 are fetched; the critically-low response of article 3 stops
	// the scan: its batch is discarded and article 4 is never requested.
	if len(hits) != 3 {
		t.Errorf("hits = %v, want 3 fetches", hits)
	}
	if len(sleeps) != 1 || sleeps[0] != rateLimitSlowDelay {
		t.Errorf("sleeps = %v, want one %s", sleeps, rateLimitSlowDelay)
	}
	if n := len(loadComments(t, database, ids[0])); n != 1 {
		t.Errorf("article 1 comments = %d, want 1", n)
	}
	if n := len(loadComments(t, database, ids[1])); n != 1 {
		t.Errorf("article 2 comments = %d, want 1 (slow still stores)", n)
	}
	if n := len(loadComments(t, database, ids[2])); n != 0 {
		t.Errorf("article 3 comments = %d, want 0 (discarded on stop)", n)
	}

	var paused int
	if err := database.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM activity_logs WHERE action = 'paused' AND target = 'fetch_comments'`).Scan(&paused); err != nil {
		t.Fatalf("count paused: %v", err)
	}
	if paused != 1 {
		t.Errorf("paused activity rows = %d, want 1", paused)
	}
}

// --- bluesky ---

// blueskyThreadFixture mirrors app.bsky.feed.getPostThread: one nested reply
// chain plus a non-post (blocked) entry that must be skipped.
const blueskyThreadFixture = `{
  "thread": {
    "$type": "app.bsky.feed.defs#threadViewPost",
    "post": {"uri": "at://did:plc:test/app.bsky.feed.post/root"},
    "replies": [
      {
        "post": {
          "uri": "at://did:plc:a/app.bsky.feed.post/r1",
          "author": {"did": "did:plc:a", "handle": "ann.example", "displayName": "Ann", "avatar": "https://cdn.bsky.app/img/ann.png"},
          "record": {"$type": "app.bsky.feed.post", "text": "nice post", "createdAt": "2026-02-03T04:05:06.000Z"}
        },
        "replies": [
          {
            "post": {
              "uri": "at://did:plc:b/app.bsky.feed.post/r2",
              "author": {"did": "did:plc:b", "handle": "ben.example", "avatar": "https://cdn.bsky.app/img/ben.png"},
              "record": {"text": "indeed", "createdAt": "2026-02-03T05:06:07.000Z"}
            }
          }
        ]
      },
      {"$type": "app.bsky.feed.defs#blockedPost", "blocked": true}
    ]
  }
}`

func TestFetchBlueskyCommentsCron(t *testing.T) {
	f, database := newTestCommentFetcher(t)

	fake := &fakeBluesky{ // createSession for verify_tokens
		sessionExp:   time.Now().Add(2 * time.Hour),
		refreshedExp: time.Now().Add(2 * time.Hour),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.identity.resolveHandle", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("handle") != "me.example" {
			t.Errorf("resolve handle = %q", r.URL.Query().Get("handle"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"did": "did:plc:test"}`)
	})
	mux.HandleFunc("/xrpc/app.bsky.feed.getPostThread", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("uri"); got != "at://did:plc:test/app.bsky.feed.post/root" {
			t.Errorf("thread uri = %q", got)
		}
		if got := r.URL.Query().Get("depth"); got != "10" {
			t.Errorf("depth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("RateLimit-Limit", "3000")
		w.Header().Set("RateLimit-Remaining", "2999")
		fmt.Fprint(w, blueskyThreadFixture)
	})
	mux.Handle("/", fake.handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	f.bluesky = blueskyPlatform{client: srv.Client(), tokens: newFakeTokenCache(), publicBase: srv.URL}

	insertFetchCrosspost(t, database, "bluesky", srv.URL, 1, 1)
	articleID := insertArticleWithStatus(t, database, "bsky-post", 1)
	recordSocialURL(t, database, articleID, "bluesky", "https://bsky.app/profile/me.example/post/root", 1000)

	runFetch(t, f, `{"platform":"bluesky"}`)

	if fake.createSessionCalls != 1 { // verify_tokens before the public call
		t.Errorf("createSession calls = %d, want 1", fake.createSessionCalls)
	}

	got := loadComments(t, database, articleID)
	if len(got) != 2 { // blockedPost skipped
		t.Fatalf("comments = %+v", got)
	}
	top, nested := got[0], got[1]

	if top.externalID != "r1" || top.authorName != "Ann" || top.authorUsername != "ann.example" ||
		top.authorAvatar != "https://cdn.bsky.app/img/ann.png" || top.content != "nice post" ||
		top.url != "https://bsky.app/profile/ann.example/post/r1" {
		t.Errorf("top = %+v", top)
	}
	if top.parentID.Valid {
		t.Errorf("top parent_id = %v, want NULL (not the original post)", top.parentID)
	}
	wantTime := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC).Unix()
	if !top.publishedAt.Valid || top.publishedAt.Int64 != wantTime {
		t.Errorf("top published_at = %v, want %d", top.publishedAt, wantTime)
	}

	if nested.externalID != "r2" || nested.authorName != "ben.example" { // missing displayName -> handle
		t.Errorf("nested = %+v", nested)
	}
	if !nested.parentID.Valid {
		t.Errorf("nested parent_id = NULL, want r1's comment")
	}
}

// --- twitter ---

// fakeTwitterComments serves the tweet lookup plus the three search/recent
// queries of a comment fetch.
func fakeTwitterComments(t *testing.T) *httptest.Server {
	t.Helper()
	search := func(query string) string {
		switch query {
		case "conversation_id:123 is:reply":
			return `{"data": [{
			    "id": "9001", "text": "first!", "author_id": "u1",
			    "conversation_id": "123", "created_at": "2026-03-04T05:06:07.000Z",
			    "referenced_tweets": [{"type": "replied_to", "id": "123"}]
			  }],
			  "includes": {"users": [{"id": "u1", "username": "carol", "name": "Carol C", "profile_image_url": "https://img/carol.png"}]}}`
		case "url:https://x.com/me/status/123 is:quote":
			return `{"data": [{
			    "id": "9002", "text": "look at this", "author_id": "u2",
			    "conversation_id": "456", "created_at": "2026-03-04T06:07:08.000Z",
			    "referenced_tweets": [{"type": "quoted", "id": "123"}]
			  }],
			  "includes": {"users": [{"id": "u2", "username": "dan", "name": "Dan D", "profile_image_url": "https://img/dan.png"}]}}`
		case "conversation_id:456 is:reply":
			return `{"data": [{
			    "id": "9003", "text": "agreed", "author_id": "u1",
			    "conversation_id": "456", "created_at": "2026-03-04T07:08:09.000Z",
			    "referenced_tweets": [{"type": "replied_to", "id": "9002"}]
			  }],
			  "includes": {"users": [{"id": "u1", "username": "carol", "name": "Carol C", "profile_image_url": "https://img/carol.png"}]}}`
		}
		t.Errorf("unexpected search query %q", query)
		return `{"data": []}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/2/tweets/123":
			fmt.Fprint(w, `{"data": {"id": "123", "conversation_id": "123", "created_at": "2026-03-04T01:02:03.000Z"}}`)
		case "/2/tweets/search/recent":
			fmt.Fprint(w, search(r.URL.Query().Get("query")))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchTwitterCommentsManual(t *testing.T) {
	f, database := newTestCommentFetcher(t)
	srv := fakeTwitterComments(t)
	f.twitter = twitterPlatform{baseURL: srv.URL + "/2", client: srv.Client()}

	// auto_fetch_comments stays 0: the manual fetch only needs enabled=1.
	insertFetchCrosspost(t, database, "twitter", "", 1, 0)
	articleID := insertArticleWithStatus(t, database, "tweet-post", 1)
	recordSocialURL(t, database, articleID, "twitter", "https://x.com/me/status/123", 1000)

	runFetch(t, f, fmt.Sprintf(`{"article_id":%d,"platform":"twitter"}`, articleID))

	got := loadComments(t, database, articleID)
	if len(got) != 3 { // reply + quote + reply-to-quote
		t.Fatalf("comments = %+v", got)
	}
	reply, quote, quoteReply := got[0], got[1], got[2]

	if reply.externalID != "9001" || reply.authorName != "Carol C" || reply.authorUsername != "carol" ||
		reply.authorAvatar != "https://img/carol.png" || reply.content != "first!" ||
		reply.url != "https://x.com/carol/status/9001" {
		t.Errorf("reply = %+v", reply)
	}
	if reply.status != 1 { // the manual fetch approves new comments
		t.Errorf("reply status = %d, want approved(1)", reply.status)
	}
	if reply.parentID.Valid { // replied_to the original post: top-level
		t.Errorf("reply parent_id = %v, want NULL", reply.parentID)
	}

	if quote.externalID != "9002" || quote.content != "look at this" {
		t.Errorf("quote = %+v", quote)
	}
	if quoteReply.externalID != "9003" || !quoteReply.parentID.Valid {
		t.Errorf("quoteReply = %+v, want parent set to the quote tweet comment", quoteReply)
	}
	wantTime := time.Date(2026, 3, 4, 7, 8, 9, 0, time.UTC).Unix()
	if !quoteReply.publishedAt.Valid || quoteReply.publishedAt.Int64 != wantTime {
		t.Errorf("quoteReply published_at = %v, want %d", quoteReply.publishedAt, wantTime)
	}
}

// --- handler dispatch ---

func TestFetchCommentsCronSkipsTwitter(t *testing.T) {
	f, database := newTestCommentFetcher(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("cron fetch must not call the twitter API, got %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	f.twitter = twitterPlatform{baseURL: srv.URL + "/2", client: srv.Client()}

	insertFetchCrosspost(t, database, "twitter", "", 1, 1)
	articleID := insertArticleWithStatus(t, database, "tweet-post", 1)
	recordSocialURL(t, database, articleID, "twitter", "https://x.com/me/status/123", 1000)

	runFetch(t, f, `{"platform":"twitter"}`) // FetchSocialCommentsJob has no twitter branch
	if n := len(loadComments(t, database, articleID)); n != 0 {
		t.Errorf("comments = %d, want 0", n)
	}
}

func TestFetchCommentsGates(t *testing.T) {
	cases := []struct {
		name      string
		enabled   int64
		autoFetch int64
		manual    bool
		wantHits  int
	}{
		{"cron disabled platform", 0, 1, false, 0},
		{"cron without auto_fetch_comments", 1, 0, false, 0},
		{"manual on disabled platform", 0, 0, true, 0},
		{"manual ignores auto_fetch_comments", 1, 0, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, database := newTestCommentFetcher(t)
			var hits int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"ancestors": [], "descendants": []}`)
			}))
			t.Cleanup(srv.Close)
			f.mastodon = mastodonPlatform{client: srv.Client()}

			insertFetchCrosspost(t, database, "mastodon", srv.URL, tc.enabled, tc.autoFetch)
			articleID := insertArticleWithStatus(t, database, "post", 1)
			recordSocialURL(t, database, articleID, "mastodon", "https://mastodon.social/@me/123", 1000)

			payload := `{"platform":"mastodon"}`
			if tc.manual {
				payload = fmt.Sprintf(`{"article_id":%d,"platform":"mastodon"}`, articleID)
			}
			runFetch(t, f, payload)
			if hits != tc.wantHits {
				t.Errorf("hits = %d, want %d", hits, tc.wantHits)
			}
		})
	}
}

func TestFetchCommentsUnknownPlatformAndBadPayload(t *testing.T) {
	f, database := newTestCommentFetcher(t)
	articleID := insertArticleWithStatus(t, database, "post", 1)
	recordSocialURL(t, database, articleID, "xiaohongshu", "https://example.com/x/1", 1000)

	// A platform without a fetcher is skipped like the Rails case/else.
	runFetch(t, f, fmt.Sprintf(`{"article_id":%d}`, articleID))

	if err := f.Handle(t.Context(), json.RawMessage(`{bad`)); err == nil {
		t.Error("Handle(bad json) = nil error")
	}
	// A missing article is a no-op (find_by + return unless article).
	runFetch(t, f, `{"article_id":99999,"platform":"mastodon"}`)
}

// --- idempotent upsert ---

func TestFetchCommentsIdempotent(t *testing.T) {
	f, database := newTestCommentFetcher(t)

	var mu sync.Mutex
	content := "<p>great post</p>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		c := content
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ancestors": [], "descendants": [{
		  "id": "1001", "created_at": "2026-01-02T03:04:05.000Z", "in_reply_to_id": "123",
		  "content": %q, "url": "https://mastodon.social/@alice/1001",
		  "account": {"display_name": "Alice A", "username": "alice", "acct": "alice", "avatar": ""}}]}`, c)
	}))
	t.Cleanup(srv.Close)
	f.mastodon = mastodonPlatform{client: srv.Client()}

	insertFetchCrosspost(t, database, "mastodon", srv.URL, 1, 1)
	articleID := insertArticleWithStatus(t, database, "post", 1)
	recordSocialURL(t, database, articleID, "mastodon", "https://mastodon.social/@me/123", 1000)

	runFetch(t, f, `{"platform":"mastodon"}`)
	if n := len(loadComments(t, database, articleID)); n != 1 {
		t.Fatalf("first run comments = %d, want 1", n)
	}

	// A moderation decision is made, then the platform content changes.
	if _, err := database.ExecContext(t.Context(),
		`UPDATE comments SET status = 1 WHERE commentable_id = ?`, articleID); err != nil {
		t.Fatalf("approve comment: %v", err)
	}
	mu.Lock()
	content = "<p>great post (edited)</p>"
	mu.Unlock()

	runFetch(t, f, `{"platform":"mastodon"}`)

	got := loadComments(t, database, articleID)
	if len(got) != 1 { // re-fetching never duplicates
		t.Fatalf("second run comments = %d, want 1", len(got))
	}
	if got[0].content != "<p>great post (edited)</p>" {
		t.Errorf("content = %q, want updated", got[0].content)
	}
	if got[0].status != 1 { // ... but never overwrites the moderation decision
		t.Errorf("status = %d, want approved(1) preserved", got[0].status)
	}
}
