package newsletter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	"rables/internal/service/activity"
	commentsvc "rables/internal/service/comments"
)

// Sender delivers one rendered message; *Mailer satisfies it via Send. The
// send handlers depend on the interface so tests can capture mail without an
// SMTP server.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// SendConfig customizes RegisterSendHandlers; the zero value is the
// production wiring. It exists so tests can swap the SMTP sender and point
// the listmonk client at an httptest server.
type SendConfig struct {
	// RoutePrefix is the public article route prefix; empty falls back to
	// the ARTICLE_ROUTE_PREFIX environment variable, like config.Load.
	RoutePrefix string
	// NewSender builds the SMTP sender for a resolved config; nil uses
	// NewMailer.
	NewSender func(SMTPConfig) Sender
	// HTTPClient overrides the listmonk API transport; nil keeps the default
	// timeouts of ListmonkClient.
	HTTPClient *http.Client
}

func (c SendConfig) withDefaults() SendConfig {
	if c.RoutePrefix == "" {
		c.RoutePrefix = os.Getenv("ARTICLE_ROUTE_PREFIX")
	}
	if c.NewSender == nil {
		c.NewSender = func(sc SMTPConfig) Sender { return NewMailer(sc) }
	}
	return c
}

// sendNewsletterPayload is the kind=send_newsletter contract, shared with the
// publish/admin enqueue paths: {"article_id": <id>}.
type sendNewsletterPayload struct {
	ArticleID int64 `json:"article_id"`
}

type confirmationPayload struct {
	SubscriberID int64 `json:"subscriber_id"`
}

type replyNotificationPayload struct {
	CommentID int64 `json:"comment_id"`
}

// RegisterSendHandlers installs the mail job handlers (plan T19; Rails
// NativeNewsletterSenderJob / ListmonkSenderJob / NewsletterConfirmationJob /
// CommentReplyNotificationJob / PasswordResetJob). dataDir is reserved for
// future inline attachments; the Rails mailers send none.
func RegisterSendHandlers(w *jobs.Worker, db *sql.DB, dataDir string, cfgs ...SendConfig) {
	cfg := SendConfig{}.withDefaults()
	if len(cfgs) > 0 {
		cfg = cfgs[0].withDefaults()
	}
	s := &sender{db: db, q: query.New(db), cfg: cfg}
	w.Register(jobs.KindSendNewsletter, s.sendNewsletter)
	w.Register(jobs.KindNewsletterConfirmation, s.sendConfirmation)
	w.Register(jobs.KindCommentReplyNotification, s.sendReplyNotification)
	// PasswordResetJob has no producer in the Go rewrite (the forgot-password
	// UI was dropped; decision recorded under T06), so the kind is a no-op:
	// imported or legacy job_runs rows drain quietly instead of failing as
	// an unknown kind.
	w.Register(jobs.KindPasswordReset, func(context.Context, json.RawMessage) error { return nil })
}

// sender carries the shared state of the mail job handlers.
type sender struct {
	db  *sql.DB
	q   *query.Queries
	cfg SendConfig
}

// sendNewsletter dispatches kind=send_newsletter at run time on the current
// newsletter_settings provider (plan decision 2026-08-03: one kind replaces
// the two Rails job classes).
func (s *sender) sendNewsletter(ctx context.Context, raw json.RawMessage) error {
	var p sendNewsletterPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode send_newsletter payload: %w", err)
	}
	st, err := s.newsletterSetting(ctx)
	if err != nil {
		return err
	}
	switch st.Provider {
	case "native":
		return s.sendNative(ctx, p.ArticleID, st)
	case "listmonk":
		return s.sendListmonk(ctx, p.ArticleID)
	default:
		// Article#handle_newsletter only logs an unknown provider.
		slog.Default().Warn("unknown newsletter provider", "provider", st.Provider)
		return nil
	}
}

