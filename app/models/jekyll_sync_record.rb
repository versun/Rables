class JekyllSyncRecord < ApplicationRecord
  enum :sync_type, { full: "full", incremental: "incremental", single: "single" }, validate: true
  enum :status, { pending: "pending", in_progress: "in_progress", completed: "completed", failed: "failed" }, validate: true

  validates :sync_type, presence: true
  validates :status, presence: true
end
