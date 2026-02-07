class Setting < ApplicationRecord
  has_rich_text :footer
  before_save :parse_social_links_json
  after_commit :clear_settings_cache
  validates :url, presence: true, if: :setup_completed?

  # Virtual attribute for JSON textarea input
  attr_accessor :social_links_json

  # Check if initial setup is incomplete
  def self.setup_incomplete?
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
  end
end
