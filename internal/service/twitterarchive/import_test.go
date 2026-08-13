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
	"rables/internal/testutil/zipfake"
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

// buildUploadZip writes the archive where a real upload lands
// (storeTwitterArchiveUpload): <dataDir>/imports with the twitter_archive_
// prefix, so the job handler's source cleanup accepts the path.
func buildUploadZip(t *testing.T, dataDir string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(dataDir, "imports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create imports dir: %v", err)
	}
	path := buildZip(t, dir, files)
	uploadPath := filepath.Join(dir, "twitter_archive_test.zip")
	if err := os.Rename(path, uploadPath); err != nil {
		t.Fatalf("rename upload zip: %v", err)
	}
	return uploadPath
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

	zipPath := buildUploadZip(t, dataDir, map[string]string{
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

// TestImportJobHandlerKeepsSourceWhenCompleteFails: when the import itself
// succeeds but the terminal Complete write fails (forced here by a trigger),
// the source zip and source_path must survive — deleting them first would
// strand the row running with the zip gone, and the re-executed job would
// then fail on the missing source and clobber the finished import to failed.
func TestImportJobHandlerKeepsSourceWhenCompleteFails(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	q := query.New(database)

	zipPath := buildUploadZip(t, dataDir, map[string]string{
		"data/tweets.js": jsPayload("tweets", `[{"tweet":{"id":"200","id_str":"200","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Original tweet"}}]`),
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
	// Force the terminal Complete write to fail.
	if _, err := database.Exec(`CREATE TRIGGER fail_complete BEFORE UPDATE ON twitter_archive_imports
		WHEN NEW.status = 'completed' BEGIN SELECT RAISE(ABORT, 'forced complete failure'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	worker := jobs.NewWorker(database)
	RegisterImportHandler(worker, database, dataDir)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	reloaded, err := q.GetTwitterArchiveImport(ctx, imp.ID)
	if err != nil {
		t.Fatalf("reload import: %v", err)
	}
	if reloaded.Status != "running" {
		t.Fatalf("status = %s, want running (not clobbered to failed)", reloaded.Status)
	}
	if !reloaded.SourcePath.Valid || reloaded.SourcePath.String != zipPath {
		t.Fatalf("source_path = %+v, want %q kept", reloaded.SourcePath, zipPath)
	}
	if _, err := os.Stat(zipPath); err != nil {
		t.Fatalf("source zip removed after the failed Complete: %v", err)
	}

	// Once the Complete write works again, the re-executed job self-heals:
	// the import completes and the source is cleaned up.
	if _, err := database.Exec(`DROP TRIGGER fail_complete`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindTwitterArchiveImport, map[string]any{"import_id": imp.ID}, time.Now()); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("second run once: %v", err)
	}
	done, err := q.GetTwitterArchiveImport(ctx, imp.ID)
	if err != nil {
		t.Fatalf("reload import: %v", err)
	}
	if done.Status != "completed" {
		t.Fatalf("status = %s, want completed", done.Status)
	}
	if done.SourcePath.Valid {
		t.Fatalf("source_path not cleared: %v", done.SourcePath)
	}
	if _, err := os.Stat(zipPath); !os.IsNotExist(err) {
		t.Fatalf("source zip not removed")
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

// TestFlushTweetsFailureReclaimsOrphanMedia: media rows and blobs are stored
// outside the tweet batch transaction, so a failed batch must reclaim the
// media it newly stored — while keeping media an earlier committed batch
// still references.
func TestFlushTweetsFailureReclaimsOrphanMedia(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	zipPath := buildZip(t, t.TempDir(), map[string]string{
		"data/tweets_media/100-photo.jpg": string(testJPEG),
		"data/tweets_media/200-photo.jpg": string(testJPEG),
	})
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	mediaFiles := map[string]*zip.File{}
	for _, f := range zr.File {
		mediaFiles[f.Name] = f
	}

	im := newImporter(database, dataDir, zipPath)
	im.q = query.New(database)
	sink := &dbSink{im: im, mediaFiles: mediaFiles, mediaIDs: map[string]int64{}}
	ctx := context.Background()

	// Batch 1 commits with the shared media entry.
	sink.tweets = []candidate{{tweetID: "100", entryType: EntryTypeTweet, screenName: "owner", fullText: "one", tweetedAt: 1, media: []string{"data/tweets_media/100-photo.jpg"}}}
	if err := sink.flushTweets(ctx); err != nil {
		t.Fatalf("flush batch 1: %v", err)
	}

	// From here on every attachment insert fails, so batch 2 rolls back after
	// its new media was already stored outside the transaction.
	if _, err := database.Exec(`CREATE TRIGGER fail_attach BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT, 'boom'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Batch 2 reuses the committed media (cache hit) and stores one new entry.
	sink.tweets = []candidate{{tweetID: "200", entryType: EntryTypeTweet, screenName: "owner", fullText: "two", tweetedAt: 2, media: []string{"data/tweets_media/100-photo.jpg", "data/tweets_media/200-photo.jpg"}}}
	if err := sink.flushTweets(ctx); err == nil {
		t.Fatal("flush batch 2 should fail")
	}

	// The failed batch's tweet is gone (rolled back).
	var tweetCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM twitter_archive_tweets WHERE tweet_id = '200'`).Scan(&tweetCount); err != nil {
		t.Fatalf("count tweets: %v", err)
	}
	if tweetCount != 0 {
		t.Fatalf("tweet 200 rows = %d, want 0", tweetCount)
	}

	// Exactly one files row survives: the media committed with batch 1. The
	// row stored for the failed batch was reclaimed.
	rows, err := database.Query(`SELECT key FROM files`)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan key: %v", err)
		}
		keys = append(keys, key)
	}
	if len(keys) != 1 {
		t.Fatalf("files rows = %d (%v), want 1", len(keys), keys)
	}
	// Its blob is on disk, and it is the only blob under files/.
	if _, err := os.Stat(im.mediaPath(keys[0])); err != nil {
		t.Fatalf("committed media blob missing: %v", err)
	}
	var blobCount int
	err = filepath.Walk(filepath.Join(dataDir, "files"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			blobCount++
		}
		return err
	})
	if err != nil {
		t.Fatalf("walk files dir: %v", err)
	}
	if blobCount != 1 {
		t.Fatalf("blobs on disk = %d, want 1", blobCount)
	}
}

// TestDiscardNewMediaReclaimsWithCanceledContext: a batch failure caused by
// worker shutdown arrives with a canceled ctx; the reclaim must still run,
// or the freshly stored files rows and disk blobs would stay orphaned.
func TestDiscardNewMediaReclaimsWithCanceledContext(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	im := newImporter(database, dataDir, "")
	im.q = query.New(database)
	ctx := context.Background()

	// One stored media ref as a failed batch leaves it behind: a files row
	// plus its blob on disk, referenced by no attachment.
	row, err := im.q.CreateFile(ctx, query.CreateFileParams{
		Key:       "0123456789abcdef0123456789abcdef",
		Filename:  "photo.jpg",
		ByteSize:  int64(len(testJPEG)),
		CreatedAt: 1,
	})
	if err != nil {
		t.Fatalf("create file row: %v", err)
	}
	blob := im.mediaPath(row.Key)
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(blob, testJPEG, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	sink := &dbSink{im: im, newMedia: []storedMediaRef{{id: row.ID, key: row.Key}}}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	// Sanity: plain queries no longer run on this ctx, so a reclaim tied to
	// it would silently skip everything.
	if err := im.q.DeleteFile(canceled, row.ID); err == nil {
		t.Fatal("canceled ctx should fail queries")
	}

	sink.discardNewMedia(canceled)

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files WHERE id = ?`, row.ID).Scan(&n); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if n != 0 {
		t.Errorf("files row survived the reclaim, want it deleted")
	}
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Errorf("blob survived the reclaim, stat err = %v", err)
	}
	if sink.newMedia != nil {
		t.Errorf("newMedia = %v, want nil after the reclaim", sink.newMedia)
	}
}

// TestClearStoredArchiveNeverRemovesUnsafeKeys: files rows can arrive with
// arbitrary keys through a database import (the bundle's files/attachments
// tables are upserted verbatim). The replace must still delete such rows,
// but must never resolve their keys into disk paths: a traversal key would
// remove files outside the data dir, and a key shorter than 4 chars would
// panic mediaPath's xx/yy slicing.
func TestClearStoredArchiveNeverRemovesUnsafeKeys(t *testing.T) {
	database := newTestDB(t)
	// Nest the data dir so the traversal key below lands inside the test's
	// temp tree (mediaPath(key) == <root>/victim.txt).
	dataDir := filepath.Join(t.TempDir(), "a", "b", "c")
	im := newImporter(database, dataDir, "")
	im.q = query.New(database)
	ctx := context.Background()

	mkFile := func(key string) query.File {
		t.Helper()
		row, err := im.q.CreateFile(ctx, query.CreateFileParams{
			Key:       key,
			Filename:  "photo.jpg",
			ByteSize:  int64(len(testJPEG)),
			CreatedAt: 1,
		})
		if err != nil {
			t.Fatalf("create file row %q: %v", key, err)
		}
		if err := im.q.CreateAttachment(ctx, query.CreateAttachmentParams{
			FileID:     row.ID,
			RecordType: "TwitterArchiveTweet",
			RecordID:   1,
			Name:       "media",
			CreatedAt:  1,
		}); err != nil {
			t.Fatalf("attach file row %q: %v", key, err)
		}
		return row
	}

	// A well-formed key with a real blob: removed as before.
	good := mkFile("0123456789abcdef0123456789abcdef")
	goodBlob := im.mediaPath(good.Key)
	if err := os.MkdirAll(filepath.Dir(goodBlob), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(goodBlob, testJPEG, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	// A traversal key: mediaPath resolves two levels above the data dir.
	evil := mkFile("../../victim.txt")
	victim := filepath.Clean(filepath.Join(dataDir, "..", "..", "victim.txt"))
	if got := im.mediaPath(evil.Key); got != victim {
		t.Fatalf("test premise broken: mediaPath(%q) = %q, want %q", evil.Key, got, victim)
	}
	if err := os.WriteFile(victim, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	// A short key: panics the xx/yy slicing when resolved.
	mkFile("ab")

	if err := im.clearStoredArchive(ctx); err != nil {
		t.Fatalf("clearStoredArchive: %v", err)
	}

	if _, err := os.Stat(goodBlob); !os.IsNotExist(err) {
		t.Errorf("well-formed blob survived the clear, stat err = %v", err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("file outside the data dir was removed via a traversal key: %v", err)
	}
	var rows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rows); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if rows != 0 {
		t.Errorf("files rows = %d, want 0 (rows are deleted even when the blob is left alone)", rows)
	}
}

// TestImportRejectsOversizedArchive: a small zip declaring >10GB of
// uncompressed media must fail validation before any replace starts
// (mirrors the extractImportZip bound on database bundles). The entries are
// never opened by the scan: the declared total alone must refuse the archive
// before anything is streamed to disk.
func TestImportRejectsOversizedArchive(t *testing.T) {
	database := newTestDB(t)
	zipPath := filepath.Join(t.TempDir(), "archive.zip")
	// 3 x 4GB > MaxArchiveExtractBytes; kept under 0xFFFFFFFF so the plain
	// 32-bit size fields hold it without a zip64 extra record.
	zipfake.Write(t, zipPath, []zipfake.Entry{
		{Name: "data/tweets_media/100-a.jpg", Payload: []byte("x"), Declared: 4_000_000_000},
		{Name: "data/tweets_media/100-b.jpg", Payload: []byte("x"), Declared: 4_000_000_000},
		{Name: "data/tweets_media/100-c.jpg", Payload: []byte("x"), Declared: 4_000_000_000},
	})
	im := newImporter(database, t.TempDir(), zipPath)
	if _, err := im.Import(context.Background()); err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("Import err = %v, want an over-the-limit error", err)
	}
}

// TestImportRejectsZip64DeclaredOverflow: the first entry declares exactly
// the 10GB limit (allowed; the boundary is inclusive) and the second declares
// 2^64-1 via a zip64 extra record. A guard that sums first and compares after
// would wrap the running total back below the limit and wave the bomb
// through; the compare-before-add ordering must refuse it instead.
func TestImportRejectsZip64DeclaredOverflow(t *testing.T) {
	database := newTestDB(t)
	zipPath := filepath.Join(t.TempDir(), "archive.zip")
	zipfake.Write(t, zipPath, []zipfake.Entry{
		{Name: "data/tweets_media/100-0.jpg", Payload: []byte("x"), Declared: MaxArchiveExtractBytes, Zip64: true},
		{Name: "data/tweets_media/101-1.jpg", Payload: []byte("x"), Declared: 0xFFFFFFFFFFFFFFFF, Zip64: true},
	})
	im := newImporter(database, t.TempDir(), zipPath)
	if _, err := im.Import(context.Background()); err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("Import err = %v, want an over-the-limit error", err)
	}
}

// TestImportRejectsTooManyEntries: an archive with more file entries than
// MaxArchiveExtractEntries is refused up front, even though every entry is a
// one-byte file far under the byte limit — millions of tiny entries would
// exhaust inodes when the media entries are streamed to disk.
func TestImportRejectsTooManyEntries(t *testing.T) {
	database := newTestDB(t)
	entries := make([]zipfake.Entry, 0, MaxArchiveExtractEntries+1)
	for i := 0; i <= MaxArchiveExtractEntries; i++ {
		entries = append(entries, zipfake.Entry{
			Name:     fmt.Sprintf("data/tweets_media/%07d-0.jpg", i),
			Payload:  []byte("x"),
			Declared: 1,
		})
	}
	zipPath := filepath.Join(t.TempDir(), "archive.zip")
	zipfake.Write(t, zipPath, entries)
	im := newImporter(database, t.TempDir(), zipPath)
	if _, err := im.Import(context.Background()); err == nil || !strings.Contains(err.Error(), fmt.Sprint(MaxArchiveExtractEntries)) {
		t.Fatalf("Import err = %v, want an entry-count-limit error", err)
	}
}

// TestImportRejectsOversizedItem: one item decoding past maxArchiveItemBytes
// fails the import instead of being materialized in memory — a multi-GB
// "full_text" deflates to a few MB on disk but would OOM a small VPS on
// decode. A large item under the cap still imports normally.
func TestImportRejectsOversizedItem(t *testing.T) {
	tweetsEntry := func(text string) string {
		return jsPayload("tweets", `[{"tweet":{"id":"1","id_str":"1","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"`+text+`"}}]`)
	}
	over := newImporter(newTestDB(t), t.TempDir(), buildZip(t, t.TempDir(), map[string]string{
		// 1MB past the cap: the decoder's buffered read-ahead (charged to the
		// previous token's budget) must not absorb the overage.
		"data/tweets.js": tweetsEntry(strings.Repeat("A", maxArchiveItemBytes+(1<<20))),
	}))
	if _, err := over.Import(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeds the 64MB") {
		t.Fatalf("Import err = %v, want a per-item size limit error", err)
	}
	under := newImporter(newTestDB(t), t.TempDir(), buildZip(t, t.TempDir(), map[string]string{
		"data/tweets.js": tweetsEntry(strings.Repeat("A", 1<<20)),
	}))
	summary, err := under.Import(context.Background())
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if summary.Tweets != 1 {
		t.Fatalf("tweets = %d, want 1", summary.Tweets)
	}
}

// TestImportJobHandlerCancellationKeepsSourceAndRequeues: a SIGTERM landing
// mid-import cancels the job ctx; the handler must propagate the cancellation
// instead of recording a failure, so the worker requeues the job for free and
// the import — source zip and running row untouched — resumes after the
// restart via the mark-running self-heal.
func TestImportJobHandlerCancellationKeepsSourceAndRequeues(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	q := query.New(database)

	// Enough items that the import cannot finish between the mark-running
	// write and the poller below noticing it.
	var items []string
	for i := 0; i < 5000; i++ {
		items = append(items, fmt.Sprintf(`{"tweet":{"id":"%d","id_str":"%d","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Cancel tweet %d with some body text to pad the entry"}}`, 100+i, 100+i, i))
	}
	zipPath := buildZip(t, dataDir, map[string]string{
		"data/tweets.js": jsPayload("tweets", "["+strings.Join(items, ",")+"]"),
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

	// Cancel the worker ctx (the SIGTERM) once the import is running.
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var status string
			if err := database.QueryRow(`SELECT status FROM twitter_archive_imports WHERE id = ?`, imp.ID).Scan(&status); err == nil && status == "running" {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	worker := jobs.NewWorker(database)
	RegisterImportHandler(worker, database, dataDir)
	claimed, err := worker.RunOnce(jobCtx)
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if !claimed {
		t.Fatalf("job was not claimed")
	}

	// The job was requeued for free: immediately due, no attempt consumed.
	var jobStatus string
	var attempts int64
	if err := database.QueryRow(`SELECT status, attempts FROM job_runs WHERE kind = ?`, jobs.KindTwitterArchiveImport).Scan(&jobStatus, &attempts); err != nil {
		t.Fatalf("load job run: %v", err)
	}
	if jobStatus != "queued" || attempts != 0 {
		t.Fatalf("job = %s/%d attempts, want queued/0", jobStatus, attempts)
	}

	// The import row is untouched by the failure path and the source zip is
	// still there, so the re-executed job can resume the import.
	reloaded, err := q.GetTwitterArchiveImport(ctx, imp.ID)
	if err != nil {
		t.Fatalf("reload import: %v", err)
	}
	if reloaded.Status != "running" {
		t.Fatalf("status = %s, want running", reloaded.Status)
	}
	if reloaded.ErrorMessage.Valid {
		t.Fatalf("error message recorded for a cancelled import: %q", reloaded.ErrorMessage.String)
	}
	if !reloaded.SourcePath.Valid || reloaded.SourcePath.String != zipPath {
		t.Fatalf("source_path = %+v, want %q kept", reloaded.SourcePath, zipPath)
	}
	if _, err := os.Stat(zipPath); err != nil {
		t.Fatalf("source zip removed on cancellation: %v", err)
	}
}

// TestImportJobHandlerSkipsTerminallyFailedImport: a job re-executed after
// its import already failed (a crash between FailTwitterArchiveImport and
// CompleteJobRun leaves the job queued) must not re-run the import: the
// terminal run already deleted the source zip, so re-marking running would
// only overwrite the original error with "Archive file not found".
func TestImportJobHandlerSkipsTerminallyFailedImport(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	q := query.New(database)

	now := time.Now().Unix()
	imp, err := q.CreateTwitterArchiveImport(ctx, query.CreateTwitterArchiveImportParams{
		SourceFilename: "broken.zip",
		SourcePath:     sql.NullString{String: filepath.Join(dataDir, "broken.zip"), Valid: true},
		QueuedAt:       now,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("create import: %v", err)
	}
	// Terminal failure state: failed with the original error, source cleared.
	if err := q.FailTwitterArchiveImport(ctx, query.FailTwitterArchiveImportParams{
		ErrorMessage: sql.NullString{String: "broken archive payload", Valid: true},
		FinishedAt:   sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:    now,
		ID:           imp.ID,
	}); err != nil {
		t.Fatalf("fail import: %v", err)
	}
	if err := q.ClearTwitterArchiveImportSource(ctx, query.ClearTwitterArchiveImportSourceParams{UpdatedAt: now, ID: imp.ID}); err != nil {
		t.Fatalf("clear source: %v", err)
	}
	if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindTwitterArchiveImport, map[string]any{"import_id": imp.ID}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	worker := jobs.NewWorker(database)
	RegisterImportHandler(worker, database, dataDir)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	reloaded, err := q.GetTwitterArchiveImport(ctx, imp.ID)
	if err != nil {
		t.Fatalf("reload import: %v", err)
	}
	if reloaded.Status != "failed" {
		t.Fatalf("status = %s, want failed", reloaded.Status)
	}
	if !reloaded.ErrorMessage.Valid || reloaded.ErrorMessage.String != "broken archive payload" {
		t.Fatalf("error message = %+v, want the original failure preserved", reloaded.ErrorMessage)
	}
	if reloaded.StartedAt.Valid {
		t.Fatalf("started_at set: the terminally failed import was re-run")
	}
	var jobStatus string
	if err := database.QueryRow(`SELECT status FROM job_runs WHERE kind = ?`, jobs.KindTwitterArchiveImport).Scan(&jobStatus); err != nil {
		t.Fatalf("load job run: %v", err)
	}
	if jobStatus != "done" {
		t.Fatalf("job status = %s, want done", jobStatus)
	}
}

// TestImportJobHandlerSkipsFailedImportWithDeletedSource: a crash between the
// terminal source removal and ClearTwitterArchiveImportSource leaves a failed
// row whose source_path still points at the deleted file. A re-executed job
// must skip it just like a cleared source_path: re-marking running would only
// fail on the missing file and overwrite the original error with "Archive
// file not found".
func TestImportJobHandlerSkipsFailedImportWithDeletedSource(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	q := query.New(database)

	now := time.Now().Unix()
	imp, err := q.CreateTwitterArchiveImport(ctx, query.CreateTwitterArchiveImportParams{
		SourceFilename: "broken.zip",
		SourcePath:     sql.NullString{String: filepath.Join(dataDir, "broken.zip"), Valid: true},
		QueuedAt:       now,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("create import: %v", err)
	}
	// Terminal failure state, but the crash hit before source_path was
	// cleared: it still points at the already-deleted zip.
	if err := q.FailTwitterArchiveImport(ctx, query.FailTwitterArchiveImportParams{
		ErrorMessage: sql.NullString{String: "broken archive payload", Valid: true},
		FinishedAt:   sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:    now,
		ID:           imp.ID,
	}); err != nil {
		t.Fatalf("fail import: %v", err)
	}
	if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindTwitterArchiveImport, map[string]any{"import_id": imp.ID}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	worker := jobs.NewWorker(database)
	RegisterImportHandler(worker, database, dataDir)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	reloaded, err := q.GetTwitterArchiveImport(ctx, imp.ID)
	if err != nil {
		t.Fatalf("reload import: %v", err)
	}
	if reloaded.Status != "failed" {
		t.Fatalf("status = %s, want failed", reloaded.Status)
	}
	if !reloaded.ErrorMessage.Valid || reloaded.ErrorMessage.String != "broken archive payload" {
		t.Fatalf("error message = %+v, want the original failure preserved", reloaded.ErrorMessage)
	}
	if reloaded.StartedAt.Valid {
		t.Fatalf("started_at set: the terminally failed import was re-run")
	}
	var jobStatus string
	if err := database.QueryRow(`SELECT status FROM job_runs WHERE kind = ?`, jobs.KindTwitterArchiveImport).Scan(&jobStatus); err != nil {
		t.Fatalf("load job run: %v", err)
	}
	if jobStatus != "done" {
		t.Fatalf("job status = %s, want done", jobStatus)
	}
}

// TestImportJobHandlerRemovesLeftoverSourceAfterCompletedImport: a crash
// between the terminal Complete write and the source cleanup leaves a
// completed row with the source zip still on disk. A re-executed job must not
// re-run the import, but it must remove the leftover zip.
func TestImportJobHandlerRemovesLeftoverSourceAfterCompletedImport(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	q := query.New(database)

	zipPath := buildUploadZip(t, dataDir, map[string]string{
		"data/tweets.js": jsPayload("tweets", `[{"tweet":{"id":"200","id_str":"200","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Original tweet"}}]`),
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
	// Completed state, but the crash hit before the source cleanup ran.
	if err := q.CompleteTwitterArchiveImport(ctx, query.CompleteTwitterArchiveImportParams{
		TweetsCount:     1,
		FollowersCount:  0,
		FollowingCount:  0,
		LikesCount:      0,
		TotalItemsCount: 1,
		FinishedAt:      sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:       now,
		ID:              imp.ID,
	}); err != nil {
		t.Fatalf("complete import: %v", err)
	}
	if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindTwitterArchiveImport, map[string]any{"import_id": imp.ID}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	worker := jobs.NewWorker(database)
	RegisterImportHandler(worker, database, dataDir)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	reloaded, err := q.GetTwitterArchiveImport(ctx, imp.ID)
	if err != nil {
		t.Fatalf("reload import: %v", err)
	}
	if reloaded.Status != "completed" {
		t.Fatalf("status = %s, want completed", reloaded.Status)
	}
	if got := importTweetIDs(t, database); len(got) != 0 {
		t.Fatalf("tweets = %v, want none (the completed import must not re-run)", got)
	}
	if _, err := os.Stat(zipPath); !os.IsNotExist(err) {
		t.Fatalf("leftover source zip not removed")
	}
	var jobStatus string
	if err := database.QueryRow(`SELECT status FROM job_runs WHERE kind = ?`, jobs.KindTwitterArchiveImport).Scan(&jobStatus); err != nil {
		t.Fatalf("load job run: %v", err)
	}
	if jobStatus != "done" {
		t.Fatalf("job status = %s, want done", jobStatus)
	}
}

// TestImportJobHandlerKeepsSourceOutsideImportsDir: a crafted transfer bundle
// can plant a twitter_archive_imports row whose source_path points at an
// arbitrary server file. When the surviving queued job re-executes, the
// import fails on the fake path and the terminal source cleanup runs — it
// must not delete the victim: only paths inside <dataDir>/imports with the
// twitter_archive_ prefix (what storeTwitterArchiveUpload writes) are removed.
func TestImportJobHandlerKeepsSourceOutsideImportsDir(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	q := query.New(database)

	victim := filepath.Join(dataDir, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	now := time.Now().Unix()
	imp, err := q.CreateTwitterArchiveImport(ctx, query.CreateTwitterArchiveImportParams{
		SourceFilename: "victim.txt",
		SourcePath:     sql.NullString{String: victim, Valid: true},
		QueuedAt:       now,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("create import: %v", err)
	}
	// The planted row looks like a recovered failure whose job is still
	// queued, so the handler re-marks it running and re-reads the source.
	if err := q.FailTwitterArchiveImport(ctx, query.FailTwitterArchiveImportParams{
		ErrorMessage: sql.NullString{String: "Process restarted before the import finished", Valid: true},
		FinishedAt:   sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:    now,
		ID:           imp.ID,
	}); err != nil {
		t.Fatalf("fail import: %v", err)
	}
	if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindTwitterArchiveImport, map[string]any{"import_id": imp.ID}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	worker := jobs.NewWorker(database)
	RegisterImportHandler(worker, database, dataDir)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	reloaded, err := q.GetTwitterArchiveImport(ctx, imp.ID)
	if err != nil {
		t.Fatalf("reload import: %v", err)
	}
	if reloaded.Status != "failed" {
		t.Fatalf("status = %s, want failed (the import must fail on the fake source)", reloaded.Status)
	}
	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("victim file removed: %v", err)
	}
	if string(content) != "keep me" {
		t.Fatalf("victim content = %q, want untouched", content)
	}
}

// TestImportJobHandlerSelfHealsRecoveredFailedImport: startup recovery fails
// imports stuck queued/running but keeps their source_path, and their job may
// still be queued. Re-executing such a job must resume the import, so the
// terminally-failed skip above keys on the source file being gone, not on
// the failed status alone.
func TestImportJobHandlerSelfHealsRecoveredFailedImport(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	q := query.New(database)

	zipPath := buildZip(t, dataDir, map[string]string{
		"data/tweets.js": jsPayload("tweets", `[{"tweet":{"id":"200","id_str":"200","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Resumed tweet"}}]`),
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
	// Startup recovery failed the row but left source_path in place.
	if err := q.FailTwitterArchiveImport(ctx, query.FailTwitterArchiveImportParams{
		ErrorMessage: sql.NullString{String: "Process restarted before the import finished", Valid: true},
		FinishedAt:   sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:    now,
		ID:           imp.ID,
	}); err != nil {
		t.Fatalf("fail import: %v", err)
	}
	if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindTwitterArchiveImport, map[string]any{"import_id": imp.ID}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	worker := jobs.NewWorker(database)
	RegisterImportHandler(worker, database, dataDir)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	reloaded, err := q.GetTwitterArchiveImport(ctx, imp.ID)
	if err != nil {
		t.Fatalf("reload import: %v", err)
	}
	if reloaded.Status != "completed" || reloaded.TweetsCount != 1 {
		t.Fatalf("import = %s/%d tweets, want completed/1", reloaded.Status, reloaded.TweetsCount)
	}
	if got := importTweetIDs(t, database); len(got) != 1 || got[0] != "200" {
		t.Fatalf("tweets = %v", got)
	}
}

// TestImportJobHandlerSkipsRecoveredImportWithNewerImport: startup recovery
// fails an import while its job stays queued, and the admin then uploads a
// newer archive whose import runs first. Re-executing the stale job must not
// roll the stored archive back to the older upload: the newer non-failed
// import row supersedes it, so the job completes without re-running.
func TestImportJobHandlerSkipsRecoveredImportWithNewerImport(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	ctx := context.Background()
	q := query.New(database)

	zipPath := buildZip(t, dataDir, map[string]string{
		"data/tweets.js": jsPayload("tweets", `[{"tweet":{"id":"100","id_str":"100","created_at":"Wed Oct 10 20:19:24 +0000 2018","full_text":"Stale tweet"}}]`),
	})
	now := time.Now().Unix()
	stale, err := q.CreateTwitterArchiveImport(ctx, query.CreateTwitterArchiveImportParams{
		SourceFilename: "stale.zip",
		SourcePath:     sql.NullString{String: zipPath, Valid: true},
		QueuedAt:       now,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("create stale import: %v", err)
	}
	// Startup recovery failed the row but left the source zip in place.
	if err := q.FailTwitterArchiveImport(ctx, query.FailTwitterArchiveImportParams{
		ErrorMessage: sql.NullString{String: "Process restarted before the import finished", Valid: true},
		FinishedAt:   sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:    now,
		ID:           stale.ID,
	}); err != nil {
		t.Fatalf("fail stale import: %v", err)
	}
	// A newer archive uploaded after the recovery already imported.
	newer, err := q.CreateTwitterArchiveImport(ctx, query.CreateTwitterArchiveImportParams{
		SourceFilename: "newer.zip",
		SourcePath:     sql.NullString{},
		QueuedAt:       now,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("create newer import: %v", err)
	}
	if err := q.CompleteTwitterArchiveImport(ctx, query.CompleteTwitterArchiveImportParams{
		TweetsCount:     1,
		FollowersCount:  0,
		FollowingCount:  0,
		LikesCount:      0,
		TotalItemsCount: 1,
		FinishedAt:      sql.NullInt64{Int64: now, Valid: true},
		UpdatedAt:       now,
		ID:              newer.ID,
	}); err != nil {
		t.Fatalf("complete newer import: %v", err)
	}
	if _, err := jobs.NewEnqueuer(database).Enqueue(ctx, jobs.KindTwitterArchiveImport, map[string]any{"import_id": stale.ID}, time.Now()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	worker := jobs.NewWorker(database)
	RegisterImportHandler(worker, database, dataDir)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	reloaded, err := q.GetTwitterArchiveImport(ctx, stale.ID)
	if err != nil {
		t.Fatalf("reload stale import: %v", err)
	}
	if reloaded.Status != "failed" {
		t.Fatalf("status = %s, want failed (the superseded import must not re-run)", reloaded.Status)
	}
	if reloaded.StartedAt.Valid {
		t.Fatalf("started_at set: the superseded import was re-run")
	}
	if got := importTweetIDs(t, database); len(got) != 0 {
		t.Fatalf("tweets = %v, want none (the stale archive must not replace the newer data)", got)
	}
	var jobStatus string
	if err := database.QueryRow(`SELECT status FROM job_runs WHERE kind = ?`, jobs.KindTwitterArchiveImport).Scan(&jobStatus); err != nil {
		t.Fatalf("load job run: %v", err)
	}
	if jobStatus != "done" {
		t.Fatalf("job status = %s, want done", jobStatus)
	}
}

// TestClearStoredArchiveKeepsStaticFileReferencedMedia: a database import can
// merge a static_files entry onto a files row that archive media also uses.
// The pre-replace cleanup must keep such a file — static_files.file_id
// references files(id) under foreign_keys enforcement, so deleting it would
// fail the whole replace. A reference landing on a variant keeps the whole
// family, because files.variant_of pins the original.
func TestClearStoredArchiveKeepsStaticFileReferencedMedia(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	im := newImporter(database, dataDir, "")
	im.q = query.New(database)
	ctx := context.Background()

	mkFile := func(key string, variantOf int64) query.File {
		t.Helper()
		vo := sql.NullInt64{}
		if variantOf > 0 {
			vo = sql.NullInt64{Int64: variantOf, Valid: true}
		}
		row, err := im.q.CreateFile(ctx, query.CreateFileParams{
			Key:       key,
			Filename:  "photo.jpg",
			ByteSize:  int64(len(testJPEG)),
			VariantOf: vo,
			CreatedAt: 1,
		})
		if err != nil {
			t.Fatalf("create file row %q: %v", key, err)
		}
		return row
	}
	attachMedia := func(id int64) {
		t.Helper()
		if err := im.q.CreateAttachment(ctx, query.CreateAttachmentParams{
			FileID:     id,
			RecordType: "TwitterArchiveTweet",
			RecordID:   1,
			Name:       "media",
			CreatedAt:  1,
		}); err != nil {
			t.Fatalf("attach file row %d: %v", id, err)
		}
	}
	linkStatic := func(filename string, id int64) {
		t.Helper()
		if _, err := im.q.CreateStaticFile(ctx, query.CreateStaticFileParams{
			Filename: filename, FileID: id, CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("create static file: %v", err)
		}
	}
	writeBlob := func(key string) {
		t.Helper()
		path := im.mediaPath(key)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, testJPEG, 0o644); err != nil {
			t.Fatalf("write blob: %v", err)
		}
	}

	// Tweet media whose original is also a static file.
	shared := mkFile("aaaa1111aaaa1111aaaa1111aaaa1111", 0)
	attachMedia(shared.ID)
	writeBlob(shared.Key)
	linkStatic("shared.jpg", shared.ID)

	// Tweet media whose variant is a static file: the referenced variant pins
	// its original, so the whole family survives.
	family := mkFile("bbbb2222bbbb2222bbbb2222bbbb2222", 0)
	familyVariant := mkFile("cccc3333cccc3333cccc3333cccc3333", family.ID)
	attachMedia(family.ID)
	writeBlob(family.Key)
	writeBlob(familyVariant.Key)
	linkStatic("variant.jpg", familyVariant.ID)

	// Plain tweet media: purged as before.
	plain := mkFile("dddd4444dddd4444dddd4444dddd4444", 0)
	attachMedia(plain.ID)
	writeBlob(plain.Key)

	if err := im.clearStoredArchive(ctx); err != nil {
		t.Fatalf("clearStoredArchive: %v", err)
	}

	kept := map[string]bool{}
	rows, err := database.Query(`SELECT key FROM files`)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scan file key: %v", err)
		}
		kept[key] = true
	}
	for _, f := range []query.File{shared, family, familyVariant} {
		if !kept[f.Key] {
			t.Errorf("files row %q purged, want kept (referenced by a static file)", f.Key)
		}
		if _, err := os.Stat(im.mediaPath(f.Key)); err != nil {
			t.Errorf("blob %q removed while a static file references it: %v", f.Key, err)
		}
	}
	if kept[plain.Key] {
		t.Errorf("files row %q kept, want purged with the old archive", plain.Key)
	}
	if _, err := os.Stat(im.mediaPath(plain.Key)); !os.IsNotExist(err) {
		t.Errorf("blob %q survived the clear, stat err = %v", plain.Key, err)
	}
	var attachments, staticFiles int
	if err := database.QueryRow(`SELECT COUNT(*) FROM attachments WHERE record_type = 'TwitterArchiveTweet'`).Scan(&attachments); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if attachments != 0 {
		t.Errorf("tweet attachments = %d, want 0 (old archive cleared)", attachments)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM static_files`).Scan(&staticFiles); err != nil {
		t.Fatalf("count static files: %v", err)
	}
	if staticFiles != 2 {
		t.Errorf("static_files rows = %d, want 2 (untouched by the clear)", staticFiles)
	}
}

// TestClearStoredArchiveKeepsContentReferencedMedia: a tweet media file whose
// /files/<key> URL was hand-reused in an article body must survive the archive
// clear — the attachment sweep would otherwise delete the row and blob and
// leave the article's embed 404.
func TestClearStoredArchiveKeepsContentReferencedMedia(t *testing.T) {
	database := newTestDB(t)
	dataDir := t.TempDir()
	im := newImporter(database, dataDir, "")
	im.q = query.New(database)
	ctx := context.Background()

	mkMedia := func(key string) query.File {
		t.Helper()
		row, err := im.q.CreateFile(ctx, query.CreateFileParams{
			Key:       key,
			Filename:  "photo.jpg",
			ByteSize:  int64(len(testJPEG)),
			CreatedAt: 1,
		})
		if err != nil {
			t.Fatalf("create file row %q: %v", key, err)
		}
		if err := im.q.CreateAttachment(ctx, query.CreateAttachmentParams{
			FileID:     row.ID,
			RecordType: "TwitterArchiveTweet",
			RecordID:   1,
			Name:       "media",
			CreatedAt:  1,
		}); err != nil {
			t.Fatalf("attach file row %q: %v", key, err)
		}
		path := im.mediaPath(key)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, testJPEG, 0o644); err != nil {
			t.Fatalf("write blob: %v", err)
		}
		return row
	}

	// Tweet media whose URL an article body reuses.
	reused := mkMedia("aaaa1111aaaa1111aaaa1111aaaa1111")
	if _, err := database.Exec(
		`INSERT INTO articles (content_html, created_at, updated_at) VALUES (?, 0, 0)`,
		`<p><img src="/files/`+reused.Key+`"></p>`,
	); err != nil {
		t.Fatalf("insert article: %v", err)
	}

	// Plain tweet media: purged as before.
	plain := mkMedia("dddd4444dddd4444dddd4444dddd4444")

	if err := im.clearStoredArchive(ctx); err != nil {
		t.Fatalf("clearStoredArchive: %v", err)
	}

	var kept int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files WHERE id = ?`, reused.ID).Scan(&kept); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if kept != 1 {
		t.Errorf("reused files row = %d, want 1 (still referenced by article content)", kept)
	}
	if _, err := os.Stat(im.mediaPath(reused.Key)); err != nil {
		t.Errorf("reused blob removed while article content references it: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM files WHERE id = ?`, plain.ID).Scan(&kept); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if kept != 0 {
		t.Errorf("plain files row = %d, want 0 (purged with the old archive)", kept)
	}
	if _, err := os.Stat(im.mediaPath(plain.Key)); !os.IsNotExist(err) {
		t.Errorf("plain blob survived the clear, stat err = %v", err)
	}
}
