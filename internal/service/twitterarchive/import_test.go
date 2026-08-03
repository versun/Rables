package twitterarchive

import (
	"archive/zip"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/jobs"
)

// testJPEG carries JPEG magic bytes so content sniffing yields image/jpeg.
var testJPEG = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}, make([]byte, 64)...)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// buildZip writes a synthetic archive; files maps entry name to content.
func buildZip(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	// Stable entry order: zip iteration follows central directory order.
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	entries := make([]zipEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, zipEntry{name: name, content: files[name]})
	}
	return buildZipOrdered(t, dir, entries)
}

type zipEntry struct {
	name    string
	content string
}

// buildZipOrdered writes entries in the given order (zip iteration follows
// central directory order, which for zip.Writer is creation order).
func buildZipOrdered(t *testing.T, dir string, entries []zipEntry) string {
	t.Helper()
	path := filepath.Join(dir, "archive.zip")
	out, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(out)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.content)); err != nil {
			t.Fatalf("write zip entry %s: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close zip file: %v", err)
	}
	return path
}

// jsPayload mirrors the window.YTD.<key>.part0 wrapper of official archives.
func jsPayload(key, jsonBody string) string {
	return fmt.Sprintf("window.YTD.%s.part0 = %s", key, jsonBody)
}

func newImporter(database *sql.DB, dataDir, sourcePath string) *Importer {
	return &Importer{DB: database, DataDir: dataDir, SourcePath: sourcePath}
}

