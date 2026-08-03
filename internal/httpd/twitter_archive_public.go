package httpd

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/db/query"
	"rables/internal/service/twitterarchive"
	"rables/internal/templates"
)

// twitterArchivePerPage mirrors TwitterArchivesController::PER_PAGE.
const twitterArchivePerPage = 20

// RegisterTwitterArchivePublicRoutes mounts the public archive page,
// mirroring the twitter_archive route: GET /twitter/archive, or
// GET /{prefix}/twitter/archive when ARTICLE_ROUTE_PREFIX is set. The
// integrator must wire it before RegisterArticleRoutes so the static path
// beats the /{slug} catch-all.
func RegisterTwitterArchivePublicRoutes(r chi.Router, s *Server) {
	r.Get(twitterArchivePublicPath(s.Cfg.ArticleRoutePrefix), s.publicTwitterArchiveShow)
}

// twitterArchiveMediaItem is one attached media file of a tweet card.
type twitterArchiveMediaItem struct {
	URL      string
	Filename string
	Kind     string // "image", "video" or "file" (blob.image?/video? equivalents)
}

// twitterArchiveTweetItem feeds one tweet card of the public archive.
type twitterArchiveTweetItem struct {
	DateISO       string // tweeted_at.iso8601
	DateLong      string // tweeted_at.to_fs(:long)
	HasScreenName bool
	ScreenName    string
	BodyHTML      template.HTML // simple_format(h(full_text))
	Media         []twitterArchiveMediaItem
	TweetURL      string // safe_archive_url-checked; empty when unusable
}

// twitterArchiveLikeItem feeds one liked-tweet card.
type twitterArchiveLikeItem struct {
	TweetID     string
	BodyHTML    template.HTML
	HasText     bool
	ExpandedURL string // safe_archive_url-checked; empty when unusable
}

// twitterArchiveTabLink is one entry of the tab navigation.
type twitterArchiveTabLink struct {
	URL    string
	Label  string
	Active bool
}

// publicTwitterArchiveData feeds public_twitter_archive.html.
type publicTwitterArchiveData struct {
	Flash          templates.Flash
	Chrome         siteChrome
	Tabs           []twitterArchiveTabLink
	ActiveTab      string
	IsLikeTab      bool
	LastUploadLong string // empty when the archive was never imported
	Tweets         []twitterArchiveTweetItem
	Likes          []twitterArchiveLikeItem
	Page           pagination
}

// publicTwitterArchiveShow renders GET /[prefix/]twitter/archive, mirroring
// TwitterArchivesController#show: four tabs, 20 entries per page.
func (s *Server) publicTwitterArchiveShow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tab := twitterarchive.TabFor(r.URL.Query().Get("tab"))
	page, ok := parseStrictPage(r.URL.Query())
	if !ok {
		s.publicNotFound(w)
		return
	}
	chrome, err := s.chrome(ctx, "")
	if err != nil {
		s.listError(w, "load site settings", err)
		return
	}

	data := publicTwitterArchiveData{
		Flash:     PopFlash(r, w),
		Chrome:    chrome,
		ActiveTab: tab,
		IsLikeTab: tab == "like",
	}
	basePath := twitterArchivePublicPath(s.Cfg.ArticleRoutePrefix)
	for _, t := range twitterarchive.Tabs {
		link := twitterArchiveTabLink{Label: t.Label, Active: t.Key == tab}
		if t.Key == twitterarchive.EntryTypeTweet {
			link.URL = basePath // the default tab carries no ?tab= param
		} else {
			link.URL = basePath + "?tab=" + t.Key
		}
		data.Tabs = append(data.Tabs, link)
	}
	if ts, ok := twitterarchive.LastImportedAt(ctx, s.Q); ok {
		data.LastUploadLong = formatArchiveTime(ts, chrome.TimeZone)
	}

	var total int64
	offset := (page - 1) * twitterArchivePerPage
	if twitterarchive.IsTweetTab(tab) {
		rows, err := s.Q.ListTwitterArchiveTweetsByType(ctx, query.ListTwitterArchiveTweetsByTypeParams{
			EntryType: tab,
			Limit:     twitterArchivePerPage,
			Offset:    offset,
		})
		if err != nil {
			s.listError(w, "list twitter archive tweets", err)
			return
		}
		if total, err = s.Q.CountTwitterArchiveTweetsOfType(ctx, tab); err != nil {
			s.listError(w, "count twitter archive tweets", err)
			return
		}
		for _, row := range rows {
			item, err := s.twitterArchiveTweetItem(ctx, row, chrome.TimeZone)
			if err != nil {
				s.listError(w, "list twitter archive media", err)
				return
			}
			data.Tweets = append(data.Tweets, item)
		}
	} else {
		rows, err := s.Q.ListTwitterArchiveLikes(ctx, query.ListTwitterArchiveLikesParams{
			Limit:  twitterArchivePerPage,
			Offset: offset,
		})
		if err != nil {
			s.listError(w, "list twitter archive likes", err)
			return
		}
		if total, err = s.Q.CountTwitterArchiveLikes(ctx); err != nil {
			s.listError(w, "count twitter archive likes", err)
			return
		}
		for _, row := range rows {
			data.Likes = append(data.Likes, twitterArchiveLikeItem{
				TweetID:     row.TweetID,
				BodyHTML:    simpleFormat(row.FullText.String),
				HasText:     row.FullText.Valid && row.FullText.String != "",
				ExpandedURL: safeArchiveURL(row.ExpandedUrl.String),
			})
		}
	}
	data.Page = buildPagination(page, total, twitterArchivePerPage, pageURLFunc(r))
	s.render(w, http.StatusOK, "public_twitter_archive", data)
}

// twitterArchiveTweetItem assembles one tweet card with its media
// attachments (has_many_attached :media).
func (s *Server) twitterArchiveTweetItem(ctx context.Context, row query.TwitterArchiveTweet, tz string) (twitterArchiveTweetItem, error) {
	loc := tzLocation(tz)
	tweetedAt := time.Unix(row.TweetedAt, 0).In(loc)
	item := twitterArchiveTweetItem{
		DateISO:       tweetedAt.Format("2006-01-02T15:04:05Z07:00"),
		DateLong:      formatArchiveTime(row.TweetedAt, tz),
		HasScreenName: row.ScreenName != "",
		ScreenName:    row.ScreenName,
		BodyHTML:      simpleFormat(row.FullText),
		TweetURL:      safeArchiveURL(twitterarchive.TweetURL(row)),
	}
	media, err := s.Q.ListTwitterArchiveTweetMedia(ctx, row.ID)
	if err != nil {
		return item, err
	}
	for _, m := range media {
		kind := "file"
		ct := m.ContentType.String
		if len(ct) >= 6 && ct[:6] == "image/" {
			kind = "image"
		} else if len(ct) >= 6 && ct[:6] == "video/" {
			kind = "video"
		}
		item.Media = append(item.Media, twitterArchiveMediaItem{
			URL:      "/files/" + m.Key,
			Filename: m.Filename,
			Kind:     kind,
		})
	}
	return item, nil
}

// formatArchiveTime renders the :long-ish timestamp used by the archive
// views ("%B %-d, %Y %H:%M" / to_fs(:long)).
func formatArchiveTime(unix int64, tzName string) string {
	return time.Unix(unix, 0).In(tzLocation(tzName)).Format("January 2, 2006 15:04")
}

// safeArchiveURL mirrors safe_archive_url: only absolute http(s) URLs with a
// host survive; anything else (javascript:, data:, relative) is dropped.
func safeArchiveURL(value string) string {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return u.String()
	}
	return ""
}
