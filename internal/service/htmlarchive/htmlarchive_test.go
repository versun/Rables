package htmlarchive

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zipEntry is one in-memory test entry; mode zero means a plain file.
type zipEntry struct {
	name string
	body string
	mode os.FileMode
}

// buildZip packs entries into an in-memory ZIP.
func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("create entry %q: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("write entry %q: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// extract builds the zip, runs Extract and returns the staging dir.
func extract(t *testing.T, dataDir string, entries []zipEntry) (string, error) {
	t.Helper()
	data := buildZip(t, entries)
	return Extract(dataDir, bytes.NewReader(data), int64(len(data)))
}

func TestExtractValid(t *testing.T) {
	dataDir := t.TempDir()
	staging, err := extract(t, dataDir, []zipEntry{
		{name: "index.html", body: "<h1>hi</h1>"},
		{name: "css/site.css", body: "body{}"},
		{name: "js/app.js", body: "console.log(1)"},
		{name: "assets/fonts/a.woff2", body: "font"},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	defer os.RemoveAll(staging)
	for _, rel := range []string{"index.html", "css/site.css", "js/app.js", "assets/fonts/a.woff2"} {
		if _, err := os.Stat(filepath.Join(staging, filepath.FromSlash(rel))); err != nil {
			t.Errorf("missing extracted file %s: %v", rel, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(staging, "index.html"))
	if err != nil || string(body) != "<h1>hi</h1>" {
		t.Errorf("index.html content = %q, %v", body, err)
	}
}

// A zip made by compressing a folder (Finder's Compress and most GUI tools)
// wraps every file in one top-level directory: the wrapper is stripped, and
// macOS metadata never reaches the disk.
func TestExtractFolderWrap(t *testing.T) {
	dataDir := t.TempDir()
	staging, err := extract(t, dataDir, []zipEntry{
		{name: "mysite/index.html", body: "<h1>wrapped</h1>"},
		{name: "mysite/css/site.css", body: "body{}"},
		{name: "mysite/.DS_Store", body: "junk"},
		{name: "__MACOSX/mysite/._index.html", body: "junk"},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	defer os.RemoveAll(staging)
	body, err := os.ReadFile(filepath.Join(staging, "index.html"))
	if err != nil || string(body) != "<h1>wrapped</h1>" {
		t.Errorf("unwrapped index.html = %q, %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(staging, "css", "site.css")); err != nil {
		t.Errorf("missing unwrapped css/site.css: %v", err)
	}
	for _, junk := range []string{".DS_Store", "__MACOSX", "mysite"} {
		if _, err := os.Stat(filepath.Join(staging, junk)); !os.IsNotExist(err) {
			t.Errorf("junk/wrapper path %q landed in the tree", junk)
		}
	}
}

// A root-level file pins the layout: a stray sibling of the wrapper keeps the
// zip from being unwrapped, so a missing root index.html stays an error.
func TestExtractNoUnwrapWithRootFile(t *testing.T) {
	_, err := extract(t, t.TempDir(), []zipEntry{
		{name: "readme.txt", body: "x"},
		{name: "site/index.html", body: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Errorf("error = %v, want missing-index rejection", err)
	}
}

func TestExtractRejects(t *testing.T) {
	cases := []struct {
		name    string
		entries []zipEntry
		wantErr string
	}{
		{"missing index", []zipEntry{{name: "other.html", body: "x"}}, "index.html"},
		{"two top folders", []zipEntry{{name: "a/index.html", body: "x"}, {name: "b/x.css", body: "x"}}, "index.html"},
		{"traversal", []zipEntry{{name: "index.html", body: "x"}, {name: "../evil.html", body: "x"}}, "unsafe path"},
		{"nested traversal", []zipEntry{{name: "index.html", body: "x"}, {name: "a/../../evil.html", body: "x"}}, "unsafe path"},
		{"absolute path", []zipEntry{{name: "index.html", body: "x"}, {name: "/etc/passwd", body: "x"}}, "unsafe path"},
		{"backslash", []zipEntry{{name: "index.html", body: "x"}, {name: `a\..\..\evil`, body: "x"}}, "unsafe path"},
		{"drive letter", []zipEntry{{name: "index.html", body: "x"}, {name: "C:/evil.html", body: "x"}}, "unsafe path"},
		{"nul byte", []zipEntry{{name: "index.html", body: "x"}, {name: "a\x00.html", body: "x"}}, "unsafe path"},
		{"duplicate exact", []zipEntry{{name: "index.html", body: "x"}, {name: "index.html", body: "y"}}, "duplicate"},
		{"duplicate case", []zipEntry{{name: "index.html", body: "x"}, {name: "Index.HTML", body: "y"}}, "duplicate"},
		{"symlink", []zipEntry{{name: "index.html", body: "x"}, {name: "link", mode: os.ModeSymlink}}, "not a regular file"},
		{"deep nesting", []zipEntry{{name: "index.html", body: "x"}, {name: "a/b/c/d/e/f/g/h/i/x.css", body: "x"}}, "deeper"},
		{"not a zip", nil, "valid ZIP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			var staging string
			var err error
			if tc.entries == nil {
				staging, err = Extract(dataDir, bytes.NewReader([]byte("not a zip at all")), 17)
			} else {
				staging, err = extract(t, dataDir, tc.entries)
			}
			if err == nil {
				os.RemoveAll(staging)
				t.Fatalf("Extract succeeded, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
			// A rejected archive must not leave a staging dir behind.
			leftovers, _ := filepath.Glob(filepath.Join(Root(dataDir), stagingPrefix+"*"))
			if len(leftovers) > 0 {
				t.Errorf("staging dirs left behind: %v", leftovers)
			}
		})
	}
}

func TestExtractEntryCountLimit(t *testing.T) {
	defer func(v int) { maxEntries = v }(maxEntries)
	maxEntries = 3
	_, err := extract(t, t.TempDir(), []zipEntry{
		{name: "index.html", body: "x"},
		{name: "a.css", body: "x"},
		{name: "b.css", body: "x"},
		{name: "c.css", body: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "more than 3 files") {
		t.Errorf("error = %v, want entry-count limit", err)
	}
}

func TestExtractTotalSizeLimit(t *testing.T) {
	defer func(v int64) { maxTotalBytes = v }(maxTotalBytes)
	maxTotalBytes = 10
	_, err := extract(t, t.TempDir(), []zipEntry{
		{name: "index.html", body: "12345"},
		{name: "a.css", body: "123456"},
	})
	if err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("error = %v, want total-size limit", err)
	}
}

func TestExtractFileSizeLimit(t *testing.T) {
	defer func(v int64) { maxFileBytes = v }(maxFileBytes)
	maxFileBytes = 4
	_, err := extract(t, t.TempDir(), []zipEntry{
		{name: "index.html", body: "12345"},
	})
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error = %v, want per-file limit", err)
	}
}

func TestCommitAndRemove(t *testing.T) {
	dataDir := t.TempDir()
	staging, err := extract(t, dataDir, []zipEntry{{name: "index.html", body: "v1"}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if err := Commit(staging, dataDir, Article, 42); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !Has(dataDir, Article, 42) {
		t.Error("Has = false after Commit")
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Error("staging dir still exists after Commit")
	}

	// A second Commit replaces the tree in place.
	staging2, err := extract(t, dataDir, []zipEntry{{name: "index.html", body: "v2"}, {name: "new.css", body: "x"}})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if err := Commit(staging2, dataDir, Article, 42); err != nil {
		t.Fatalf("Commit replace: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(Dir(dataDir, Article, 42), "index.html"))
	if string(body) != "v2" {
		t.Errorf("index.html = %q, want v2", body)
	}

	if err := Remove(dataDir, Article, 42); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if Has(dataDir, Article, 42) {
		t.Error("Has = true after Remove")
	}
	// Remove of a missing tree is a no-op.
	if err := Remove(dataDir, Page, 1); err != nil {
		t.Errorf("Remove missing: %v", err)
	}
}

func TestCommitRejectsNonStaging(t *testing.T) {
	dataDir := t.TempDir()
	outside := t.TempDir()
	if err := Commit(outside, dataDir, Article, 1); err == nil {
		t.Error("Commit of a non-staging dir succeeded")
	}
}

func TestSafeRelPath(t *testing.T) {
	ok := map[string]string{
		"":                "",
		"index.html":      "index.html",
		"css/site.css":    "css/site.css",
		"./css/site.css":  "css/site.css",
		"a/./b.png":       "a/b.png",
		"sp ace/file.txt": "sp ace/file.txt",
	}
	for in, want := range ok {
		got, good := SafeRelPath(in)
		if !good || got != want {
			t.Errorf("SafeRelPath(%q) = %q, %v; want %q, true", in, got, good, want)
		}
	}
	for _, bad := range []string{"../x", "a/../../x", "..", `\x`, "a\\b", "a\x00b"} {
		if got, good := SafeRelPath(bad); good {
			t.Errorf("SafeRelPath(%q) = %q, true; want rejected", bad, got)
		}
	}
}

func TestParseKind(t *testing.T) {
	if k, ok := ParseKind("article"); !ok || k != Article {
		t.Error("article not parsed")
	}
	if k, ok := ParseKind("page"); !ok || k != Page {
		t.Error("page not parsed")
	}
	if _, ok := ParseKind("files"); ok {
		t.Error("files parsed as a kind")
	}
}
