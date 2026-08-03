package newsletter

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"time"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/service/activity"
)

// sendListmonk mirrors ListmonkSenderJob#perform + Listmonk#send_newsletter:
// create the campaign, then flip it to running. The Rails model rescues and
// activity-logs every API failure (send_newsletter returns false instead of
// raising), and the job rescues RecordNotFound, so neither case fails the
// job here.
func (s *sender) sendListmonk(ctx context.Context, articleID int64) error {
	article, err := s.q.GetArticleByID(ctx, articleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // rescued RecordNotFound
	}
	if err != nil {
		return fmt.Errorf("load article %d: %w", articleID, err)
	}
	lm, err := s.q.GetListmonk(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // Listmonk.first is blank
	}
	if err != nil {
		return fmt.Errorf("load listmonk config: %w", err)
	}
	// return unless listmonk.present? && list_id.present? && template_id.present?
	if !lm.ListID.Valid || !lm.TemplateID.Valid {
		return nil
	}

	title := article.Title.String
	activity.Log(ctx, s.db, "info", "started", "newsletter", fmt.Sprintf(
		"title=%s slug=%s mode=%s",
		activity.Quote(title), activity.Quote(article.Slug.String), activity.Quote("listmonk")))

	siteTitle, _, err := s.siteInfo(ctx)
	if err != nil {
		return err
	}
	client := ListmonkClient{
		URL:        lm.Url.String,
		Username:   lm.Username.String,
		APIKey:     lm.ApiKey.String,
		HTTPClient: s.cfg.HTTPClient,
	}

	campaignID, err := client.createCampaign(ctx, article, lm, siteTitle)
	if err != nil {
		activity.Log(ctx, s.db, "error", "failed", "newsletter", fmt.Sprintf(
			"title=%s operation=%s error=%s",
			activity.Quote(title), activity.Quote("campaign"), activity.Quote(err.Error())))
		return nil
	}
	activity.Log(ctx, s.db, "info", "created", "newsletter", fmt.Sprintf(
		"title=%s operation=%s campaign_id=%d",
		activity.Quote(title), activity.Quote("campaign"), campaignID))

	if err := client.setCampaignRunning(ctx, campaignID); err != nil {
		activity.Log(ctx, s.db, "error", "failed", "newsletter", fmt.Sprintf(
			"title=%s operation=%s campaign_id=%d error=%s",
			activity.Quote(title), activity.Quote("campaign_send"), campaignID, activity.Quote(err.Error())))
		return nil
	}
	activity.Log(ctx, s.db, "info", "sent", "newsletter", fmt.Sprintf(
		"title=%s operation=%s campaign_id=%d",
		activity.Quote(title), activity.Quote("campaign"), campaignID))
	return nil
}

// campaignRequest is the POST /api/campaigns JSON body of
// Listmonk#create_campaigns.
type campaignRequest struct {
	Name        string  `json:"name"`
	Subject     string  `json:"subject"`
	Lists       []int64 `json:"lists"`
	Type        string  `json:"type"`
	ContentType string  `json:"content_type"`
	Messenger   string  `json:"messenger"`
	Body        string  `json:"body"`
	TemplateID  int64   `json:"template_id"`
	SendLater   bool    `json:"send_later"`
}

// createCampaign mirrors Listmonk#create_campaigns, returning the new
// campaign id; the error keeps the Rails "Create Campaign failed!" text.
func (c ListmonkClient) createCampaign(ctx context.Context, article query.Article, lm query.Listmonk, siteTitle string) (int64, error) {
	reqBody := campaignRequest{
		Name:        article.Title.String,
		Subject:     article.Title.String + " | " + siteTitle,
		Lists:       []int64{lm.ListID.Int64},
		Type:        "regular",
		ContentType: "html",
		Messenger:   "email",
		Body:        campaignBody(article),
		TemplateID:  lm.TemplateID.Int64,
		SendLater:   false,
	}
	var resp struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	err := c.doJSON(ctx, http.MethodPost, c.URL+"/api/campaigns", reqBody, &resp)
	if err == nil && resp.Data.ID == 0 {
		err = errors.New("200 - missing data.id")
	}
	if err != nil {
		return 0, fmt.Errorf("Create Campaign failed! Title:%s,Code:%s", article.Title.String, err)
	}
	return resp.Data.ID, nil
}

