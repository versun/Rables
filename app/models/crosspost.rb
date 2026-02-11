class Crosspost < ApplicationRecord
  # encrypts :access_token, :refresh_token, :client_id, :client_secret,
  #          :client_key, :app_password, :username
  # Uncomment when Active Record encryption is configured
  PLATFORMS = %w[mastodon twitter bluesky xiaohongshu].freeze

  PLATFORM_ICONS = {
    "mastodon" => "fa-brands fa-mastodon",
    "twitter" => "fa-brands fa-square-x-twitter",
    "bluesky" => "fa-brands fa-square-bluesky",
    "xiaohongshu" => "svg:xiaohongshu"
  }.freeze

  validates :platform, presence: true,
                      uniqueness: true,
                      inclusion: { in: PLATFORMS }

  validates :client_key, :client_secret, :access_token, presence: true, if: -> { mastodon? && enabled? }
  validate :twitter_credentials_presence, if: -> { twitter? && enabled? }
  validates :username, :app_password, presence: true, if: -> { bluesky? && enabled? }
  validate :server_url_http_format, if: -> { server_url.present? }

  scope :mastodon, -> { find_or_create_by(platform: "mastodon") }
  scope :twitter, -> { find_or_create_by(platform: "twitter") }
  scope :bluesky, -> { find_or_create_by(platform: "bluesky") }
  scope :xiaohongshu, -> { find_or_create_by(platform: "xiaohongshu") }

  def mastodon?
    platform == "mastodon"
  end

  def twitter?
    platform == "twitter"
  end

  def bluesky?
    platform == "bluesky"
  end

  def xiaohongshu?
    platform == "xiaohongshu"
  end

  def enabled?
    enabled == true
  end

  # 获取平台默认的最大字符数
  def default_max_characters
    case platform
    when "mastodon"
      500
    when "twitter"
      250
    when "bluesky"
      300
    else
      300
    end
  end

  # 获取有效的最大字符数（如果未设置则使用默认值）
  def effective_max_characters
    max_characters || default_max_characters
  end

  private

  def twitter_credentials_presence
    oauth2_valid = client_id.present? && client_secret.present? && access_token.present? && refresh_token.present?
    return if oauth2_valid

    errors.add(:base, "Twitter requires OAuth 2.0 credentials (client id/secret + access token + refresh token)")
  end

  def server_url_http_format
    uri = URI.parse(server_url.to_s.strip)
    unless uri.is_a?(URI::HTTP) && uri.host.present?
      errors.add(:server_url, "must be a valid http(s) URL")
      return
    end

    errors.add(:server_url, "must not include credentials") if uri.userinfo.present?
  rescue URI::InvalidURIError
    errors.add(:server_url, "must be a valid http(s) URL")
  end
end