func importTweetIDs(t *testing.T, database *sql.DB) []string {
	t.Helper()
	rows, err := database.Query(`SELECT tweet_id FROM twitter_archive_tweets ORDER BY tweeted_at DESC, tweet_id DESC`)
	if err != nil {
		t.Fatalf("query tweets: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan tweet id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestImportFullArchive(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	zipPath := buildZip(t, t.TempDir(), map[string]string{
		"data/account.js": jsPayload("account", `[{"account":{"username":"archive_owner"}}]`),
		"data/tweets.js": jsPayload("tweets", `[
			{"tweet":{"id":"100","id_str":"100","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Original tweet"}},
			{"tweet":{"id":"101","id_str":"101","created_at":"Thu Oct 11 20:19:24 +0000 2018","full_text":"@friend Reply tweet","in_reply_to_status_id_str":"50"}},
			{"tweet":{"id":"102","id_str":"102","created_at":"Fri Oct 12 20:19:24 +0000 2018","full_text":"Quoted tweet","quoted_status_id_str":"88"}},
			{"tweet":{"id":"100","id_str":"100","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Original tweet duplicate"}}
		]`),
		"data/follower.js":                jsPayload("follower", `[{"follower":{"accountId":"900","userLink":"https://twitter.com/follower_one"}}]`),
		"data/following.js":               jsPayload("following", `[{"following":{"accountId":"901","userLink":"https://twitter.com/following_one"}}]`),
		"data/like.js":                    jsPayload("like", `[{"like":{"tweetId":"777","fullText":"Liked tweet text","expandedUrl":"https://twitter.com/someone/status/777"}}]`),
		"data/tweets_media/100-photo.jpg": string(testJPEG),
		"data/tweets_media/100-clip.mp4":  "fake-mp4-data",
	})

	var progress []int
	im := newImporter(database, dataDir, zipPath)
	im.Progress = func(p int, _ string) { progress = append(progress, p) }
	summary, err := im.Import(context.Background())
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if summary != (Summary{Tweets: 3, Followers: 1, Following: 1, Likes: 1, TotalItems: 6}) {
		t.Fatalf("summary = %+v", summary)
	}
	if got, want := strings.Join(importTweetIDs(t, database), ","), "102,101,100"; got != want {
		t.Fatalf("tweet order = %s, want %s", got, want)
	}

	q := query.New(database)
	tweets, err := q.ListTwitterArchiveTweetsByType(context.Background(), query.ListTwitterArchiveTweetsByTypeParams{
		EntryType: EntryTypeRetweetQuote, Limit: 10, Offset: 0,
	})
	if err != nil || len(tweets) != 1 || tweets[0].TweetID != "102" {
		t.Fatalf("retweet_quote rows = %+v, err = %v", tweets, err)
	}

	// The duplicate row keeps the longer text and the archive owner name.
	var text, screenName string
	if err := database.QueryRow(`SELECT full_text, screen_name FROM twitter_archive_tweets WHERE tweet_id = '100'`).Scan(&text, &screenName); err != nil {
		t.Fatalf("load tweet 100: %v", err)
	}
	if text != "Original tweet duplicate" || screenName != "archive_owner" {
		t.Fatalf("tweet 100 = %q / %q", text, screenName)
	}

	// tweeted_at parsed from the Twitter date format (2018-10-10 20:19:24 UTC).
	var tweetedAt int64
	if err := database.QueryRow(`SELECT tweeted_at FROM twitter_archive_tweets WHERE tweet_id = '100'`).Scan(&tweetedAt); err != nil {
		t.Fatalf("load tweeted_at: %v", err)
	}
	if want := time.Date(2018, 10, 10, 20, 19, 24, 0, time.UTC).Unix(); tweetedAt != want {
		t.Fatalf("tweeted_at = %d, want %d", tweetedAt, want)
	}

	// Connections and likes.
	var followerLink string
	if err := database.QueryRow(`SELECT user_link FROM twitter_archive_connections WHERE relationship_type = 'follower' AND account_id = '900'`).Scan(&followerLink); err != nil {
		t.Fatalf("load follower: %v", err)
	}
	if followerLink != "https://twitter.com/follower_one" {
		t.Fatalf("follower link = %q", followerLink)
	}
	var likeText string
	if err := database.QueryRow(`SELECT full_text FROM twitter_archive_likes WHERE tweet_id = '777'`).Scan(&likeText); err != nil {
		t.Fatalf("load like: %v", err)
	}
	if likeText != "Liked tweet text" {
		t.Fatalf("like text = %q", likeText)
	}

	// Media: two files rows, attached to tweet 100, content on disk.
	var mediaCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM attachments a JOIN twitter_archive_tweets t ON t.id = a.record_id
		WHERE a.record_type = 'TwitterArchiveTweet' AND a.name = 'media' AND t.tweet_id = '100'`).Scan(&mediaCount); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if mediaCount != 2 {
		t.Fatalf("media attachments = %d, want 2", mediaCount)
	}
	rows, err := database.Query(`SELECT f.key, f.filename, f.content_type, f.byte_size FROM attachments a
		JOIN files f ON f.id = a.file_id
		JOIN twitter_archive_tweets t ON t.id = a.record_id
		WHERE a.record_type = 'TwitterArchiveTweet' AND t.tweet_id = '100' ORDER BY f.filename`)
	if err != nil {
		t.Fatalf("list media: %v", err)
	}
	defer rows.Close()
	type mf struct {
		key, filename, contentType string
		size                       int64
	}
	var files []mf
	for rows.Next() {
		var f mf
		var ct sql.NullString
		if err := rows.Scan(&f.key, &f.filename, &ct, &f.size); err != nil {
			t.Fatalf("scan media: %v", err)
		}
		f.contentType = ct.String
		files = append(files, f)
	}
	if len(files) != 2 || files[0].filename != "100-clip.mp4" || files[1].filename != "100-photo.jpg" {
		t.Fatalf("media files = %+v", files)
	}
	if files[0].contentType != "video/mp4" || files[1].contentType != "image/jpeg" {
		t.Fatalf("content types = %q / %q", files[0].contentType, files[1].contentType)
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dataDir, "files", f.key[0:2], f.key[2:4], f.key)); err != nil {
			t.Fatalf("media file %s not on disk: %v", f.filename, err)
		}
	}

	// Progress milestones (report order of the Rails importer).
	for _, milestone := range []int{5, 25, 55, 80, 100} {
		found := false
		for _, p := range progress {
			if p == milestone {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("progress %v misses milestone %d", progress, milestone)
		}
	}
	if progress[len(progress)-1] != 100 {
		t.Fatalf("last progress = %d, want 100", progress[len(progress)-1])
	}
}

func TestImportMergesDuplicateRowsAcrossEntries(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	zipPath := buildZip(t, t.TempDir(), map[string]string{
		"data/account.js": jsPayload("account", `[{"account":{"username":"archive_owner"}}]`),
		"data/tweets_01.js": jsPayload("tweets", `[
			{"tweet":{"id":"100","id_str":"100","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Short text"}}
		]`),
		"data/tweets_02.js": jsPayload("tweets", `[
			{"tweet":{"id":"100","id_str":"100","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"This duplicate row carries the longer text version"}}
		]`),
		"data/tweets_03.js": jsPayload("tweets", `[
			{"tweet":{"id":"100","id_str":"100","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Short text",
				"extended_entities":{"media":[{"media_url_https":"https://pbs.twimg.com/media/100-photo.jpg"}]}}}
		]`),
		"data/tweets_media/100-photo.jpg": string(testJPEG),
	})

	summary, err := newImporter(database, dataDir, zipPath).Import(context.Background())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Tweets != 1 {
		t.Fatalf("tweets = %d, want 1", summary.Tweets)
	}
	var text string
	if err := database.QueryRow(`SELECT full_text FROM twitter_archive_tweets WHERE tweet_id = '100'`).Scan(&text); err != nil {
		t.Fatalf("load tweet: %v", err)
	}
	if text != "This duplicate row carries the longer text version" {
		t.Fatalf("full_text = %q", text)
	}
	var mediaCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM attachments a JOIN twitter_archive_tweets t ON t.id = a.record_id
		WHERE a.record_type = 'TwitterArchiveTweet' AND t.tweet_id = '100'`).Scan(&mediaCount); err != nil {
		t.Fatalf("count media: %v", err)
	}
	if mediaCount != 1 {
		t.Fatalf("media attachments = %d, want 1", mediaCount)
	}
}

func TestImportUsesAccountNameSeenLaterInZipOrder(t *testing.T) {
	database := newTestDB(t)
	// tweets.js deliberately precedes account.js in the central directory.
	zipPath := buildZipOrdered(t, t.TempDir(), []zipEntry{
		{name: "data/tweets.js", content: jsPayload("tweets", `[{"tweet":{"id":"100","id_str":"100","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Original tweet"}}]`)},
		{name: "data/account.js", content: jsPayload("account", `[{"account":{"username":"archive_owner"}}]`)},
	})
	if _, err := newImporter(database, t.TempDir(), zipPath).Import(context.Background()); err != nil {
		t.Fatalf("import: %v", err)
	}
	var screenName string
	if err := database.QueryRow(`SELECT screen_name FROM twitter_archive_tweets WHERE tweet_id = '100'`).Scan(&screenName); err != nil {
		t.Fatalf("load tweet: %v", err)
	}
	if screenName != "archive_owner" {
		t.Fatalf("screen_name = %q, want archive_owner", screenName)
	}
}

func TestImportParsesISOCreatedAt(t *testing.T) {
	database := newTestDB(t)
	zipPath := buildZip(t, t.TempDir(), map[string]string{
		"data/tweets.js": jsPayload("tweets", `[
			{"tweet":{"id":"100","id_str":"100","createdAt":"2018-10-10T20:19:24.000Z","full_text":"ISO tweet"}},
			{"tweet":{"id":"101","id_str":"101","legacy":{"created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Legacy tweet"}}}
		]`),
	})
	summary, err := newImporter(database, t.TempDir(), zipPath).Import(context.Background())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Tweets != 2 {
		t.Fatalf("tweets = %d, want 2", summary.Tweets)
	}
	want := time.Date(2018, 10, 10, 20, 19, 24, 0, time.UTC).Unix()
	for _, id := range []string{"100", "101"} {
		var ts int64
		if err := database.QueryRow(`SELECT tweeted_at FROM twitter_archive_tweets WHERE tweet_id = ?`, id).Scan(&ts); err != nil {
			t.Fatalf("load %s: %v", id, err)
		}
		if ts != want {
			t.Fatalf("tweet %s tweeted_at = %d, want %d", id, ts, want)
		}
	}
}

func TestImportMediaOnlyTweet(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	zipPath := buildZip(t, t.TempDir(), map[string]string{
		"data/tweets.js": jsPayload("tweets", `[
			{"tweet":{"id":"100","id_str":"100","legacy":{"created_at":"Wed Oct 10 20:19:24 +0000 2018",
				"extended_entities":{"media":[{"media_url_https":"https://pbs.twimg.com/media/100-photo.jpg"}]}}}}
		]`),
		"data/tweets_media/100-photo.jpg": string(testJPEG),
	})
	summary, err := newImporter(database, dataDir, zipPath).Import(context.Background())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Tweets != 1 {
		t.Fatalf("tweets = %d, want 1", summary.Tweets)
	}
	var text string
	if err := database.QueryRow(`SELECT full_text FROM twitter_archive_tweets WHERE tweet_id = '100'`).Scan(&text); err != nil {
		t.Fatalf("load tweet: %v", err)
	}
	if text != "" {
		t.Fatalf("full_text = %q, want empty", text)
	}
}

func TestImportMediaFilenameLeadingDigitsRule(t *testing.T) {
	database := newTestDB(t)
	zipPath := buildZip(t, t.TempDir(), map[string]string{
		"data/tweets.js": jsPayload("tweets", `[
			{"tweet":{"id":"12345","id_str":"12345","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Tweet with inferred media"}},
			{"tweet":{"id":"2024","id_str":"2024","created_at":"Thu Oct 11 20:19:24 +0000 2018","full_text":"Tweet without media"}},
			{"tweet":{"id":"4","id_str":"4","created_at":"Fri Oct 12 20:19:24 +0000 2018","full_text":"Another tweet without media"}}
		]`),
		"data/tweets_media/12345-clip-2024.mp4": "fake-mp4-data",
	})
	if _, err := newImporter(database, t.TempDir(), zipPath).Import(context.Background()); err != nil {
		t.Fatalf("import: %v", err)
	}
	countFor := func(tweetID string) int {
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM attachments a JOIN twitter_archive_tweets t ON t.id = a.record_id
			WHERE a.record_type = 'TwitterArchiveTweet' AND t.tweet_id = ?`, tweetID).Scan(&n); err != nil {
			t.Fatalf("count media for %s: %v", tweetID, err)
		}
		return n
	}
	if countFor("12345") != 1 || countFor("2024") != 0 || countFor("4") != 0 {
		t.Fatalf("media counts = %d/%d/%d, want 1/0/0", countFor("12345"), countFor("2024"), countFor("4"))
	}
}

func TestImportReplacesExistingArchive(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	now := time.Now().Unix()

	// Seed an existing archive with media on disk.
	res, err := database.Exec(`INSERT INTO twitter_archive_tweets (tweet_id, screen_name, full_text, entry_type, tweeted_at, created_at, updated_at)
		VALUES ('existing-1', 'archive_owner', 'Existing archive entry', 'tweet', 1000, 1000, 1000)`)
	if err != nil {
		t.Fatalf("seed tweet: %v", err)
	}
	tweetID, _ := res.LastInsertId()
	oldKey := "0123456789abcdef0123456789abcdef"
	oldPath := filepath.Join(dataDir, "files", oldKey[0:2], oldKey[2:4], oldKey)
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("old-image"), 0o644); err != nil {
		t.Fatalf("write old media: %v", err)
	}
	res, err = database.Exec(`INSERT INTO files (key, filename, content_type, byte_size, created_at) VALUES (?, 'old.png', 'image/png', 9, ?)`, oldKey, now)
	if err != nil {
		t.Fatalf("seed file: %v", err)
	}
	oldFileID, _ := res.LastInsertId()
	if _, err := database.Exec(`INSERT INTO attachments (file_id, record_type, record_id, name, created_at) VALUES (?, 'TwitterArchiveTweet', ?, 'media', ?)`, oldFileID, tweetID, now); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO twitter_archive_connections (account_id, relationship_type, created_at, updated_at) VALUES ('1', 'follower', 1000, 1000)`); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO twitter_archive_likes (tweet_id, created_at, updated_at) VALUES ('9', 1000, 1000)`); err != nil {
		t.Fatalf("seed like: %v", err)
	}

	zipPath := buildZip(t, t.TempDir(), map[string]string{
		"data/account.js": jsPayload("account", `[{"account":{"username":"archive_owner"}}]`),
		"data/tweets.js":  jsPayload("tweets", `[{"tweet":{"id":"200","id_str":"200","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Replacement tweet"}}]`),
	})
	if _, err := newImporter(database, dataDir, zipPath).Import(ctx); err != nil {
		t.Fatalf("import: %v", err)
	}

	if got := importTweetIDs(t, database); len(got) != 1 || got[0] != "200" {
		t.Fatalf("tweets after replace = %v", got)
	}
	for _, table := range []string{"twitter_archive_connections", "twitter_archive_likes"} {
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s rows = %d, want 0", table, n)
		}
	}
	var attachments int
	if err := database.QueryRow(`SELECT COUNT(*) FROM attachments WHERE record_type = 'TwitterArchiveTweet'`).Scan(&attachments); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if attachments != 0 {
		t.Fatalf("attachments = %d, want 0", attachments)
	}
	var fileRows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files WHERE id = ?`, oldFileID).Scan(&fileRows); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if fileRows != 0 {
		t.Fatalf("old files row still present")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old media file still on disk")
	}
}

