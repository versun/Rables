// Command migrate-content converts every rich_text article/page in a Rables
// SQLite database to the markdown content type: the Lexxy/Action Text HTML in
// content_html becomes Markdown source in content_markdown via
// contentmigrate.ToMarkdown, re-rendering content_html through the standard
// write path. updated_at is preserved so feeds and the render cache see no
// fake bump; restart the app afterwards to drop the in-memory render cache.
//
// Default is a dry-run printing a per-row status; -yes applies, taking a
// VACUUM INTO backup first. -rerender instead re-renders content_html from
// content_markdown for every markdown row (repairing earlier conversions
// that predated the AddLazyLoading write-path step). Examples:
//
//	migrate-content -db /var/lib/docker/volumes/versun-me-go/_data/rables.db
//	migrate-content -db /var/lib/docker/volumes/versun-me-go/_data/rables.db -yes
//	migrate-content -db /var/lib/docker/volumes/versun-me-go/_data/rables.db -rerender -yes
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/html"
	_ "modernc.org/sqlite"

	"rables/internal/domain"
	"rables/internal/service/contentmigrate"
)

type row struct {
	kind string
	id   int64
	slug string
	body string // rich_text: content_html; markdown: content_markdown
	html string // markdown rows only: current content_html
}

func main() {
	dbPath := flag.String("db", "", "path to rables.db (required)")
	yes := flag.Bool("yes", false, "apply the migration (default is a dry-run)")
	rerender := flag.Bool("rerender", false, "re-render content_html from content_markdown for all markdown rows")
	backup := flag.String("backup", "", "backup path (default <db>.pre-content-migration-<timestamp>)")
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

	if *rerender {
		runRerender(db, *dbPath, *yes, *backup)
		return
	}

	rows, err := loadRows(db)
	must(err)
	fmt.Printf("found %d rich_text rows in %s\n", len(rows), *dbPath)
	if len(rows) == 0 {
		return
	}

	type converted struct {
		row
		md, html string
	}
	var todo []converted
	var textDiffs int
	for _, r := range rows {
		if domain.IsBlank(r.body) {
			todo = append(todo, converted{r, "", r.body})
			continue
		}
		md, newHTML, err := contentmigrate.ToMarkdown(r.body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL %s:%d (%s): %v\n", r.kind, r.id, r.slug, err)
			os.Exit(1)
		}
		status := "ok"
		switch {
		case visibleText(r.body) == visibleText(newHTML):
		case stripWS(visibleText(r.body)) == stripWS(visibleText(newHTML)):
			status = "ws-only"
		default:
			status = "TEXT-DIFF"
			textDiffs++
		}
		fmt.Printf("%-9s %s:%d %s (%d -> %d bytes md)\n", status, r.kind, r.id, r.slug, len(r.body), len(md))
		todo = append(todo, converted{r, md, newHTML})
	}
	fmt.Printf("\ndry-run: %d rows convertible, %d with visible-text differences\n", len(todo), textDiffs)

	if !*yes {
		fmt.Println("re-run with -yes to apply")
		return
	}

	backupPath := backupDB(db, *dbPath, *backup)
	fmt.Printf("backup written to %s\n", backupPath)

	tx, err := db.Begin()
	must(err)
	for _, c := range todo {
		table := "articles"
		if c.kind == "page" {
			table = "pages"
		}
		var mdVal any
		if c.md != "" {
			mdVal = c.md
		}
		if _, err := tx.Exec(
			fmt.Sprintf("UPDATE %s SET content_type='markdown', content_markdown=?, content_html=? WHERE id=?", table),
			mdVal, c.html, c.id); err != nil {
			tx.Rollback()
			must(fmt.Errorf("update %s:%d: %w", c.kind, c.id, err))
		}
	}
	must(tx.Commit())
	fmt.Printf("migrated %d rows to markdown (updated_at preserved)\n", len(todo))
}