// setCampaignRunning mirrors the status update of Listmonk#send_newsletter.
// The source uses a PUT (the plan's PATCH mention defers to listmonk.rb).
func (c ListmonkClient) setCampaignRunning(ctx context.Context, campaignID int64) error {
	err := c.doJSON(ctx, http.MethodPut,
		fmt.Sprintf("%s/api/campaigns/%d/status", c.URL, campaignID),
		map[string]string{"status": "running"}, nil)
	if err != nil {
		return fmt.Errorf("Send Campaign failed! %s", err)
	}
	return nil
}

// doJSON performs one authenticated JSON API call, mirroring the Net::HTTP
// requests of the model (basic auth, JSON content type, 5s dial / 10s read
// timeouts); non-2xx answers carry the "CODE - BODY" text of the Rails
// raise. It duplicates the transport of verify.go's get, which is GET-only.
func (c ListmonkClient) doJSON(ctx context.Context, method, rawURL string, payload, out any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(payload); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, &buf)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Username, c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 15 * time.Second, // whole request, including the body read
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var snippet [512]byte
		n, _ := resp.Body.Read(snippet[:])
		return fmt.Errorf("%d - %s", resp.StatusCode, string(snippet[:n]))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// campaignBody mirrors Listmonk#campaign_body: the stored html with the
// source-reference partial prepended when the article has a source.
func campaignBody(article query.Article) string {
	body := article.ContentHtml.String
	if domain.IsBlank(article.SourceUrl.String) { // !Article#has_source?
		return body
	}
	return renderSourceReference(article) + "\n" + body
}

// renderSourceReference ports articles/_source_reference.html.erb for the
// campaign body (the Rails model renders it via
// ApplicationController.renderer). Called only when has_source?, so the
// blockquote branch is always present.
func renderSourceReference(article query.Article) string {
	var b strings.Builder
	b.WriteString(`<div style="display: flex; align-items: flex-start; gap: 0.75rem; margin-bottom: 0.75rem;">`)
	b.WriteString(`<i class="fas fa-quote-left" style="color: #6c757d; font-size: 1.25rem; margin-top: 0.125rem; opacity: 0.6;"></i>`)
	b.WriteString(`<div style="flex: 1;">`)
	if !domain.IsBlank(article.SourceAuthor.String) {
		b.WriteString(`<span style="font-weight: 600; color: #495057; font-size: 0.95rem;">`)
		b.WriteString(html.EscapeString(article.SourceAuthor.String))
		b.WriteString(`</span>`)
	}
	b.WriteString(`</div></div>`)
	b.WriteString(`<blockquote class="source-reference__quote">`)
	if !domain.IsBlank(article.SourceContent.String) {
		b.WriteString(string(simpleFormat(article.SourceContent.String, "span")))
	}
	b.WriteString(`<div class="source-reference__links" style="display: flex; flex-wrap: wrap; gap: 0.75rem; font-size: 0.85rem;">`)
	b.WriteString(`<a href="` + html.EscapeString(article.SourceUrl.String) + `" target="_blank" rel="noopener noreferrer" style="color: #007bff; text-decoration: none; display: inline-flex; align-items: center; gap: 0.375rem; transition: color 0.2s;">`)
	b.WriteString(`<i class="fas fa-external-link-alt" style="font-size: 0.75rem;"></i>`)
	b.WriteString(`<small>Original</small>`)
	b.WriteString(`</a></div></blockquote>`)
	return b.String()
}
