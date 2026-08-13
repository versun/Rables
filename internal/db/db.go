// Package db opens the SQLite database and brings it up to date with the
// embedded goose migrations.
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"rables/migrations"
)

// Open creates dataDir if needed, opens <dataDir>/rables.db and migrates it to
// the latest version. Opening an already migrated database is a no-op.
func Open(dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	// MkdirAll is a no-op for an existing directory, so pin the mode here to
	// tighten a data dir created by an older release or a loose umask.
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("restrict data dir permissions: %w", err)
	}
	// Build the DSN via net/url so a "?" or "%" in the path cannot leak
	// into the query string (sqlite percent-decodes the path after
	// splitting on the first literal "?").
	abs, err := filepath.Abs(filepath.Join(dataDir, "rables.db"))
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set dialect: %w", err)
	}
	if err := goose.Up(db, "."); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// The DB holds session tokens and API keys; pin it owner-only even when
	// it was created by an older release with the process umask (0644).
	if err := os.Chmod(abs, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("restrict database permissions: %w", err)
	}
	// In WAL mode new writes (session pages included) land in the -wal/-shm
	// sidecar files, which SQLite creates with the umask whenever the main DB
	// was still 0644 at creation time; pin them owner-only too. They are
	// absent after a clean shutdown, so tolerate that; they are not recreated
	// during this process, so chmod-ing once here covers the runtime.
	for _, sidecar := range []string{abs + "-wal", abs + "-shm"} {
		if err := os.Chmod(sidecar, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("restrict database permissions: %w", err)
		}
	}
	return db, nil
}
