-- Newsletter send jobs (plan T19): recipient selection for the native
-- sender. The tag intersection itself runs in Go (subscribers.FilterRelevant),
-- mirroring the Ruby select in NativeNewsletterSenderJob#perform.
-- Article/subscriber/comment reads reuse GetArticleByID, ListTagsForArticle,
-- GetSubscriberByID, GetCommentByID and the GetCommentable* queries.

-- Subscriber.active.includes(:tags): every row; FilterRelevant drops the
-- inactive ones, so the scan stays unordered-by-state like the Rails scope.
-- name: ListAllSubscribers :many
SELECT * FROM subscribers ORDER BY id;

-- name: ListAllSubscriberTags :many
SELECT subscriber_id, tag_id FROM subscriber_tags ORDER BY id;
