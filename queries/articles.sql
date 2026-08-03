-- name: GetArticleByID :one
SELECT * FROM articles WHERE id = ?;
