require "uri"

module ApplicationHelper
  # 检测是否是手机设备（通过 User-Agent）
  def mobile_device?
    return @is_mobile_device if defined?(@is_mobile_device)

    user_agent = request.user_agent.to_s.downcase
    @is_mobile_device = user_agent.match?(/mobile|android|iphone|ipod|webos|blackberry|opera mini|iemobile/)
  end

  def site_settings
    CacheableSettings.site_info
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

  def public_article_path(article_or_slug)
    slug = article_or_slug.respond_to?(:slug) ? article_or_slug.slug : article_or_slug
    prefix = Rails.application.config.x.article_route_prefix.to_s
    prefix = prefix.delete_prefix("/")
    segments = [prefix.presence, slug.to_s].compact
    "/#{segments.join("/")}"
  end

  def public_page_path(page_or_slug)
    page = page_or_slug
    return page.redirect_url if page.respond_to?(:redirect?) && page.redirect?

    slug = page.respond_to?(:slug) ? page.slug : page
    "/pages/#{slug}"
  end

  def public_article_url(article_or_slug)
    site_url = normalized_site_url
    path = public_article_path(article_or_slug)
    site_url.present? ? "#{site_url}#{path}" : path
  end

  def public_page_url(page_or_slug)
    site_url = normalized_site_url
    path = public_page_path(page_or_slug)
    return path if path.to_s.match?(%r{^https?://})

    site_url.present? ? "#{site_url}#{path}" : path
  end

  def public_commentable_url(commentable)
    commentable.is_a?(Page) ? public_page_url(commentable) : public_article_url(commentable)
  end

  # Safely render HTML content by sanitizing dangerous tags while preserving common formatting
  def safe_html_content(html_content)
    return "".html_safe if html_content.blank?

    sanitized = sanitize(html_content.to_s, tags: allowed_html_tags, attributes: allowed_html_attributes)

    # Add loading="lazy" to all images for better performance
    doc = Nokogiri::HTML5.fragment(sanitized)
    doc.css("img").each do |img|
      img.set_attribute("loading", "lazy") unless img["loading"].present?
    end
    doc.to_html.html_safe
  end

  private

  # List of allowed HTML tags for content rendering.
  def allowed_html_tags
    %w[
      p br div span
      h1 h2 h3 h4 h5 h6
      a img
      ul ol li dl dt dd
      table thead tbody tfoot tr th td caption colgroup col
      strong b em i u s strike del ins mark small
      blockquote q cite pre code kbd samp var
      hr
      figure figcaption
      article section aside header footer nav main
      details summary
      abbr address time
      sub sup
      ruby rt rp
      iframe video audio source
    ]
  end

  # List of allowed HTML attributes.
  def allowed_html_attributes
    %w[
      href src alt title class id style
      target rel
      width height
      colspan rowspan
      data-controller data-action data-target
      loading
      controls autoplay loop muted
      frameborder allow allowfullscreen
      name content
    ]
  end
end
