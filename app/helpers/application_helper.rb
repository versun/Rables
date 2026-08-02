require "uri"

module ApplicationHelper
  include Sanitization

  def site_settings
    @site_settings ||= CacheableSettings.site_info
  end

  def rails_api_url
    # Get Rails API URL from environment variable or use site URL as fallback
    api_url = ENV.fetch("RAILS_API_URL", nil)
    if api_url.present?
      api_url = api_url.chomp("/")
      api_url = "https://#{api_url}" unless api_url.match?(%r{^https?://})
      # In development, force HTTP for localhost to avoid SSL connection errors
      if Rails.env.development? && api_url.include?("localhost") && api_url.start_with?("https://")
        api_url = api_url.sub("https://", "http://")
      end
      return api_url
    end

    # Fallback to site URL if no API URL is configured
    site_url = site_settings[:url].presence || "http://localhost:3000"
    site_url = site_url.chomp("/")

    # Ensure URL has a protocol
    site_url = "http://#{site_url}" unless site_url.match?(%r{^https?://})

    # In development, force HTTP for localhost to avoid SSL connection errors
    # This prevents "server unexpectedly closed connection" errors when
    # site_settings[:url] is configured with HTTPS but local server only supports HTTP
    if Rails.env.development?
      uri = URI.parse(site_url)
      if uri.host == "localhost" || uri.host == "127.0.0.1" || uri.host&.start_with?("127.")
        site_url = site_url.sub(/^https:/, "http:")
      end
    end

    site_url
  end

  def normalized_site_url
    raw_url = site_settings[:url].to_s.strip
    return "" if raw_url.blank?

    site_url = raw_url.chomp("/")
    site_url = "https://#{site_url}" unless site_url.match?(%r{^https?://})
    site_url
  end

  # Build an absolute URL for og:image / twitter:image from a possibly relative path:
  # - "/uploads/a.png"   -> "<normalized site url>/uploads/a.png"
  # - "https://…/a.png"  -> returned unchanged
  # - "uploads/a.png"    -> "<normalized site url>/uploads/a.png" (leading slash added)
  # Falls back to the original path when no site URL is configured.
  def absolute_og_image(path)
    path = path.to_s.strip
    return path if path.blank? || path.match?(%r{^https?://})

    site_url = normalized_site_url
    return path if site_url.blank?

    path.start_with?("/") ? "#{site_url}#{path}" : "#{site_url}/#{path}"
  end

  # Safely render HTML content by sanitizing dangerous tags while preserving common formatting
  def safe_html_content(html_content)
    return "".html_safe if html_content.blank?

    sanitized = sanitize(html_content.to_s, tags: Sanitization::ALLOWED_HTML_TAGS, attributes: Sanitization::ALLOWED_HTML_ATTRIBUTES)

    # Add loading="lazy" to all images for better performance
    add_lazy_loading_to_images(sanitized)
  end
end
