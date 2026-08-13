package subscribers

import (
	"database/sql"
	"encoding/base64"
	"strings"
	"testing"

	"rables/internal/db"
	"rables/internal/db/query"
)

func insertTag(t *testing.T, database *sql.DB, name string) int64 {
	t.Helper()
	res, err := database.Exec("INSERT INTO tags (name, slug, created_at, updated_at) VALUES (?, ?, 1, 1)", name, strings.ToLower(name))
	if err != nil {
		t.Fatalf("insert tag: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestValidEmail(t *testing.T) {
	tests := []struct {
		email string
		want  bool
	}{
		{"user@example.com", true},
		{"user+tag@example.co", true},
		{"user.name@sub.domain.com", true},
		{"user%percent@example.com", true},
		{"a@b.co", true},
		{"USER@EXAMPLE.COM", true},
		{"", false},
		{"plain", false},
		{"@example.com", false},
		{"user@", false},
		{"user name@example.com", false},
		{"user@exa mple.com", false},
		{"user@example..com", false},
		{"user@example.com ", false}, // no trimming, like the Rails model
	}
	for _, tt := range tests {
		if got := ValidEmail(tt.email); got != tt.want {
			t.Errorf("ValidEmail(%q) = %v, want %v", tt.email, got, tt.want)
		}
	}
}

func TestNewToken(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	// SecureRandom.urlsafe_base64(32): 32 bytes, base64url, no padding.
	raw, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil {
		t.Fatalf("token is not raw base64url: %v", err)
	}
	if len(raw) != 32 || len(a) != 43 {
		t.Errorf("token decodes to %d bytes (%d chars), want 32 (43)", len(raw), len(a))
	}
	b, _ := NewToken()
	if a == b {
		t.Error("two tokens are identical")
	}
}

// TestFilterRelevant covers the tag semantics of plan section 4.6: no tags =
// subscribed to all content; with tags = only matching articles; inactive
// subscribers never receive anything.
func TestFilterRelevant(t *testing.T) {
	active := Recipient{ID: 1, Email: "all@example.com", Active: true}
	tagged12 := Recipient{ID: 2, Email: "t12@example.com", Active: true, TagIDs: []int64{1, 2}}
	tagged3 := Recipient{ID: 3, Email: "t3@example.com", Active: true, TagIDs: []int64{3}}
	pending := Recipient{ID: 4, Email: "pending@example.com", Active: false}
	unsubbed := Recipient{ID: 5, Email: "gone@example.com", Active: false, TagIDs: []int64{1}}
	all := []Recipient{active, tagged12, tagged3, pending, unsubbed}

	tests := []struct {
		name          string
		articleTagIDs []int64
		wantIDs       []int64
	}{
		{"article without tags reaches only all-content subscribers", nil, []int64{1}},
		{"tag 1 reaches all-content and tag-1 subscribers", []int64{1}, []int64{1, 2}},
		{"tag 3 reaches all-content and tag-3 subscribers", []int64{3}, []int64{1, 3}},
		{"multiple article tags union", []int64{2, 3}, []int64{1, 2, 3}},
		{"unknown tag reaches only all-content subscribers", []int64{9}, []int64{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterRelevant(all, tt.articleTagIDs)
			var gotIDs []int64
			for _, r := range got {
				gotIDs = append(gotIDs, r.ID)
			}
			if len(gotIDs) != len(tt.wantIDs) {
				t.Fatalf("ids = %v, want %v", gotIDs, tt.wantIDs)
			}
			for i, id := range tt.wantIDs {
				if gotIDs[i] != id {
					t.Fatalf("ids = %v, want %v", gotIDs, tt.wantIDs)
				}
			}
		})
	}
}

func TestCreateGeneratesTokens(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	sub, err := Create(t.Context(), query.New(database), "a@example.com", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !sub.ConfirmationToken.Valid || len(sub.ConfirmationToken.String) != 43 {
		t.Errorf("confirmation_token = %+v, want a 43-char token", sub.ConfirmationToken)
	}
	if !sub.UnsubscribeToken.Valid || len(sub.UnsubscribeToken.String) != 43 {
		t.Errorf("unsubscribe_token = %+v, want a 43-char token", sub.UnsubscribeToken)
	}

	// ||= semantics: pre-set tokens (e.g. from imports) are kept.
	kept, err := Create(t.Context(), query.New(database), "b@example.com", "preset-confirm", "preset-unsub")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if kept.ConfirmationToken.String != "preset-confirm" || kept.UnsubscribeToken.String != "preset-unsub" {
		t.Errorf("pre-set tokens not kept: %+v", kept)
	}
}

// TestCreateNormalizesEmail: the stored address is the normalized form, so
// UNIQUE(email) rejects a case variant of an existing row instead of
// forking it into a duplicate.
func TestCreateNormalizesEmail(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	sub, err := Create(t.Context(), query.New(database), "  Foo@Example.COM ", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sub.Email != "foo@example.com" {
		t.Errorf("email = %q, want normalized foo@example.com", sub.Email)
	}
	if _, err := Create(t.Context(), query.New(database), "FOO@example.com", "", ""); err == nil {
		t.Error("Create of a case variant: want a UNIQUE violation, got nil")
	}
}

func TestReplaceTags(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	q := query.New(database)

	sub, err := Create(t.Context(), q, "a@example.com", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tagIDs := func() []int64 {
		var ids []int64
		rows, err := database.Query("SELECT tag_id FROM subscriber_tags WHERE subscriber_id = ? ORDER BY tag_id", sub.ID)
		if err != nil {
			t.Fatalf("query tags: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			ids = append(ids, id)
		}
		return ids
	}
	t1, t2, t3 := insertTag(t, database, "Go"), insertTag(t, database, "Rails"), insertTag(t, database, "SQLite")

	if err := ReplaceTags(t.Context(), database, sub.ID, []int64{t1, t2}); err != nil {
		t.Fatalf("ReplaceTags: %v", err)
	}
	if got := tagIDs(); len(got) != 2 || got[0] != t1 || got[1] != t2 {
		t.Fatalf("tags = %v, want [%d %d]", got, t1, t2)
	}

	// Reassignment replaces the whole set.
	if err := ReplaceTags(t.Context(), database, sub.ID, []int64{t3}); err != nil {
		t.Fatalf("ReplaceTags: %v", err)
	}
	if got := tagIDs(); len(got) != 1 || got[0] != t3 {
		t.Fatalf("tags = %v, want [%d]", got, t3)
	}

	// Empty means subscribed to all content.
	if err := ReplaceTags(t.Context(), database, sub.ID, nil); err != nil {
		t.Fatalf("ReplaceTags: %v", err)
	}
	if got := tagIDs(); len(got) != 0 {
		t.Fatalf("tags = %v, want empty", got)
	}
}

// TestReplaceTagsAtomicOnError: a failing insert (dangling tag id violates
// the FK) rolls the delete back too, leaving the previous tag set intact.
func TestReplaceTagsAtomicOnError(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	sub, err := Create(t.Context(), query.New(database), "a@example.com", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tagID, otherID := insertTag(t, database, "Go"), insertTag(t, database, "Rails")
	if err := ReplaceTags(t.Context(), database, sub.ID, []int64{tagID}); err != nil {
		t.Fatalf("ReplaceTags: %v", err)
	}

	// The first insert would succeed, the dangling id fails: without a
	// transaction the subscriber would be left with [otherID].
	if err := ReplaceTags(t.Context(), database, sub.ID, []int64{otherID, 99999}); err == nil {
		t.Fatal("ReplaceTags with a dangling tag id: want an FK error")
	}
	var got []int64
	rows, err := database.Query("SELECT tag_id FROM subscriber_tags WHERE subscriber_id = ?", sub.ID)
	if err != nil {
		t.Fatalf("query tags: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan tag id: %v", err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tags: %v", err)
	}
	if len(got) != 1 || got[0] != tagID {
		t.Fatalf("tags = %v, want unchanged [%d] after the rolled-back call", got, tagID)
	}
}

func TestDestroyCascadesSubscriberTags(t *testing.T) {
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	q := query.New(database)

	sub, err := Create(t.Context(), q, "a@example.com", "", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, err := database.Exec("INSERT INTO tags (name, slug, created_at, updated_at) VALUES ('Go', 'go', 1, 1)")
	if err != nil {
		t.Fatalf("insert tag: %v", err)
	}
	tagID, _ := res.LastInsertId()
	if err := ReplaceTags(t.Context(), database, sub.ID, []int64{tagID}); err != nil {
		t.Fatalf("ReplaceTags: %v", err)
	}

	if err := Destroy(t.Context(), database, sub.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	var subs, joins int
	database.QueryRow("SELECT COUNT(*) FROM subscribers").Scan(&subs)
	database.QueryRow("SELECT COUNT(*) FROM subscriber_tags").Scan(&joins)
	if subs != 0 || joins != 0 {
		t.Errorf("subscribers = %d, subscriber_tags = %d, want 0 and 0", subs, joins)
	}
}
