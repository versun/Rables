class ApplicationMailer < ActionMailer::Base
  layout "mailer"

  def self.default_from_email
    if NewsletterSetting.table_exists?
      setting = NewsletterSetting.instance
      return setting.from_email if SmtpConfigurable.active?(setting)
    end
    "from@example.com"
  end

  # 使用 lambda 延迟求值，确保每封邮件都读取最新的 newsletter 设置
  default from: -> { ApplicationMailer.default_from_email }
end
