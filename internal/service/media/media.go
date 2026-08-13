// Package media stores uploaded files on disk under DATA_DIR/files and tracks
// them in the files/attachments tables, replacing Rails ActiveStorage.
//
// Disk layout mirrors the ActiveStorage Disk service so migrated installs can
// mount the old storage/ directory unchanged: <DataDir>/files/xx/yy/<key>
// where xx/yy are the first four key characters split in half. Checksums are
// MD5 of the content, base64-encoded, same as
// ActiveStorage::Blob#compute_checksum_in_chunks.
package media

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // decode only; variants are never produced from GIFs
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/disintegration/imaging"

	"rables/internal/db/query"
)

// Variant bounds mirror the resize_to_limit used when Rails renders embedded
// attachments (app/views/active_storage/blobs/_blob.html.erb).
const (
	variantMaxW = 1024
	variantMaxH = 768
	// variantQuality mirrors the Q=80 compression in
	// app/models/concerns/active_storage_compression.rb.
	variantQuality = 80
	// maxVariantPixels caps the decoded size for variant generation.
	// Decoding allocates ~4 bytes per pixel, so a tiny but highly compressed
	// image (e.g. a solid 40000x40000 PNG) would need gigabytes of memory and
	// can OOM-kill the process on a small VPS. Oversized images keep the
	// original only.
	maxVariantPixels = 50_000_000
)

// Service stores and retrieves uploaded files.
type Service struct {
	DB      *sql.DB
	DataDir string
	Log     *slog.Logger

	q *query.Queries
}

// New builds a Service rooted at dataDir (files live under dataDir/files).
func New(db *sql.DB, dataDir string) *Service {
	return &Service{
		DB:      db,
		DataDir: dataDir,
		Log:     slog.Default(),
		q:       query.New(db),
	}
}

// Store writes r to disk under a fresh random key, inserts the files row and
// returns it. For decodable non-GIF images it additionally stores a downsized
// variant (variant_of pointing at the original) when the variant is smaller,
// mirroring active_storage_compression.rb. The variant filename keeps the
// original one, like ActiveStorage variant blobs.
func (s *Service) Store(ctx context.Context, r io.Reader, filename, contentType string) (query.File, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return query.File{}, fmt.Errorf("media: read upload: %w", err)
	}
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	key, err := newKey()
	if err != nil {
		return query.File{}, err
	}
	if err := s.writeFile(key, data); err != nil {
		return query.File{}, err
	}
	file, err := s.q.CreateFile(ctx, query.CreateFileParams{
		Key:         key,
		Filename:    filename,
		ContentType: sql.NullString{String: contentType, Valid: contentType != ""},
		ByteSize:    int64(len(data)),
		Checksum:    sql.NullString{String: checksum(data), Valid: true},
		CreatedAt:   time.Now().Unix(),
	})
	if err != nil {
		// The blob is already on disk; without a files row pointing at it,
		// it would be an orphan no one can reach.
		if removeErr := os.Remove(s.PathFor(key)); removeErr != nil {
			s.Log.Warn("media: remove orphan blob", "key", key, "error", removeErr)
		}
		return query.File{}, fmt.Errorf("media: insert file row: %w", err)
	}
	if err := s.storeVariant(ctx, data, file); err != nil {
		// Mirrors the Rails concern's rescue: a failed variant never fails
		// the upload.
		s.Log.Error("media: variant generation failed", "key", key, "error", err)
	}
	return file, nil
}

// Attach links a stored file to a record (record_type/record_id/name),
// mirroring ActiveStorage attachments. Duplicate links are ignored.
func (s *Service) Attach(ctx context.Context, fileID int64, recordType string, recordID int64, name string) error {
	return s.q.CreateAttachment(ctx, query.CreateAttachmentParams{
		FileID:     fileID,
		RecordType: recordType,
		RecordID:   recordID,
		Name:       name,
		CreatedAt:  time.Now().Unix(),
	})
}

// FileByKey returns the files row for key (sql.ErrNoRows when unknown).
func (s *Service) FileByKey(ctx context.Context, key string) (query.File, error) {
	return s.q.GetFileByKey(ctx, key)
}

