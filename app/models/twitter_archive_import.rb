class TwitterArchiveImport < ApplicationRecord
  STATUSES = %w[queued running completed failed].freeze
  ACTIVE_STATUSES = %w[queued running].freeze

  before_validation :sync_active_slot

  validates :source_filename, presence: true
  validates :status, presence: true, inclusion: { in: STATUSES }
  validates :progress, presence: true, inclusion: { in: 0..100 }
  validates :queued_at, presence: true
  validates :active_slot, uniqueness: true, allow_nil: true

  scope :active, -> { where(status: ACTIVE_STATUSES) }
  scope :recent_first, -> { order(created_at: :desc) }
  scope :completed, -> { where(status: "completed") }

  def status_label
    status.to_s.humanize
  end

  private

  def sync_active_slot
    self.active_slot = ACTIVE_STATUSES.include?(status.to_s) ? 1 : nil
  end
end