// sendConfirmation mirrors NewsletterConfirmationJob: the confirmation mail
// goes out regardless of the enabled flag (the job has no guard); a missing
// subscriber is notified and dropped.
func (s *sender) sendConfirmation(ctx context.Context, raw json.RawMessage) error {
	var p confirmationPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode newsletter_confirmation payload: %w", err)
	}
	sub, err := s.q.GetSubscriberByID(ctx, p.SubscriberID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // the job rescues RecordNotFound and only notifies
	}
	if err != nil {
		return fmt.Errorf("load subscriber %d: %w", p.SubscriberID, err)
	}
	st, err := s.newsletterSetting(ctx)
	if err != nil {
		return err
	}
	siteTitle, rawURL, err := s.siteInfo(ctx)
	if err != nil {
		return err
	}
	htmlBody, textBody, err := RenderConfirmationEmail(ConfirmationEmailData{
		SiteTitle:       siteTitle,
		ConfirmationURL: confirmURL(tokenBaseURL(rawURL), sub.ConfirmationToken.String),
	})
	if err != nil {
		return fmt.Errorf("render confirmation email: %w", err)
	}
	return s.cfg.NewSender(ConfigFromSetting(st)).Send(ctx, Message{
		To:      sub.Email,
		From:    fromEmail(st),
		Subject: "请确认您的订阅 | " + siteTitle,
		HTML:    htmlBody,
		Text:    textBody,
	})
}

// sendReplyNotification mirrors CommentReplyNotificationJob: the parent
// comment's author is mailed about an approved local reply.
func (s *sender) sendReplyNotification(ctx context.Context, raw json.RawMessage) error {
	var p replyNotificationPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode comment_reply_notification payload: %w", err)
	}
	comment, err := s.q.GetCommentByID(ctx, p.CommentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // find_by + early return
	}
	if err != nil {
		return fmt.Errorf("load comment %d: %w", p.CommentID, err)
	}
	parent, eligible, err := s.eligibleReply(ctx, comment)
	if err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	st, err := s.newsletterSetting(ctx)
	if err != nil {
		return err
	}
	// return unless enabled? && native? && configured?
	if st.Enabled == 0 || st.Provider != "native" || !settingConfigured(st, false) {
		return nil
	}
	siteTitle, rawURL, err := s.siteInfo(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(siteTitle) == "" {
		siteTitle = "Site" // @site_info[:title].presence || "Site"
	}
	commentableTitle, commentableURL, err := s.commentableRef(ctx, comment, parent, siteURL(rawURL))
	if err != nil {
		return err
	}
	htmlBody, textBody, err := RenderReplyNotificationEmail(ReplyNotificationEmailData{
		ReplyAuthor:      comment.AuthorName,
		ReplyContent:     comment.Content,
		ParentContent:    parent.Content,
		CommentableTitle: commentableTitle,
		CommentableURL:   commentableURL,
		SiteTitle:        siteTitle,
	})
	if err != nil {
		return fmt.Errorf("render reply notification email: %w", err)
	}
	msg := Message{
		To:      parent.AuthorEmail.String,
		From:    fromEmail(st),
		Subject: "New reply to your comment | " + siteTitle,
		HTML:    htmlBody,
		Text:    textBody,
	}
	if err := s.cfg.NewSender(ConfigFromSetting(st)).Send(ctx, msg); err != nil {
		// log_failure of the job's rescue, minus the event notifications.
		activity.Log(ctx, s.db, "error", "failed", "comment_reply_notification", fmt.Sprintf(
			"email=%s error=%s comment_id=%d",
			activity.Quote(parent.AuthorEmail.String), activity.Quote(err.Error()), comment.ID))
		return fmt.Errorf("deliver reply notification for comment %d: %w", comment.ID, err)
	}
	activity.Log(ctx, s.db, "info", "sent", "comment_reply_notification", fmt.Sprintf(
		"email=%s author=%s comment_id=%d",
		activity.Quote(parent.AuthorEmail.String), activity.Quote(comment.AuthorName), comment.ID))
	return nil
}

// eligibleReply mirrors eligible_for_notification?; the returned parent is
// the notification recipient.
func (s *sender) eligibleReply(ctx context.Context, c query.Comment) (query.Comment, bool, error) {
	if c.Status != int64(domain.CommentApproved) || !c.ParentID.Valid || c.Platform.Valid {
		return query.Comment{}, false, nil
	}
	parent, err := s.q.GetCommentByID(ctx, c.ParentID.Int64)
	if errors.Is(err, sql.ErrNoRows) {
		return query.Comment{}, false, nil // parent&.author_email is nil
	}
	if err != nil {
		return query.Comment{}, false, fmt.Errorf("load parent comment %d: %w", c.ParentID.Int64, err)
	}
	if domain.IsBlank(parent.AuthorEmail.String) || parent.Platform.Valid {
		return query.Comment{}, false, nil
	}
	// A reply by the same address (case-insensitive) does not notify.
	if !domain.IsBlank(c.AuthorEmail.String) && strings.EqualFold(c.AuthorEmail.String, parent.AuthorEmail.String) {
		return query.Comment{}, false, nil
	}
	return parent, true, nil
}