func TestImportFailureKeepsExistingArchive(t *testing.T) {
	database := newTestDB(t)
	if _, err := database.Exec(`INSERT INTO twitter_archive_tweets (tweet_id, screen_name, full_text, entry_type, tweeted_at, created_at, updated_at)
		VALUES ('existing-1', 'archive_owner', 'Existing archive entry', 'tweet', 1000, 1000, 1000)`); err != nil {
		t.Fatalf("seed tweet: %v", err)
	}
	zipPath := buildZip(t, t.TempDir(), map[string]string{
		"data/tweets.js": "window.YTD.tweets.part0 = not-valid-json",
	})
	if _, err := newImporter(database, t.TempDir(), zipPath).Import(context.Background()); err == nil {
		t.Fatalf("expected import error")
	}
	if got := importTweetIDs(t, database); len(got) != 1 || got[0] != "existing-1" {
		t.Fatalf("tweets after failed import = %v", got)
	}
}

func TestImportEmptyArchiveRejected(t *testing.T) {
	database := newTestDB(t)
	zipPath := buildZip(t, t.TempDir(), map[string]string{
		"data/account.js": jsPayload("account", `[{"account":{"username":"archive_owner"}}]`),
	})
	_, err := newImporter(database, t.TempDir(), zipPath).Import(context.Background())
	if err == nil || err.Error() != "No supported archive items found in archive" {
		t.Fatalf("err = %v", err)
	}
}

