class TwitterArchiveImport < ApplicationRecord
  STATUSES = %w[queued running completed failed].freeze
  ACTIVE_STATUSES = %w[queued running].freeze

  before_validation :sync_active_slot

  validates :source_filename, presence: true
  validates :status, presence: true, inclusion: { in: STATUSES }
  validates :progress, presence: true, inclusion: { in: 0..100 }
  validates :queued_at, presence: true
  validates :active_slot, uniqueness: true, allow_nil: true, if: :supports_active_slot?

  scope :active, -> { where(status: ACTIVE_STATUSES) }
  scope :recent_first, -> { order(created_at: :desc) }
  scope :completed, -> { where(status: "completed") }

  def self.create_queued!(source_filename:, source_path:)
    create!(
      source_filename: source_filename,
      source_path: source_path,
      status: "queued",
      progress: 0,
      status_message: "Queued",
      queued_at: Time.current
    )
  end

  def self.last_imported_at
    completed.maximum(:finished_at) || TwitterArchiveTweet.maximum(:created_at)
  end

  def status_label
    status.to_s.humanize
  end

  def mark_running!
    update!(
      status: "running",
      progress: 5,
      status_message: "Reading archive",
      started_at: Time.current,
      finished_at: nil,
      error_message: nil
    )
  end

  def update_import_progress!(progress, message)
    update!(
      progress: progress.to_i.clamp(0, 100),
      status_message: message
    )
  end

  def complete_import!(tweets:, followers:, following:, likes:, total_items:)
    update!(
      status: "completed",
      progress: 100,
      status_message: "Import completed",
      tweets_count: tweets,
      followers_count: followers,
      following_count: following,
      likes_count: likes,
      total_items_count: total_items,
      finished_at: Time.current,
      error_message: nil
    )
  end

  def fail_import!(error)
    update!(
      status: "failed",
      status_message: "Import failed",
      error_message: error.message,
      finished_at: Time.current
    )
  end

  def cleanup_source_file!
    path = source_path.to_s
    if TwitterArchiveImportSubmission.direct_upload_source_path?(path)
      TwitterArchiveImportSubmission.direct_upload_blob_from(path)&.purge
    elsif path.present? && File.exist?(path)
      File.delete(path)
    end

    if persisted?
      update_column(:source_path, nil)
    else
      self.source_path = nil
    end
  end

  private

  def sync_active_slot
    return unless supports_active_slot?

    self.active_slot = ACTIVE_STATUSES.include?(status.to_s) ? 1 : nil
  end

  def supports_active_slot?
    has_attribute?(:active_slot)
  end
end
