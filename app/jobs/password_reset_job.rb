class PasswordResetJob < ApplicationJob
  include SmtpConfigurable
  queue_as :default

  RETRIED_ERRORS = [ *TransientNetworkErrors::TRANSIENT_ERRORS, Net::SMTPServerBusy ].freeze

  # 瞬时错误重试耗尽后只在块中记录一次最终失败，避免每次重试都写失败日志
  retry_on(*RETRIED_ERRORS, wait: :polynomially_longer, attempts: 3) do |job, error|
    job.log_failure(error)
  end

  def perform(user_id)
    user = User.find(user_id)
    newsletter_setting = NewsletterSetting.instance

    mail = PasswordsMailer.reset(user)

    # 应用 SMTP 配置到邮件对象
    apply_smtp_config_to_mail(mail, newsletter_setting)

    mail.deliver_now
    Rails.event.notify "password_reset_job.email_sent",
      level: "info",
      component: "PasswordResetJob",
      user_email: user.email_address
  rescue ActiveRecord::RecordNotFound => e
    Rails.event.notify "password_reset_job.user_not_found",
      level: "error",
      component: "PasswordResetJob",
      user_id: user_id,
      error_message: e.message
  rescue => e
    # retry_on 覆盖的瞬时错误由上面的块统一记录；未覆盖的错误在此记录一次
    log_failure(e, user: user) unless retried_error?(e)
    raise
  end

  def log_failure(error, user: nil)
    user ||= User.find_by(id: arguments.first)
    Rails.event.notify "password_reset_job.email_failed",
      level: "error",
      component: "PasswordResetJob",
      user_email: user&.email_address,
      error_message: error.message
    Rails.event.notify "password_reset_job.error_backtrace",
      level: "error",
      component: "PasswordResetJob",
      backtrace: error.backtrace.join("\n") if error.backtrace
  end

  private

  def retried_error?(error)
    RETRIED_ERRORS.any? { |klass| error.is_a?(klass) }
  end
end
