package domain

// Status is the article/page status. Integer values mirror the Rails enum
// order (draft=0 publish=1 schedule=2 trash=3 shared=4); the DB and the
// Rails migration depend on these exact values.
type Status int

const (
	StatusDraft Status = iota
	StatusPublish
	StatusSchedule
	StatusTrash
	StatusShared
)

func (s Status) String() string {
	switch s {
	case StatusDraft:
		return "draft"
	case StatusPublish:
		return "publish"
	case StatusSchedule:
		return "schedule"
	case StatusTrash:
		return "trash"
	case StatusShared:
		return "shared"
	}
	return ""
}

// CommentStatus is the comment status (pending=0 approved=1 rejected=2).
type CommentStatus int

const (
	CommentPending CommentStatus = iota
	CommentApproved
	CommentRejected
)

func (s CommentStatus) String() string {
	switch s {
	case CommentPending:
		return "pending"
	case CommentApproved:
		return "approved"
	case CommentRejected:
		return "rejected"
	}
	return ""
}

// ContentType mirrors the Rails content_type enum ('rich_text'|'html').
type ContentType string

const (
	ContentTypeRichText ContentType = "rich_text"
	ContentTypeHTML     ContentType = "html"
)