func TestImportRejectsNonZip(t *testing.T) {
	database := newTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "archive.txt")
	if err := os.WriteFile(path, []byte("nope"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := newImporter(database, t.TempDir(), path).Import(context.Background())
	if err == nil || err.Error() != "Archive file must be a zip" {
		t.Fatalf("err = %v", err)
	}
	_, err = newImporter(database, t.TempDir(), filepath.Join(dir, "missing.zip")).Import(context.Background())
	if err == nil || err.Error() != "Archive file not found" {
		t.Fatalf("err = %v", err)
	}
}

func TestImportCommitsInBatches(t *testing.T) {
	database := newTestDB(t)
	var items []string
	for i := 1; i <= 5; i++ {
		items = append(items, fmt.Sprintf(`{"tweet":{"id":"%d","id_str":"%d","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Batch tweet %d"}}`, 100+i, 100+i, i))
	}
	zipPath := buildZip(t, t.TempDir(), map[string]string{
		"data/tweets.js": jsPayload("tweets", "["+strings.Join(items, ",")+"]"),
	})
	im := newImporter(database, t.TempDir(), zipPath)
	im.BatchSize = 2
	summary, err := im.Import(context.Background())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Tweets != 5 {
		t.Fatalf("tweets = %d, want 5", summary.Tweets)
	}
	if got := importTweetIDs(t, database); len(got) != 5 {
		t.Fatalf("imported ids = %v", got)
	}
}

func TestImportNonstandardEntriesAndRawJSON(t *testing.T) {
	database := newTestDB(t)
	zipPath := buildZip(t, t.TempDir(), map[string]string{
		"data/account.json": `[{"account":{"username":"archive_owner"}}]`,
		"data/other.js":     jsPayload("other", `[{"id":"150","id_str":"150","created_at":"Sat Oct 13 20:19:24 +0000 2018","full_text":"Tweet from another archive entry"}]`),
		"data/tweets.json":  `[{"tweet":{"id":"160","id_str":"160","created_at":"Sat Oct 13 20:19:24 +0000 2018","full_text":"Archive link https://example.com/?token=a=b"}}]`,
	})
	summary, err := newImporter(database, t.TempDir(), zipPath).Import(context.Background())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Tweets != 2 {
		t.Fatalf("tweets = %d, want 2", summary.Tweets)
	}
	var text string
	if err := database.QueryRow(`SELECT full_text FROM twitter_archive_tweets WHERE tweet_id = '160'`).Scan(&text); err != nil {
		t.Fatalf("load tweet: %v", err)
	}
	if text != "Archive link https://example.com/?token=a=b" {
		t.Fatalf("full_text = %q", text)
	}
}

// TestImportStreamingScale drives a moderately large archive through the
// importer to prove the streaming pipeline handles it; the implementation
// keeps memory flat by construction (item-wise json.Decoder over the zip
// stream, 100-row batches, no full-entry buffering anywhere).
func TestImportStreamingScale(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scale test in short mode")
	}
	database := newTestDB(t)
	const n = 20000
	var b strings.Builder
	b.WriteString("window.YTD.tweets.part0 = [")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"tweet":{"id":"%d","id_str":"%d","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Scale tweet %d with some body text to pad the entry"}}`, 1000000+i, 1000000+i, i)
	}
	b.WriteString("]")
	zipPath := buildZip(t, t.TempDir(), map[string]string{"data/tweets.js": b.String()})

	summary, err := newImporter(database, t.TempDir(), zipPath).Import(context.Background())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Tweets != n {
		t.Fatalf("tweets = %d, want %d", summary.Tweets, n)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM twitter_archive_tweets`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != n {
		t.Fatalf("rows = %d, want %d", count, n)
	}
}

