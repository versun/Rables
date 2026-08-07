# frozen_string_literal: true

# A background export run (Full Backup or Go migration package) triggered
# from Admin → Migrate. The artifact is stored as a single ZIP directly under
# tmp/exports (purged after a few days by CleanOldExportsJob).
class Export < ApplicationRecord
  enum :kind, { backup: "backup", site_export: "site_export" }
  enum :status, { pending: 0, running: 1, completed: 2, failed: 3 }

  validates :filename, presence: true, if: :completed?

  # A pending/running row whose job worker disappeared (deploy, OOM kill)
  # would sit there forever, and the export tab would keep auto-refreshing
  # waiting for it. The admin export tab calls this on every visit and
  # CleanOldExportsJob calls it daily to fail such rows.
  STALE_AFTER = 6.hours

  def self.fail_stale!(threshold: STALE_AFTER.ago)
    where(status: %i[pending running])
      .where("updated_at < ?", threshold)
      .update_all(status: statuses[:failed], error: "Export worker stopped before finishing; marked failed automatically", updated_at: Time.current)
  end

  def file_path
    return if filename.blank?

    # filename is generated server-side; File.basename is belt-and-braces
    # against path traversal.
    Rails.root.join("tmp", "exports", File.basename(filename))
  end

  def file_available?
    completed? && file_path && File.file?(file_path)
  end
end
