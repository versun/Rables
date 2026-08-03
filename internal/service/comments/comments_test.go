package comments

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"rables/internal/db"
	"rables/internal/db/query"
	"rables/internal/domain"
	"rables/internal/jobs"
)

func cm(id int64, platform string, status domain.CommentStatus, parentID int64, publishedAt int64) query.Comment {
	return query.Comment{
		ID:              id,
		CommentableType: sql.NullString{String: "Article", Valid: true},
		CommentableID:   sql.NullInt64{Int64: 1, Valid: true},
		ParentID:        sql.NullInt64{Int64: parentID, Valid: parentID != 0},
		AuthorName:      "n",
		Content:         "c",
		Status:          int64(status),
		Platform:        sql.NullString{String: platform, Valid: platform != ""},
		PublishedAt:     sql.NullInt64{Int64: publishedAt, Valid: publishedAt != 0},
		CreatedAt:       1000,
	}
}

// treeShape flattens the tree to (id, type, depth) tuples in display order.
func treeShape(nodes []Threaded) [][3]any {
	var out [][3]any
	var walk func(ns []Threaded)
	walk = func(ns []Threaded) {
		for _, n := range ns {
			out = append(out, [3]any{n.Comment.ID, n.Type, n.Depth})
			walk(n.Replies)
		}
	}
	walk(nodes)
	return out
}

