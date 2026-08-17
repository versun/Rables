package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/jpeg"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"rables/internal/db"
)

// countBlobs returns the number of regular files under <dataDir>/files.
func countBlobs(t *testing.T, dataDir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(dataDir, "files"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			n++
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("walk blobs: %v", err)
	}
	return n
}

// noisyJPEG encodes an image whose variant re-encode is guaranteed to be
// smaller than the original (random noise does not compress well).
func noisyJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2048, 2048))
	if _, err := rand.Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// hugePNGHeader returns PNG bytes whose IHDR declares a w×h image without
// any pixel data: enough for image.DecodeConfig to report the dimensions,
// so the pixel-budget path can be tested without decoding gigabytes.
func hugePNGHeader(t *testing.T, w, h int) []byte {
	t.Helper()
	var ihdr bytes.Buffer
	if err := binary.Write(&ihdr, binary.BigEndian, int32(w)); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&ihdr, binary.BigEndian, int32(h)); err != nil {
		t.Fatal(err)
	}
	ihdr.Write([]byte{8, 2, 0, 0, 0}) // 8-bit truecolor, deflate, no interlace
	chunk := ihdr.Bytes()

	var buf bytes.Buffer
	buf.Write([]byte("\x89PNG\r\n\x1a\n"))
	if err := binary.Write(&buf, binary.BigEndian, int32(len(chunk))); err != nil {
		t.Fatal(err)
	}
	buf.WriteString("IHDR")
	buf.Write(chunk)
	crc := crc32.ChecksumIEEE(append([]byte("IHDR"), chunk...))
	if err := binary.Write(&buf, binary.BigEndian, crc); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestStoreSkipsVariantOverPixelBudget: an image whose declared dimensions
// exceed maxVariantPixels must not be decoded for a variant — decoding it
// would allocate ~4 bytes per pixel and can OOM the process. The original
// upload is kept as-is.
func TestStoreSkipsVariantOverPixelBudget(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	dataDir := t.TempDir()
	svc := New(database, dataDir)
	file, err := svc.Store(t.Context(), bytes.NewReader(hugePNGHeader(t, 40000, 40000)), "huge.png", "image/png")
	if err != nil {
		t.Fatalf("Store: %v (oversized image must not fail the upload)", err)
	}
	if _, err := os.Stat(svc.PathFor(file.Key)); err != nil {
		t.Errorf("original blob missing: %v", err)
	}
	if n := countBlobs(t, dataDir); n != 1 {
		t.Errorf("blobs on disk = %d, want 1 (original only, no variant)", n)
	}
	var rows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("files rows = %d, want 1 (original only)", rows)
	}
}

// left behind as an orphan when the files row cannot be created.
func TestStoreRemovesBlobWhenInsertFails(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Exec(`CREATE TRIGGER reject_file_inserts BEFORE INSERT ON files
		BEGIN SELECT RAISE(ABORT, 'insert blocked'); END`); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	_, err = New(database, dataDir).Store(t.Context(), bytes.NewReader([]byte("data")), "x.txt", "text/plain")
	if err == nil {
		t.Fatal("Store succeeded despite the blocked insert")
	}
	if n := countBlobs(t, dataDir); n != 0 {
		t.Errorf("blobs on disk = %d, want 0 (orphan blob not cleaned up)", n)
	}
}

// TestStoreVariantRemovesBlobWhenInsertFails: when only the variant row
// insert fails, the variant blob is removed while the original upload stays
// intact (variant failures are swallowed by design).
func TestStoreVariantRemovesBlobWhenInsertFails(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Exec(`CREATE TRIGGER reject_variant_inserts BEFORE INSERT ON files
		WHEN NEW.variant_of IS NOT NULL
		BEGIN SELECT RAISE(ABORT, 'variant insert blocked'); END`); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	svc := New(database, dataDir)
	file, err := svc.Store(t.Context(), bytes.NewReader(noisyJPEG(t)), "big.jpg", "image/jpeg")
	if err != nil {
		t.Fatalf("Store: %v (variant failure must not fail the upload)", err)
	}
	if _, err := os.Stat(svc.PathFor(file.Key)); err != nil {
		t.Errorf("original blob missing: %v", err)
	}
	if n := countBlobs(t, dataDir); n != 1 {
		t.Errorf("blobs on disk = %d, want 1 (orphan variant blob not cleaned up)", n)
	}
	var rows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("files rows = %d, want 1 (original only)", rows)
	}
}

