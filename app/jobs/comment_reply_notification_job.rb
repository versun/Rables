class CommentReplyNotificationJob < ApplicationJob
  include CacheableSettings
  include SmtpConfigurable
  queue_as :default

  RETRIED_ERRORS = [ *TransientNetworkErrors::TRANSIENT_ERRORS, Net::SMTPServerBusy ].freeze

  # 瞬时错误重试耗尽后只在块中记录一次最终失败，避免每次重试都写失败日志
  retry_on(*RETRIED_ERRORS, wait: :polynomially_longer, attempts: 3) do |job, error|
    job.log_failure(error)
  end

  def perform(comment_id)
    comment = Comment.find_by(id: comment_id)
    return unless comment
    return unless eligible_for_notification?(comment)

    newsletter_setting = NewsletterSetting.instance
    return unless newsletter_setting.enabled? && newsletter_setting.native? && newsletter_setting.configured?

    mail = CommentMailer.reply_notification(comment, CacheableSettings.site_info)
    apply_smtp_config_to_mail(mail, newsletter_setting)

    mail.deliver_now
    ActivityLog.log!(
      action: :sent,
      target: :comment_reply_notification,
      level: :info,
      comment_id: comment.id,
      email: comment.parent&.author_email,
      author: comment.author_name
    )
    Rails.event.notify "comment_reply_notification_job.email_sent",
      level: "info",
      component: "CommentReplyNotificationJob",
      comment_id: comment.id,
      parent_comment_id: comment.parent_id,
      recipient_email: comment.parent&.author_email
  rescue => e
    # retry_on 覆盖的瞬时错误由上面的块统一记录；未覆盖的错误在此记录一次
    log_failure(e, comment: comment) unless retried_error?(e)
    raise
  end

  def log_failure(error, comment: nil)
    comment_id = arguments.first
    comment ||= Comment.find_by(id: comment_id)
    ActivityLog.log!(
      action: :failed,
      target: :comment_reply_notification,
      level: :error,
      comment_id: comment_id,
      email: comment&.parent&.author_email,
      error: error.message
    )
    Rails.event.notify "comment_reply_notification_job.email_failed",
      level: "error",
      component: "CommentReplyNotificationJob",
      comment_id: comment_id,
      error_message: error.message
    Rails.event.notify "comment_reply_notification_job.error_backtrace",
      level: "error",
      component: "CommentReplyNotificationJob",
      backtrace: error.backtrace.join("\n") if error.backtrace
  end

  private

  def retried_error?(error)
    RETRIED_ERRORS.any? { |klass| error.is_a?(klass) }
  end

  def eligible_for_notification?(comment)
    return false unless comment.approved?
    return false unless comment.parent_id?
    return false unless comment.platform.nil?

    parent = comment.parent
    return false unless parent&.author_email.present?
    return false unless parent.platform.nil?

    if comment.author_email.present? && comment.author_email.casecmp?(parent.author_email.to_s)
      return false
    end

    true
  end
end