// Purge reclaims a stored file and its image variants — files rows and disk
// blobs — when a step after Store fails and the client never learns the key,
// so the file would otherwise be an orphan no one can reach. Best effort like
// Store's own orphan cleanup: failures are logged, not fatal.
func (s *Service) Purge(ctx context.Context, file query.File) {
	// The failure that triggered the purge may be the ctx itself being
	// canceled (client disconnect, SIGTERM); the cleanup must still run, so
	// it uses a context that cannot be canceled.
	ctx = context.WithoutCancel(ctx)
	remove := func(f query.File) {
		if err := s.q.DeleteFile(ctx, f.ID); err != nil {
			s.Log.Warn("media: delete orphan file row", "id", f.ID, "error", err)
			return
		}
		if err := os.Remove(s.PathFor(f.Key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.Log.Warn("media: remove orphan blob", "key", f.Key, "error", err)
		}
	}
	// Variants reference their original via files.variant_of; under
	// foreign_keys enforcement they must be deleted first.
	variants, err := s.q.ListFileVariants(ctx, sql.NullInt64{Int64: file.ID, Valid: true})
	if err != nil {
		s.Log.Warn("media: list file variants", "id", file.ID, "error", err)
	}
	for _, variant := range variants {
		remove(variant)
	}
	remove(file)
}

// PathFor returns the on-disk path for a key. Callers must validate the key
// (see ValidKey) before using it.
func (s *Service) PathFor(key string) string {
	return filepath.Join(s.DataDir, "files", key[0:2], key[2:4], key)
}

// ValidKey reports whether key is safe to use in a disk path: lowercase or
// uppercase alphanumerics only, long enough for the xx/yy split. Covers both
// new 32-char hex keys and migrated ActiveStorage base58 keys; rejects any
// traversal attempt.
func ValidKey(key string) bool {
	if len(key) < 4 || len(key) > 64 {
		return false
	}
	for _, c := range key {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			continue
		}
		return false
	}
	return true
}

// storeVariant downsizes images to fit variantMaxW x variantMaxH and stores
// the result as a second files row pointing at the original. Non-images and
// GIFs (possibly animated; re-encoding would flatten them) are skipped, as
// are images over maxVariantPixels and variants that end up no smaller than
// the original.
func (s *Service) storeVariant(ctx context.Context, data []byte, orig query.File) error {
	contentType := orig.ContentType.String
	if !strings.HasPrefix(contentType, "image/") || contentType == "image/gif" {
		return nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil // undecodable image: keep the original only
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxVariantPixels {
		return nil // too large to decode safely: keep the original only
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil // undecodable image: keep the original only
	}
	var encodeFormat imaging.Format
	switch format {
	case "jpeg":
		encodeFormat = imaging.JPEG
	case "png":
		encodeFormat = imaging.PNG
	default:
		return nil
	}
	bounds := img.Bounds()
	if bounds.Dx() > variantMaxW || bounds.Dy() > variantMaxH {
		img = imaging.Fit(img, variantMaxW, variantMaxH, imaging.Lanczos)
	}
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, encodeFormat, imaging.JPEGQuality(variantQuality)); err != nil {
		return fmt.Errorf("encode variant: %w", err)
	}
	if buf.Len() >= len(data) {
		return nil
	}
	key, err := newKey()
	if err != nil {
		return err
	}
	if err := s.writeFile(key, buf.Bytes()); err != nil {
		return err
	}
	_, err = s.q.CreateFile(ctx, query.CreateFileParams{
		Key:         key,
		Filename:    orig.Filename,
		ContentType: orig.ContentType,
		ByteSize:    int64(buf.Len()),
		Checksum:    sql.NullString{String: checksum(buf.Bytes()), Valid: true},
		VariantOf:   sql.NullInt64{Int64: orig.ID, Valid: true},
		CreatedAt:   time.Now().Unix(),
	})
	if err != nil {
		// Same orphan-blob rule as Store.
		if removeErr := os.Remove(s.PathFor(key)); removeErr != nil {
			s.Log.Warn("media: remove orphan blob", "key", key, "error", removeErr)
		}
		return fmt.Errorf("insert variant row: %w", err)
	}
	return nil
}

func (s *Service) writeFile(key string, data []byte) error {
	path := s.PathFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("media: create dir: %w", err)
	}
	if err := writeBlob(path, data, 0o644); err != nil {
		// A failed write may have left a partial blob behind; with no files
		// row pointing at the key it would be an orphan no one can reach.
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			s.Log.Warn("media: remove orphan blob", "key", key, "error", removeErr)
		}
		return fmt.Errorf("media: write file: %w", err)
	}
	return nil
}

// writeBlob is os.WriteFile, overridable in tests to inject write failures.
var writeBlob = os.WriteFile

// newKey returns 16 random bytes hex-encoded (32 chars).
func newKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("media: generate key: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// checksum is MD5 of the content, base64-encoded — the same value ActiveStorage
// stores in blobs.checksum.
func checksum(data []byte) string {
	sum := md5.Sum(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}
