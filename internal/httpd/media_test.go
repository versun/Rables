package httpd

import (
	"bytes"
	"crypto/md5"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"rables/internal/config"
	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/templates"
)

// newMediaServer builds a Server with a real SQLite DB and files dir under a
// temp dir, mounted on a test router via RegisterMediaRoutes.
func newMediaServer(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	renderer, err := templates.New()
	if err != nil {
		t.Fatalf("load templates: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	s := NewServer(database, config.Config{Addr: ":8080", DataDir: dir, HMACSecret: "x"}, logger, renderer)
	r := chi.NewRouter()
	RegisterMediaRoutes(r, s)
	return s, r, dir
}

// mediaSession inserts a user plus session row and returns the session cookie.
func mediaSession(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	now := time.Now().Unix()
	user, err := s.Q.CreateUser(t.Context(), query.CreateUserParams{
		UserName: "admin", PasswordDigest: "x", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := s.Q.CreateSession(t.Context(), query.CreateSessionParams{
		Token: "media-test-token", UserID: user.ID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: "media-test-token"}
}

// uploadFile posts one multipart file (plus extra fields) to /admin/uploads.
func uploadFile(t *testing.T, h http.Handler, filename, contentType string, data []byte, fields map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="` + filename + `"`},
		"Content-Type":        {contentType},
	})
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/uploads", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// noisyPNG renders a wxh PNG of pseudo-random pixels: large enough that the
// resized variant is guaranteed smaller on disk.
func noisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(rng.Intn(256)), G: uint8(rng.Intn(256)), B: uint8(rng.Intn(256)), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func uploadResponse(t *testing.T, rec *httptest.ResponseRecorder) (key, url string) {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201 (body %q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Key string `json:"key"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if resp.Key == "" || resp.URL != "/files/"+resp.Key {
		t.Fatalf("upload response = %+v, want key and /files/<key> url", resp)
	}
	return resp.Key, resp.URL
}

func TestUploadAndServeImage(t *testing.T) {
	s, h, dir := newMediaServer(t)
	session := mediaSession(t, s)
	pngData := noisyPNG(t, 2000, 1500)

	key, url := uploadResponse(t, uploadFile(t, h, "photo.png", "image/png", pngData, nil, session))

	// The upload form renders for authenticated users.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/uploads/new", nil)
	req.AddCookie(session)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `enctype="multipart/form-data"`) {
		t.Errorf("upload form: status = %d, contains form = %v", rec.Code, strings.Contains(rec.Body.String(), "multipart/form-data"))
	}

	// Public serving: 200, correct Content-Type, immutable cache header,
	// original bytes served unchanged.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", url, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
	if !bytes.Equal(rec.Body.Bytes(), pngData) {
		t.Error("served bytes differ from the uploaded original")
	}

	// files row: checksum follows ActiveStorage semantics (base64 MD5).
	file, err := s.Media().FileByKey(t.Context(), key)
	if err != nil {
		t.Fatalf("file row: %v", err)
	}
	sum := md5.Sum(pngData)
	if want := base64.StdEncoding.EncodeToString(sum[:]); file.Checksum.String != want {
		t.Errorf("checksum = %q, want %q", file.Checksum.String, want)
	}
	if file.ByteSize != int64(len(pngData)) {
		t.Errorf("byte_size = %d, want %d", file.ByteSize, len(pngData))
	}

	// Disk layout: <DataDir>/files/xx/yy/<key>.
	if _, err := os.Stat(s.Media().PathFor(key)); err != nil {
		t.Errorf("original missing on disk at %s: %v", s.Media().PathFor(key), err)
	}
	if wantPrefix := dir + "/files/" + key[0:2] + "/" + key[2:4] + "/"; !strings.HasPrefix(s.Media().PathFor(key), wantPrefix) {
		t.Errorf("path %q does not follow %s layout", s.Media().PathFor(key), wantPrefix)
	}

	// Variant: exactly one, variant_of -> original, smaller, within 1024x768.
	variants, err := s.Q.ListFileVariants(t.Context(), sql.NullInt64{Int64: file.ID, Valid: true})
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 1 {
		t.Fatalf("variant count = %d, want 1", len(variants))
	}
	variant := variants[0]
	if !variant.VariantOf.Valid || variant.VariantOf.Int64 != file.ID {
		t.Errorf("variant_of = %+v, want %d", variant.VariantOf, file.ID)
	}
	if variant.ByteSize >= file.ByteSize {
		t.Errorf("variant byte_size = %d, want < %d", variant.ByteSize, file.ByteSize)
	}
	vf, err := os.Open(s.Media().PathFor(variant.Key))
	if err != nil {
		t.Fatalf("variant missing on disk: %v", err)
	}
	defer vf.Close()
	vimg, err := png.Decode(vf)
	if err != nil {
		t.Fatalf("decode variant: %v", err)
	}
	if b := vimg.Bounds(); b.Dx() > 1024 || b.Dy() > 768 {
		t.Errorf("variant bounds = %dx%d, want within 1024x768", b.Dx(), b.Dy())
	}

	// The variant is publicly served too.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+variant.Key, nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("GET variant: status = %d content-type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestUploadNonImageHasNoVariant(t *testing.T) {
	s, h, _ := newMediaServer(t)
	session := mediaSession(t, s)
	text := []byte("plain text file\n")

	key, url := uploadResponse(t, uploadFile(t, h, "notes.txt", "text/plain", text, nil, session))

	file, err := s.Media().FileByKey(t.Context(), key)
	if err != nil {
		t.Fatalf("file row: %v", err)
	}
	variants, err := s.Q.ListFileVariants(t.Context(), sql.NullInt64{Int64: file.ID, Valid: true})
	if err != nil {
		t.Fatalf("list variants: %v", err)
	}
	if len(variants) != 0 {
		t.Errorf("non-image variant count = %d, want 0", len(variants))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("GET text: status = %d content-type = %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestUploadCreatesAttachment(t *testing.T) {
	s, h, _ := newMediaServer(t)
	session := mediaSession(t, s)

	key, _ := uploadResponse(t, uploadFile(t, h, "a.txt", "text/plain", []byte("x"),
		map[string]string{"record_type": "Article", "record_id": "7", "name": "embeds"}, session))

	file, err := s.Media().FileByKey(t.Context(), key)
	if err != nil {
		t.Fatalf("file row: %v", err)
	}
	atts, err := s.Q.ListAttachmentsForFile(t.Context(), file.ID)
	if err != nil {
		t.Fatalf("list attachments: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("attachment count = %d, want 1", len(atts))
	}
	if atts[0].RecordType != "Article" || atts[0].RecordID != 7 || atts[0].Name != "embeds" {
		t.Errorf("attachment = %+v, want Article/7/embeds", atts[0])
	}
}

// TestUploadAttachFailureLeavesNoOrphan: when Attach fails after Store, the
// client never learns the key, so the upload — files rows plus the original
// and variant blobs — must be reclaimed instead of staying orphaned.
func TestUploadAttachFailureLeavesNoOrphan(t *testing.T) {
	s, h, _ := newMediaServer(t)
	session := mediaSession(t, s)

	if _, err := s.DB.Exec(`CREATE TRIGGER reject_attachment_inserts BEFORE INSERT ON attachments
		BEGIN SELECT RAISE(ABORT, 'attach blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	rec := uploadFile(t, h, "photo.png", "image/png", noisyPNG(t, 2000, 1500),
		map[string]string{"record_type": "Article", "record_id": "7", "name": "embeds"}, session)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("upload: status = %d, want 500", rec.Code)
	}
	var rows int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rows); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if rows != 0 {
		t.Errorf("files rows = %d, want 0 (orphan rows not reclaimed)", rows)
	}
	blobs := 0
	walkErr := filepath.Walk(filepath.Join(s.Cfg.DataDir, "files"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			blobs++
		}
		return err
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		t.Fatalf("walk files dir: %v", walkErr)
	}
	if blobs != 0 {
		t.Errorf("blobs on disk = %d, want 0 (orphan blob not reclaimed)", blobs)
	}
}

func TestUploadRequiresAuth(t *testing.T) {
	_, h, _ := newMediaServer(t)
	rec := uploadFile(t, h, "a.txt", "text/plain", []byte("x"), nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/session/new" {
		t.Errorf("unauthenticated upload: status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
}

// fillReader yields n bytes of 'a' without allocating them, so an over-limit
// upload body can be streamed cheaply.
type fillReader struct{ n int64 }

func (r *fillReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = 'a'
	}
	r.n -= int64(len(p))
	return len(p), nil
}

// TestUploadOversize: a body past maxUploadSize is rejected 413 (the multipart
// parser surfaces *http.MaxBytesError), while a malformed multipart body
// stays 400.
func TestUploadOversize(t *testing.T) {
	s, h, _ := newMediaServer(t)
	session := mediaSession(t, s)

	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	// CreatePart writes the part headers into head; the payload streams from
	// fillReader past maxUploadSize, so MaxBytesReader trips before the (never
	// written) closing boundary is reached.
	if _, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="file"; filename="big.bin"`},
		"Content-Type":        {"application/octet-stream"},
	}); err != nil {
		t.Fatalf("create part: %v", err)
	}
	body := io.MultiReader(&head, &fillReader{n: maxUploadSize + 1})
	req := httptest.NewRequest(http.MethodPost, "/admin/uploads", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(session)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize upload: status = %d, want 413 (body %q)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/uploads", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.AddCookie(session)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed upload: status = %d, want 400", rec.Code)
	}
}

// TestServeFileActiveContentTypes: stored active content types (HTML, SVG,
// XML, JS) are forced to a binary attachment download instead of being
// rendered inline (stored-XSS defense, mirroring Rails ActiveStorage);
// inert types keep their inline Content-Type. Every response carries
// X-Content-Type-Options: nosniff.
func TestServeFileActiveContentTypes(t *testing.T) {
	s, h, _ := newMediaServer(t)
	session := mediaSession(t, s)

	tests := []struct {
		name        string
		filename    string
		contentType string
		wantCT      string
		wantAttach  bool
	}{
		{name: "html", filename: "page.html", contentType: "text/html", wantCT: "application/octet-stream", wantAttach: true},
		{name: "svg", filename: "icon.svg", contentType: "image/svg+xml", wantCT: "application/octet-stream", wantAttach: true},
		{name: "javascript", filename: "app.js", contentType: "application/javascript", wantCT: "application/octet-stream", wantAttach: true},
		{name: "case and parameters normalized", filename: "x.html", contentType: "Text/HTML; charset=UTF-8", wantCT: "application/octet-stream", wantAttach: true},
		{name: "rss +xml subtype", filename: "feed.xml", contentType: "application/rss+xml", wantCT: "application/octet-stream", wantAttach: true},
		{name: "plain text stays inline", filename: "notes.txt", contentType: "text/plain", wantCT: "text/plain", wantAttach: false},
		{name: "png stays inline", filename: "photo.png", contentType: "image/png", wantCT: "image/png", wantAttach: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, url := uploadResponse(t, uploadFile(t, h, tt.filename, tt.contentType, []byte("data"), nil, session))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: status = %d, want 200", url, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != tt.wantCT {
				t.Errorf("Content-Type = %q, want %q", ct, tt.wantCT)
			}
			if cd := rec.Header().Get("Content-Disposition"); tt.wantAttach != (cd == "attachment") {
				t.Errorf("Content-Disposition = %q, want attachment = %v", cd, tt.wantAttach)
			}
			if xcto := rec.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", xcto)
			}
		})
	}
}

// TestServeFileEmptyContentType: rows without a stored content type (the
// Rails import upserts the files table wholesale, so it can write
// content_type NULL with an .html filename) must not fall through to
// ServeContent's extension-based guessing — that would render x.html inline
// as text/html (stored XSS). Like active content types, they are forced to
// a binary attachment download.
func TestServeFileEmptyContentType(t *testing.T) {
	s, h, _ := newMediaServer(t)

	key := "0123456789abcdef0123456789abcdef"
	data := []byte("<html><script>alert(1)</script></html>")
	path := s.Media().PathFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create blob dir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if _, err := s.Q.CreateFile(t.Context(), query.CreateFileParams{
		Key:       key,
		Filename:  "x.html",
		ByteSize:  int64(len(data)),
		CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("insert file row: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+key, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /files/%s: status = %d, want 200", key, rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "attachment" {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if xcto := rec.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", xcto)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Error("served bytes differ from the stored blob")
	}
}

// TestServeFileMalformedContentType: rows with a malformed stored content
// type (written verbatim by the import paths) match no active-type entry,
// but browsers drop the invalid Content-Type of a top-level navigation and
// sniff the body as HTML (nosniff only constrains script/style) —
// same-origin stored XSS. Anything that does not parse as a well-formed
// type/subtype media type is forced to a binary attachment download, same
// as active content types.
func TestServeFileMalformedContentType(t *testing.T) {
	s, h, _ := newMediaServer(t)

	data := []byte("<html><script>alert(1)</script></html>")
	tests := []struct {
		name        string
		contentType string
	}{
		{name: "trailing junk after subtype", contentType: "text/html x"},
		{name: "space inside type", contentType: "tex t/html"},
		{name: "missing subtype", contentType: "text"},
		{name: "invalid parameter", contentType: "text/plain;;"},
		{name: "control byte in quoted parameter", contentType: "text/plain; name=\"a\x01b\""},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := fmt.Sprintf("0123456789abcdef0123456789abc%x", i)
			path := s.Media().PathFor(key)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("create blob dir: %v", err)
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatalf("write blob: %v", err)
			}
			if _, err := s.Q.CreateFile(t.Context(), query.CreateFileParams{
				Key:         key,
				Filename:    "x.html",
				ContentType: sql.NullString{String: tt.contentType, Valid: true},
				ByteSize:    int64(len(data)),
				CreatedAt:   time.Now().Unix(),
			}); err != nil {
				t.Fatalf("insert file row: %v", err)
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+key, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /files/%s: status = %d, want 200", key, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
				t.Errorf("Content-Type = %q, want application/octet-stream", ct)
			}
			if cd := rec.Header().Get("Content-Disposition"); cd != "attachment" {
				t.Errorf("Content-Disposition = %q, want attachment", cd)
			}
			if xcto := rec.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", xcto)
			}
			if !bytes.Equal(rec.Body.Bytes(), data) {
				t.Error("served bytes differ from the stored blob")
			}
		})
	}
}

func TestServeFileKeyHandling(t *testing.T) {
	_, h, _ := newMediaServer(t)

	tests := []struct {
		name string
		path string
		want int
	}{
		{name: "unknown but well-formed key", path: "/files/0123456789abcdef0123456789abcdef", want: http.StatusNotFound},
		{name: "dot segments rejected", path: "/files/..", want: http.StatusNotFound},
		{name: "encoded traversal rejected", path: "/files/%2e%2e%2f%2e%2e%2fetc%2fpasswd", want: http.StatusNotFound},
		{name: "too short", path: "/files/ab", want: http.StatusNotFound},
		{name: "non-alphanumeric", path: "/files/zz-zz", want: http.StatusNotFound},
		{name: "path with extra segment not routed", path: "/files/ab/cd", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if rec.Code != tt.want {
				t.Errorf("GET %s: status = %d, want %d", tt.path, rec.Code, tt.want)
			}
		})
	}
}
