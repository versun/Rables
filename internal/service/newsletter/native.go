package newsletter

import (
	"context"
	"fmt"
	"html/template"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/service/activity"
	commentsvc "rables/internal/service/comments"
	subscribersvc "rables/internal/service/subscribers"
)

// sendNative mirrors NativeNewsletterSenderJob#perform: the native provider
// mails each relevant subscriber individually; a single failure is logged
// and counted but never aborts the batch.
func (s *sender) sendNative(ctx context.Context, articleID int64, st query.NewsletterSetting) error {
	article, err := s.q.GetArticleByID(ctx, articleID)
	if err != nil {
		// NativeNewsletterSenderJob does not rescue RecordNotFound: the job
		// fails, so a missing article is an error here as well.
		return fmt.Errorf("load article %d: %w", articleID, err)
	}
	// return unless enabled? && native? && configured?
	if st.Enabled == 0 || st.Provider != "native" || !settingConfigured(st, false) {
		return nil
	}

	recipients, tokens, err := s.recipients(ctx)
	if err != nil {
		return err
	}
	activeCount := 0
	for _, r := range recipients {
		if r.Active {
			activeCount++
		}
	}
	if activeCount == 0 {
		return nil // return if subscribers.empty?
	}

	tagIDs, err := s.articleTagIDs(ctx, articleID)
	if err != nil {
		return err
	}
	relevant := subscribersvc.FilterRelevant(recipients, tagIDs)
	if len(relevant) == 0 {
		return nil
	}

	title := article.Title.String
	slug := article.Slug.String
	activity.Log(ctx, s.db, "info", "started", "newsletter", fmt.Sprintf(
		"title=%s slug=%s mode=%s subscriber_count=%d total_subscribers=%d",
		activity.Quote(title), activity.Quote(slug), activity.Quote("native"), len(relevant), activeCount))

	smtpCfg := ConfigFromSetting(st)
	if smtpCfg.Address == "" {
		activity.Log(ctx, s.db, "error", "failed", "newsletter", fmt.Sprintf(
			"title=%s slug=%s mode=%s error=%s",
			activity.Quote(title), activity.Quote(slug), activity.Quote("native"), activity.Quote("smtp_config_missing")))
		return nil
	}

	siteTitle, rawURL, err := s.siteInfo(ctx)
	if err != nil {
		return err
	}
	base := siteURL(rawURL)
	tokenBase := tokenBaseURL(rawURL)

	mailer := s.cfg.NewSender(smtpCfg)
	successCount, failCount := 0, 0
	for _, r := range relevant {
		msg, err := s.articleMessage(article, r, tokens[r.ID], siteTitle, base, tokenBase, st)
		if err == nil {
			err = mailer.Send(ctx, msg)
		}
		if err != nil {
			failCount++
			activity.Log(ctx, s.db, "error", "failed", "newsletter", fmt.Sprintf(
				"title=%s slug=%s email=%s mode=%s error=%s",
				activity.Quote(title), activity.Quote(slug), activity.Quote(r.Email),
				activity.Quote("native"), activity.Quote(err.Error())))
			continue
		}
		successCount++
	}
	activity.Log(ctx, s.db, "info", "completed", "newsletter", fmt.Sprintf(
		"title=%s slug=%s mode=%s success_count=%d error_count=%d",
		activity.Quote(title), activity.Quote(slug), activity.Quote("native"), successCount, failCount))
	return nil
}

// recipients loads every subscriber with its tag ids
// (Subscriber.includes(:tags)); the unsubscribe tokens come along keyed by
// subscriber id for the per-recipient mail.
func (s *sender) recipients(ctx context.Context) ([]subscribersvc.Recipient, map[int64]string, error) {
	subs, err := s.q.ListAllSubscribers(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list subscribers: %w", err)
	}
	links, err := s.q.ListAllSubscriberTags(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list subscriber tags: %w", err)
	}
	tagsBySub := map[int64][]int64{}
	for _, l := range links {
		tagsBySub[l.SubscriberID] = append(tagsBySub[l.SubscriberID], l.TagID)
	}
	recipients := make([]subscribersvc.Recipient, 0, len(subs))
	tokens := make(map[int64]string, len(subs))
	for _, sub := range subs {
		recipients = append(recipients, subscribersvc.Recipient{
			ID:     sub.ID,
			Email:  sub.Email,
			Active: subscribersvc.Active(sub),
			TagIDs: tagsBySub[sub.ID],
		})
		tokens[sub.ID] = sub.UnsubscribeToken.String
	}
	return recipients, tokens, nil
}

// articleTagIDs is article.tags.pluck(:id).
func (s *sender) articleTagIDs(ctx context.Context, articleID int64) ([]int64, error) {
	tags, err := s.q.ListTagsForArticle(ctx, articleID)
	if err != nil {
		return nil, fmt.Errorf("list article %d tags: %w", articleID, err)
	}
	ids := make([]int64, len(tags))
	for i, t := range tags {
		ids[i] = t.ID
	}
	return ids, nil
}

// articleMessage renders NewsletterMailer#article_email for one subscriber.
func (s *sender) articleMessage(article query.Article, r subscribersvc.Recipient, unsubscribeToken, siteTitle, base, tokenBase string, st query.NewsletterSetting) (Message, error) {
	htmlBody, textBody, err := RenderArticleEmail(ArticleEmailData{
		Title:         article.Title.String,
		Description:   article.Description.String,
		HasSource:     !domain.IsBlank(article.SourceUrl.String), // Article#has_source?
		SourceAuthor:  article.SourceAuthor.String,
		SourceContent: article.SourceContent.String,
		SourceURL:     article.SourceUrl.String,
		//nolint:gosec // sanitized at write time (plan section 4.4)
		ContentHTML:    template.HTML(article.ContentHtml.String),
		ContentText:    domain.PlainText(article.ContentHtml.String),
		ArticleURL:     base + commentsvc.ArticlePath(s.cfg.RoutePrefix, article.Slug.String),
		UnsubscribeURL: unsubscribeURL(tokenBase, unsubscribeToken),
	})
	if err != nil {
		return Message{}, fmt.Errorf("render article email: %w", err)
	}
	return Message{
		To:      r.Email,
		From:    fromEmail(st),
		Subject: article.Title.String + " | " + siteTitle,
		HTML:    htmlBody,
		Text:    textBody,
	}, nil
}
