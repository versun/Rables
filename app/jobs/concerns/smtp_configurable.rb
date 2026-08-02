module SmtpConfigurable
  extend ActiveSupport::Concern

  def self.active?(newsletter_setting)
    newsletter_setting&.enabled? && newsletter_setting.native? && newsletter_setting.configured?
  end

  private

  def prepare_smtp_config(newsletter_setting)
    return {} unless newsletter_setting&.configured?

    domain = newsletter_setting.smtp_domain.presence || newsletter_setting.from_email&.split("@")&.last
    authentication = newsletter_setting.smtp_authentication.presence || "plain"

    # 转换认证类型为符号
    auth_type = case authentication.to_s.downcase
    when "plain"
      :plain
    when "login"
      :login
    when "cram_md5"
      :cram_md5
    else
      :plain
    end

    {
      address: newsletter_setting.smtp_address,
      port: newsletter_setting.smtp_port || 587,
      domain: domain,
      user_name: newsletter_setting.smtp_user_name,
      password: newsletter_setting.smtp_password,
      authentication: auth_type,
      enable_starttls_auto: newsletter_setting.smtp_enable_starttls != false
    }
  end

  def apply_smtp_config_to_mail(mail, newsletter_setting)
    return mail unless SmtpConfigurable.active?(newsletter_setting)

    smtp_config = prepare_smtp_config(newsletter_setting)
    return mail unless smtp_config[:address].present?

    mail.delivery_method(:smtp, smtp_config)
    Rails.event.notify "smtp_configurable.mail_configured",
      level: "info",
      component: "SmtpConfigurable",
      smtp_address: newsletter_setting.smtp_address,
      smtp_port: newsletter_setting.smtp_port
    mail
  end
end
