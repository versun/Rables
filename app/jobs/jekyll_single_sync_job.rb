class JekyllSingleSyncJob < ApplicationJob
  queue_as :default

  def perform(record_type, record_id)
    record = JekyllSyncRecord.create!(
      sync_type: :single,
      status: :in_progress,
      started_at: Time.current
    )

    service = JekyllSyncService.new
    git_commit_sha = nil
    case record_type
    when "Article"
      article = Article.find(record_id)
      git_commit_sha = service.sync_article(article)
      record.update!(articles_count: 1)
    when "Page"
      page = Page.find(record_id)
      git_commit_sha = service.sync_page(page)
      record.update!(pages_count: 1)
    else
      raise ArgumentError, "Unknown record type: #{record_type}"
    end

    record.update!(status: :completed, completed_at: Time.current, git_commit_sha: git_commit_sha)
  rescue => e
    record&.update!(status: :failed, completed_at: Time.current, error_message: e.message)
    Rails.event.notify("jekyll_single_sync_job.failed", component: self.class.name, error: e.message, level: "error")
    raise
  end
end
