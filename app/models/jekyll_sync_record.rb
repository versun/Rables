# frozen_string_literal: true

class JekyllSyncRecord < ApplicationRecord
  # Enums
  enum :sync_type, { full: "full", incremental: "incremental", single: "single" }, prefix: true
  enum :status, { pending: "pending", in_progress: "in_progress", completed: "completed", failed: "failed" }, prefix: true
  enum :triggered_by, { manual: "manual", auto: "auto", publish: "publish" }, prefix: true

  # Validations
  validates :sync_type, presence: true
  validates :status, presence: true

  # Scopes
  scope :recent, -> { order(created_at: :desc) }
  scope :successful, -> { where(status: :completed) }
  scope :failed_records, -> { where(status: :failed) }

  # Callbacks
  before_create :set_started_at

  # Status helpers
  def mark_in_progress!
    update!(status: :in_progress, started_at: Time.current)
  end

  def mark_completed!(articles: 0, pages: 0, attachments: 0, git_sha: nil)
    update!(
      status: :completed,
      completed_at: Time.current,
      articles_count: articles,
      pages_count: pages,
      attachments_count: attachments,
      git_commit_sha: git_sha
    )
  end

  def mark_failed!(message)
    update!(
      status: :failed,
      completed_at: Time.current,
      error_message: message
    )
  end

  # Duration calculation
  def duration
    return nil unless started_at

    end_time = completed_at || Time.current
    end_time - started_at
  end

  def duration_in_words
    seconds = duration
    return nil unless seconds

    if seconds < 60
      "#{seconds.round(1)} seconds"
    elsif seconds < 3600
      "#{(seconds / 60).round(1)} minutes"
    else
      "#{(seconds / 3600).round(1)} hours"
    end
  end

  # Details accessors (JSON)
  def details_hash
    return {} if details.blank?

    JSON.parse(details)
  rescue JSON::ParserError
    {}
  end

  def details_hash=(hash)
    self.details = hash.to_json
  end

  def add_detail(key, value)
    current = details_hash
    current[key.to_s] = value
    self.details_hash = current
  end

  private

  def set_started_at
    self.started_at ||= Time.current if status_in_progress?
  end
end
