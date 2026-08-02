class Setting < ApplicationRecord
  # Singleton pattern - use implicit_order_column to avoid Rails 8.1 ordering warnings
  self.implicit_order_column = :id

  has_rich_text :footer
  before_save :parse_social_links_json
  after_commit :clear_settings_cache
  validates :url, presence: true, if: :setup_completed?

  # Virtual attribute for JSON textarea input
  attr_accessor :social_links_json

  SETUP_INCOMPLETE_CACHE_KEY = "setting:setup_incomplete"

  # Check if initial setup is incomplete (cached briefly; called on every request)
  def self.setup_incomplete?
    Rails.cache.fetch(SETUP_INCOMPLETE_CACHE_KEY, expires_in: 5.minutes) do
      setup_incomplete_fresh?
    end
  end

  # Uncached variant for the setup transaction re-check, where a stale cached
  # value would weaken the TOCTOU guard
  def self.setup_incomplete_fresh?
    User.count.zero? || Setting.first_or_create.setup_completed == false
  end

  private

  def parse_social_links_json
    # If social_links_json is provided, parse it and update social_links
    if social_links_json.present?
      begin
        parsed_data = JSON.parse(social_links_json)
        self.social_links = parsed_data if parsed_data.is_a?(Hash)
      rescue JSON::ParserError => e
        errors.add(:social_links_json, "包含无效的 JSON 格式: #{e.message}")
        throw :abort
      end
    end
  end

  def clear_settings_cache
    CacheableSettings.refresh_site_info
    Rails.cache.delete(SETUP_INCOMPLETE_CACHE_KEY)
  end
end
