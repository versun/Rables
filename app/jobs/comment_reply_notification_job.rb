class CommentReplyNotificationJob < ApplicationJob
  include CacheableSettings
  include SmtpConfigurable
  queue_as :default

  def perform(comment_id)
    comment = Comment.find_by(id: comment_id)
    return unless comment
    return unless eligible_for_notification?(comment)

    newsletter_setting = NewsletterSetting.instance
    return unless newsletter_setting.enabled? && newsletter_setting.native? && newsletter_setting.configured?

    configure_action_mailer(newsletter_setting) unless Rails.env.test?

    mail = CommentMailer.reply_notification(comment, CacheableSettings.site_info)
    apply_smtp_config_to_mail(mail, newsletter_setting) unless Rails.env.test?

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
    ActivityLog.log!(
      action: :failed,
      target: :comment_reply_notification,
      level: :error,
      comment_id: comment_id,
      email: comment&.parent&.author_email,
      error: e.message
    )
    Rails.event.notify "comment_reply_notification_job.email_failed",
      level: "error",
      component: "CommentReplyNotificationJob",
      comment_id: comment_id,
      error_message: e.message
    Rails.event.notify "comment_reply_notification_job.error_backtrace",
      level: "error",
      component: "CommentReplyNotificationJob",
      backtrace: e.backtrace.join("\n") if e.backtrace
    raise
  end

  private

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
