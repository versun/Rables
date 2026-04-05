class TwitterArchiveHandleSyncJob < ApplicationJob
  queue_as :default

  BATCH_SIZE = 100

  def self.enqueue_if_needed(wait_until: nil, retry_scheduled: false, include_in_progress: true)
    return unless pending_sync?
    return unless credentials_available?
    return if already_enqueued?(include_in_progress: include_in_progress)

    job = wait_until.present? ? set(wait_until: wait_until) : self
    job.perform_later(retry_scheduled: retry_scheduled)
  end

  def self.pending_sync?
    return false unless TwitterArchiveConnection.attribute_names.include?("screen_name")

    TwitterArchiveConnection.unresolved_screen_name.exists?
  end

  def self.already_enqueued?(include_in_progress: true)
    if Rails.env.test?
      ActiveJob::Base.queue_adapter.enqueued_jobs.any? { |job| job[:job] == self }
    else
      jobs = ActiveJob::Base.jobs

      relation_has_jobs?(jobs.pending.where(job_class_name: name)) ||
        relation_has_jobs?(jobs.scheduled.where(job_class_name: name)) ||
        (include_in_progress && relation_has_jobs?(jobs.in_progress.where(job_class_name: name)))
    end
  end

  def self.credentials_available?
    settings = Crosspost.twitter
    settings.enabled? &&
      settings.api_key.present? &&
      settings.api_key_secret.present? &&
      settings.access_token.present? &&
      settings.access_token_secret.present?
  end

  def self.relation_has_jobs?(relation)
    relation.respond_to?(:any?) && relation.any?
  end
  private_class_method :relation_has_jobs?

  def perform(retry_scheduled: false)
    return unless self.class.pending_sync?
    return unless self.class.credentials_available?

    service = TwitterService.new

    unresolved_account_ids.each_slice(BATCH_SIZE) do |account_ids|
      result = service.lookup_users_by_ids(account_ids)
      persist_users(result[:users])

      if result[:error_message].present?
        log_sync_failure(result[:error_message])
        break
      end

      retry_at = result.dig(:rate_limit, :reset_at) || result[:retry_at]
      next unless retry_at.present?

      schedule_retry(retry_at)
      break
    end
  end

  private

  def unresolved_account_ids
    TwitterArchiveConnection.unresolved_screen_name.distinct.order(:account_id).pluck(:account_id)
  end

  def persist_users(users)
    return if users.blank?

    now = Time.current
    users.each do |account_id, screen_name|
      TwitterArchiveConnection.where(account_id: account_id).update_all(screen_name: screen_name, updated_at: now)
    end
  end

  def schedule_retry(reset_at)
    return unless self.class.pending_sync?

    wait_until = reset_at || 15.minutes.from_now
    self.class.enqueue_if_needed(wait_until: wait_until, retry_scheduled: true, include_in_progress: false)
  end

  def log_sync_failure(error_message)
    ActivityLog.log!(
      action: :failed,
      target: :twitter_archive,
      level: :error,
      operation: "handle_sync",
      error: error_message
    )
  end
end
