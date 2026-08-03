package httpd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// twitterOEmbedEndpoint mirrors TwitterOembedService's endpoint; a var so
// tests can point it at an httptest server.
var twitterOEmbedEndpoint = "https://publish.x.com/oembed"

// oEmbedTimeout bounds the whole oEmbed request (task spec: 10s); a var so
// tests can shrink it.
var oEmbedTimeout = 10 * time.Second

// maxTweetContentLength mirrors TwitterOembedService::MAX_CONTENT_LENGTH.
const maxTweetContentLength = 250

// RegisterSourcesRoutes mounts POST /admin/sources/fetch_twitter behind
// RequireAuth, mirroring Admin::SourcesController.
func RegisterSourcesRoutes(r chi.Router, s *Server) {
	r.Route("/admin/sources", func(r chi.Router) {
		r.Use(s.RequireAuth)
		r.Post("/fetch_twitter", s.adminFetchTwitter)
	})
}

// adminFetchTwitter handles POST /admin/sources/fetch_twitter: fetch a tweet's
// author/content via the oEmbed endpoint to backfill an article's Source
// Reference. The JSON shape matches the Rails controller (success/author/
// content on success, error with 422/503 otherwise).
func (s *Server) adminFetchTwitter(w http.ResponseWriter, r *http.Request) {
	tweetURL, err := fetchTwitterParam(r)
	if err != nil {
		// ActionDispatch maps a malformed JSON body to 400 Bad Request.
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if tweetURL == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "URL is required"})
		return
	}
	if !isTwitterURL(tweetURL) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "Not a valid Twitter/X URL"})
		return
	}

	author, content, ok := fetchTweetOEmbed(r.Context(), tweetURL)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Failed to fetch tweet content"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "author": author, "content": content})
}

// fetchTwitterParam reads params[:url] from a JSON body or a form post.
func fetchTwitterParam(r *http.Request) (string, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return "", err
		}
		return body.URL, nil
	}
	return r.FormValue("url"), nil
}

// isTwitterURL mirrors twitter_url?: the host must be twitter.com or x.com
// (with optional www.); unparseable URLs fail like URI::InvalidURIError.
func isTwitterURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "twitter.com", "www.twitter.com", "x.com", "www.x.com":
		return true
	}
	return false
}

// fetchTweetOEmbed ports TwitterOembedService#fetch: GET the oEmbed endpoint
// with the tweet URL (omit_script/dnt like the Ruby query), join every <p>
// text of the returned html and cap at 250 characters. Any failure — non-2xx,
// timeout, bad JSON — maps to ok=false (the Ruby rescue → nil → 503 path).
func fetchTweetOEmbed(ctx context.Context, tweetURL string) (author, content string, ok bool) {
	endpoint, err := url.Parse(twitterOEmbedEndpoint)
	if err != nil {
		return "", "", false
	}
	q := endpoint.Query()
	q.Set("url", tweetURL)
	q.Set("omit_script", "true")
	q.Set("dnt", "true")
	endpoint.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, oEmbedTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", "", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 { // Net::HTTPSuccess
		return "", "", false
	}

	var data struct {
		AuthorName string `json:"author_name"`
		HTML       string `json:"html"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", "", false
	}
	return data.AuthorName, tweetContent(data.HTML), true
}

// tweetContent extracts the joined <p> texts of the oEmbed html fragment —
// Nokogiri's css("p").map(&:text).join(" ").strip — capped at 250 characters.
func tweetContent(fragment string) string {
	nodes, err := html.ParseFragment(strings.NewReader(fragment), tweetFragmentContext)
	if err != nil {
		return ""
	}
	var ps []string
	for _, n := range nodes {
		ps = append(ps, paragraphTexts(n)...)
	}
	content := strings.TrimSpace(strings.Join(ps, " "))
	if runes := []rune(content); len(runes) > maxTweetContentLength {
		content = string(runes[:maxTweetContentLength])
	}
	return content
}

// tweetFragmentContext is the fragment parsing context (a body element).
var tweetFragmentContext = &html.Node{Type: html.ElementNode, DataAtom: atom.Body, Data: "body"}

// paragraphTexts returns the text of every <p> in the tree (Nokogiri .text:
// descendant text nodes concatenated).
func paragraphTexts(n *html.Node) []string {
	var out []string
	if n.Type == html.ElementNode && n.DataAtom == atom.P {
		var b strings.Builder
		writeTweetText(&b, n)
		out = append(out, b.String())
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, paragraphTexts(c)...)
	}
	return out
}

// writeTweetText concatenates all descendant text nodes of n.
func writeTweetText(b *strings.Builder, n *html.Node) {
	if n.Type == html.TextNode {
		b.WriteString(n.Data)
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		writeTweetText(b, c)
	}
}
