// Package twitterarchive imports official Twitter archive ZIPs (plan section
// 4.10), mirroring app/services/twitter_archive_importer.rb.
//
// Unlike the Rails original (which accumulates every parsed row in memory
// before writing), this importer is streaming end to end: zip entries are
// read with zip.File.Open and decoded item by item, tweets are upserted in
// batches of 100 per transaction, connections/likes are deduplicated by the
// database unique indexes (INSERT OR IGNORE), and media entries are streamed
// to disk. A full validation pass over the archive runs before any write so a
// broken archive never replaces the stored one, matching the Rails
// parse-then-replace semantics.
package twitterarchive

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/md5"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
	"rables/internal/service/activity"
	"rables/internal/service/media"
)

// Tweet entry types (TwitterArchiveTweet::ENTRY_TYPES).
const (
	EntryTypeTweet        = "tweet"
	EntryTypeReply        = "reply"
	EntryTypeRetweetQuote = "retweet_quote"
)

// mediaDirectories mirrors TwitterArchiveImporter::MEDIA_DIRECTORIES.
var mediaDirectories = []string{"data/tweets_media/", "data/tweet_media/"}

// DefaultTweetBatchSize mirrors TwitterArchiveImporter::TWEET_BATCH_SIZE:
// tweet rows committed per transaction during the replace.
const DefaultTweetBatchSize = 100

// MaxArchiveExtractBytes caps the total declared uncompressed size of an
// archive ZIP (10GB, the same bound extractImportZip applies to database
// bundles). Media entries are streamed straight to disk, and Go's zip reader
// only enforces each entry's own declared size, so without a total cap a
// crafted archive (a small zip declaring huge media entries) would fill the
// disk mid-import.
const MaxArchiveExtractBytes = 10 << 30

// MaxArchiveExtractEntries caps the number of file entries in an archive ZIP
// (the same bound MaxImportExtractEntries applies to database bundles):
// millions of tiny entries stay under the byte limit but would exhaust
// inodes when the media entries are streamed to disk.
const MaxArchiveExtractEntries = 100000

// maxArchiveItemBytes caps one decoded archive item (64MB). Both passes
// decode each item fully into memory, and a hostile archive stays valid
// (deflate shrinks it to a few MB) while declaring gigabytes in a single
// string value, so without a per-item cap one "full_text": "AAA..." entry
// would OOM a small VPS. 64MB is far beyond any real tweet, account, or
// connection object.
const maxArchiveItemBytes = 64 << 20

// Summary reports the imported row counts
// (TwitterArchiveImporter#build_summary).
type Summary struct {
	Tweets     int64
	Followers  int64
	Following  int64
	Likes      int64
	TotalItems int64
}

// Importer streams a Twitter archive ZIP into the archive tables.
type Importer struct {
	DB         *sql.DB
	DataDir    string // media files land under DataDir/files (media layout)
	SourcePath string
	Progress   func(progress int, message string) // optional callback
	BatchSize  int                                // <= 0 uses DefaultTweetBatchSize

	q *query.Queries

	lastProgress int
	lastMessage  string
}

// candidate is one parsed tweet row awaiting its batch commit.
type candidate struct {
	tweetID    string
	entryType  string
	screenName string
	fullText   string
	tweetedAt  int64
	media      []string // zip entry names
}

type connectionRow struct {
	accountID string
	userLink  string
	relType   string
}

type likeRow struct {
	tweetID     string
	fullText    string
	expandedURL string
}

// scan holds the result of the validation pass: dedup key sets (the Rails
// tweets_by_id/seen_connections/seen_likes equivalents, without the rows),
// the media index and the discovered account name.
type scan struct {
	summary        Summary
	mediaIndex     map[string][]string // tweet id -> sorted media entry names
	accountName    string
	tweetIDs       map[string]struct{}
	connectionKeys map[string]struct{}
	likeIDs        map[string]struct{}
	dataEntries    int
}

// Import runs the full import, mirroring TwitterArchiveImporter#import!:
// validate, scan (no writes), then replace the stored archive.
func (im *Importer) Import(ctx context.Context) (Summary, error) {
	im.q = query.New(im.DB)
	im.report(5, "Validating archive")
	if err := im.validateSource(); err != nil {
		return Summary{}, err
	}

	im.report(25, "Scanning archive")
	sc, err := im.scanPass(ctx)
	if err != nil {
		return Summary{}, err
	}
	if sc.summary.TotalItems == 0 {
		return Summary{}, errors.New("No supported archive items found in archive")
	}

	im.report(55, "Archive parsed")
	im.report(80, "Replacing stored archive")
	if err := im.replacePass(ctx, sc); err != nil {
		return Summary{}, err
	}

	im.report(100, "Import completed")
	return sc.summary, nil
}

// validateSource mirrors ensure_zip_file!.
func (im *Importer) validateSource() error {
	if im.SourcePath == "" {
		return errors.New("Archive file not found")
	}
	if _, err := os.Stat(im.SourcePath); err != nil {
		return errors.New("Archive file not found")
	}
	if !strings.EqualFold(filepath.Ext(im.SourcePath), ".zip") {
		return errors.New("Archive file must be a zip")
	}
	return nil
}

