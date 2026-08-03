-- Singleton settings row (id = 1). GetSettings and EnsureSettings live in
-- auth.sql; this file adds the admin update.

-- name: UpdateSettings :exec
UPDATE settings
SET title = ?, description = ?, author = ?, url = ?, time_zone = ?,
    head_code = ?, custom_css = ?, tool_code = ?, giscus = ?,
    social_links = ?, updated_at = ?
WHERE id = 1;