func TestBuildTree(t *testing.T) {
	approved, pending, rejected := domain.CommentApproved, domain.CommentPending, domain.CommentRejected
	tests := []struct {
		name string
		list []query.Comment
		want [][3]any // (id, type, depth) in display order
	}{
		{
			name: "published_at ascending",
			list: []query.Comment{cm(2, "", approved, 0, 200), cm(1, "", approved, 0, 100), cm(3, "", approved, 0, 300)},
			want: [][3]any{{int64(1), "local", 0}, {int64(2), "local", 0}, {int64(3), "local", 0}},
		},
		{
			name: "nesting and depth",
			list: []query.Comment{cm(1, "", approved, 0, 100), cm(2, "", approved, 1, 200), cm(3, "", approved, 2, 300)},
			want: [][3]any{{int64(1), "local", 0}, {int64(2), "local", 1}, {int64(3), "local", 2}},
		},
		{
			name: "local replies must be approved",
			list: []query.Comment{cm(1, "", approved, 0, 100), cm(2, "", pending, 1, 200), cm(3, "", rejected, 1, 300), cm(4, "", approved, 1, 400)},
			want: [][3]any{{int64(1), "local", 0}, {int64(4), "local", 1}},
		},
		{
			name: "pending or rejected roots hidden",
			list: []query.Comment{cm(1, "", pending, 0, 100), cm(2, "", rejected, 0, 200)},
			want: nil,
		},
		{
			name: "platform groups follow locals in fixed order",
			list: []query.Comment{
				cm(1, "twitter", approved, 0, 100), cm(2, "", approved, 0, 200),
				cm(3, "mastodon", pending, 0, 300), cm(4, "bluesky", approved, 0, 400),
			},
			want: [][3]any{{int64(2), "local", 0}, {int64(3), "mastodon", 0}, {int64(4), "bluesky", 0}, {int64(1), "twitter", 0}},
		},
		{
			name: "platform replies keep the same platform any status",
			list: []query.Comment{
				cm(1, "mastodon", pending, 0, 100), cm(2, "mastodon", rejected, 1, 200),
				cm(3, "bluesky", approved, 1, 300), cm(4, "", approved, 1, 400),
			},
			want: [][3]any{{int64(1), "mastodon", 0}, {int64(2), "mastodon", 1}},
		},
		{
			name: "orphan subtree dropped",
			list: []query.Comment{cm(2, "", approved, 99, 200)},
			want: nil,
		},
		{
			name: "child of invisible parent dropped",
			list: []query.Comment{cm(1, "", pending, 0, 100), cm(2, "", approved, 1, 200)},
			want: nil,
		},
		{
			name: "cycle does not recurse forever",
			list: []query.Comment{cm(1, "", approved, 2, 100), cm(2, "", approved, 1, 200)},
			want: nil,
		},
		{
			name: "reply order follows published_at",
			list: []query.Comment{cm(1, "", approved, 0, 100), cm(3, "", approved, 1, 300), cm(2, "", approved, 1, 200)},
			want: [][3]any{{int64(1), "local", 0}, {int64(2), "local", 1}, {int64(3), "local", 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := treeShape(BuildTree(tt.list))
			if len(got) != len(tt.want) {
				t.Fatalf("shape = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("shape = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestPrepareDisplay(t *testing.T) {
	nodes := BuildTree([]query.Comment{
		cm(1, "", domain.CommentApproved, 0, 100),
		cm(2, "", domain.CommentApproved, 1, 200),
		cm(3, "mastodon", domain.CommentApproved, 0, 300),
	})
	PrepareDisplay(nodes, "Asia/Shanghai", FormData{Action: "/comments?article_id=x", Question: "1 + 2 =", Token: "tok"})

	if nodes[0].Form == nil || nodes[0].Form.ParentID != 1 || nodes[0].Form.Token != "tok" {
		t.Errorf("local root missing reply form: %+v", nodes[0].Form)
	}
	if nodes[0].Replies[0].Form == nil || nodes[0].Replies[0].Form.ParentID != 2 {
		t.Errorf("local child missing reply form: %+v", nodes[0].Replies[0].Form)
	}
	if nodes[1].Form != nil {
		t.Error("platform root must not get a reply form")
	}
	if nodes[0].TimeZone != "Asia/Shanghai" || nodes[0].Replies[0].TimeZone != "Asia/Shanghai" {
		t.Error("time zone not propagated")
	}
}

func TestValidateNew(t *testing.T) {
	base := cm(1, "", domain.CommentPending, 0, 100)
	parent := cm(9, "", domain.CommentApproved, 0, 100)
	otherParent := parent
	otherParent.CommentableID = sql.NullInt64{Int64: 2, Valid: true}

	reply := cm(2, "", domain.CommentPending, 9, 200)
	selfReply := reply
	selfReply.ID = 9 // same as parent.ID
	badEmail := cm(1, "", domain.CommentPending, 0, 100)
	badEmail.AuthorEmail = sql.NullString{String: "nope", Valid: true}
	badURL := cm(1, "", domain.CommentPending, 0, 100)
	badURL.AuthorUrl = sql.NullString{String: "ftp://x", Valid: true}

	tests := []struct {
		name    string
		comment query.Comment
		parent  *query.Comment
		want    []string
	}{
		{name: "valid top-level", comment: base},
		{name: "valid reply", comment: reply, parent: &parent},
		{name: "blank author", comment: func() query.Comment { c := base; c.AuthorName = " "; return c }(), want: []string{"Author name can't be blank"}},
		{name: "blank content", comment: func() query.Comment { c := base; c.Content = ""; return c }(), want: []string{"Content can't be blank"}},
		{name: "bad author url", comment: badURL, want: []string{"Author url must be a valid URL"}},
		{name: "bad email", comment: badEmail, want: []string{"Author email must be a valid email"}},
		{name: "parent missing", comment: reply, parent: nil, want: []string{"Parent does not exist"}},
		{name: "parent of other commentable", comment: reply, parent: &otherParent, want: []string{"Parent must belong to the same Article"}},
		{name: "self reference", comment: selfReply, parent: &parent, want: []string{"Parent cannot reference itself"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateNew(tt.comment, tt.parent)
			if len(got) != len(tt.want) {
				t.Fatalf("ValidateNew = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("ValidateNew = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestAcceptsComments(t *testing.T) {
	tests := []struct {
		status  domain.Status
		comment int64
		want    bool
	}{
		{domain.StatusPublish, 1, true},
		{domain.StatusShared, 1, true},
		{domain.StatusDraft, 1, false},
		{domain.StatusSchedule, 1, false},
		{domain.StatusTrash, 1, false},
		{domain.StatusPublish, 0, false},
		{domain.StatusShared, 0, false},
	}
	for _, tt := range tests {
		if got := AcceptsComments(int64(tt.status), tt.comment); got != tt.want {
			t.Errorf("AcceptsComments(%s, %d) = %v, want %v", tt.status, tt.comment, got, tt.want)
		}
	}
}

func TestVisibleCount(t *testing.T) {
	list := []query.Comment{
		cm(1, "", domain.CommentApproved, 0, 100),        // local approved: visible
		cm(2, "", domain.CommentPending, 0, 200),         // local pending: hidden
		cm(3, "mastodon", domain.CommentPending, 0, 300), // external: visible
	}
	if got := VisibleCount(list); got != 2 {
		t.Errorf("VisibleCount = %d, want 2", got)
	}
}

func newTestQueries(t *testing.T) *query.Queries {
	t.Helper()
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return query.New(database)
}

func TestUpsertExternal(t *testing.T) {
	q := newTestQueries(t)
	ctx := t.Context()
	approved := domain.CommentApproved
	pending := domain.CommentPending
	data := ExternalData{
		ExternalID: "ext-1", AuthorName: "Ann", AuthorUsername: "ann",
		Content: "hi", URL: "https://example.com/1", PublishedAt: 500,
	}

	// New row takes the requested status.
	c, res, err := UpsertExternal(ctx, q, "Article", 1, "mastodon", data, &approved)
	if err != nil || res != UpsertCreated {
		t.Fatalf("first upsert: res = %s, err = %v", res, err)
	}
	if c.Status != int64(approved) {
		t.Errorf("created status = %d, want approved", c.Status)
	}

	// Identical re-fetch changes nothing.
	_, res, err = UpsertExternal(ctx, q, "Article", 1, "mastodon", data, &pending)
	if err != nil || res != UpsertUnchanged {
		t.Fatalf("identical upsert: res = %s, err = %v", res, err)
	}

	// Changed content updates the row but never the moderated status.
	changed := data
	changed.Content = "edited"
	c, res, err = UpsertExternal(ctx, q, "Article", 1, "mastodon", changed, &pending)
	if err != nil || res != UpsertUpdated {
		t.Fatalf("changed upsert: res = %s, err = %v", res, err)
	}
	if c.Content != "edited" {
		t.Errorf("content = %q, want edited", c.Content)
	}
	if c.Status != int64(approved) {
		t.Errorf("status overwritten: got %d, want approved", c.Status)
	}

	// Nil status creates a pending row (model default).
	c, _, err = UpsertExternal(ctx, q, "Article", 1, "mastodon", ExternalData{
		ExternalID: "ext-2", AuthorName: "Bo", Content: "x", PublishedAt: 600,
	}, nil)
	if err != nil {
		t.Fatalf("nil-status upsert: %v", err)
	}
	if c.Status != int64(domain.CommentPending) {
		t.Errorf("nil status: got %d, want pending", c.Status)
	}
}

func TestEnqueueReplyNotification(t *testing.T) {
	newEnv := func(t *testing.T) (*sql.DB, *query.Queries, *jobs.Enqueuer) {
		t.Helper()
		database, err := db.Open(t.TempDir())
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		t.Cleanup(func() { database.Close() })
		return database, query.New(database), jobs.NewEnqueuer(database)
	}
	create := func(t *testing.T, q *query.Queries, parentID int64, status domain.CommentStatus, platform, email string) query.Comment {
		t.Helper()
		now := time.Now().UTC().Unix()
		c, err := q.CreateComment(t.Context(), query.CreateCommentParams{
			CommentableType: sql.NullString{String: "Article", Valid: true},
			CommentableID:   sql.NullInt64{Int64: 1, Valid: true},
			ParentID:        sql.NullInt64{Int64: parentID, Valid: parentID != 0},
			AuthorName:      "n",
			AuthorEmail:     sql.NullString{String: email, Valid: email != ""},
			Content:         "c",
			Status:          int64(status),
			Platform:        sql.NullString{String: platform, Valid: platform != ""},
			CreatedAt:       now,
			UpdatedAt:       now,
		})
		if err != nil {
			t.Fatalf("create comment: %v", err)
		}
		return c
	}
	jobCount := func(t *testing.T, database *sql.DB) int {
		t.Helper()
		var n int
		if err := database.QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM job_runs WHERE kind = ?", jobs.KindCommentReplyNotification).Scan(&n); err != nil {
			t.Fatalf("count jobs: %v", err)
		}
		return n
	}

	tests := []struct {
		name        string
		parentEmail string
		parentPlat  string
		status      domain.CommentStatus
		plat        string
		email       string
		wantEnqueue bool
	}{
		{name: "approved local reply notifies", parentEmail: "p@example.com", status: domain.CommentApproved, wantEnqueue: true},
		{name: "pending reply does not notify", parentEmail: "p@example.com", status: domain.CommentPending},
		{name: "parent without email", status: domain.CommentApproved},
		{name: "external parent", parentEmail: "p@example.com", parentPlat: "mastodon", status: domain.CommentApproved},
		{name: "external reply", parentEmail: "p@example.com", status: domain.CommentApproved, plat: "mastodon"},
		{name: "same author email", parentEmail: "p@example.com", status: domain.CommentApproved, email: "P@example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, q, enq := newEnv(t)
			parent := create(t, q, 0, domain.CommentApproved, tt.parentPlat, tt.parentEmail)
			reply := create(t, q, parent.ID, tt.status, tt.plat, tt.email)

			enqueued, err := EnqueueReplyNotification(t.Context(), q, enq, reply)
			if err != nil {
				t.Fatalf("EnqueueReplyNotification: %v", err)
			}
			if enqueued != tt.wantEnqueue {
				t.Fatalf("enqueued = %v, want %v", enqueued, tt.wantEnqueue)
			}
			if got := jobCount(t, database); (got == 1) != tt.wantEnqueue {
				t.Fatalf("job_runs rows = %d, want enqueue=%v", got, tt.wantEnqueue)
			}
			if tt.wantEnqueue {
				var payload string
				if err := database.QueryRowContext(t.Context(),
					"SELECT payload FROM job_runs WHERE kind = ?", jobs.KindCommentReplyNotification).Scan(&payload); err != nil {
					t.Fatalf("read payload: %v", err)
				}
				want := `{"comment_id":` + strconv.FormatInt(reply.ID, 10) + `}`
				if payload != want {
					t.Errorf("payload = %s, want %s", payload, want)
				}
			}
		})
	}
}
