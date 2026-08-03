// Package comments holds the comment domain logic shared by the public
// submission endpoint, the admin moderation UI and the social comment
// fetchers (T22): display tree building, model validations and the
// idempotent external upsert (plan section 4.5, mirroring comment.rb,
// comments_helper.rb and comments/_comment.html.erb).
package comments

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
)

// maxReplyDepth mirrors max_depth in comments/_comment.html.erb: the reply
// button/form stop appearing at this depth (the tree itself is unbounded).
const maxReplyDepth = 5

// externalPlatforms orders the platform groups after the local comments,
// mirroring CommentsHelper::EXTERNAL_COMMENT_PLATFORMS.
var externalPlatforms = []string{"mastodon", "bluesky", "twitter"}

// Threaded is one node of the display tree built by BuildTree.
type Threaded struct {
	Comment  query.Comment
	Type     string // "local" or the external platform name
	Depth    int
	Replies  []Threaded
	TimeZone string    // display zone for timestamps; empty falls back to UTC
	Form     *FormData // reply form context, attached by PrepareDisplay
}

// FormData feeds the "comment_form" partial.
type FormData struct {
	Action   string // /comments?article_id=<slug> or /comments?page_id=<slug>
	ParentID int64  // zero for the top-level form
	Question string
	Token    string
	A, B     int    // challenge operands for the client-side validator
	Op       string // challenge operator
}

// IsReply reports whether the form answers an existing comment.
func (f FormData) IsReply() bool { return f.ParentID != 0 }

// BuildTree turns the flat comment list of one commentable into the display
// tree, mirroring grouped_comment_items + comments/_comment.html.erb: top
// level shows approved local comments first, then external comments grouped
// by platform; the display type is inherited from the root, so descendants
// of a local node keep the approved-only filter and descendants of a
// platform node keep the same-platform filter; every level is ordered by
// published_at ascending. Orphaned subtrees (parent missing or filtered out)
// are dropped, matching what the Rails views render.
func BuildTree(list []query.Comment) []Threaded {
	sorted := sortByPublishedAt(list)

	var build func(parent query.Comment, dispType string, depth int, seen map[int64]bool) []Threaded
	build = func(parent query.Comment, dispType string, depth int, seen map[int64]bool) []Threaded {
		var out []Threaded
		for _, c := range sorted {
			if !c.ParentID.Valid || c.ParentID.Int64 != parent.ID || seen[c.ID] {
				continue
			}
			if dispType == "local" {
				// _comment.html.erb: local subtrees select approved replies.
				if c.Status != int64(domain.CommentApproved) {
					continue
				}
			} else if !c.Platform.Valid || c.Platform.String != dispType {
				continue
			}
			seen[c.ID] = true
			node := Threaded{Comment: c, Type: dispType, Depth: depth}
			node.Replies = build(c, dispType, depth+1, seen)
			out = append(out, node)
		}
		return out
	}

	var roots []Threaded
	appendRoots := func(dispType string, match func(query.Comment) bool) {
		for _, c := range sorted {
			if c.ParentID.Valid || !match(c) {
				continue
			}
			seen := map[int64]bool{c.ID: true}
			node := Threaded{Comment: c, Type: dispType}
			node.Replies = build(c, dispType, 1, seen)
			roots = append(roots, node)
		}
	}
	appendRoots("local", func(c query.Comment) bool {
		return !c.Platform.Valid && c.Status == int64(domain.CommentApproved)
	})
	for _, platform := range externalPlatforms {
		appendRoots(platform, func(c query.Comment) bool {
			return c.Platform.Valid && c.Platform.String == platform
		})
	}
	// Parentless comments of any other platform are not displayed, matching
	// grouped_comment_items' fixed platform list.
	return roots
}

// PrepareDisplay walks the tree attaching the reply-form context (local
// nodes below maxReplyDepth, each with its own ParentID) and the display
// time zone to every node. Page handlers call it before rendering the
// "comments_tree" partial.
func PrepareDisplay(nodes []Threaded, timeZone string, base FormData) {
	for i := range nodes {
		n := &nodes[i]
		n.TimeZone = timeZone
		if n.Type == "local" && n.Depth < maxReplyDepth {
			form := base
			form.ParentID = n.Comment.ID
			n.Form = &form
		}
		PrepareDisplay(n.Replies, timeZone, base)
	}
}

// VisibleCount mirrors visible_comments_count: approved local comments plus
// all external comments, replies included.
func VisibleCount(list []query.Comment) int {
	n := 0
	for _, c := range list {
		if visible(c) {
			n++
		}
	}
	return n
}

// AcceptsComments mirrors the CommentsController gate: only publish/shared
// content with comment=1 takes submissions.
func AcceptsComments(status, comment int64) bool {
	return comment == 1 &&
		(status == int64(domain.StatusPublish) || status == int64(domain.StatusShared))
}

// ArticlePath is the public article URL, honoring the ARTICLE_ROUTE_PREFIX
// scope of the Rails routes.
func ArticlePath(routePrefix, slug string) string {
	prefix := strings.Trim(routePrefix, "/")
	if prefix == "" {
		return "/" + slug
	}
	return "/" + prefix + "/" + slug
}