func TestImportJobHandler(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	q := query.New(database)

	zipPath := buildZip(t, dataDir, map[string]string{
		"data/account.js": jsPayload("account", `[{"account":{"username":"archive_owner"}}]`),
		"data/tweets.js":  jsPayload("tweets", `[{"tweet":{"id":"200","id_str":"200","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Original tweet"}}]`),
		"data/like.js":    jsPayload("like", `[{"like":{"tweetId":"777","fullText":"Liked","expandedUrl":"https://twitter.com/s/status/777"}}]`),
	})
	now := time.Now().Unix()
	imp, err := q.CreateTwitterArchiveImport(ctx, query.CreateTwitterArchiveImportParams{
		SourceFilename: "archive.zip",
		SourcePath:     sql.NullString{String: zipPath, Valid: true},
		QueuedAt:       now,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("create import: %v", err)
	}
	if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindTwitterArchiveImport, map[string]any{"import_id": imp.ID}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	worker := jobs.NewWorker(database)
	RegisterImportHandler(worker, database, dataDir)
	claimed, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if !claimed {
		t.Fatalf("job was not claimed")
	}

	done, err := q.GetTwitterArchiveImport(ctx, imp.ID)
	if err != nil {
		t.Fatalf("reload import: %v", err)
	}
	if done.Status != "completed" || done.Progress != 100 {
		t.Fatalf("import = %s/%d%%, want completed/100", done.Status, done.Progress)
	}
	if done.TweetsCount != 1 || done.LikesCount != 1 || done.TotalItemsCount != 2 {
		t.Fatalf("counts = %d tweets/%d likes/%d total", done.TweetsCount, done.LikesCount, done.TotalItemsCount)
	}
	if done.SourcePath.Valid {
		t.Fatalf("source_path not cleared: %v", done.SourcePath)
	}
	if _, err := os.Stat(zipPath); !os.IsNotExist(err) {
		t.Fatalf("source zip not removed")
	}
	if got := importTweetIDs(t, database); len(got) != 1 || got[0] != "200" {
		t.Fatalf("tweets = %v", got)
	}
	var logs int
	if err := database.QueryRow(`SELECT COUNT(*) FROM activity_logs WHERE target = 'twitter_archive'`).Scan(&logs); err != nil {
		t.Fatalf("count activity logs: %v", err)
	}
	if logs < 2 { // started + completed
		t.Fatalf("activity logs = %d, want >= 2", logs)
	}
}

func TestImportJobHandlerFailure(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	q := query.New(database)

	zipPath := buildZip(t, dataDir, map[string]string{
		"data/tweets.js": "window.YTD.tweets.part0 = broken[",
	})
	now := time.Now().Unix()
	imp, err := q.CreateTwitterArchiveImport(ctx, query.CreateTwitterArchiveImportParams{
		SourceFilename: "broken.zip",
		SourcePath:     sql.NullString{String: zipPath, Valid: true},
		QueuedAt:       now,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("create import: %v", err)
	}
	if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindTwitterArchiveImport, map[string]any{"import_id": imp.ID}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	worker := jobs.NewWorker(database)
	RegisterImportHandler(worker, database, dataDir)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	done, err := q.GetTwitterArchiveImport(ctx, imp.ID)
	if err != nil {
		t.Fatalf("reload import: %v", err)
	}
	if done.Status != "failed" {
		t.Fatalf("status = %s, want failed", done.Status)
	}
	if !done.ErrorMessage.Valid || done.ErrorMessage.String == "" {
		t.Fatalf("error message not recorded")
	}
	// The job run itself completed (the Rails job rescues the failure).
	var jobStatus string
	if err := database.QueryRow(`SELECT status FROM job_runs WHERE kind = ?`, jobs.KindTwitterArchiveImport).Scan(&jobStatus); err != nil {
		t.Fatalf("load job run: %v", err)
	}
	if jobStatus != "done" {
		t.Fatalf("job status = %s, want done", jobStatus)
	}
	// active_slot freed so a new import can be queued.
	if done.ActiveSlot.Valid {
		t.Fatalf("active_slot not released")
	}
	var active int64
	if active, err = q.HasActiveTwitterArchiveImport(ctx); err != nil || active != 0 {
		t.Fatalf("active imports = %d, err = %v", active, err)
	}
}

// TestImportRealFixtureZip imports a small archive produced by the Rails
// test suite (read-only fixture, copied to a temp dir first).
func TestImportRealFixtureZip(t *testing.T) {
	src := "/Users/versun/Projects/Rables/tmp/twitter_archives/twitter_archive_1784772174_f7acb9016365f7e3.zip"
	if _, err := os.Stat(src); err != nil {
		t.Skip("Rails fixture zip not available")
	}
	dir := t.TempDir()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	zipPath := filepath.Join(dir, "fixture.zip")
	if err := os.WriteFile(zipPath, data, 0o644); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	database := newTestDB(t)
	summary, err := newImporter(database, dir, zipPath).Import(context.Background())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Tweets != 2 {
		t.Fatalf("tweets = %d, want 2", summary.Tweets)
	}
	// The fixture holds tweet 200 (original) and 201 (reply), newest first.
	if got, want := strings.Join(importTweetIDs(t, database), ","), "201,200"; got != want {
		t.Fatalf("tweet order = %s, want %s", got, want)
	}
	var entryType, screenName string
	if err := database.QueryRow(`SELECT entry_type, screen_name FROM twitter_archive_tweets WHERE tweet_id = '201'`).Scan(&entryType, &screenName); err != nil {
		t.Fatalf("load reply: %v", err)
	}
	if entryType != EntryTypeReply || screenName != "archive_owner" {
		t.Fatalf("tweet 201 = %s / %s", entryType, screenName)
	}
}
