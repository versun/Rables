xml.instruct! :xml, version: "1.0", encoding: "UTF-8"
xml.urlset xmlns: "http://www.sitemaps.org/schemas/sitemap/0.9" do
  raw_site_url = site_settings[:url].to_s.strip
  site_url = raw_site_url.presence&.chomp("/")
  site_url = "https://#{site_url}" if site_url.present? && !site_url.match?(%r{^https?://})

  # Sitemap locs must be absolute URLs; without a configured site URL there is
  # nothing valid to emit, so skip all entries instead of outputting relative locs.
  if site_url.present?
    latest_article_update = @articles.maximum(:updated_at)

    xml.url do
      xml.loc site_url # site root URL
      xml.lastmod((latest_article_update || Time.now).strftime("%Y-%m-%d")) # last modified at
      xml.changefreq "daily" # change frequency
      xml.priority 1.0 # priority
    end

    @pages.each do |post|
      xml.url do
        xml.loc "#{site_url}/pages/#{post.slug}"
        xml.lastmod post.updated_at.strftime("%Y-%m-%d")
        xml.changefreq "weekly"
        xml.priority 0.8
      end
    end

    @articles.each do |post|
      xml.url do
        article_path = [ Rails.application.config.x.article_route_prefix, post.slug ].reject(&:blank?).join("/")
        article_path = "/#{article_path}" unless article_path.start_with?("/")
        xml.loc "#{site_url}#{article_path}"
        xml.lastmod post.updated_at.strftime("%Y-%m-%d")
        xml.changefreq "weekly"
        xml.priority 0.8
      end
    end
  end
end