// TestStoreRemovesBlobWhenWriteFails: a write that fails midway (e.g. disk
// full) leaves a truncated blob behind; with no files row pointing at the
// key it would be an unreachable orphan, so Store must remove it.
func TestStoreRemovesBlobWhenWriteFails(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	saved := writeBlob
	writeBlob = func(path string, data []byte, perm os.FileMode) error {
		if err := os.WriteFile(path, data[:len(data)/2], perm); err != nil {
			return err
		}
		return errors.New("injected write failure")
	}
	t.Cleanup(func() { writeBlob = saved })

	dataDir := t.TempDir()
	_, err = New(database, dataDir).Store(t.Context(), bytes.NewReader([]byte("data")), "x.txt", "text/plain")
	if err == nil {
		t.Fatal("Store succeeded despite the failed write")
	}
	if n := countBlobs(t, dataDir); n != 0 {
		t.Errorf("blobs on disk = %d, want 0 (partial blob not cleaned up)", n)
	}
	var rows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("files rows = %d, want 0", rows)
	}
}

// TestStoreVariantRemovesBlobWhenWriteFails: when only the variant write
// fails, the partial variant blob is removed while the original upload stays
// intact (variant failures are swallowed by design).
func TestStoreVariantRemovesBlobWhenWriteFails(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	saved := writeBlob
	writes := 0
	writeBlob = func(path string, data []byte, perm os.FileMode) error {
		writes++
		if writes == 2 { // the variant blob
			if err := os.WriteFile(path, data[:len(data)/2], perm); err != nil {
				return err
			}
			return errors.New("injected write failure")
		}
		return os.WriteFile(path, data, perm)
	}
	t.Cleanup(func() { writeBlob = saved })

	dataDir := t.TempDir()
	svc := New(database, dataDir)
	file, err := svc.Store(t.Context(), bytes.NewReader(noisyJPEG(t)), "big.jpg", "image/jpeg")
	if err != nil {
		t.Fatalf("Store: %v (variant failure must not fail the upload)", err)
	}
	if writes != 2 {
		t.Fatalf("blob writes = %d, want 2 (original + variant)", writes)
	}
	if _, err := os.Stat(svc.PathFor(file.Key)); err != nil {
		t.Errorf("original blob missing: %v", err)
	}
	if n := countBlobs(t, dataDir); n != 1 {
		t.Errorf("blobs on disk = %d, want 1 (partial variant blob not cleaned up)", n)
	}
	var rows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("files rows = %d, want 1 (original only)", rows)
	}
}

// TestPurgeRunsWithCanceledContext: Purge runs when a step after Store
// failed, and that failure may be the ctx itself being canceled (client
// disconnect, SIGTERM). The cleanup must still delete rows and blobs.
func TestPurgeRunsWithCanceledContext(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	dataDir := t.TempDir()
	svc := New(database, dataDir)
	file, err := svc.Store(t.Context(), bytes.NewReader(noisyJPEG(t)), "big.jpg", "image/jpeg")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if n := countBlobs(t, dataDir); n != 2 {
		t.Fatalf("blobs on disk = %d, want 2 (original + variant)", n)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	svc.Purge(ctx, file)

	if n := countBlobs(t, dataDir); n != 0 {
		t.Errorf("blobs on disk = %d, want 0 (purge must run despite canceled ctx)", n)
	}
	var rows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("files rows = %d, want 0 (purge must run despite canceled ctx)", rows)
	}
}
