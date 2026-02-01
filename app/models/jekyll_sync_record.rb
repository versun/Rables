class JekyllSyncRecord < ApplicationRecord
  SYNC_TYPES = %w[full incremental single].freeze
  STATUSES = %w[pending in_progress completed failed].freeze

  validates :sync_type, inclusion: { in: SYNC_TYPES }
  validates :status, inclusion: { in: STATUSES }

  scope :recent, -> { order(created_at: :desc) }
  scope :completed, -> { where(status: "completed") }
  scope :failed, -> { where(status: "failed") }
  scope :in_progress, -> { where(status: "in_progress") }

  # Mark sync as started
  def mark_started!
    update!(status: "in_progress", started_at: Time.current)
  end

  # Mark sync as completed
  def mark_completed!(commit_sha: nil)
    update!(
      status: "completed",
      completed_at: Time.current,
      git_commit_sha: commit_sha
    )
  end

  # Mark sync as failed
  def mark_failed!(error_message)
    update!(
      status: "failed",
      completed_at: Time.current,
      error_message: error_message
    )
  end

  # Calculate duration in seconds
  def duration
    return nil unless started_at
    return nil unless completed_at

    (completed_at - started_at).round(2)
  end

  # Check if sync was successful
  def successful?
    status == "completed"
  end

  # Get summary of synced items
  def summary
    parts = []
    parts << "#{articles_count} articles" if articles_count.to_i > 0
    parts << "#{pages_count} pages" if pages_count.to_i > 0
    parts.join(", ").presence || "No items"
  end
end