// report mirrors report_progress: clamped to 0-100, identical consecutive
// pairs are emitted only once.
func (im *Importer) report(progress int, message string) {
	if im.Progress == nil {
		return
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	if progress == im.lastProgress && message == im.lastMessage {
		return
	}
	im.lastProgress, im.lastMessage = progress, message
	im.Progress(progress, message)
}

func (im *Importer) batchSize() int {
	if im.BatchSize > 0 {
		return im.BatchSize
	}
	return DefaultTweetBatchSize
}

// scanPass streams every data entry once, validating JSON and collecting the
// dedup key sets; nothing is written to the database or disk.
func (im *Importer) scanPass(ctx context.Context) (*scan, error) {
	zr, err := zip.OpenReader(im.SourcePath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	sc := &scan{
		mediaIndex:     buildMediaIndex(&zr.Reader),
		tweetIDs:       map[string]struct{}{},
		connectionKeys: map[string]struct{}{},
		likeIDs:        map[string]struct{}{},
	}
	// Refuse bombs before anything is written: the declared uncompressed
	// sizes bound what extraction can actually write (Go's zip reader fails
	// an entry that outgrows its declared size). The comparison runs before
	// the addition: declared stays <= MaxArchiveExtractBytes across
	// iterations, so the subtraction cannot underflow and the running total
	// can never wrap around past the limit on a forged zip64 entry. The
	// entry count is capped too: a flood of tiny entries stays under the
	// byte limit but would exhaust inodes when media lands on disk.
	var declared uint64
	var entries int
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		entries++
		if entries > MaxArchiveExtractEntries {
			return nil, fmt.Errorf("twitter archive: ZIP has more than %d file entries", MaxArchiveExtractEntries)
		}
		if f.UncompressedSize64 > MaxArchiveExtractBytes-declared {
			return nil, fmt.Errorf("twitter archive: ZIP entry %q declares %d uncompressed bytes, over the %d total limit", f.Name, f.UncompressedSize64, MaxArchiveExtractBytes)
		}
		declared += f.UncompressedSize64
		if isArchiveDataEntry(f.Name) {
			sc.dataEntries++
		}
	}

	done := 0
	err = im.walkDataEntries(ctx, &zr.Reader, func(_ context.Context, item map[string]any, entryType string) error {
		if sc.accountName == "" {
			sc.accountName = extractAccountName(item)
		}
		if c, ok := buildCandidate(item, sc.accountName, sc.mediaIndex); ok {
			if _, seen := sc.tweetIDs[c.tweetID]; !seen {
				sc.tweetIDs[c.tweetID] = struct{}{}
				sc.summary.Tweets++
			}
		}
		switch entryType {
		case "follower", "following":
			if row, ok := extractConnection(item, entryType); ok {
				key := row.relType + ":" + row.accountID
				if _, seen := sc.connectionKeys[key]; !seen {
					sc.connectionKeys[key] = struct{}{}
					if entryType == "follower" {
						sc.summary.Followers++
					} else {
						sc.summary.Following++
					}
				}
			}
		case "like":
			if row, ok := extractLike(item); ok {
				if _, seen := sc.likeIDs[row.tweetID]; !seen {
					sc.likeIDs[row.tweetID] = struct{}{}
					sc.summary.Likes++
				}
			}
		}
		return nil
	}, func() {
		done++
		im.report(25+30*done/max(sc.dataEntries, 1), "Scanning archive")
	})
	if err != nil {
		return nil, err
	}
	sc.summary.TotalItems = sc.summary.Tweets + sc.summary.Followers + sc.summary.Following + sc.summary.Likes
	return sc, nil
}

// replacePass clears the three archive tables (plus the tweet media
// attachments and their files) in one transaction, then streams the archive
// again writing rows in batches.
func (im *Importer) replacePass(ctx context.Context, sc *scan) error {
	zr, err := zip.OpenReader(im.SourcePath)
	if err != nil {
		return err
	}
	defer zr.Close()

	if err := im.clearStoredArchive(ctx); err != nil {
		return err
	}

	mediaFiles := map[string]*zip.File{}
	for _, f := range zr.File {
		if isArchiveMediaEntry(f.Name) {
			mediaFiles[f.Name] = f
		}
	}

	sink := &dbSink{
		im:         im,
		scan:       sc,
		mediaFiles: mediaFiles,
		mediaIDs:   map[string]int64{},
	}
	done := 0
	if err := im.walkDataEntries(ctx, &zr.Reader, sink.consume, func() {
		done++
		im.report(80+19*done/max(sc.dataEntries, 1), "Replacing stored archive")
	}); err != nil {
		return err
	}
	return sink.flush(ctx)
}

// fileReferenced reports whether a files row is still referenced by an
// attachment, a static_files entry or a /files/<key> URL in stored content.
// static_files.file_id references files(id) under foreign_keys enforcement,
// and a database import can merge a static file onto a row that tweet media
// also uses, so both count; content embeds files by URL without any
// attachment row (the model ReapOrphanFiles already uses), so a URL reuse in
// an article, page, comment, setting or redirect counts too.
func fileReferenced(ctx context.Context, q *query.Queries, id int64, key string) (bool, error) {
	attached, err := q.CountAttachmentsForFile(ctx, id)
	if err != nil {
		return false, err
	}
	static, err := q.CountStaticFilesForFile(ctx, id)
	if err != nil {
		return false, err
	}
	content, err := q.CountFileKeyContentReferences(ctx, sql.NullString{String: key, Valid: true})
	if err != nil {
		return false, err
	}
	return attached+static+content > 0, nil
}

// clearStoredArchive mirrors the destroy_all transaction of
// replace_archive_data plus the has_many_attached dependent: :purge_later
// cleanup of tweet media (attachments, unreferenced files rows, disk files).
func (im *Importer) clearStoredArchive(ctx context.Context) error {
	type fileRef struct {
		id  int64
		key string
	}
	var doomed []fileRef // file rows deleted in the tx, unlinked after commit

	tx, err := im.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("twitter archive: begin clear tx: %w", err)
	}
	defer tx.Rollback()
	q := im.q.WithTx(tx)

	mediaFiles, err := q.ListTwitterArchiveTweetMediaFiles(ctx)
	if err != nil {
		return fmt.Errorf("twitter archive: list old media: %w", err)
	}
	if err := q.DeleteTwitterArchiveTweetAttachments(ctx); err != nil {
		return fmt.Errorf("twitter archive: delete old attachments: %w", err)
	}
	for _, mf := range mediaFiles {
		referenced, err := fileReferenced(ctx, q, mf.ID, mf.Key)
		if err != nil {
			return fmt.Errorf("twitter archive: count file references: %w", err)
		}
		if referenced {
			continue // still referenced elsewhere: keep row, variants and disk file
		}
		variants, err := q.ListFileVariants(ctx, sql.NullInt64{Int64: mf.ID, Valid: true})
		if err != nil {
			return fmt.Errorf("twitter archive: list old media variants: %w", err)
		}
		refs := make([]fileRef, 0, len(variants))
		shared := false
		for _, v := range variants {
			vReferenced, err := fileReferenced(ctx, q, v.ID, v.Key)
			if err != nil {
				return fmt.Errorf("twitter archive: count file references: %w", err)
			}
			if vReferenced {
				// A variant that is itself referenced pins its original via
				// files.variant_of: keep the whole family.
				shared = true
				break
			}
			refs = append(refs, fileRef{id: v.ID, key: v.Key})
		}
		if shared {
			continue
		}
		// Variants reference their original via files.variant_of; under
		// foreign_keys enforcement they must be deleted before the originals.
		for _, ref := range refs {
			if err := q.DeleteFile(ctx, ref.id); err != nil {
				return fmt.Errorf("twitter archive: delete file row: %w", err)
			}
			doomed = append(doomed, ref)
		}
		if err := q.DeleteFile(ctx, mf.ID); err != nil {
			return fmt.Errorf("twitter archive: delete file row: %w", err)
		}
		doomed = append(doomed, fileRef{id: mf.ID, key: mf.Key})
	}
	if err := q.DeleteAllTwitterArchiveTweets(ctx); err != nil {
		return fmt.Errorf("twitter archive: clear tweets: %w", err)
	}
	if err := q.DeleteAllTwitterArchiveConnections(ctx); err != nil {
		return fmt.Errorf("twitter archive: clear connections: %w", err)
	}
	if err := q.DeleteAllTwitterArchiveLikes(ctx); err != nil {
		return fmt.Errorf("twitter archive: clear likes: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("twitter archive: commit clear tx: %w", err)
	}

	// Disk files are removed only after the transaction commits; failures are
	// logged, never fatal (mirrors purge_later best-effort semantics). The
	// key comes from the files table, which a database import fills verbatim:
	// a crafted bundle could plant a traversal (or short, slice-panicking)
	// key here, so only well-formed keys ever reach a disk path. The row is
	// already deleted; the blob is left alone when the key looks unsafe.
	for _, ref := range doomed {
		if !media.ValidKey(ref.key) {
			slog.Warn("twitter archive: skip blob removal, unsafe key", "key", ref.key)
			continue
		}
		if err := os.Remove(im.mediaPath(ref.key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("twitter archive: remove replaced media file", "key", ref.key, "error", err)
		}
	}
	return nil
}

// dbSink is the pass-2 consumer: it buffers parsed rows and commits them in
// batches, extracting media to disk before each tweet batch transaction
// opens (slow disk writes stay out of the SQLite write lock, like the Rails
// upload_media_blobs ordering).
type dbSink struct {
	im         *Importer
	scan       *scan
	mediaFiles map[string]*zip.File
	mediaIDs   map[string]int64 // zip entry name -> files.id
	// newMedia tracks media stored since the last committed tweet batch.
	// Their files rows live outside the batch transaction, so a batch
	// failure would orphan them; discardNewMedia reclaims them.
	newMedia []storedMediaRef

	tweets      []candidate
	connections []connectionRow
	likes       []likeRow
}

// storedMediaRef identifies one files row + disk blob written by
// storeMediaEntry outside the tweet batch transaction.
type storedMediaRef struct {
	id  int64
	key string
}

func (s *dbSink) consume(ctx context.Context, item map[string]any, entryType string) error {
	if c, ok := buildCandidate(item, s.scan.accountName, s.scan.mediaIndex); ok {
		s.tweets = append(s.tweets, c)
		if len(s.tweets) >= s.im.batchSize() {
			if err := s.flushTweets(ctx); err != nil {
				return err
			}
		}
	}
	switch entryType {
	case "follower", "following":
		if row, ok := extractConnection(item, entryType); ok {
			s.connections = append(s.connections, row)
			if len(s.connections) >= s.im.batchSize() {
				if err := s.flushConnections(ctx); err != nil {
					return err
				}
			}
		}
	case "like":
		if row, ok := extractLike(item); ok {
			s.likes = append(s.likes, row)
			if len(s.likes) >= s.im.batchSize() {
				if err := s.flushLikes(ctx); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *dbSink) flush(ctx context.Context) error {
	if err := s.flushTweets(ctx); err != nil {
		return err
	}
	if err := s.flushConnections(ctx); err != nil {
		return err
	}
	return s.flushLikes(ctx)
}

// flushTweets stores the buffered media entries on disk (outside the
// transaction) and then upserts the batch in one transaction.
func (s *dbSink) flushTweets(ctx context.Context) error {
	if len(s.tweets) == 0 {
		return nil
	}
	batch := s.tweets
	s.tweets = nil

	// Media extraction happens before the transaction opens.
	attached := make([][]int64, len(batch))
	for i, c := range batch {
		for _, entryName := range c.media {
			fileID, err := s.mediaFileID(ctx, entryName)
			if err != nil {
				// Media already stored for this batch is unreferenced
				// (the batch transaction never opened): reclaim it like
				// the commit-failure path below does.
				s.discardNewMedia(ctx)
				return err
			}
			if fileID > 0 {
				attached[i] = append(attached[i], fileID)
			}
		}
	}

	if err := s.commitTweetBatch(ctx, batch, attached); err != nil {
		// The batch transaction has rolled back by now, so the media rows
		// stored for it above are unreferenced orphans: reclaim them.
		s.discardNewMedia(ctx)
		return err
	}
	s.newMedia = nil // committed; now protected by attachment links
	return nil
}

// commitTweetBatch upserts one batch of tweets and their media attachments
// in a single transaction.
func (s *dbSink) commitTweetBatch(ctx context.Context, batch []candidate, attached [][]int64) error {
	tx, err := s.im.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("twitter archive: begin tweet batch: %w", err)
	}
	defer tx.Rollback()
	q := s.im.q.WithTx(tx)
	now := time.Now().Unix()
	for i, c := range batch {
		id, err := q.UpsertTwitterArchiveTweet(ctx, query.UpsertTwitterArchiveTweetParams{
			TweetID:    c.tweetID,
			ScreenName: c.screenName,
			FullText:   c.fullText,
			EntryType:  c.entryType,
			TweetedAt:  c.tweetedAt,
			CreatedAt:  now,
			UpdatedAt:  now,
		})
		if err != nil {
			return fmt.Errorf("twitter archive: upsert tweet %s: %w", c.tweetID, err)
		}
		for _, fileID := range attached[i] {
			if err := q.CreateAttachment(ctx, query.CreateAttachmentParams{
				FileID:     fileID,
				RecordType: "TwitterArchiveTweet",
				RecordID:   id,
				Name:       "media",
				CreatedAt:  now,
			}); err != nil {
				return fmt.Errorf("twitter archive: attach media: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("twitter archive: commit tweet batch: %w", err)
	}
	return nil
}

// discardNewMedia reclaims the files rows and disk blobs stored for a tweet
// batch whose transaction failed. It must run after the batch transaction
// has rolled back (its attachment rows would otherwise still reference the
// files and block the deletes). Best effort: failures are logged, not
// fatal, mirroring the purge_later semantics of clearStoredArchive.
//
// The batch failure may be a canceled ctx (worker shutdown), and the cleanup
// must still run or the fresh rows and blobs would stay orphaned — so it
// goes through a context that cannot be canceled. No attachment liveness
// check is needed before deleting: every ref was created by storeMediaEntry
// within this flush window, and the only attachments that could reference it
// belonged to the rolled-back batch, so the rows are never live.
func (s *dbSink) discardNewMedia(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	for _, ref := range s.newMedia {
		if err := s.im.q.DeleteFile(ctx, ref.id); err != nil {
			slog.Warn("twitter archive: delete orphan media row", "id", ref.id, "error", err)
			continue
		}
		if err := os.Remove(s.im.mediaPath(ref.key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("twitter archive: remove orphan media file", "key", ref.key, "error", err)
		}
	}
	s.newMedia = nil
}

// mediaFileID returns the files row id for a media zip entry, extracting it
// to disk on first sight. Zero means the entry was empty or missing (skipped,
// like the Rails importer's blank/missing handling).
func (s *dbSink) mediaFileID(ctx context.Context, entryName string) (int64, error) {
	if id, ok := s.mediaIDs[entryName]; ok {
		return id, nil
	}
	f, ok := s.mediaFiles[entryName]
	if !ok {
		slog.Warn("Twitter archive media not found in zip", "entry", entryName)
		s.mediaIDs[entryName] = 0
		return 0, nil
	}
	id, key, err := s.im.storeMediaEntry(ctx, f)
	if err != nil {
		return 0, err
	}
	if id > 0 {
		s.newMedia = append(s.newMedia, storedMediaRef{id: id, key: key})
	}
	s.mediaIDs[entryName] = id
	return id, nil
}

func (s *dbSink) flushConnections(ctx context.Context) error {
	if len(s.connections) == 0 {
		return nil
	}
	batch := s.connections
	s.connections = nil
	tx, err := s.im.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("twitter archive: begin connection batch: %w", err)
	}
	defer tx.Rollback()
	q := s.im.q.WithTx(tx)
	now := time.Now().Unix()
	for _, row := range batch {
		if err := q.InsertTwitterArchiveConnection(ctx, query.InsertTwitterArchiveConnectionParams{
			AccountID:        row.accountID,
			UserLink:         nullString(row.userLink),
			RelationshipType: row.relType,
			CreatedAt:        now,
			UpdatedAt:        now,
		}); err != nil {
			return fmt.Errorf("twitter archive: insert connection: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("twitter archive: commit connection batch: %w", err)
	}
	return nil
}

func (s *dbSink) flushLikes(ctx context.Context) error {
	if len(s.likes) == 0 {
		return nil
	}
	batch := s.likes
	s.likes = nil
	tx, err := s.im.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("twitter archive: begin like batch: %w", err)
	}
	defer tx.Rollback()
	q := s.im.q.WithTx(tx)
	now := time.Now().Unix()
	for _, row := range batch {
		if err := q.InsertTwitterArchiveLike(ctx, query.InsertTwitterArchiveLikeParams{
			TweetID:     row.tweetID,
			FullText:    nullString(row.fullText),
			ExpandedUrl: nullString(row.expandedURL),
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			return fmt.Errorf("twitter archive: insert like: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("twitter archive: commit like batch: %w", err)
	}
	return nil
}

// walkDataEntries streams every archive data entry (data/*.js|json, in zip
// order) item by item. entryType is the entry basename without extension,
// mirroring archive_entry_type. afterEntry runs once per consumed entry.
func (im *Importer) walkDataEntries(ctx context.Context, zr *zip.Reader, consume func(ctx context.Context, item map[string]any, entryType string) error, afterEntry func()) error {
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !isArchiveDataEntry(f.Name) {
			continue
		}
		if err := streamEntryItems(ctx, f, consume, entryTypeOf(f.Name)); err != nil {
			return err
		}
		if afterEntry != nil {
			afterEntry()
		}
	}
	return nil
}

// streamEntryItems opens one zip entry and decodes its JS/JSON payload
// without ever buffering the whole entry. A single item larger than
// maxArchiveItemBytes fails the import (errItemTooLarge) instead of being
// materialized in memory.
func streamEntryItems(ctx context.Context, f *zip.File, consume func(ctx context.Context, item map[string]any, entryType string) error, entryType string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	br, err := stripJSPayloadPrefix(rc)
	if err != nil {
		return err
	}
	if br == nil {
		return nil // blank payload: parse_js_payload returns nil
	}
	lim := &itemBudgetReader{r: br}
	dec := json.NewDecoder(lim)
	dec.UseNumber()

	if peek, err := peekNonSpace(br); err != nil {
		return fmt.Errorf("invalid JSON payload: %w", err)
	} else if peek == '[' {
		lim.reset()
		if _, err := dec.Token(); err != nil {
			return err
		}
		for {
			lim.reset()
			if !dec.More() {
				break
			}
			var item any
			if err := dec.Decode(&item); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if m, ok := item.(map[string]any); ok {
				if err := consume(ctx, m, entryType); err != nil {
					return err
				}
			}
		}
		lim.reset()
		if _, err := dec.Token(); err != nil {
			return err
		}
		return nil
	}
	// Single top-level value (an object in practice).
	lim.reset()
	var item any
	if err := dec.Decode(&item); err != nil {
		return err
	}
	if m, ok := item.(map[string]any); ok {
		return consume(ctx, m, entryType)
	}
	return nil
}

// errItemTooLarge aborts an import when one archive item pulls more than
// maxArchiveItemBytes from the entry stream; like any malformed payload, the
// entry cannot be resumed mid-item, so the whole import fails.
var errItemTooLarge = errors.New("twitter archive: single item exceeds the 64MB size limit")

// itemBudgetReader bounds how many entry bytes one decoder step may pull.
// json.Decoder keeps its own buffer, so an io.LimitReader around the whole
// entry would cap the entry rather than the item; instead the budget is
// reset before every Token/More/Decode step, letting a stream of any length
// through while a multi-GB single value errors out during the scan, before
// its string is materialized.
type itemBudgetReader struct {
	r         io.Reader
	remaining int64
}

func (l *itemBudgetReader) reset() { l.remaining = maxArchiveItemBytes }

func (l *itemBudgetReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, errItemTooLarge
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	return n, err
}

// stripJSPayloadPrefix mirrors parse_js_payload: content starting with '{' or
// '[' is raw JSON; otherwise everything up to the first '=' (the
// window.YTD.*.part0 assignment) plus following whitespace is dropped. A
// blank payload yields a nil reader. The trailing ";" is never consumed
// because the JSON decoder stops after the top-level value.
func stripJSPayloadPrefix(rc io.Reader) (*bufio.Reader, error) {
	br := bufio.NewReaderSize(rc, 64*1024)
	b, err := peekNonSpace(br)
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if b == '{' || b == '[' {
		return br, nil
	}
	// Skip through the first '='; JSON.parse raises when there is none.
	for {
		c, err := br.ReadByte()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("invalid JSON payload")
		}
		if err != nil {
			return nil, err
		}
		if c == '=' {
			break
		}
	}
	// The \s* after '=' is skipped lazily by the first peek of the decoder
	// path (peekNonSpace), so nothing more to do here.
	return br, nil
}

// peekNonSpace returns the first non-whitespace byte without consuming it.
func peekNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		return b, br.UnreadByte()
	}
}

func isArchiveDataEntry(name string) bool {
	if strings.HasSuffix(name, "/") {
		return false
	}
	return strings.HasPrefix(name, "data/") &&
		(strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".json"))
}

func isArchiveMediaEntry(name string) bool {
	if strings.HasSuffix(name, "/") {
		return false
	}
	for _, dir := range mediaDirectories {
		if strings.HasPrefix(name, dir) {
			return true
		}
	}
	return false
}

// entryTypeOf mirrors archive_entry_type: basename without extension.
func entryTypeOf(name string) string {
	base := path.Base(name)
	return strings.TrimSuffix(base, path.Ext(base))
}

// buildMediaIndex mirrors build_media_index: tweet id -> sorted unique media
// entry names, keyed by the leading digits of the media file basename.
func buildMediaIndex(zr *zip.Reader) map[string][]string {
	index := map[string][]string{}
	for _, f := range zr.File {
		if !isArchiveMediaEntry(f.Name) {
			continue
		}
		if id := leadingDigits(f.Name); id != "" {
			index[id] = append(index[id], f.Name)
		}
	}
	for id, names := range index {
		sort.Strings(names)
		index[id] = dedupeStrings(names)
	}
	return index
}

// leadingDigits mirrors extract_media_tweet_ids (/\A\d+/ on the basename
// without extension).
func leadingDigits(entryName string) string {
	base := path.Base(entryName)
	base = strings.TrimSuffix(base, path.Ext(base))
	i := 0
	for i < len(base) && base[i] >= '0' && base[i] <= '9' {
		i++
	}
	return base[:i]
}

func dedupeStrings(sorted []string) []string {
	out := sorted[:0]
	var prev string
	for i, s := range sorted {
		if i == 0 || s != prev {
			out = append(out, s)
			prev = s
		}
	}
	return out
}

// buildCandidate mirrors build_tweet_row plus its skip rules; ok is false
// when the row would be skipped (blank id, blank tweeted_at, or blank text
// without media).
func buildCandidate(item map[string]any, accountName string, mediaIndex map[string][]string) (candidate, bool) {
	tweet, ok := extractTweet(item)
	if !ok {
		return candidate{}, false
	}
	id := firstPresent(tweet, "id_str", "id", "tweet_id")
	if id == "" {
		return candidate{}, false
	}
	c := candidate{
		tweetID:    id,
		entryType:  extractEntryType(tweet),
		screenName: extractScreenName(tweet, accountName),
		fullText:   stringify(extractFullText(tweet)),
		media:      mediaEntriesFor(tweet, id, mediaIndex),
	}
	tweetedAt, ok := extractTweetedAt(tweet)
	if !ok {
		return candidate{}, false
	}
	c.tweetedAt = tweetedAt
	if c.fullText == "" && len(c.media) == 0 {
		return candidate{}, false
	}
	return c, true
}

// extractTweet mirrors extract_tweets for a single item: the "tweet" wrapper
// hash, or the item itself when it looks like a tweet.
func extractTweet(item map[string]any) (map[string]any, bool) {
	if t, ok := item["tweet"].(map[string]any); ok {
		return t, true
	}
	if tweetLikeHash(item) {
		return item, true
	}
	return nil, false
}

// tweetLikeHash mirrors tweet_like_hash?.
func tweetLikeHash(item map[string]any) bool {
	for _, key := range []string{"id", "id_str", "full_text", "created_at", "text"} {
		if _, ok := item[key]; ok {
			return true
		}
	}
	return false
}

func extractAccountName(item map[string]any) string {
	account, _ := item["account"].(map[string]any)
	return presentString(account["username"])
}

// extractTweetedAt mirrors extract_tweeted_at; ok is false when no usable
// timestamp exists (the Rails row-skip rule for blank tweeted_at).
func extractTweetedAt(tweet map[string]any) (int64, bool) {
	for _, v := range []any{
		tweet["created_at"],
		tweet["createdAt"],
		dig(tweet, "legacy", "created_at"),
	} {
		if s := presentString(v); s != "" {
			return parseArchiveTime(s)
		}
	}
	if s := presentString(tweet["timestamp_ms"]); s != "" {
		ms, err := strconv.ParseFloat(s, 64)
		if err != nil {
			ms = 0 // mirrors "abc".to_f == 0.0
		}
		return int64(ms / 1000.0), true
	}
	return 0, false
}

// parseArchiveTime accepts the timestamp shapes Time.zone.parse sees in
// official archives. Zone-less strings fall back to UTC (deviation from
// Time.zone.parse, which would use the site zone; real archives always carry
// an offset).
func parseArchiveTime(s string) (int64, bool) {
	layouts := []string{
		"Mon Jan 2 15:04:05 -0700 2006", // Twitter archive format
		time.RFC3339,
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}

func extractScreenName(tweet map[string]any, accountName string) string {
	if accountName != "" {
		return accountName
	}
	if s := presentString(dig(tweet, "user", "screen_name")); s != "" {
		return s
	}
	if s := presentString(dig(tweet, "legacy", "user", "screen_name")); s != "" {
		return s
	}
	return "archive"
}

// extractEntryType mirrors extract_entry_type.
func extractEntryType(tweet map[string]any) string {
	if present(tweet["retweeted_status_id_str"]) || present(tweet["retweeted_status"]) ||
		present(tweet["quoted_status_id_str"]) || present(tweet["quoted_status"]) ||
		strings.HasPrefix(stringify(extractFullText(tweet)), "RT @") {
		return EntryTypeRetweetQuote
	}
	if present(tweet["in_reply_to_status_id_str"]) || present(tweet["in_reply_to_user_id_str"]) ||
		present(tweet["in_reply_to_screen_name"]) {
		return EntryTypeReply
	}
	return EntryTypeTweet
}

// extractFullText mirrors extract_full_text.
func extractFullText(tweet map[string]any) any {
	for _, v := range []any{
		tweet["full_text"],
		tweet["text"],
		dig(tweet, "legacy", "full_text"),
		dig(tweet, "legacy", "text"),
		dig(tweet, "note_tweet", "text"),
	} {
		if present(v) {
			return v
		}
	}
	return ""
}

// mediaEntriesFor mirrors media_entries_for: the indexed entries for the
// tweet id, narrowed to the basenames referenced by the tweet entities when
// any are present.
func mediaEntriesFor(tweet map[string]any, tweetID string, mediaIndex map[string][]string) []string {
	entryNames := mediaIndex[tweetID]
	if len(entryNames) == 0 {
		return nil
	}
	referenced := referencedMediaBasenames(tweet)
	if len(referenced) == 0 {
		return entryNames
	}
	var filtered []string
	for _, entryName := range entryNames {
		filename := path.Base(entryName)
		for _, base := range referenced {
			if filename == base || strings.HasSuffix(filename, "-"+base) {
				filtered = append(filtered, entryName)
				break
			}
		}
	}
	if len(filtered) > 0 {
		return filtered
	}
	return entryNames
}

// referencedMediaBasenames mirrors extract_referenced_media_basenames.
func referencedMediaBasenames(tweet map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range mediaEntitiesFor(tweet) {
		var base string
		for _, v := range []any{m["media_url_https"], m["media_url"]} {
			if base = urlBasename(presentString(v)); base != "" {
				break
			}
		}
		if base == "" {
			base = videoVariantBasename(m)
		}
		if base != "" && !seen[base] {
			seen[base] = true
			out = append(out, base)
		}
	}
	return out
}

// mediaEntitiesFor mirrors media_entities_for: the first non-empty media
// list among the extended/entities paths.
func mediaEntitiesFor(tweet map[string]any) []map[string]any {
	for _, v := range []any{
		dig(tweet, "extended_entities", "media"),
		dig(tweet, "entities", "media"),
		dig(tweet, "legacy", "extended_entities", "media"),
		dig(tweet, "legacy", "entities", "media"),
	} {
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			continue
		}
		var out []map[string]any
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// videoVariantBasename mirrors extract_video_variant_basename.
func videoVariantBasename(media map[string]any) string {
	variants, _ := dig(media, "video_info", "variants").([]any)
	for _, v := range variants {
		if m, ok := v.(map[string]any); ok {
			if base := urlBasename(presentString(m["url"])); base != "" {
				return base
			}
		}
	}
	return ""
}

// urlBasename mirrors extract_media_basename.
func urlBasename(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

// extractConnection mirrors extract_connection_rows for a single item.
func extractConnection(item map[string]any, relType string) (connectionRow, bool) {
	conn, _ := item[relType].(map[string]any)
	if conn == nil {
		return connectionRow{}, false
	}
	accountID := firstPresent(conn, "accountId", "account_id")
	if accountID == "" {
		return connectionRow{}, false
	}
	return connectionRow{
		accountID: accountID,
		userLink:  firstPresent(conn, "userLink", "user_link"),
		relType:   relType,
	}, true
}

// extractLike mirrors extract_like_rows for a single item.
func extractLike(item map[string]any) (likeRow, bool) {
	like, _ := item["like"].(map[string]any)
	if like == nil {
		return likeRow{}, false
	}
	tweetID := firstPresent(like, "tweetId", "tweet_id")
	if tweetID == "" {
		return likeRow{}, false
	}
	return likeRow{
		tweetID:     tweetID,
		fullText:    firstPresent(like, "fullText", "full_text"),
		expandedURL: firstPresent(like, "expandedUrl", "expanded_url"),
	}, true
}

// storeMediaEntry streams one media zip entry to the media disk layout
// (DataDir/files/xx/yy/<key>, the ActiveStorage-compatible layout of the
// media service) and inserts the files row. The entry is never fully
// buffered: the content type is sniffed from the first 512 bytes and the
// body is copied straight to disk. Returns the new row id and its blob key;
// id 0 means the entry was empty (skipped like the Rails importer's blank
// check).
func (im *Importer) storeMediaEntry(ctx context.Context, f *zip.File) (int64, string, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, "", err
	}
	defer rc.Close()

	key, err := newFileKey()
	if err != nil {
		return 0, "", err
	}
	filePath := im.mediaPath(key)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return 0, "", fmt.Errorf("twitter archive: create media dir: %w", err)
	}
	out, err := os.Create(filePath)
	if err != nil {
		return 0, "", fmt.Errorf("twitter archive: create media file: %w", err)
	}

	hash := md5.New()
	w := io.MultiWriter(out, hash)
	head := make([]byte, 512)
	headLen, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		out.Close()
		os.Remove(filePath)
		return 0, "", fmt.Errorf("twitter archive: read media: %w", err)
	}
	head = head[:headLen]
	if _, err := w.Write(head); err != nil {
		out.Close()
		os.Remove(filePath)
		return 0, "", fmt.Errorf("twitter archive: extract media: %w", err)
	}
	rest, err := io.Copy(w, rc)
	if err != nil {
		out.Close()
		os.Remove(filePath)
		return 0, "", fmt.Errorf("twitter archive: extract media: %w", err)
	}
	size := int64(headLen) + rest
	if err := out.Close(); err != nil {
		os.Remove(filePath)
		return 0, "", fmt.Errorf("twitter archive: write media: %w", err)
	}
	if size == 0 {
		os.Remove(filePath)
		return 0, "", nil
	}

	filename := path.Base(f.Name)
	contentType := mediaContentType(head, filename)
	sum := base64.StdEncoding.EncodeToString(hash.Sum(nil))
	row, err := im.q.CreateFile(ctx, query.CreateFileParams{
		Key:         key,
		Filename:    filename,
		ContentType: sql.NullString{String: contentType, Valid: contentType != ""},
		ByteSize:    size,
		Checksum:    sql.NullString{String: sum, Valid: true},
		CreatedAt:   time.Now().Unix(),
	})
	if err != nil {
		os.Remove(filePath)
		return 0, "", fmt.Errorf("twitter archive: insert media file row: %w", err)
	}
	return row.ID, key, nil
}

// mediaPath mirrors media.Service.PathFor.
func (im *Importer) mediaPath(key string) string {
	return filepath.Join(im.DataDir, "files", key[0:2], key[2:4], key)
}

// mediaContentType sniffs the magic bytes, falling back to the file
// extension like Marcel does when the content is not recognized.
func mediaContentType(head []byte, filename string) string {
	ct := http.DetectContentType(head)
	if ct == "application/octet-stream" || strings.HasPrefix(ct, "text/plain") {
		if byExt := mime.TypeByExtension(strings.ToLower(path.Ext(filename))); byExt != "" {
			return byExt
		}
	}
	return ct
}

// newFileKey mirrors the media service's key generation (16 random bytes,
// hex-encoded).
func newFileKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("twitter archive: generate file key: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// dig walks nested string-keyed maps.
func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

// present mirrors ActiveSupport present? for JSON values: nil, blank strings,
// empty maps/slices and false are absent.
func present(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return !domain.IsBlank(t)
	case bool:
		return t
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	}
	return true
}

// presentString returns the string form of v, or "" when absent.
func presentString(v any) string {
	if !present(v) {
		return ""
	}
	return stringify(v)
}

// stringify mirrors the to_s calls of the Rails extractor (ids arrive as
// strings or numbers depending on the payload).
func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

// firstPresent returns the first present string value among keys.
func firstPresent(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := presentString(m[k]); s != "" {
			return s
		}
	}
	return ""
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// sourceFilePresent reports whether the import's source zip is still on disk
// to import: a NULL/empty path or a missing file both mean the terminal
// cleanup already ran. Other stat errors count as present, so the import
// fails on the real error instead of being silently skipped.
func sourceFilePresent(path sql.NullString) bool {
	if !path.Valid || path.String == "" {
		return false
	}
	_, err := os.Stat(path.String)
	return !errors.Is(err, os.ErrNotExist)
}

// removeSourceFile deletes the uploaded archive zip once the import reached a
// terminal state. path comes from the twitter_archive_imports row, which a
// crafted transfer bundle fills verbatim, so it is removed only when it is a
// path this feature owns: directly inside <dataDir>/imports with the
// twitter_archive_ prefix storeTwitterArchiveUpload writes (the same
// ownership rule jobs.CleanupOrphanImportFiles sweeps by). Without the check
// a planted row would make the re-executed job os.Remove an arbitrary server
// file once the import fails on the fake path; such a path is only logged.
func removeSourceFile(dataDir, path string) {
	if filepath.Dir(path) != filepath.Join(dataDir, "imports") ||
		!strings.HasPrefix(filepath.Base(path), "twitter_archive_") {
		slog.Warn("twitter archive import: skip source removal, unexpected path", "path", path)
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("twitter archive import: remove source", "path", path, "error", err)
	}
}

// RecoverStaleImports fails twitter_archive_imports rows stuck in queued or
// running since before cutoff and releases their active_slot, so an import
// interrupted by a process crash does not block new imports forever
// (idx_tai_active_slot allows a single active row, and
// HasActiveTwitterArchiveImport rejects submissions while one exists). It
// runs at startup before the worker starts polling and returns the number of
// rows recovered. A row whose job is still legitimately queued self-heals:
// the handler re-marks it running (re-claiming active_slot) when the job
// executes; if a newer import took the freed slot in between, the unique
// index on active_slot fails the late re-mark instead of allowing two
// concurrent imports.
//
// Caveat: the cutoff leaves a blind spot when a crashed process is relaunched
// within the cutoff window (an instant supervisor restart). Rows the dead
// process touched last still carry an updated_at newer than the cutoff, so
// recovery skips them: they stay queued/running with active_slot held and
// block new submissions even though no live process owns them. They heal on
// the next restart after their updated_at falls behind the cutoff, when
// recovery finally fails them.
func RecoverStaleImports(ctx context.Context, q *query.Queries, cutoff time.Time) (int64, error) {
	now := time.Now().Unix()
	return q.FailStaleTwitterArchiveImports(ctx, query.FailStaleTwitterArchiveImportsParams{
		ErrorMessage: nullString("Process restarted before the import finished"),
		FinishedAt:   sql.NullInt64{Int64: now, Valid: true},
		Now:          now,
		Cutoff:       cutoff.Unix(),
	})
}

// RegisterImportHandler installs the twitter_archive_import job handler
// (TwitterArchiveImportJob): it marks the import running, streams the archive
// in, records the summary and always cleans up the uploaded source file.
// Import failures mark the import row failed but do not fail the job run,
// mirroring the Rails job's rescue.
func RegisterImportHandler(w *jobs.Worker, db *sql.DB, dataDir string) {
	q := query.New(db)
	w.Register(jobs.KindTwitterArchiveImport, func(ctx context.Context, payload json.RawMessage) error {
		var p struct {
			ImportID int64 `json:"import_id"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("twitter archive import: decode payload: %w", err)
		}
		imp, err := q.GetTwitterArchiveImport(ctx, p.ImportID)
		if err != nil {
			return fmt.Errorf("twitter archive import: load import %d: %w", p.ImportID, err)
		}
		// A job re-executed after its import already terminally failed (a
		// crash between FailTwitterArchiveImport and CompleteJobRun) must not
		// re-run: the terminal run already deleted the source zip, so
		// re-marking running would only fail on the missing file and overwrite
		// the original error with "Archive file not found". source_path is
		// cleared after the file is removed, so a crash in between can leave
		// a failed row whose source_path points at a deleted file — that row
		// is terminal too. Rows that can still self-heal (failed by startup
		// recovery while their job stayed queued) keep their source file, so
		// they pass this check.
		if imp.Status == "failed" && !sourceFilePresent(imp.SourcePath) {
			return nil
		}
		// The self-heal re-run is only safe while nothing superseded it: if the
		// admin uploaded a fresh archive after the recovery and that import
		// already ran (or is queued), re-running this one would roll the
		// stored archive back to the older upload. A newer non-failed row
		// makes this import superseded — skip it like a terminal row.
		if imp.Status == "failed" {
			newer, err := q.HasNewerNonFailedTwitterArchiveImport(ctx, imp.ID)
			if err != nil {
				return fmt.Errorf("twitter archive import: check newer imports: %w", err)
			}
			if newer > 0 {
				slog.Info("twitter archive import: skip recovered import superseded by a newer import", "import_id", imp.ID)
				return nil
			}
		}

		now := time.Now().Unix()
		marked, err := q.MarkTwitterArchiveImportRunning(ctx, query.MarkTwitterArchiveImportRunningParams{
			StartedAt: sql.NullInt64{Int64: now, Valid: true}, UpdatedAt: now, ID: imp.ID,
		})
		if err != nil {
			return fmt.Errorf("twitter archive import: mark running: %w", err)
		}
		if marked == 0 {
			// The import already completed; only a re-executed job can see
			// this (its own completion write was lost to a crash). A re-run
			// could only clobber the history row back to failed — skip it
			// and let the job complete. The crash may also have happened
			// before the source cleanup below ran, so remove a leftover
			// zip here too.
			if imp.SourcePath.Valid && imp.SourcePath.String != "" {
				removeSourceFile(dataDir, imp.SourcePath.String)
			}
			return nil
		}
		logActivity(ctx, db, "info", "started", fmt.Sprintf("filename=%s import_id=%d", activity.Quote(imp.SourceFilename), imp.ID))

		importer := &Importer{
			DB:         db,
			DataDir:    dataDir,
			SourcePath: imp.SourcePath.String,
			Progress: func(progress int, message string) {
				_ = q.UpdateTwitterArchiveImportProgress(ctx, query.UpdateTwitterArchiveImportProgressParams{
					Progress:      int64(progress),
					StatusMessage: nullString(message),
					UpdatedAt:     time.Now().Unix(),
					ID:            imp.ID,
				})
			},
		}
		summary, runErr := importer.Import(ctx)

		// A shutdown-cancelled import (SIGTERM) is not a failure: leave the
		// source zip and the running row untouched and propagate the
		// cancellation, so the worker requeues the job for free and the
		// import resumes from the still-present zip after the restart.
		if errors.Is(runErr, context.Canceled) {
			return runErr
		}

		// Like the worker's own bookkeeping, the terminal import writes must
		// land even when the job ctx is already cancelled (SIGTERM landed
		// mid-import): otherwise the row would stay running with active_slot
		// held until the next restart's recovery, and a finished import could
		// go unrecorded and be clobbered by the re-executed job.
		bookCtx := context.WithoutCancel(ctx)

		if runErr != nil {
			now = time.Now().Unix()
			_ = q.FailTwitterArchiveImport(bookCtx, query.FailTwitterArchiveImportParams{
				ErrorMessage: nullString(runErr.Error()),
				FinishedAt:   sql.NullInt64{Int64: now, Valid: true},
				UpdatedAt:    now,
				ID:           imp.ID,
			})
			logActivity(ctx, db, "error", "failed", fmt.Sprintf("filename=%s error=%s import_id=%d",
				activity.Quote(imp.SourceFilename), activity.Quote(runErr.Error()), imp.ID))
		} else {
			now = time.Now().Unix()
			if err := q.CompleteTwitterArchiveImport(bookCtx, query.CompleteTwitterArchiveImportParams{
				TweetsCount:     summary.Tweets,
				FollowersCount:  summary.Followers,
				FollowingCount:  summary.Following,
				LikesCount:      summary.Likes,
				TotalItemsCount: summary.TotalItems,
				FinishedAt:      sql.NullInt64{Int64: now, Valid: true},
				UpdatedAt:       now,
				ID:              imp.ID,
			}); err != nil {
				return fmt.Errorf("twitter archive import: complete import: %w", err)
			}
			logActivity(ctx, db, "info", "completed", fmt.Sprintf(
				"filename=%s followers=%d following=%d import_id=%d likes=%d total_items=%d tweets=%d",
				activity.Quote(imp.SourceFilename), summary.Followers, summary.Following, imp.ID,
				summary.Likes, summary.TotalItems, summary.Tweets))
		}

		// cleanup_source_file!: the uploaded zip is always removed — but only
		// after the terminal status write above landed. Removing it first
		// would make a lost terminal write (a crash, or a failed Complete)
		// unrecoverable: the re-executed job would find the row still running
		// with the source gone, fail on the missing file, and clobber an
		// actually finished import back to failed.
		if imp.SourcePath.Valid && imp.SourcePath.String != "" {
			removeSourceFile(dataDir, imp.SourcePath.String)
		}
		_ = q.ClearTwitterArchiveImportSource(bookCtx, query.ClearTwitterArchiveImportSourceParams{
			UpdatedAt: time.Now().Unix(), ID: imp.ID,
		})
		return nil
	})
}

// logActivity mirrors the ActivityLog.log! calls of the job via the shared
// activity helper; like the Rails original, it never breaks the main flow.
func logActivity(ctx context.Context, db *sql.DB, level, action, description string) {
	activity.Log(ctx, db, level, action, "twitter_archive", description)
}
