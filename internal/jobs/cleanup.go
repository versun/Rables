package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rables/internal/db/query"
	"rables/internal/service/media"
)

// CleanupOrphanImportFiles removes leftovers in <dataDir>/imports whose
// owning process died before finishing its work. Three kinds exist:
//   - import_* / twitter_archive_* uploads: the import handlers
//     (saveImportUpload in httpd; the removed archive feature had its own)
//     stream the upload to disk first and hand ownership to the job row
//     second, so a crash in between leaves a file nothing points at. The
//     admin file list hides the import_ prefix, so without this sweep
//     there is no way to reclaim those (a single upload can be 4 GB).
//     twitter_archive_* names are pure leftovers — the feature and its
//     tables are gone (migration 0006), so nothing creates or references
//     that prefix anymore. The one exception: an admin reusing the prefix
//     for a manual server-side import, whose enqueued job then keeps the
//     file via the reference check below, as any import file deserves.
//   - *.part temp files: the handlers write the upload under a .part temp
//     name and atomically rename it to the final import_* name once fully
//     written, so only a crash mid-write leaves one. A .part file is never
//     referenced by a job, so no reference check is needed.
//   - extract_* staging directories of the import_db job
//     (transfer.importStagingDir): the job defers RemoveAll, but a
//     SIGKILL/OOM leaves the directory (up to 10 GB of extracted bundle)
//     behind with no other way to reclaim it.
//
// An upload candidate is deleted only when all of the following hold:
//   - the name carries the import_ or twitter_archive_ prefix (a trailing
//     .queued does not matter: the prefix alone marks an owned upload);
//   - its mtime predates this process start, so an upload another process is
//     still streaming during a rolling deploy is not swept from under it;
//   - no queued/running import job payload references the path.
//
// An extract_* directory is removed only when its mtime predates this
// process start AND no import_db job is queued or running
// anywhere: during a rolling deploy the other process may be mid-extraction,
// and its active job row is what keeps the staging directory safe.
//
// It runs at startup after RecoverStaleJobs, so jobs recovery just
// terminalized stop protecting their files in the same sweep. It returns the
// number of entries removed; one that cannot be removed is reported but does
// not stop the sweep.
func CleanupOrphanImportFiles(ctx context.Context, q *query.Queries, dataDir string, processStarted time.Time) (int, error) {
	dir := filepath.Join(dataDir, "imports")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	var candidates, tempFiles, extractDirs []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if !strings.HasPrefix(name, "extract_") {
				continue
			}
		} else if !strings.HasPrefix(name, "import_") && !strings.HasPrefix(name, "twitter_archive_") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(processStarted) {
			continue
		}
		switch {
		case entry.IsDir():
			extractDirs = append(extractDirs, filepath.Join(dir, name))
		case strings.HasSuffix(name, ".part"):
			tempFiles = append(tempFiles, filepath.Join(dir, name))
		default:
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	if len(candidates) == 0 && len(tempFiles) == 0 && len(extractDirs) == 0 {
		return 0, nil
	}

	protected, importJobActive, err := activeImportPaths(ctx, q)
	if err != nil {
		return 0, err
	}

	removed := 0
	var firstErr error
	for _, path := range tempFiles {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	for _, path := range candidates {
		if protected[path] {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	if !importJobActive {
		for _, path := range extractDirs {
			if err := os.RemoveAll(path); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			removed++
		}
	}
	return removed, firstErr
}

// activeImportPaths collects the data/imports paths still referenced by an
// active import: queued/running import job payloads. It also reports whether
// any import_db job is queued or running: that is the job
// extracting into extract_* staging dirs, so the sweep may only remove such
// a dir when none of them is active.
func activeImportPaths(ctx context.Context, q *query.Queries) (map[string]bool, bool, error) {
	protected := map[string]bool{}
	importJobActive := false

	rows, err := q.ListActiveImportJobPayloads(ctx)
	if err != nil {
		return nil, false, err
	}
	// The field name mirrors transfer.ImportDBPayload and
	// transfer.ImportMarkdownPayload; jobs cannot import transfer (transfer
	// registers its handlers here), so the payload is decoded structurally.
	for _, row := range rows {
		if row.Kind == KindImportDB {
			importJobActive = true
		}
		if !row.Payload.Valid {
			continue
		}
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(row.Payload.String), &p); err != nil {
			continue
		}
		if p.Path != "" {
			protected[p.Path] = true
		}
	}
	return protected, importJobActive, nil
}

// ReapOrphanFiles deletes files rows and disk blobs a dead process left
// behind. The media write paths (twittersync downloadMedia,
// media.Service.Store) insert the files row and write the blob before — and
// outside — whatever transaction later references them, so a SIGKILL/OOM in
// between leaves a row nothing points at. The failure paths
// (discardStoredMedia, media.Purge) never run on that crash, and nothing
// else reclaims the row.
//
// A files row is reaped only when all of the following hold:
//   - its created_at predates this process start by 24 hours. The machine-
//     speed cases (a row another process stored moments ago — mid import
//     batch, mid sync — during a rolling deploy) need only minutes; the wide
//     margin covers the human-timescale editor workflow: an admin upload via
//     /admin/uploads gets no attachment row, its URL is pasted into a draft,
//     and the draft may be saved hours later, after any number of restarts.
//     A crash orphan never gains a reference on its own, so the margin costs
//     nothing;
//   - no attachment and no static_files entry references the row or any of
//     its variants (a variant is reachable only through its original, so the
//     family shares one fate; variants are deleted first under foreign_keys
//     enforcement);
//   - no stored content references /files/<key> of the row or any of its
//     variants: admin uploads and RSS-imported images are embedded by URL
//     only and never get an attachment row, and content merged by a
//     database import can carry a variant URL.
//
// It runs at startup after RecoverStaleJobs, like
// CleanupOrphanImportFiles. It returns the number of files rows deleted; a
// row or blob that cannot be removed is reported but does not stop the
// sweep.
func ReapOrphanFiles(ctx context.Context, q *query.Queries, dataDir string, processStarted time.Time) (int, error) {
	cutoff := processStarted.Add(-24 * time.Hour).Unix()
	candidates, err := q.ListOrphanFileCandidates(ctx, cutoff)
	if err != nil {
		return 0, err
	}

	removed := 0
	var firstErr error
	remove := func(id int64, key string) {
		if err := q.DeleteFile(ctx, id); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		removed++
		if !media.ValidKey(key) {
			return
		}
		if err := os.Remove(fileBlobPath(dataDir, key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	for _, c := range candidates {
		refs, err := q.CountFileKeyContentReferences(ctx, sql.NullString{String: c.Key, Valid: true})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if refs > 0 {
			continue
		}
		variants, err := q.ListFileVariants(ctx, sql.NullInt64{Int64: c.ID, Valid: true})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Each variant gets the same content check as the family root, like
		// articles.Destroy: a hit keeps the whole family.
		keep := false
		for _, v := range variants {
			vRefs, err := q.CountFileKeyContentReferences(ctx, sql.NullString{String: v.Key, Valid: true})
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				keep = true
				break
			}
			if vRefs > 0 {
				keep = true
				break
			}
		}
		if keep {
			continue
		}
		for _, v := range variants {
			remove(v.ID, v.Key)
		}
		remove(c.ID, c.Key)
	}
	return removed, firstErr
}

// fileBlobPath mirrors media.Service.PathFor.
func fileBlobPath(dataDir, key string) string {
	return filepath.Join(dataDir, "files", key[0:2], key[2:4], key)
}
