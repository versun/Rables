package railsmigrate

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// real samples captured from the Rails development database
const (
	realSGID     = "eyJfcmFpbHMiOnsiZGF0YSI6ImdpZDovL3ZlcnN1bi1jbXMvQWN0aXZlU3RvcmFnZTo6QmxvYi81ODQ_ZXhwaXJlc19pbiIsInB1ciI6ImF0dGFjaGFibGUifX0=--bb309933c10faffcb177ee80c615496c0b844917"
	realSignedID = "eyJfcmFpbHMiOnsiZGF0YSI6MSwicHVyIjoiYmxvYl9pZCJ9fQ==--ea6d2173acb07836641cfe11fbfed44a526a650f"
)

// makeSGID builds a Rails 8 style signed gid: base64 envelope wrapping a
// base64 gid payload, with a dummy signature trailer.
func makeSGID(gid string) string {
	inner := base64.StdEncoding.EncodeToString([]byte(gid))
	env := fmt.Sprintf(`{"_rails":{"data":%q,"pur":"attachable"}}`, inner)
	return base64.StdEncoding.EncodeToString([]byte(env)) + "--dummysignature"
}

// makeSignedID builds a blob signed_id whose data is the bare JSON number.
func makeSignedID(id int64) string {
	env := fmt.Sprintf(`{"_rails":{"data":%d,"pur":"blob_id"}}`, id)
	return base64.StdEncoding.EncodeToString([]byte(env)) + "--dummysignature"
}