// PagePath is the public page URL (resources :pages, param: :slug).
func PagePath(slug string) string { return "/pages/" + slug }

// ValidateNew mirrors the Comment model validations for a comment about to
// be saved. parent must be the loaded ParentID record (nil when none or not
// found). It returns the violation messages (Rails full_message style),
// empty when valid.
func ValidateNew(c query.Comment, parent *query.Comment) []string {
	var msgs []string
	if strings.TrimSpace(c.AuthorName) == "" {
		msgs = append(msgs, "Author name can't be blank")
	}
	if strings.TrimSpace(c.Content) == "" {
		msgs = append(msgs, "Content can't be blank")
	}
	if c.AuthorUrl.Valid && c.AuthorUrl.String != "" && !validHTTPURL(c.AuthorUrl.String) {
		msgs = append(msgs, "Author url must be a valid URL")
	}
	if c.Url.Valid && c.Url.String != "" && !validHTTPURL(c.Url.String) {
		msgs = append(msgs, "Url must be a valid URL")
	}
	if c.AuthorEmail.Valid && c.AuthorEmail.String != "" && !validEmail(c.AuthorEmail.String) {
		msgs = append(msgs, "Author email must be a valid email")
	}
	if c.ParentID.Valid {
		switch {
		case parent == nil:
			msgs = append(msgs, "Parent does not exist")
		case parent.ID == c.ID:
			msgs = append(msgs, "Parent cannot reference itself")
		case parent.CommentableType.String != c.CommentableType.String ||
			parent.CommentableID.Int64 != c.CommentableID.Int64:
			msgs = append(msgs, fmt.Sprintf("Parent must belong to the same %s", c.CommentableType.String))
		}
	}
	return msgs
}

// UpsertResult is the outcome of UpsertExternal (:created/:updated/:unchanged).
type UpsertResult int

const (
	UpsertUnchanged UpsertResult = iota
	UpsertCreated
	UpsertUpdated
)

func (r UpsertResult) String() string {
	switch r {
	case UpsertCreated:
		return "created"
	case UpsertUpdated:
		return "updated"
	}
	return "unchanged"
}

// ExternalData is the re-fetched platform payload for one external comment.
type ExternalData struct {
	ExternalID      string
	AuthorName      string
	AuthorUsername  string
	AuthorAvatarURL string
	Content         string
	URL             string
	PublishedAt     int64 // unix seconds; zero stores NULL
}

// UpsertExternal mirrors Comment.upsert_from_external: keyed on
// (commentable, platform, external_id); status applies only to newly created
// rows so re-fetching never overwrites a moderation decision. Content fields
// are re-assigned and saved only when something actually changed.
func UpsertExternal(ctx context.Context, q *query.Queries, commentableType string, commentableID int64, platform string, data ExternalData, status *domain.CommentStatus) (query.Comment, UpsertResult, error) {
	existing, err := q.GetExternalComment(ctx, query.GetExternalCommentParams{
		CommentableType: nullString(commentableType),
		CommentableID:   nullInt64(commentableID),
		Platform:        nullString(platform),
		ExternalID:      nullString(data.ExternalID),
	})
	now := time.Now().UTC().Unix()
	publishedAt := sql.NullInt64{Int64: data.PublishedAt, Valid: data.PublishedAt != 0}

	if errors.Is(err, sql.ErrNoRows) {
		st := domain.CommentPending
		if status != nil {
			st = *status
		}
		c, err := q.CreateComment(ctx, query.CreateCommentParams{
			CommentableType: nullString(commentableType),
			CommentableID:   nullInt64(commentableID),
			AuthorName:      data.AuthorName,
			AuthorUsername:  nullString(data.AuthorUsername),
			AuthorAvatarUrl: nullString(data.AuthorAvatarURL),
			Content:         data.Content,
			Status:          int64(st),
			Platform:        nullString(platform),
			ExternalID:      nullString(data.ExternalID),
			Url:             nullString(data.URL),
			PublishedAt:     publishedAt,
			CreatedAt:       now,
			UpdatedAt:       now,
		})
		if err != nil {
			return query.Comment{}, UpsertUnchanged, fmt.Errorf("create external comment: %w", err)
		}
		return c, UpsertCreated, nil
	}
	if err != nil {
		return query.Comment{}, UpsertUnchanged, fmt.Errorf("find external comment: %w", err)
	}

	if existing.AuthorName == data.AuthorName &&
		existing.AuthorUsername.String == data.AuthorUsername &&
		existing.AuthorAvatarUrl.String == data.AuthorAvatarURL &&
		existing.Content == data.Content &&
		existing.Url.String == data.URL &&
		existing.PublishedAt == publishedAt {
		return existing, UpsertUnchanged, nil
	}
	c, err := q.UpdateExternalComment(ctx, query.UpdateExternalCommentParams{
		AuthorName:      data.AuthorName,
		AuthorUsername:  nullString(data.AuthorUsername),
		AuthorAvatarUrl: nullString(data.AuthorAvatarURL),
		Content:         data.Content,
		PublishedAt:     publishedAt,
		Url:             nullString(data.URL),
		UpdatedAt:       now,
		ID:              existing.ID,
	})
	if err != nil {
		return query.Comment{}, UpsertUnchanged, fmt.Errorf("update external comment: %w", err)
	}
	return c, UpsertUpdated, nil
}

