-- name: GetTagByID :one
SELECT * FROM tags WHERE id = ?;

-- Case-insensitive lookup mirroring Tag.find_or_create_by_names'
-- where("LOWER(name) = ?", name.downcase). SQLite LOWER folds ASCII only,
-- exactly like the Rails app running on SQLite.
-- name: GetTagByLowerName :one
SELECT * FROM tags WHERE LOWER(name) = LOWER(?);

-- Slug-uniqueness probe for Tag#generate_slug's "-1", "-2", ... suffix loop
-- (excludeID plays the role of where.not(id: id || 0)).
-- name: CountTagsBySlug :one
SELECT COUNT(*) FROM tags WHERE slug = ? AND id != ?;

-- name: CreateTag :one
INSERT INTO tags (name, slug, created_at, updated_at) VALUES (?, ?, ?, ?) RETURNING *;

-- Race-safe insert for find-or-create: the name check and the insert are one
-- atomic statement (SQLite serializes writers), so even case-variant names
-- ("Go" vs "go", which the case-sensitive UNIQUE(name) would both admit)
-- cannot duplicate; the row is re-read afterwards, mirroring the
-- RecordNotUnique rescue. A slug conflict still errors so the caller loops.
-- name: InsertTagIfAbsent :exec
INSERT INTO tags (name, slug, created_at, updated_at)
SELECT ?, ?, ?, ?
WHERE NOT EXISTS (SELECT 1 FROM tags WHERE LOWER(name) = LOWER(sqlc.arg(lower_name)));

-- Admin index: Tag.alphabetical.left_joins(:articles) with articles_total.
-- name: ListTagsWithArticleCount :many
SELECT tags.*, COUNT(articles.id) AS articles_total
FROM tags
LEFT JOIN article_tags ON article_tags.tag_id = tags.id
LEFT JOIN articles ON articles.id = article_tags.article_id
GROUP BY tags.id
ORDER BY tags.name;

-- Rename keeps the slug: Tag#generate_slug only fills blank slugs.
-- name: UpdateTagName :exec
UPDATE tags SET name = ?, updated_at = ? WHERE id = ?;

-- name: DeleteTag :exec
DELETE FROM tags WHERE id = ?;

-- name: DeleteArticleTagsByTagID :exec
DELETE FROM article_tags WHERE tag_id = ?;

-- name: DeleteSubscriberTagsByTagID :exec
DELETE FROM subscriber_tags WHERE tag_id = ?;

-- Tag#touch_articles: busts the render cache keyed on article id+updated_at.
-- name: TouchArticlesByTagID :exec
UPDATE articles SET updated_at = ? WHERE id IN (SELECT article_id FROM article_tags WHERE tag_id = ?);
