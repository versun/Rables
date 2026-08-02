class NewsletterConfirmationJob < ApplicationJob
  include CacheableSettings
  include SmtpConfigurable
  queue_as :default

  RETRIED_ERRORS = [ *TransientNetworkErrors::TRANSIENT_ERRORS, Net::SMTPServerBusy ].freeze

  # 瞬时错误重试耗尽后只在块中记录一次最终失败，避免每次重试都写失败日志
  retry_on(*RETRIED_ERRORS, wait: :polynomially_longer, attempts: 3) do |job, error|
    job.log_failure(error)
  end

  def perform(subscriber_id)
    subscriber = Subscriber.find(subscriber_id)
    site_info = CacheableSettings.site_info
    newsletter_setting = NewsletterSetting.instance

    mail = NewsletterMailer.confirmation_email(subscriber, site_info)

    # 应用 SMTP 配置到邮件对象
    apply_smtp_config_to_mail(mail, newsletter_setting)

    mail.deliver_now
    Rails.event.notify "newsletter_confirmation_job.email_sent",
      level: "info",
      component: "NewsletterConfirmationJob",
      subscriber_email: subscriber.email
  rescue ActiveRecord::RecordNotFound => e
    Rails.event.notify "newsletter_confirmation_job.subscriber_not_found",
      level: "error",
      component: "NewsletterConfirmationJob",
      subscriber_id: subscriber_id,
      error_message: e.message
  rescue => e
    # retry_on 覆盖的瞬时错误由上面的块统一记录；未覆盖的错误在此记录一次
    log_failure(e, subscriber: subscriber) unless retried_error?(e)
    raise
  end

  def log_failure(error, subscriber: nil)
    subscriber ||= Subscriber.find_by(id: arguments.first)
    Rails.event.notify "newsletter_confirmation_job.email_failed",
      level: "error",
      component: "NewsletterConfirmationJob",
      subscriber_email: subscriber&.email,
      subscriber_id: arguments.first,
      error_message: error.message
    Rails.event.notify "newsletter_confirmation_job.error_backtrace",
      level: "error",
      component: "NewsletterConfirmationJob",
      backtrace: error.backtrace.join("\n") if error.backtrace
  end

  private

  def retried_error?(error)
    RETRIED_ERRORS.any? { |klass| error.is_a?(klass) }
  end
end
