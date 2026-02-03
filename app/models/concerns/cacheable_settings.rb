module CacheableSettings
  extend ActiveSupport::Concern

  def self.site_info
    Rails.cache.fetch("site_info", expires_in: 24.hours) do
      setting = Setting.first
      return {} unless setting

      {
        title: setting.title,
        description: setting.description,
        author: setting.author,
        url: setting.url,
        head_code: setting.head_code,
        footer: setting.footer,
        custom_css: setting.custom_css,
        social_links: setting.social_links,
        tool_code: setting.tool_code,
        giscus: setting.giscus,
        time_zone: setting.time_zone || "UTC"
      }
    end
  end

  def self.navbar_items
    Rails.cache.fetch("navbar_items", expires_in: 24.hours) do
      Page.published.order(page_order: :desc).select(:id, :title, :slug, :redirect_url)
    end
  end

  def self.refresh_site_info
    Rails.cache.delete("site_info")
  end

  def self.refresh_navbar_items
    Rails.cache.delete("navbar_items")
  end

  def self.refresh_all
    refresh_site_info
    refresh_navbar_items
    refresh_newsletter_setting
  end

  # Cache key version: v1
  # Cached fields: enabled, native
  # Remember to bump version if changing the returned hash structure
  def self.newsletter_setting
    Rails.cache.fetch("newsletter_setting:v1", expires_in: 24.hours) do
      setting = NewsletterSetting.first
      {
        enabled: setting&.enabled || false,
        native: setting&.native? || false
      }
    end
  end

  def self.refresh_newsletter_setting
    Rails.cache.delete("newsletter_setting:v1")
  end
end