// commentableRef mirrors Comment#display_commentable plus the mailer's
// commentable_path/url: the comment's own commentable, then the parent's,
// then the legacy article association. base is the normalized site URL; when
// blank the relative path is used, like the Rails view.
func (s *sender) commentableRef(ctx context.Context, c query.Comment, parent query.Comment, base string) (title, refURL string, err error) {
	title, path, ok, err := s.resolveCommentable(ctx, c.CommentableType.String, c.CommentableID)
	if err != nil {
		return "", "", err
	}
	if !ok {
		title, path, ok, err = s.resolveCommentable(ctx, parent.CommentableType.String, parent.CommentableID)
		if err != nil {
			return "", "", err
		}
	}
	if !ok && c.ArticleID.Valid {
		title, path, ok, err = s.resolveCommentable(ctx, "Article", c.ArticleID)
		if err != nil {
			return "", "", err
		}
	}
	if !ok {
		return "", "", nil
	}
	return title, base + path, nil
}

func (s *sender) resolveCommentable(ctx context.Context, commentableType string, id sql.NullInt64) (title, path string, ok bool, err error) {
	if !id.Valid {
		return "", "", false, nil
	}
	switch commentableType {
	case "Article":
		a, err := s.q.GetCommentableArticleByID(ctx, id.Int64)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		if err != nil {
			return "", "", false, fmt.Errorf("load article %d: %w", id.Int64, err)
		}
		return a.Title.String, commentsvc.ArticlePath(s.cfg.RoutePrefix, a.Slug.String), true, nil
	case "Page":
		p, err := s.q.GetCommentablePageByID(ctx, id.Int64)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		if err != nil {
			return "", "", false, fmt.Errorf("load page %d: %w", id.Int64, err)
		}
		return p.Title.String, commentsvc.PagePath(p.Slug.String), true, nil
	}
	return "", "", false, nil
}

// newsletterSetting loads the singleton row, falling back to the column
// defaults (disabled, native) when no row exists: NewsletterSetting.instance
// is first_or_initialize, not first_or_create.
func (s *sender) newsletterSetting(ctx context.Context) (query.NewsletterSetting, error) {
	st, err := s.q.GetNewsletterSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return query.NewsletterSetting{Provider: "native"}, nil
	}
	if err != nil {
		return query.NewsletterSetting{}, fmt.Errorf("load newsletter settings: %w", err)
	}
	return st, nil
}

// siteInfo mirrors CacheableSettings.site_info for the two fields the
// mailers use; an absent settings row yields blanks, like the Rails {}
// fallback.
func (s *sender) siteInfo(ctx context.Context) (title, rawURL string, err error) {
	row, err := s.q.GetSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("load settings: %w", err)
	}
	return row.Title.String, row.Url.String, nil
}

// settingConfigured mirrors NewsletterSetting#configured?.
func settingConfigured(st query.NewsletterSetting, listmonkConfigured bool) bool {
	if st.Enabled == 0 {
		return false
	}
	if st.Provider == "native" {
		return !domain.IsBlank(st.SmtpAddress.String) && st.SmtpPort.Valid &&
			!domain.IsBlank(st.SmtpUserName.String) && !domain.IsBlank(st.SmtpPassword.String) &&
			!domain.IsBlank(st.FromEmail.String)
	}
	return listmonkConfigured
}

// siteURL mirrors normalized_site_url: trimmed, one trailing slash chomped,
// https:// prepended when schemeless, "" when unset.
func siteURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	u = strings.TrimSuffix(u, "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	return u
}

// tokenBaseURL mirrors the site-URL fallback branch of
// ApplicationHelper#rails_api_url, which builds the confirm/unsubscribe
// links: the RAILS_API_URL override is not ported (the Go config has no such
// field), so this is settings.url with an http:// scheme default and the
// localhost:3000 fallback when unset.
func tokenBaseURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		u = "http://localhost:3000"
	}
	u = strings.TrimSuffix(u, "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	return u
}

// confirmURL is confirm_subscription_url(token:) — GET /confirm?token=.
func confirmURL(base, token string) string {
	return base + "/confirm?token=" + url.QueryEscape(token)
}

// unsubscribeURL is unsubscribe_url(token:) — GET /unsubscribe?token=.
func unsubscribeURL(base, token string) string {
	return base + "/unsubscribe?token=" + url.QueryEscape(token)
}

// fromEmail mirrors NewsletterMailer#resolved_from_email.
func fromEmail(st query.NewsletterSetting) string {
	if !domain.IsBlank(st.FromEmail.String) {
		return st.FromEmail.String
	}
	return "noreply@example.com"
}