func TestBlobIDFromSGID(t *testing.T) {
	tests := []struct {
		name    string
		sgid    string
		want    int64
		wantErr bool
	}{
		{"real sample", realSGID, 584, false},
		{"generated rails8 envelope", makeSGID("gid://rables/ActiveStorage::Blob/42?expires_in"), 42, false},
		{"old message wrapper", base64.StdEncoding.EncodeToString([]byte(
			`{"_rails":{"message":"`+base64.StdEncoding.EncodeToString([]byte("gid://rables/ActiveStorage::Blob/7"))+`","exp":null,"pur":"attachable"}}`)) + "--sig", 7, false},
		{"bare gid json", base64.StdEncoding.EncodeToString([]byte(`{"gid":"gid://app/ActiveStorage::Blob/9"}`)) + "--sig", 9, false},
		{"no signature trailer", strings.TrimSuffix(makeSGID("gid://rables/ActiveStorage::Blob/3"), "--dummysignature"), 3, false},
		{"empty", "", 0, true},
		{"garbage", "!!!not-base64!!!", 0, true},
		{"no blob in payload", makeSGID("gid://rables/Article/5"), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := blobIDFromSGID(tt.sgid)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBlobIDFromSignedURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    int64
		wantErr bool
	}{
		{"real redirect url", "/rails/active_storage/blobs/redirect/" + realSignedID + "/pic.png", 1, false},
		{"generated", "/rails/active_storage/blobs/redirect/" + makeSignedID(17) + "/x.png", 17, false},
		{"proxy variant", "/rails/active_storage/blobs/proxy/" + makeSignedID(8) + "/x.png", 8, false},
		{"representation", "/rails/active_storage/representations/redirect/" + makeSignedID(5) + "/digest/x.png", 5, false},
		{"not a rails url", "/files/abc123", 0, true},
		{"absolute url with host", "https://versun.me/rails/active_storage/blobs/redirect/" + makeSignedID(262) + "/pic.jpg", 262, false},
		{"absolute http url", "http://versun.me/rails/active_storage/blobs/redirect/" + makeSignedID(33) + "/pic.jpg", 33, false},
		{"bad payload", "/rails/active_storage/blobs/redirect/" + base64.StdEncoding.EncodeToString([]byte(`{"_rails":{"data":"abc","pur":"blob_id"}}`)) + "--s/x.png", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := blobIDFromSignedURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRewriteContent(t *testing.T) {
	blobs := map[int64]blobRef{
		1: {key: "imgkey1111", filename: "pic.png", contentType: "image/png"},
		2: {key: "dockey2222", filename: "doc.pdf", contentType: "application/pdf"},
	}
	tests := []struct {
		name        string
		body        string
		want        string // exact match when non-empty
		contains    []string
		notContains []string
		rewritten   int
		kept        int
	}{
		{
			name:      "no markers returned byte-identical",
			body:      "<p>hello <b>world</b></p>",
			want:      "<p>hello <b>world</b></p>",
			rewritten: 0,
		},
		{
			name:      "empty body",
			body:      "",
			want:      "",
			rewritten: 0,
		},
		{
			name:        "image attachment becomes img",
			body:        `<p>a</p><action-text-attachment sgid="` + makeSGID("gid://rables/ActiveStorage::Blob/1?expires_in") + `" content-type="image/png"></action-text-attachment>`,
			contains:    []string{`<img src="/files/imgkey1111" alt="pic.png" loading="lazy"/>`},
			notContains: []string{"action-text-attachment"},
			rewritten:   1,
		},
		{
			name:      "non-image attachment becomes link",
			body:      `<action-text-attachment sgid="` + makeSGID("gid://rables/ActiveStorage::Blob/2?expires_in") + `" content-type="application/pdf"></action-text-attachment>`,
			contains:  []string{`<a href="/files/dockey2222">doc.pdf</a>`},
			rewritten: 1,
		},
		{
			name:      "unknown blob is kept and listed",
			body:      `<action-text-attachment sgid="` + makeSGID("gid://rables/ActiveStorage::Blob/99?expires_in") + `"></action-text-attachment>`,
			contains:  []string{"action-text-attachment"},
			rewritten: 0,
			kept:      1,
		},
		{
			name:      "broken sgid falls back to url attribute",
			body:      `<action-text-attachment sgid="bogus" url="/rails/active_storage/blobs/redirect/` + makeSignedID(1) + `/pic.png"></action-text-attachment>`,
			contains:  []string{`<img src="/files/imgkey1111"`},
			rewritten: 1,
		},
		{
			name:      "broken sgid without url is kept",
			body:      `<action-text-attachment sgid="bogus"></action-text-attachment>`,
			contains:  []string{"action-text-attachment"},
			rewritten: 0,
			kept:      1,
		},
		{
			name:      "old style img src rewritten, other attrs preserved",
			body:      `<p><img src="/rails/active_storage/blobs/redirect/` + realSignedID + `/pic.png" alt="old" width="10"/></p>`,
			contains:  []string{`<img src="/files/imgkey1111" alt="old" width="10"/>`},
			rewritten: 1,
		},
		{
			name:      "old style anchor href rewritten",
			body:      `<a href="/rails/active_storage/blobs/redirect/` + makeSignedID(2) + `/doc.pdf">download</a>`,
			contains:  []string{`<a href="/files/dockey2222">download</a>`},
			rewritten: 1,
		},
		{
			name:      "chinese text survives re-serialization",
			body:      `<p>中文段落</p><action-text-attachment sgid="` + makeSGID("gid://rables/ActiveStorage::Blob/1?expires_in") + `"></action-text-attachment>`,
			contains:  []string{"中文段落", `<img src="/files/imgkey1111"`},
			rewritten: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := &RewriteStats{}
			got := rewriteContent(tt.body, blobs, "Article/1", stats)
			if tt.want != "" && got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			for _, c := range tt.contains {
				if !strings.Contains(got, c) {
					t.Errorf("result missing %q: %q", c, got)
				}
			}
			for _, nc := range tt.notContains {
				if strings.Contains(got, nc) {
					t.Errorf("result should not contain %q: %q", nc, got)
				}
			}
			if stats.Rewritten != tt.rewritten {
				t.Errorf("rewritten = %d, want %d", stats.Rewritten, tt.rewritten)
			}
			if len(stats.Kept) != tt.kept {
				t.Errorf("kept = %d, want %d (%+v)", len(stats.Kept), tt.kept, stats.Kept)
			}
			for _, k := range stats.Kept {
				if k.Record != "Article/1" || k.Reason == "" || k.Snippet == "" {
					t.Errorf("incomplete kept ref: %+v", k)
				}
			}
		})
	}
}
