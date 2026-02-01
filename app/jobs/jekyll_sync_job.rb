class JekyllSyncJob < ApplicationJob
  queue_as :default

  def perform
    record = JekyllSyncRecord.create!(
      sync_type: :full,
      status: :in_progress,
      started_at: Time.current
    )

    result = JekyllSyncService.new.sync_all
    record.update!(
      status: :completed,
      completed_at: Time.current,
      articles_count: result[:articles_count],
      pages_count: result[:pages_count],
      git_commit_sha: result[:git_commit_sha]
    )
  rescue => e
    record&.update!(status: :failed, completed_at: Time.current, error_message: e.message)
    Rails.event.notify("jekyll_sync_job.failed", component: self.class.name, error: e.message, level: "error")
    raise
  end
end
