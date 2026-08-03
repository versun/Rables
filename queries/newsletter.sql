-- Newsletter settings singleton (id = 1), mirroring NewsletterSetting.instance,
-- and the listmonk singleton (id = 1), mirroring Listmonk.first_or_initialize.
-- Reads reuse GetNewsletterSettings / GetListmonk from articles_admin.sql.

-- name: EnsureNewsletterSetting :exec
INSERT INTO newsletter_settings (id, created_at, updated_at)
VALUES (1, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: UpdateNewsletterSetting :exec
UPDATE newsletter_settings
SET enabled = ?, provider = ?, from_email = ?,
    smtp_address = ?, smtp_port = ?, smtp_user_name = ?, smtp_password = ?,
    smtp_domain = ?, smtp_authentication = ?, smtp_enable_starttls = ?,
    updated_at = ?
WHERE id = 1;

-- name: EnsureListmonk :exec
INSERT INTO listmonks (id, created_at, updated_at)
VALUES (1, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: UpdateListmonk :exec
UPDATE listmonks
SET url = ?, username = ?, api_key = ?, list_id = ?, template_id = ?,
    enabled = ?, updated_at = ?
WHERE id = 1;
