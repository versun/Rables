// migrate-rails copies a Rails rables SQLite database into the Go schema
// (plan section 8). One-shot tool: the old database is opened read-only, the
// new one is created/migrated under DATA_DIR via internal/db.Open.
//
//	migrate-rails -old /path/to/development.sqlite3 -data ./data [--verify-files]
//
// The exit code is non-zero exactly when a table row count diverges (or on a
// fatal error); re-running is safe and skips already-migrated rows.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	_ "modernc.org/sqlite"

	"rables/internal/db"
	"rables/internal/service/railsmigrate"
)

func main() {
	oldPath := flag.String("old", "", "path to the Rails SQLite database (opened read-only)")
	dataDir := flag.String("data", "./data", "Go DATA_DIR (rables.db is created/migrated inside)")
	verifyFiles := flag.Bool("verify-files", false, "check every files row has a blob on disk under DATA_DIR/files")
	flag.Parse()
	if *oldPath == "" {
		fmt.Fprintln(os.Stderr, "usage: migrate-rails -old <rails.sqlite3> [-data ./data] [--verify-files]")
		os.Exit(2)
	}
	// Fail fast on a bad path: a 0-byte file is a valid empty sqlite database
	// and would otherwise die later with a cryptic "no such table".
	fi, err := os.Stat(*oldPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open old database:", err)
		os.Exit(1)
	}
	if fi.Size() == 0 {
		fmt.Fprintf(os.Stderr, "open old database: %s is empty (0 bytes); the Rails database is usually db/development.sqlite3\n", *oldPath)
		os.Exit(1)
	}

	// read-only: the old database must never be modified
	oldDB, err := sql.Open("sqlite", "file:"+*oldPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		fmt.Fprintln(os.Stderr, "open old database:", err)
		os.Exit(1)
	}
	defer oldDB.Close()

	newDB, err := db.Open(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open new database:", err)
		os.Exit(1)
	}
	defer newDB.Close()

	rep, err := railsmigrate.Run(context.Background(), oldDB, newDB, railsmigrate.Options{
		Out:         os.Stdout,
		DataDir:     *dataDir,
		VerifyFiles: *verifyFiles,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	if rep.Mismatch() {
		os.Exit(1)
	}
}
