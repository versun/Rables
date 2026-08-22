// Command migrate-quote-media repairs twitter-synced quote articles archived
// before the media-placement fix: the sync used to append the quoted tweet's
// media embeds to the article body (after the tweet's own media); they belong
// to the source-reference block. For each tweet article with a source_url,
// the quoted tweet's media (files named tweet-<quoted-id>-*) is located in
// content_markdown — markdown image, markdown link (rich_text-era video), or
// raw <video> — removed from the body, and appended to source_content as the
// HTML fragment the fixed sync writes. content_html is re-rendered from the
// new markdown through the standard write path; updated_at is preserved, so
// restart the app afterwards to drop the in-memory render cache.
//
// Default is a dry-run printing the per-article plan; -yes applies, taking a
// VACUUM INTO backup first. Examples:
//
//	migrate-quote-media -db /var/lib/docker/volumes/versun-me-go/_data/rables.db
//	migrate-quote-media -db /var/lib/docker/volumes/versun-me-go/_data/rables.db -yes
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"rables/internal/domain"
)

const sourceURLPrefix = "https://x.com/i/web/status/"

type article struct {
	id            int64
	slug          string
	quotedID      string
	sourceContent string
	markdown      string
}

type mediaFile struct {
	key         string
	filename    string
	contentType string
}

// move is one quoted-media embed found in the body.
type move struct {
	file  mediaFile
	pos   int    // token position in the original markdown (embed order)
	token string // exact markdown token to remove
}

func main() {
	dbPath := flag.String("db", "", "path to rables.db (required)")
	yes := flag.Bool("yes", false, "apply the migration (default is a dry-run)")
	backup := flag.String("backup", "", "backup path (default <db>.pre-quote-media-<timestamp>)")
	flag.Parse()
	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "-db is required")
		os.Exit(2)
	}

	db, err := sql.Open("sqlite", *dbPath)
	must(err)
	defer db.Close()
	if _, err := db.Exec("PRAGMA busy_timeout=30000"); err != nil {
		must(err)
	}

	articles, err := loadQuoteArticles(db)
	must(err)
	fmt.Printf("found %d quote articles in %s\n", len(articles), *dbPath)

	type plan struct {
		article
		moves          []move
		warnings       []string
		newMarkdown    string
		newSource      string
		newContentHTML string
	}
	var plans []plan
	skipped := 0
	for _, a := range articles {
		files, err := quotedMediaFiles(db, a.id, a.quotedID)
		if err != nil {
			must(err)
		}
		if len(files) == 0 {
			continue
		}
		p := plan{article: a}
		for _, f := range files {
			token, pos := findEmbedToken(a.markdown, f)
			if pos < 0 {
				p.warnings = append(p.warnings, "embed not found in content_markdown: "+f.filename)
				continue
			}
			p.moves = append(p.moves, move{file: f, pos: pos, token: token})
		}
		if len(p.moves) == 0 {
			skipped++
			for _, w := range p.warnings {
				fmt.Printf("SKIP article:%d %s (%s)\n", a.id, a.slug, w)
			}
			continue
		}
		sort.Slice(p.moves, func(i, j int) bool { return p.moves[i].pos < p.moves[j].pos })

		md := a.markdown
		var embeds []string
		for _, m := range p.moves {
			md = strings.Replace(md, m.token, "", 1)
			embeds = append(embeds, embedHTML(m.file))
		}
		p.newMarkdown = strings.TrimRight(md, "\n")
		p.newSource = buildQuoteFragment(a.sourceContent, embeds)

		// Safety: every moved file key leaves the body exactly once and lands
		// in the quote block exactly once — no media is lost or duplicated.
		for _, m := range p.moves {
			if n := strings.Count(p.newMarkdown, "/files/"+m.file.key); n != 0 {
				must(fmt.Errorf("article:%d %s: %d references to %s left in body", a.id, a.slug, n, m.file.filename))
			}
			if n := strings.Count(p.newSource, "/files/"+m.file.key); n != 1 {
				must(fmt.Errorf("article:%d %s: %d references to %s in quote block", a.id, a.slug, n, m.file.filename))
			}
		}
		p.newContentHTML = domain.AddLazyLoading(domain.SanitizeHTML(domain.RenderMarkdown(p.newMarkdown)))
		plans = append(plans, p)

		names := make([]string, len(p.moves))
		for i, m := range p.moves {
			names[i] = m.file.filename
		}
		fmt.Printf("MOVE article:%d %s <- %s\n", a.id, a.slug, strings.Join(names, ", "))
		for _, w := range p.warnings {
			fmt.Printf("  WARN article:%d %s\n", a.id, w)
		}
	}
	fmt.Printf("\ndry-run: %d articles to fix, %d skipped (no quoted media in body)\n", len(plans), skipped)
	if !*yes {
		fmt.Println("re-run with -yes to apply")
		return
	}

	backupPath := backupDB(db, *dbPath, *backup)
	fmt.Printf("backup written to %s\n", backupPath)

	tx, err := db.Begin()
	must(err)
	for _, p := range plans {
		if _, err := tx.Exec(
			`UPDATE articles SET content_markdown=?, content_html=?, source_content=? WHERE id=?`,
			p.newMarkdown, p.newContentHTML, p.newSource, p.id); err != nil {
			tx.Rollback()
			must(fmt.Errorf("update article:%d: %w", p.id, err))
		}
	}
	must(tx.Commit())
	fmt.Printf("moved quoted media into the source block of %d articles (updated_at preserved)\n", len(plans))
}