// EnqueueReplyNotification mirrors Comment#enqueue_reply_notification: queue
// comment_reply_notification when an approved local reply's parent left an
// email address (and the reply author is someone else). The payload is
// {"comment_id": <reply id>}, consumed by the mail jobs (T18/T19). Call it
// only when the comment transitioned to approved.
func EnqueueReplyNotification(ctx context.Context, q *query.Queries, enq *jobs.Enqueuer, c query.Comment) (bool, error) {
	if c.Status != int64(domain.CommentApproved) || !c.ParentID.Valid || c.Platform.Valid {
		return false, nil
	}
	parent, err := q.GetCommentByID(ctx, c.ParentID.Int64)
	if err != nil {
		return false, nil // parent gone: nothing to notify
	}
	if parent.Platform.Valid || strings.TrimSpace(parent.AuthorEmail.String) == "" {
		return false, nil
	}
	if strings.TrimSpace(c.AuthorEmail.String) != "" &&
		strings.EqualFold(c.AuthorEmail.String, parent.AuthorEmail.String) {
		return false, nil
	}
	_, err = enq.Enqueue(ctx, jobs.KindCommentReplyNotification,
		map[string]int64{"comment_id": c.ID}, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("enqueue reply notification: %w", err)
	}
	return true, nil
}

// visible mirrors the display filter: external comments always show, local
// comments only once approved.
func visible(c query.Comment) bool {
	return c.Platform.Valid || c.Status == int64(domain.CommentApproved)
}

// sortByPublishedAt mirrors default_scope order(published_at: :asc); SQLite
// sorts NULLs first in ASC order.
func sortByPublishedAt(list []query.Comment) []query.Comment {
	sorted := make([]query.Comment, len(list))
	copy(sorted, list)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].PublishedAt, sorted[j].PublishedAt
		if a.Valid != b.Valid {
			return !a.Valid
		}
		return a.Int64 < b.Int64
	})
	return sorted
}

// validHTTPURL mirrors the author_url/url format validation (http/https).
func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// emailRE approximates URI::MailTo::EMAIL_REGEXP (simplified).
var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func validEmail(s string) bool { return emailRE.MatchString(s) }

// commentPolicy mirrors the sanitize call in comments/_comment.html.erb:
// tags p br a span, attributes href class rel target.
var commentPolicy = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "br", "a", "span")
	p.AllowAttrs("href", "class", "rel", "target").OnElements("a")
	p.AllowAttrs("class").OnElements("p", "span")
	p.AllowURLSchemes("http", "https", "mailto")
	return p
}()

// SanitizeComment renders comment content safe for display.
func SanitizeComment(content string) template.HTML {
	return template.HTML(commentPolicy.Sanitize(content)) //nolint:gosec // sanitized above
}

// ContentHTML returns the sanitized comment body for templates.
func (t Threaded) ContentHTML() template.HTML { return SanitizeComment(t.Comment.Content) }

// PublishedUnix is the display timestamp (published_at, else created_at).
func (t Threaded) PublishedUnix() int64 {
	if t.Comment.PublishedAt.Valid {
		return t.Comment.PublishedAt.Int64
	}
	return t.Comment.CreatedAt
}

// Initial is the avatar fallback letter (author_name.to_s.first.upcase).
func (t Threaded) Initial() string {
	r, _ := utf8.DecodeRuneInString(t.Comment.AuthorName)
	return string(unicode.ToUpper(r))
}

// BorderColor is the per-platform accent color of the Rails partial.
func (t Threaded) BorderColor() string {
	switch t.Type {
	case "mastodon":
		return "#6364FF"
	case "bluesky":
		return "#0085FF"
	case "twitter":
		return "#1DA1F2"
	}
	return "#333"
}

// PlatformTitle is the titleized platform name (Mastodon, Bluesky, ...).
func (t Threaded) PlatformTitle() string {
	if t.Type == "" {
		return ""
	}
	return strings.ToUpper(t.Type[:1]) + t.Type[1:]
}

// CanReply mirrors the partial: reply affordances only on local comments
// below maxReplyDepth.
func (t Threaded) CanReply() bool { return t.Type == "local" && t.Depth < maxReplyDepth }

// IndentRem is the reply indentation (depth * 0.75rem).
func (t Threaded) IndentRem() string { return fmt.Sprintf("%g", float64(t.Depth)*0.75) }

// FormIndentRem is the reply-form left margin ((depth+1) * 0.75 + 0.25rem).
func (t Threaded) FormIndentRem() string { return fmt.Sprintf("%g", float64(t.Depth+1)*0.75+0.25) }

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullInt64(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: true} }
