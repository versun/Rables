// Package htmlarchive validates, extracts and stores the static-site ZIP
// archives behind the html_archive content type. An extracted archive lives
// on disk at <DataDir>/archives/<kind>/<id>/ (no files-table rows: archive
// entries have directory structure, which the key-based blob model cannot
// express), must contain an index.html at its root, and is served only
// through the sandboxed /archives route.
//
// Validation is deliberately strict because the served content is active
// (HTML/JS): entry names are rejected (not sanitized) on any traversal risk,
// declared sizes are bounded up front so zip bombs are refused before the
// first write, and extraction re-checks the actual byte counts while it
// streams. Directory entries are skipped without validation — they are never
// written; every file path is derived from a validated file entry.
package htmlarchive

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Kind is the owning record type, used as the <kind> segment of both the
// public /archives route and the on-disk directory.
type Kind string

const (
	Article Kind = "article"
	Page    Kind = "page"
)

// ParseKind maps a route segment to a Kind; ok is false for anything else.
func ParseKind(s string) (Kind, bool) {
	switch Kind(s) {
	case Article, Page:
		return Kind(s), true
	}
	return "", false
}

// Extraction limits. Vars (not constants) so tests can lower them.
var (
	// maxEntries caps the file-entry count: millions of tiny entries stay
	// under the byte limit but would exhaust inodes.
	maxEntries = 2000
	// maxTotalBytes caps the uncompressed total (256MB): upload limits cover
	// only the compressed size, so without this a zip bomb fills the disk.
	maxTotalBytes = int64(256 << 20)
	// maxFileBytes caps one uncompressed entry (64MB).
	maxFileBytes = int64(64 << 20)
	// maxDepth caps the cleaned path depth of an entry.
	maxDepth = 8
)

// stagingPrefix marks the temporary directories Extract writes into; Commit
// only renames directories carrying this prefix, and the export skips them.
const stagingPrefix = ".staging-"

// Root is the directory every archive tree lives under.
func Root(dataDir string) string {
	return filepath.Join(dataDir, "archives")
}

// Dir is the extracted tree of one record's archive.
func Dir(dataDir string, kind Kind, id int64) string {
	return filepath.Join(Root(dataDir), string(kind), strconv.FormatInt(id, 10))
}

// Has reports whether the record currently has an extracted archive on disk
// (an index.html at the tree root).
func Has(dataDir string, kind Kind, id int64) bool {
	info, err := os.Stat(filepath.Join(Dir(dataDir, kind, id), "index.html"))
	return err == nil && info.Mode().IsRegular()
}

// Remove deletes the record's extracted tree (a no-op when absent).
func Remove(dataDir string, kind Kind, id int64) error {
	return os.RemoveAll(Dir(dataDir, kind, id))
}

// Extract validates the ZIP read from src (size bytes) and extracts it into a
// fresh staging directory under the archives root, returning the staging
// path. On any error the staging directory is removed. The caller moves the
// result into place with Commit or removes it when the surrounding operation
// fails. Errors are user-presentable: they land in the admin form's error
// list, so they say what to fix ("Archive must contain ...").
//
// A ZIP made by compressing a folder (Finder's Compress and most GUI tools)
// wraps every file in one top-level directory; that wrapper is treated as
// the archive root and stripped, so both "zip the folder" and "zip the
// contents" work. macOS metadata (__MACOSX trees, .DS_Store files) is never
// extracted or counted.
func Extract(dataDir string, src io.ReaderAt, size int64) (string, error) {
	zr, err := zip.NewReader(src, size)
	if err != nil {
		return "", fmt.Errorf("Archive is not a valid ZIP file")
	}

	// Pass 1: validate every entry up front — names, counts and declared
	// sizes — so a hostile archive is refused before anything is written.
	type zipEntry struct {
		name string // cleaned, wrapper-stripped relative path
		file *zip.File
	}
	var files []zipEntry
	var declaredTotal uint64
	topDirs := map[string]bool{}
	rootFile := false
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue // never written; file parents are derived from validated file names
		}
		clean, err := cleanEntryName(f.Name)
		if err != nil {
			return "", err
		}
		top, _, _ := strings.Cut(clean, "/")
		if top == "__MACOSX" || path.Base(clean) == ".DS_Store" {
			continue
		}
		if mode := f.FileInfo().Mode(); !mode.IsRegular() {
			// Symlinks (and device/pipe entries, which zip tools can forge via
			// SetMode) must never reach the disk.
			return "", fmt.Errorf("Archive entry %q is not a regular file", f.Name)
		}
		if len(files)+1 > maxEntries {
			return "", fmt.Errorf("Archive has more than %d files", maxEntries)
		}
		if f.UncompressedSize64 > uint64(maxFileBytes) {
			return "", fmt.Errorf("Archive entry %q is larger than %d MB", f.Name, maxFileBytes>>20)
		}
		// Compare before adding, like the import extractor: the invariant
		// declaredTotal <= maxTotalBytes keeps the subtraction from
		// underflowing on a forged zip64 declared size.
		if f.UncompressedSize64 > uint64(maxTotalBytes)-declaredTotal {
			return "", fmt.Errorf("Archive uncompresses to more than %d MB", maxTotalBytes>>20)
		}
		declaredTotal += f.UncompressedSize64
		files = append(files, zipEntry{name: clean, file: f})
		if top == clean {
			rootFile = true // an entry sitting at the ZIP root
		} else {
			topDirs[top] = true
		}
	}

	// Unwrap exactly one shared top-level directory; several top-level
	// directories (or any root-level file) mean the ZIP is taken as-is.
	strip := ""
	if !rootFile && len(topDirs) == 1 {
		for top := range topDirs {
			strip = top + "/"
		}
	}

	hasIndex := false
	seen := map[string]bool{} // lowercased names: case-insensitive filesystems collapse A.css/a.css
	for i := range files {
		name := strings.TrimPrefix(files[i].name, strip)
		if len(strings.Split(name, "/")) > maxDepth {
			return "", fmt.Errorf("Archive entry %q is nested deeper than %d levels", files[i].file.Name, maxDepth)
		}
		if seen[strings.ToLower(name)] {
			return "", fmt.Errorf("Archive has duplicate entries for %q", name)
		}
		seen[strings.ToLower(name)] = true
		if name == "index.html" {
			hasIndex = true
		}
		files[i].name = name
	}
	if !hasIndex {
		return "", fmt.Errorf("Archive must contain an index.html at the ZIP root")
	}

	staging, err := newStagingDir(dataDir)
	if err != nil {
		return "", err
	}
	// Pass 2: stream the files out. The declared sizes passed validation, but
	// the copy still counts actual bytes: a lying header is cut off at the
	// same limits (Go's zip reader then fails the CRC check at EOF anyway).
	var writtenTotal int64
	fail := func(err error) (string, error) {
		os.RemoveAll(staging)
		return "", err
	}
	for _, entry := range files {
		target := filepath.Join(staging, filepath.FromSlash(entry.name))
		if target != staging && !strings.HasPrefix(target, staging+string(os.PathSeparator)) {
			return fail(fmt.Errorf("Archive entry %q has an unsafe path", entry.file.Name))
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fail(fmt.Errorf("Archive extraction failed: %v", err))
		}
		rc, err := entry.file.Open()
		if err != nil {
			return fail(fmt.Errorf("Archive entry %q could not be read", entry.file.Name))
		}
		// O_EXCL: duplicates were rejected in pass 1, so an existing target
		// means the validation was raced or bypassed — fail loudly.
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			rc.Close()
			return fail(fmt.Errorf("Archive extraction failed: %v", err))
		}
		written, copyErr := io.Copy(out, io.LimitReader(rc, maxFileBytes+1))
		closeErr := out.Close()
		rc.Close()
		if copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return fail(fmt.Errorf("Archive entry %q could not be extracted", entry.file.Name))
		}
		if written > maxFileBytes || written > maxTotalBytes-writtenTotal {
			return fail(fmt.Errorf("Archive uncompresses to more than the allowed size"))
		}
		writtenTotal += written
	}
	return staging, nil
}

