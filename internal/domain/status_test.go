package domain

import "testing"

func TestStatusValues(t *testing.T) {
	tests := []struct {
		status Status
		value  int
		name   string
	}{
		{StatusDraft, 0, "draft"},
		{StatusPublish, 1, "publish"},
		{StatusSchedule, 2, "schedule"},
		{StatusTrash, 3, "trash"},
		{StatusShared, 4, "shared"},
	}
	for _, tt := range tests {
		if int(tt.status) != tt.value {
			t.Errorf("Status %s = %d, want %d", tt.name, int(tt.status), tt.value)
		}
		if tt.status.String() != tt.name {
			t.Errorf("Status(%d).String() = %q, want %q", tt.value, tt.status.String(), tt.name)
		}
	}
}

func TestCommentStatusValues(t *testing.T) {
	tests := []struct {
		status CommentStatus
		value  int
		name   string
	}{
		{CommentPending, 0, "pending"},
		{CommentApproved, 1, "approved"},
		{CommentRejected, 2, "rejected"},
	}
	for _, tt := range tests {
		if int(tt.status) != tt.value {
			t.Errorf("CommentStatus %s = %d, want %d", tt.name, int(tt.status), tt.value)
		}
		if tt.status.String() != tt.name {
			t.Errorf("CommentStatus(%d).String() = %q, want %q", tt.value, tt.status.String(), tt.name)
		}
	}
}

func TestContentTypeValues(t *testing.T) {
	if ContentTypeRichText != "rich_text" || ContentTypeHTML != "html" {
		t.Errorf("unexpected content types: %q %q", ContentTypeRichText, ContentTypeHTML)
	}
}