// loadQuoteArticles reads twitter-synced quote articles: slug tweet-<id> plus
// a source_url pointing at the quoted tweet.
func loadQuoteArticles(db *sql.DB) ([]article, error) {
	rs, err := db.Query(`SELECT id, COALESCE(slug,''), COALESCE(source_url,''), COALESCE(source_content,''), COALESCE(content_markdown,'')
		FROM articles WHERE slug LIKE 'tweet-%' AND source_url LIKE '` + sourceURLPrefix + `%' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []article
	for rs.Next() {
		var a article
		var sourceURL string
		if err := rs.Scan(&a.id, &a.slug, &sourceURL, &a.sourceContent, &a.markdown); err != nil {
			return nil, err
		}
		a.quotedID = strings.TrimPrefix(sourceURL, sourceURLPrefix)
		if a.quotedID == "" {
			continue
		}
		out = append(out, a)
	}
	return out, rs.Err()
}

// quotedMediaFiles lists the article's embeds whose filename marks them as
// downloaded from the quoted tweet (tweet-<quoted-id>-<hex><ext>).
func quotedMediaFiles(db *sql.DB, articleID int64, quotedID string) ([]mediaFile, error) {
	rs, err := db.Query(`SELECT f.key, f.filename, COALESCE(f.content_type,'')
		FROM attachments at JOIN files f ON f.id = at.file_id
		WHERE at.record_type='Article' AND at.record_id=? AND at.name='embeds'
		  AND f.filename LIKE 'tweet-' || ? || '-%'`, articleID, quotedID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []mediaFile
	for rs.Next() {
		var f mediaFile
		if err := rs.Scan(&f.key, &f.filename, &f.contentType); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rs.Err()
}

// findEmbedToken locates the file's embed in content_markdown, trying every
// shape the sync or the rich_text migration produced: markdown image, markdown
// link (rich_text-era video), raw <video>. Returns the exact token and its
// position, or -1.
func findEmbedToken(markdown string, f mediaFile) (string, int) {
	tokens := []string{
		"![" + f.filename + "](/files/" + f.key + ")",            // markdown image
		"[" + f.filename + "](/files/" + f.key + ")",             // rich_text-era video link
		`<video src="/files/` + f.key + `" controls=""></video>`, // post-sanitize raw video
		`<video src="/files/` + f.key + `" controls></video>`,    // pre-sanitize raw video
	}
	for _, token := range tokens {
		if pos := strings.Index(markdown, token); pos >= 0 {
			return token, pos
		}
	}
	return "", -1
}

// embedHTML renders the moved media the way the fixed sync writes it into the
// quote block: <img> for images, <video controls> for video (the rich_text-era
// download link is upgraded to a player).
func embedHTML(f mediaFile) string {
	src := "/files/" + f.key
	if strings.HasPrefix(f.contentType, "video/") {
		return `<video src="` + src + `" controls></video>`
	}
	return `<img src="` + src + `" alt="` + escapeHTML(f.filename) + `" loading="lazy">`
}

// buildQuoteFragment converts the plain-text quote to escaped paragraphs and
// appends the moved embeds — the same fragment twittersync's
// buildQuotedSourceContent writes for new syncs.
func buildQuoteFragment(text string, embeds []string) string {
	var parts []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts = append(parts, "<p>"+escapeHTML(line)+"</p>")
	}
	return strings.Join(append(parts, embeds...), "")
}

// escapeHTML mirrors CGI.escapeHTML (& < > " only), same as twittersync.
func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// backupDB snapshots the database with VACUUM INTO and returns the path.
func backupDB(db *sql.DB, dbPath, backup string) string {
	if backup == "" {
		base := dbPath + ".pre-quote-media-" + time.Now().Format("20060102-150405")
		backup = base
		for i := 1; ; i++ {
			if _, err := os.Stat(backup); err != nil {
				break // not exists (or unstatable; VACUUM INTO reports it)
			}
			backup = fmt.Sprintf("%s-%d", base, i)
		}
	}
	if _, err := db.Exec(fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(backup, "'", "''"))); err != nil {
		must(fmt.Errorf("backup: %w", err))
	}
	return backup
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-quote-media:", err)
		os.Exit(1)
	}
}