// Commit moves a validated staging directory into place as the record's
// archive, replacing any previous tree. The rename is atomic on the same
// filesystem (staging is a sibling of the target), so concurrent readers
// never see a half-extracted tree.
func Commit(staging, dataDir string, kind Kind, id int64) error {
	root := Root(dataDir)
	if filepath.Dir(staging) != root || !strings.HasPrefix(filepath.Base(staging), stagingPrefix) {
		return fmt.Errorf("htmlarchive: %q is not a staging directory", staging)
	}
	final := Dir(dataDir, kind, id)
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	return os.Rename(staging, final)
}

// newStagingDir creates <dataDir>/archives/.staging-<rand>.
func newStagingDir(dataDir string) (string, error) {
	root := Root(dataDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	return os.MkdirTemp(root, stagingPrefix+hex.EncodeToString(rnd[:]))
}

// cleanEntryName validates one ZIP entry name and returns its cleaned
// slash-separated relative path. Anything with a traversal risk is rejected,
// never sanitized: absolute paths, drive letters, NUL and other control
// bytes, backslashes (a separator on Windows, where the "/"-based ".." check
// would miss "..\.."), and ".." segments. The depth limit applies later, on
// the wrapper-stripped name.
func cleanEntryName(name string) (string, error) {
	unsafe := fmt.Errorf("Archive entry %q has an unsafe path", name)
	if name == "" || strings.HasPrefix(name, "/") {
		return "", unsafe
	}
	if len(name) >= 2 && name[1] == ':' { // Windows drive letter
		return "", unsafe
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", unsafe
		}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", unsafe
		}
	}
	clean := path.Clean(name)
	if clean == "." || clean == "" {
		return "", unsafe
	}
	return clean, nil
}

// SafeRelPath cleans a percent-decoded wildcard request path into a relative
// slash path safe to resolve inside an extracted archive tree; ok is false
// for any traversal attempt. The empty path (the archive root) is ok and
// resolves to the tree root, whose index.html the caller serves.
func SafeRelPath(p string) (string, bool) {
	if p == "" {
		return "", true
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", false
		}
	}
	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	if clean == "" {
		return "", true
	}
	return clean, true
}

// TreeRel reports whether a slash-separated path relative to the archives
// root names a file inside a real tree (<kind>/<id>/<file...>): the kind is
// article|page, the id is numeric, and the rest has no empty/dot segments.
// The database export and import both filter their walk through this, so
// staging dirs and stray litter never travel in a backup bundle.
func TreeRel(rel string) bool {
	parts := strings.Split(rel, "/")
	if len(parts) < 3 {
		return false
	}
	if _, ok := ParseKind(parts[0]); !ok {
		return false
	}
	if parts[1] == "" {
		return false
	}
	for _, r := range parts[1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, seg := range parts[2:] {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