// runRerender re-renders content_html from content_markdown through the
// standard write path for every markdown article/page, preserving updated_at.
func runRerender(db *sql.DB, dbPath string, yes bool, backupPath string) {
	rows, err := loadMarkdownRows(db)
	must(err)
	fmt.Printf("found %d markdown rows\n", len(rows))
	if len(rows) == 0 {
		return
	}
	type rendered struct {
		row
		html string
	}
	var todo []rendered
	var changed int
	for _, r := range rows {
		newHTML := domain.AddLazyLoading(domain.SanitizeHTML(domain.RenderMarkdown(r.body)))
		if newHTML != r.html {
			changed++
		}
		todo = append(todo, rendered{r, newHTML})
	}
	fmt.Printf("dry-run: %d rows would re-render, %d with changed content_html\n", len(todo), changed)
	if !yes {
		fmt.Println("re-run with -rerender -yes to apply")
		return
	}
	fmt.Printf("backup written to %s\n", backupDB(db, dbPath, backupPath))
	tx, err := db.Begin()
	must(err)
	for _, c := range todo {
		table := "articles"
		if c.kind == "page" {
			table = "pages"
		}
		if _, err := tx.Exec(fmt.Sprintf("UPDATE %s SET content_html=? WHERE id=?", table), c.html, c.id); err != nil {
			tx.Rollback()
			must(fmt.Errorf("update %s:%d: %w", c.kind, c.id, err))
		}
	}
	must(tx.Commit())
	fmt.Printf("re-rendered %d markdown rows (updated_at preserved)\n", len(todo))
}

// loadMarkdownRows reads markdown articles and pages with both the markdown
// source (body) and the current content_html.
func loadMarkdownRows(db *sql.DB) ([]row, error) {
	var rows []row
	for _, q := range []struct {
		kind, sql string
	}{
		{"article", `SELECT id, COALESCE(slug,''), COALESCE(content_markdown,''), COALESCE(content_html,'') FROM articles WHERE content_type='markdown' ORDER BY id`},
		{"page", `SELECT id, COALESCE(slug,''), COALESCE(content_markdown,''), COALESCE(content_html,'') FROM pages WHERE content_type='markdown' ORDER BY id`},
	} {
		rs, err := db.Query(q.sql)
		if err != nil {
			return nil, err
		}
		for rs.Next() {
			var r row
			r.kind = q.kind
			if err := rs.Scan(&r.id, &r.slug, &r.body, &r.html); err != nil {
				rs.Close()
				return nil, err
			}
			rows = append(rows, r)
		}
		if err := rs.Err(); err != nil {
			rs.Close()
			return nil, err
		}
		rs.Close()
	}
	return rows, nil
}

// backupDB snapshots the database with VACUUM INTO and returns the path.
func backupDB(db *sql.DB, dbPath, backup string) string {
	if backup == "" {
		// Two applies within the same second (a scripted convert + rerender)
		// would collide on the timestamped default name.
		base := dbPath + ".pre-content-migration-" + time.Now().Format("20060102-150405")
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
		fmt.Fprintln(os.Stderr, "migrate-content:", err)
		os.Exit(1)
	}
}

// loadRows reads rich_text articles and pages (body is content_html).
func loadRows(db *sql.DB) ([]row, error) {
	var rows []row
	for _, q := range []struct {
		kind, sql string
	}{
		{"article", `SELECT id, COALESCE(slug,''), COALESCE(content_html,'') FROM articles WHERE content_type='rich_text' ORDER BY id`},
		{"page", `SELECT id, COALESCE(slug,''), COALESCE(content_html,'') FROM pages WHERE content_type='rich_text' ORDER BY id`},
	} {
		rs, err := db.Query(q.sql)
		if err != nil {
			return nil, err
		}
		for rs.Next() {
			var r row
			r.kind = q.kind
			if err := rs.Scan(&r.id, &r.slug, &r.body); err != nil {
				rs.Close()
				return nil, err
			}
			rows = append(rows, r)
		}
		if err := rs.Err(); err != nil {
			rs.Close()
			return nil, err
		}
		rs.Close()
	}
	return rows, nil
}

// visibleText extracts rendered text with whitespace collapsed, as a
// round-trip safety check (not a full fidelity comparison).
func visibleText(raw string) string {
	z := html.NewTokenizer(strings.NewReader(raw))
	var b strings.Builder
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt == html.TextToken {
			b.Write(z.Text())
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func stripWS(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
