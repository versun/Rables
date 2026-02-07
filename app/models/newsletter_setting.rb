class NewsletterSetting < ApplicationRecord
  # Singleton pattern - use implicit_order_column to avoid Rails 8.1 ordering warnings
  self.implicit_order_column = :id

  has_rich_text :footer

  validates :provider, inclusion: { in: %w[native listmonk] }
  validates :from_email, presence: true, format: { with: URI::MailTo::EMAIL_REGEXP }, if: -> { enabled? && provider == "native" }

  def self.instance
    first_or_initialize
  end

  def native?
    provider == "native"
  end

  def listmonk?
    provider == "listmonk"
  end

  def configured?
    return false unless enabled?

    if native?
      smtp_address.present? && smtp_port.present? && smtp_user_name.present? &&
      smtp_password.present? && from_email.present?
    else
      listmonk = Listmonk.first
      listmonk&.configured?
    end
  end

  def missing_config_fields
    return [] unless enabled?

    if native?
      missing = []
      missing << "smtp_address" unless smtp_address.present?
      missing << "smtp_port" unless smtp_port.present?
      missing << "smtp_user_name" unless smtp_user_name.present?
      missing << "smtp_password" unless smtp_password.present?
      missing << "from_email" unless from_email.present?
      missing
    else
      listmonk = Listmonk.first
      listmonk ? [] : [ "listmonk configuration" ]
    end
  end
end
